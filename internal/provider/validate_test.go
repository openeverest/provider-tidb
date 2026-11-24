package provider

import (
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/definition"
	"github.com/openeverest/provider-tidb/internal/common"
)

// Admission runs Validate without a client, so it must only decode the
// Instance and leave the checks that read the cluster to ValidateReferences.
func TestValidateLeavesClusterChecksToReferences(t *testing.T) {
	c, _ := certManagerContext(t, issuerTLS("corp", definition.IssuerKindClusterIssuer, ""), false, testProvider())
	in := c.Instance()
	pd := in.Spec.Components[common.ComponentPD]
	pd.Replicas = ptr.To[int32](2)
	in.Spec.Components[common.ComponentPD] = pd

	got := New().Validate(controller.ValidationInput{Instance: in, Provider: testProvider()}).Errors
	if len(got) != 1 || got[0].Field != "spec.components[pd].replicas" {
		t.Fatalf("Validate errors = %v, want only the even pd replicas", got)
	}

	pd.Replicas = ptr.To[int32](3)
	in.Spec.Components[common.ComponentPD] = pd
	if got := New().Validate(controller.ValidationInput{Instance: in, Provider: testProvider()}).Errors; len(got) != 0 {
		t.Fatalf("Validate errors = %v, want none", got)
	}
	refs, err := New().ValidateReferences(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Field != "spec.parameters.tls" ||
		!strings.Contains(refs[0].Detail, "the Certificate CRD") {
		t.Fatalf("ValidateReferences errors = %v, want the missing cert-manager CRD", refs)
	}
}
