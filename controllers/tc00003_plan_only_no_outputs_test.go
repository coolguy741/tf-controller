package controllers

import (
	"context"
	corev1 "k8s.io/api/core/v1"
	"time"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"

	infrav1 "github.com/chanwit/tf-controller/api/v1alpha1"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:docs-gen:collapse=Imports

var _ = Describe("TF controller", func() {
	const (
		sourceName    = "test-tf-controller-w-plan-no-output"
		terraformName = "helloworld-w-plan-no-outputs"
	)

	Context("When create a hello world TF object", func() {
		It("should do planning the TF hello world program from the BLOB, and get the correct planned as a secret", func() {
			ctx := context.Background()

			By("creating a new Git repository object")
			updatedTime := time.Now()
			testRepo := sourcev1.GitRepository{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "source.toolkit.fluxcd.io/v1beta1",
					Kind:       "GitRepository",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      sourceName,
					Namespace: "flux-system",
				},
				Spec: sourcev1.GitRepositorySpec{
					URL: "https://github.com/openshift-fluxv2-poc/podinfo",
					Reference: &sourcev1.GitRepositoryRef{
						Branch: "master",
					},
					Interval:          metav1.Duration{Duration: time.Second * 30},
					GitImplementation: "go-git",
				},
			}
			Expect(k8sClient.Create(ctx, &testRepo)).Should(Succeed())

			By("setting the git repo status object, the URL, and the correct checksum")
			testRepo.Status = sourcev1.GitRepositoryStatus{
				ObservedGeneration: int64(1),
				Conditions: []metav1.Condition{
					{
						Type:               "Ready",
						Status:             metav1.ConditionTrue,
						LastTransitionTime: metav1.Time{Time: updatedTime},
						Reason:             "GitOperationSucceed",
						Message:            "Fetched revision: master/b8e362c206e3d0cbb7ed22ced771a0056455a2fb",
					},
				},
				URL: server.URL() + "/file.tar.gz",
				Artifact: &sourcev1.Artifact{
					Path:           "gitrepository/flux-system/test-tf-controller/b8e362c206e3d0cbb7ed22ced771a0056455a2fb.tar.gz",
					URL:            server.URL() + "/file.tar.gz",
					Revision:       "master/b8e362c206e3d0cbb7ed22ced771a0056455a2fb",
					Checksum:       "80ddfd18eb96f7d31cadc1a8a5171c6e2d95df3f6c23b0ed9cd8dddf6dba1406", // must be the real checksum value
					LastUpdateTime: metav1.Time{Time: updatedTime},
				},
			}
			Expect(k8sClient.Status().Update(ctx, &testRepo)).Should(Succeed())

			By("checking that the status and its URL gets reconciled")
			gitRepoKey := types.NamespacedName{Namespace: "flux-system", Name: sourceName}
			createdRepo := sourcev1.GitRepository{}
			Expect(k8sClient.Get(ctx, gitRepoKey, &createdRepo)).Should(Succeed())

			By("creating a new TF and attaching to the repo, with no approve plan")
			helloWorldTF := infrav1.Terraform{
				TypeMeta: metav1.TypeMeta{
					APIVersion: "infra.contrib.fluxcd.io/v1alpha1",
					Kind:       "Terraform",
				},
				ObjectMeta: metav1.ObjectMeta{
					Name:      terraformName,
					Namespace: "flux-system",
				},
				Spec: infrav1.TerraformSpec{
					Path: "./terraform-hello-world-example",
					SourceRef: infrav1.CrossNamespaceSourceReference{
						Kind:      "GitRepository",
						Name:      sourceName,
						Namespace: "flux-system",
					},
				},
			}
			Expect(k8sClient.Create(ctx, &helloWorldTF)).Should(Succeed())

			helloWorldTFKey := types.NamespacedName{Namespace: "flux-system", Name: terraformName}
			createdHelloWorldTF := infrav1.Terraform{}
			By("checking that the hello world TF get created")
			Eventually(func() bool {
				err := k8sClient.Get(ctx, helloWorldTFKey, &createdHelloWorldTF)
				if err != nil {
					return false
				}
				return true
			}, timeout, interval).Should(BeTrue())

			By("checking that the TF condition got created")
			Eventually(func() int {
				err := k8sClient.Get(ctx, helloWorldTFKey, &createdHelloWorldTF)
				if err != nil {
					return -1
				}
				return len(createdHelloWorldTF.Status.Conditions)
			}, timeout*3, interval).Should(Equal(1))

			By("checking that the planned status of the TF program is created successfully")
			Eventually(func() map[string]interface{} {
				err := k8sClient.Get(ctx, helloWorldTFKey, &createdHelloWorldTF)
				if err != nil {
					return nil
				}
				return map[string]interface{}{
					"Type":    createdHelloWorldTF.Status.Conditions[0].Type,
					"Reason":  createdHelloWorldTF.Status.Conditions[0].Reason,
					"Pending": createdHelloWorldTF.Status.Plan.Pending,
				}
			}, timeout, interval).Should(Equal(map[string]interface{}{
				"Type":    "Plan",
				"Reason":  "TerraformPlannedSucceed",
				"Pending": "plan-master-b8e362c206",
			}))

			By("checking that the planned secret got created")
			tfplanKey := types.NamespacedName{Namespace: "flux-system", Name: "tfplan-default-" + terraformName}
			tfplanSecret := corev1.Secret{}
			Eventually(func() map[string]interface{} {
				err := k8sClient.Get(ctx, tfplanKey, &tfplanSecret)
				if err != nil {
					return nil
				}
				return map[string]interface{}{
					"SavedPlan":   tfplanSecret.Labels["savedPlan"],
					"TFPlanEmpty": string(tfplanSecret.Data["tfplan"]) == "",
				}
			}, timeout, interval).Should(Equal(map[string]interface{}{
				"SavedPlan":   "plan-master-b8e362c206",
				"TFPlanEmpty": false,
			}))

		})
	})
})
