package provider

import (
	"testing"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

// TestUISchemaIsReconciled asserts that every field the UI schema exposes is
// actually consumed by Sync — a UI control bound to a path the provider ignores
// would be published cluster-wide but do nothing.
func TestUISchemaIsReconciled(t *testing.T) {
	pinRootPassword(t)
	certManagerNeedsTLS := "takes effect only with tls.cluster or tls.client on, which the probe leaves off; " +
		"TestCertManagerFieldsAreReconciled probes it with TLS on"
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider: New(),
		Unverifiable: map[string]string{
			"spec.topology.parameters.tls.certManager.enabled":        certManagerNeedsTLS,
			"spec.topology.parameters.tls.certManager.issuerRef.name": certManagerNeedsTLS,
			"spec.topology.parameters.tls.certManager.issuerRef.kind": certManagerNeedsTLS,
		},
	})
}

// TestSupportedFieldsAreReconciled asserts that every field declared as
// supported in a topology is honoured by Sync.
func TestSupportedFieldsAreReconciled(t *testing.T) {
	pinRootPassword(t)
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{
		Provider: New(),
	})
}

// pinRootPassword fixes the generated root password: the harness compares each
// probe's output with a baseline render, and a fresh random password per render
// would make every field look reconciled.
func pinRootPassword(t *testing.T) {
	t.Helper()
	original := newRootPassword
	newRootPassword = func() (string, error) { return "conformance", nil }
	t.Cleanup(func() { newRootPassword = original })
}
