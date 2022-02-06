package controllers

import (
	"testing"

	. "github.com/onsi/gomega"
	infrav1 "github.com/weaveworks/tf-controller/api/v1alpha1"
)

// +kubebuilder:docs-gen:collapse=Imports

func Test_000200_disable_drift_detection(t *testing.T) {
	Spec("This spec describes behaviour when drift detection is disabled")

	g := NewWithT(t)

	tf1 := infrav1.Terraform{
		Spec: infrav1.TerraformSpec{
			DisableDriftDetection: true,
		},
		Status: infrav1.TerraformStatus{
			LastAttemptedRevision: "main/1234",
			LastPlannedRevision:   "main/1234",
			LastAppliedRevision:   "main/1234",
			Plan: infrav1.PlanStatus{
				Pending: "",
			},
		},
	}

	It("should not detect drift when true")
	g.Expect(reconciler.shouldDetectDrift(tf1, "main/1234")).Should(BeFalse())

	tf2 := infrav1.Terraform{
		Spec: infrav1.TerraformSpec{
			DisableDriftDetection: false,
		},
		Status: infrav1.TerraformStatus{
			LastAttemptedRevision: "main/2345",
			LastPlannedRevision:   "main/2345",
			LastAppliedRevision:   "main/1234",
			Plan: infrav1.PlanStatus{
				Pending: "",
			},
		},
	}

	It("should detect drift when false")
	g.Expect(reconciler.shouldDetectDrift(tf2, "main/2345")).Should(BeTrue())
}
