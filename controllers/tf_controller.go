/*
Copyright 2021.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controllers

import (
	"bytes"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/dependency"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/metrics"
	"github.com/fluxcd/pkg/runtime/predicates"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/hashicorp/go-retryablehttp"
	infrav1 "github.com/weaveworks/tf-controller/api/v1alpha1"
	"github.com/weaveworks/tf-controller/mtls"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	kuberecorder "k8s.io/client-go/tools/record"
	"k8s.io/client-go/tools/reference"
	"sigs.k8s.io/cli-utils/pkg/kstatus/polling"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"
)

// TerraformReconciler reconciles a Terraform object
type TerraformReconciler struct {
	client.Client
	httpClient        *retryablehttp.Client
	statusManager     string
	requeueDependency time.Duration

	EventRecorder            kuberecorder.EventRecorder
	MetricsRecorder          *metrics.Recorder
	StatusPoller             *polling.StatusPoller
	Scheme                   *runtime.Scheme
	CertRotator              *mtls.CertRotator
	RunnerGRPCPort           int
	RunnerCreationTimeout    time.Duration
	RunnerGRPCMaxMessageSize int
}

//+kubebuilder:rbac:groups=infra.contrib.fluxcd.io,resources=terraforms,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=infra.contrib.fluxcd.io,resources=terraforms/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=infra.contrib.fluxcd.io,resources=terraforms/finalizers,verbs=get;create;update;patch;delete
//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=buckets;gitrepositories;ocirepositories,verbs=get;list;watch
//+kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=buckets/status;gitrepositories/status;ocirepositories/status,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps;secrets;serviceaccounts,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
// TODO(user): Modify the Reconcile function to compare the state specified by
// the Terraform object against the actual cluster state, and then
// perform operations to make the cluster state reflect the state specified by
// the user.
//
// For more details, check Reconcile and its Result here:
// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.10.0/pkg/reconcile
func (r *TerraformReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)
	reconcileStart := time.Now()

	<-r.CertRotator.Ready

	if isCAValid, _ := r.CertRotator.IsCAValid(); isCAValid == false && r.CertRotator.TriggerCARotation != nil {
		readyCh := make(chan *mtls.TriggerResult)
		r.CertRotator.TriggerCARotation <- mtls.Trigger{Namespace: "", Ready: readyCh}
		<-readyCh
	}

	var terraform infrav1.Terraform
	if err := r.Get(ctx, req.NamespacedName, &terraform); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	log.Info(fmt.Sprintf(">> Started Generation: %d", terraform.GetGeneration()))

	// Record suspended status metric
	defer r.recordSuspensionMetric(ctx, terraform)

	// Add our finalizer if it does not exist
	if !controllerutil.ContainsFinalizer(&terraform, infrav1.TerraformFinalizer) {
		patch := client.MergeFrom(terraform.DeepCopy())
		controllerutil.AddFinalizer(&terraform, infrav1.TerraformFinalizer)
		if err := r.Patch(ctx, &terraform, patch, client.FieldOwner(r.statusManager)); err != nil {
			log.Error(err, "unable to register finalizer")
			return ctrl.Result{}, err
		}
	}

	// Return early if the Terraform is suspended.
	if terraform.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// resolve source reference
	log.Info("getting source")
	sourceObj, err := r.getSource(ctx, terraform)
	if err != nil {
		if apierrors.IsNotFound(err) {
			msg := fmt.Sprintf("Source '%s' not found", terraform.Spec.SourceRef.String())
			terraform = infrav1.TerraformNotReady(terraform, "", infrav1.ArtifactFailedReason, msg)
			if err := r.patchStatus(ctx, req.NamespacedName, terraform.Status); err != nil {
				log.Error(err, "unable to update status for source not found")
				return ctrl.Result{Requeue: true}, err
			}
			r.recordReadinessMetric(ctx, terraform)
			log.Info(msg)
			// do not requeue immediately, when the source is created the watcher should trigger a reconciliation
			return ctrl.Result{RequeueAfter: terraform.GetRetryInterval()}, nil
		} else {
			// retry on transient errors
			log.Error(err, "retry")
			return ctrl.Result{Requeue: true}, err
		}
	}

	// sourceObj does not exist, return early
	if sourceObj.GetArtifact() == nil {
		msg := "Source is not ready, artifact not found"
		terraform = infrav1.TerraformNotReady(terraform, "", infrav1.ArtifactFailedReason, msg)
		if err := r.patchStatus(ctx, req.NamespacedName, terraform.Status); err != nil {
			log.Error(err, "unable to update status for artifact not found")
			return ctrl.Result{Requeue: true}, err
		}
		r.recordReadinessMetric(ctx, terraform)
		log.Info(msg)
		// do not requeue immediately, when the artifact is created the watcher should trigger a reconciliation
		return ctrl.Result{RequeueAfter: terraform.GetRetryInterval()}, nil
	}

	// check dependencies, if not being deleted
	if len(terraform.Spec.DependsOn) > 0 && terraform.ObjectMeta.DeletionTimestamp.IsZero() {
		if err := r.checkDependencies(sourceObj, terraform); err != nil {
			terraform = infrav1.TerraformNotReady(
				terraform, sourceObj.GetArtifact().Revision, infrav1.DependencyNotReadyReason, err.Error())

			if err := r.patchStatus(ctx, req.NamespacedName, terraform.Status); err != nil {
				log.Error(err, "unable to update status for dependency not ready")
				return ctrl.Result{Requeue: true}, err
			}
			// we can't rely on exponential backoff because it will prolong the execution too much,
			// instead we requeue on a fix interval.
			msg := fmt.Sprintf("Dependencies do not meet ready condition, retrying in %s", terraform.GetRetryInterval().String())
			log.Info(msg)
			r.event(ctx, terraform, sourceObj.GetArtifact().Revision, events.EventSeverityInfo, msg, nil)
			r.recordReadinessMetric(ctx, terraform)

			return ctrl.Result{RequeueAfter: terraform.GetRetryInterval()}, nil
		}
		log.Info("All dependencies are ready, proceeding with reconciliation")
	}

	// Skip update the status if the ready condition is still unknown
	// so that the Plan prompt is still shown.
	ready := apimeta.FindStatusCondition(terraform.Status.Conditions, meta.ReadyCondition)
	log.Info("before lookup runner: checking ready condition", "ready", ready)
	if ready == nil || ready.Status != metav1.ConditionUnknown {
		log.Info("before lookup runner: updating status", "ready", ready)
		terraform = infrav1.TerraformProgressing(terraform, "Reconciliation in progress")
		if err := r.patchStatus(ctx, req.NamespacedName, terraform.Status); err != nil {
			log.Error(err, "unable to update status before Terraform initialization")
			return ctrl.Result{Requeue: true}, err
		}
		log.Info("before lookup runner: updated status", "ready", ready)
		r.recordReadinessMetric(ctx, terraform)
	}

	// Create Runner Pod.
	// Wait for the Runner Pod to start.
	runnerClient, closeConn, err := r.LookupOrCreateRunner(ctx, terraform)
	if err != nil {
		log.Error(err, "unable to lookup or create runner")
		if closeConn != nil {
			if err := closeConn(); err != nil {
				log.Error(err, "unable to close connection")
			}
		}
		return ctrl.Result{}, err
	}
	log.Info("runner is running")

	defer func(ctx context.Context, cli client.Client, terraform infrav1.Terraform) {
		// make sure defer does not affect the return value

		if closeConn != nil {
			if err := closeConn(); err != nil {
				log.Error(err, "unable to close connection")
			}
		}

		if os.Getenv("INSECURE_LOCAL_RUNNER") == "1" {
			// nothing to delete
			log.Info("insecure local runner")
			return
		}

		if terraform.Spec.GetAlwaysCleanupRunnerPod() == true {
			// wait for runner pod complete termination
			var (
				interval = time.Second * 5
				timeout  = time.Second * 120
			)
			err := wait.PollImmediate(interval, timeout, func() (bool, error) {
				var runnerPod corev1.Pod
				err := r.Get(ctx, getRunnerPodObjectKey(terraform), &runnerPod)

				if err != nil && apierrors.IsNotFound(err) {
					return true, nil
				}

				if err := cli.Delete(ctx, &runnerPod,
					client.GracePeriodSeconds(1), // force kill = 1 second
					client.PropagationPolicy(metav1.DeletePropagationForeground),
				); err != nil {
					log.Error(err, "unable to delete pod")
					return false, nil
				}

				return false, err
			})

			if err != nil {
				log.Error(fmt.Errorf("failed waiting for the terminating runner pod: %v", err), "error in polling")
			}
		}
	}(ctx, r.Client, terraform)

	// Examine if the object is under deletion
	if !terraform.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, terraform, runnerClient, sourceObj)
	}

	// If revision is changed, and there's no intend to apply,
	// we should clear the Pending Plan to trigger re-plan
	if sourceObj.GetArtifact().Revision != terraform.Status.LastAttemptedRevision && !r.shouldApply(terraform) {
		terraform.Status.Plan.Pending = ""
		if err := r.patchStatus(ctx, req.NamespacedName, terraform.Status); err != nil {
			log.Error(err, "unable to update status to clear pending plan (revision != last attempted)")
			return ctrl.Result{Requeue: true}, err
		}
	}

	// Return early if it's manually mode and pending
	if terraform.Status.Plan.Pending != "" && !r.forceOrAutoApply(terraform) && !r.shouldApply(terraform) {
		log.Info("reconciliation is stopped to wait for a manual approve")
		return ctrl.Result{}, nil
	}

	// reconcile Terraform by applying the latest revision
	reconciledTerraform, reconcileErr := r.reconcile(ctx, runnerClient, *terraform.DeepCopy(), sourceObj)
	if err := r.patchStatus(ctx, req.NamespacedName, reconciledTerraform.Status); err != nil {
		log.Error(err, "unable to update status after the reconciliation is complete")
		return ctrl.Result{Requeue: true}, err
	}

	r.recordReadinessMetric(ctx, *reconciledTerraform)

	if reconcileErr != nil && reconcileErr.Error() == infrav1.DriftDetectedReason {
		log.Error(reconcileErr, fmt.Sprintf("Drift detected after %s, next try in %s",
			time.Since(reconcileStart).String(),
			terraform.GetRetryInterval().String()),
			"revision",
			sourceObj.GetArtifact().Revision)
		return ctrl.Result{RequeueAfter: terraform.GetRetryInterval()}, nil
	} else if reconcileErr != nil {
		// broadcast the reconciliation failure and requeue at the specified retry interval
		log.Error(reconcileErr, fmt.Sprintf("Reconciliation failed after %s, next try in %s",
			time.Since(reconcileStart).String(),
			terraform.GetRetryInterval().String()),
			"revision",
			sourceObj.GetArtifact().Revision)
		r.event(ctx, *reconciledTerraform, sourceObj.GetArtifact().Revision, events.EventSeverityError, reconcileErr.Error(), nil)
		return ctrl.Result{RequeueAfter: terraform.GetRetryInterval()}, nil
	}

	log.Info(fmt.Sprintf("Reconciliation completed. Generation: %d", reconciledTerraform.GetGeneration()))

	if reconciledTerraform.Status.Plan.Pending != "" && !r.forceOrAutoApply(*reconciledTerraform) {
		log.Info("Reconciliation is stopped to wait for a manual approve")
		return ctrl.Result{}, nil
	}

	// next reconcile is .Spec.Interval in the future
	log.Info("requeue after interval", "interval", terraform.Spec.Interval.Duration.String())
	return ctrl.Result{RequeueAfter: terraform.Spec.Interval.Duration}, nil
}

// Revision is in main/abcdefabcdefabcdefabcdefabcdefabcdefabcdef format
// We want to return main/abcdefa
func shortRev(revision string) string {
	const maxLength = 8
	if strings.Contains(revision, "/") {
		return revision[:strings.Index(revision, "/")+maxLength]
	} else {
		return revision[:maxLength]
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *TerraformReconciler) SetupWithManager(mgr ctrl.Manager, maxConcurrentReconciles int, httpRetry int) error {
	// Index the Terraforms by the GitRepository references they (may) point at.
	if err := mgr.GetCache().IndexField(context.TODO(), &infrav1.Terraform{}, infrav1.GitRepositoryIndexKey,
		r.IndexBy(sourcev1.GitRepositoryKind)); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	// Index the Terraforms by the Bucket references they (may) point at.
	if err := mgr.GetCache().IndexField(context.TODO(), &infrav1.Terraform{}, infrav1.BucketIndexKey,
		r.IndexBy(sourcev1.BucketKind)); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	// Index the Terraforms by the OCIRepository references they (may) point at.
	if err := mgr.GetCache().IndexField(context.TODO(), &infrav1.Terraform{}, infrav1.OCIRepositoryIndexKey,
		r.IndexBy(sourcev1.OCIRepositoryKind)); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	// Configure the retryable http client used for fetching artifacts.
	// By default it retries 10 times within a 3.5 minutes window.
	httpClient := retryablehttp.NewClient()
	httpClient.RetryWaitMin = 5 * time.Second
	httpClient.RetryWaitMax = 30 * time.Second
	httpClient.RetryMax = httpRetry
	httpClient.Logger = nil
	r.httpClient = httpClient
	r.statusManager = "tf-controller"
	r.requeueDependency = 30 * time.Second

	return ctrl.NewControllerManagedBy(mgr).
		For(&infrav1.Terraform{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&source.Kind{Type: &sourcev1.GitRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForRevisionChangeOf(infrav1.GitRepositoryIndexKey)),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.Bucket{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForRevisionChangeOf(infrav1.BucketIndexKey)),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.OCIRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForRevisionChangeOf(infrav1.OCIRepositoryIndexKey)),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &corev1.Secret{}},
			&handler.EnqueueRequestForOwner{
				OwnerType:    &infrav1.Terraform{},
				IsController: true,
			},
			builder.WithPredicates(SecretDeletePredicate{}),
		).
		WithOptions(controller.Options{
			MaxConcurrentReconciles: maxConcurrentReconciles,
			RecoverPanic:            true,
		}).
		Complete(r)
}

func (r *TerraformReconciler) checkDependencies(source sourcev1.Source, terraform infrav1.Terraform) error {
	for _, d := range terraform.Spec.DependsOn {
		if d.Namespace == "" {
			d.Namespace = terraform.GetNamespace()
		}
		dName := types.NamespacedName{
			Namespace: d.Namespace,
			Name:      d.Name,
		}
		var tf infrav1.Terraform
		err := r.Get(context.Background(), dName, &tf)
		if err != nil {
			return fmt.Errorf("unable to get '%s' dependency: %w", dName, err)
		}

		if len(tf.Status.Conditions) == 0 || tf.Generation != tf.Status.ObservedGeneration {
			return fmt.Errorf("dependency '%s' is not ready", dName)
		}

		if !apimeta.IsStatusConditionTrue(tf.Status.Conditions, meta.ReadyCondition) {
			return fmt.Errorf("dependency '%s' is not ready", dName)
		}

		revision := source.GetArtifact().Revision
		if tf.Spec.SourceRef.Name == terraform.Spec.SourceRef.Name &&
			tf.Spec.SourceRef.Namespace == terraform.Spec.SourceRef.Namespace &&
			tf.Spec.SourceRef.Kind == terraform.Spec.SourceRef.Kind &&
			revision != tf.Status.LastAppliedRevision &&
			revision != tf.Status.LastPlannedRevision {
			return fmt.Errorf("dependency '%s' is not updated yet", dName)
		}

		if tf.Spec.WriteOutputsToSecret != nil {
			outputSecret := tf.Spec.WriteOutputsToSecret.Name
			outputSecretName := types.NamespacedName{
				Namespace: tf.GetNamespace(),
				Name:      outputSecret,
			}
			if err := r.Get(context.Background(), outputSecretName, &corev1.Secret{}); err != nil {
				return fmt.Errorf("dependency output secret: '%s' of '%s' is not ready yet", outputSecret, dName)
			}
		}

	}

	return nil
}

func (r *TerraformReconciler) requestsForRevisionChangeOf(indexKey string) func(obj client.Object) []reconcile.Request {
	return func(obj client.Object) []reconcile.Request {
		repo, ok := obj.(interface {
			GetArtifact() *sourcev1.Artifact
		})
		if !ok {
			panic(fmt.Sprintf("Expected an object conformed with GetArtifact() method, but got a %T", obj))
		}
		// If we do not have an artifact, we have no requests to make
		if repo.GetArtifact() == nil {
			return nil
		}

		ctx := context.Background()
		var list infrav1.TerraformList
		if err := r.List(ctx, &list, client.MatchingFields{
			indexKey: client.ObjectKeyFromObject(obj).String(),
		}); err != nil {
			return nil
		}
		var dd []dependency.Dependent
		for _, d := range list.Items {
			// If the revision of the artifact equals to the last attempted revision,
			// we should not make a request for this Terraform
			if repo.GetArtifact().Revision == d.Status.LastAttemptedRevision {
				continue
			}
			dd = append(dd, d.DeepCopy())
		}
		sorted, err := dependency.Sort(dd)
		if err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, len(sorted))
		for i, t := range sorted {
			reqs[i].NamespacedName.Name = t.Name
			reqs[i].NamespacedName.Namespace = t.Namespace
		}
		return reqs
	}

}

func (r *TerraformReconciler) getSource(ctx context.Context, terraform infrav1.Terraform) (sourcev1.Source, error) {
	var sourceObj sourcev1.Source
	sourceNamespace := terraform.GetNamespace()
	if terraform.Spec.SourceRef.Namespace != "" {
		sourceNamespace = terraform.Spec.SourceRef.Namespace
	}
	namespacedName := types.NamespacedName{
		Namespace: sourceNamespace,
		Name:      terraform.Spec.SourceRef.Name,
	}
	switch terraform.Spec.SourceRef.Kind {
	case sourcev1.GitRepositoryKind:
		var repository sourcev1.GitRepository
		err := r.Client.Get(ctx, namespacedName, &repository)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &repository
	case sourcev1.BucketKind:
		var bucket sourcev1.Bucket
		err := r.Client.Get(ctx, namespacedName, &bucket)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &bucket
	case sourcev1.OCIRepositoryKind:
		var repository sourcev1.OCIRepository
		err := r.Client.Get(ctx, namespacedName, &repository)
		if err != nil {
			if apierrors.IsNotFound(err) {
				return sourceObj, err
			}
			return sourceObj, fmt.Errorf("unable to get source '%s': %w", namespacedName, err)
		}
		sourceObj = &repository
	default:
		return sourceObj, fmt.Errorf("source `%s` kind '%s' not supported",
			terraform.Spec.SourceRef.Name, terraform.Spec.SourceRef.Kind)
	}
	return sourceObj, nil
}

func (r *TerraformReconciler) downloadAsBytes(artifact *sourcev1.Artifact) (*bytes.Buffer, error) {
	artifactURL := artifact.URL
	if hostname := os.Getenv("SOURCE_CONTROLLER_LOCALHOST"); hostname != "" {
		u, err := url.Parse(artifactURL)
		if err != nil {
			return nil, err
		}
		u.Host = hostname
		artifactURL = u.String()
	}

	req, err := retryablehttp.NewRequest(http.MethodGet, artifactURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create a new request: %w", err)
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to download artifact, error: %w", err)
	}
	defer resp.Body.Close()

	// check response
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("failed to download artifact from %s, status: %s", artifactURL, resp.Status)
	}

	var buf bytes.Buffer

	// verify checksum matches origin
	if err := r.verifyArtifact(artifact, &buf, resp.Body); err != nil {
		return nil, err
	}

	return &buf, nil
}

func (r *TerraformReconciler) recordReadinessMetric(ctx context.Context, terraform infrav1.Terraform) {
	if r.MetricsRecorder == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)

	objRef, err := reference.GetReference(r.Scheme, &terraform)
	if err != nil {
		log.Error(err, "unable to record readiness metric")
		return
	}
	if rc := apimeta.FindStatusCondition(terraform.Status.Conditions, meta.ReadyCondition); rc != nil {
		r.MetricsRecorder.RecordCondition(*objRef, *rc,
			!terraform.DeletionTimestamp.IsZero())
	} else {
		r.MetricsRecorder.RecordCondition(*objRef, metav1.Condition{
			Type:   meta.ReadyCondition,
			Status: metav1.ConditionUnknown,
		}, !terraform.DeletionTimestamp.IsZero())
	}
}

func (r *TerraformReconciler) recordSuspensionMetric(ctx context.Context, terraform infrav1.Terraform) {
	if r.MetricsRecorder == nil {
		return
	}
	log := ctrl.LoggerFrom(ctx)

	objRef, err := reference.GetReference(r.Scheme, &terraform)
	if err != nil {
		log.Error(err, "unable to record suspended metric")
		return
	}

	if !terraform.DeletionTimestamp.IsZero() {
		r.MetricsRecorder.RecordSuspend(*objRef, false)
	} else {
		r.MetricsRecorder.RecordSuspend(*objRef, terraform.Spec.Suspend)
	}
}

func (r *TerraformReconciler) patchStatus(ctx context.Context, objectKey types.NamespacedName, newStatus infrav1.TerraformStatus) error {
	var terraform infrav1.Terraform
	if err := r.Get(ctx, objectKey, &terraform); err != nil {
		return err
	}

	patch := client.MergeFrom(terraform.DeepCopy())
	terraform.Status = newStatus

	return r.Status().Patch(ctx, &terraform, patch, client.FieldOwner(r.statusManager))
}

func (r *TerraformReconciler) verifyArtifact(artifact *sourcev1.Artifact, buf *bytes.Buffer, reader io.Reader) error {
	hasher := sha256.New()

	// for backwards compatibility with source-controller v0.17.2 and older
	if len(artifact.Checksum) == 40 {
		hasher = sha1.New()
	}

	// compute checksum
	mw := io.MultiWriter(hasher, buf)
	if _, err := io.Copy(mw, reader); err != nil {
		return err
	}

	if checksum := fmt.Sprintf("%x", hasher.Sum(nil)); checksum != artifact.Checksum {
		return fmt.Errorf("failed to verify artifact: computed checksum '%s' doesn't match advertised '%s'",
			checksum, artifact.Checksum)
	}

	return nil
}

func (r *TerraformReconciler) IndexBy(kind string) func(o client.Object) []string {
	return func(o client.Object) []string {
		terraform, ok := o.(*infrav1.Terraform)
		if !ok {
			panic(fmt.Sprintf("Expected a Kustomization, got %T", o))
		}

		if terraform.Spec.SourceRef.Kind == kind {
			namespace := terraform.GetNamespace()
			if terraform.Spec.SourceRef.Namespace != "" {
				namespace = terraform.Spec.SourceRef.Namespace
			}
			return []string{fmt.Sprintf("%s/%s", namespace, terraform.Spec.SourceRef.Name)}
		}

		return nil
	}
}

func (r *TerraformReconciler) event(ctx context.Context, terraform infrav1.Terraform, revision, severity, msg string, metadata map[string]string) {
	if metadata == nil {
		metadata = map[string]string{}
	}
	if revision != "" {
		metadata[infrav1.GroupVersion.Group+"/revision"] = revision
	}

	reason := severity
	if c := apimeta.FindStatusCondition(terraform.Status.Conditions, meta.ReadyCondition); c != nil {
		reason = c.Reason
	}

	eventType := "Normal"
	if severity == events.EventSeverityError {
		eventType = "Warning"
	}

	r.EventRecorder.AnnotatedEventf(&terraform, metadata, eventType, reason, msg)
}
