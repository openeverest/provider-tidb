package provider

import (
	"testing"

	"github.com/openeverest/openeverest/v2/provider-runtime/conformance"
)

// TestUISchemaIsReconciled asserts that every field the UI schema exposes is
// actually consumed by Sync — a UI control bound to a path the provider ignores
// would be published cluster-wide but do nothing.
func TestUISchemaIsReconciled(t *testing.T) {
	conformance.UISchemaIsReconciled(t, conformance.Config{
		Provider: New(),
	})
}

// TestSupportedFieldsAreReconciled asserts that every field declared as
// supported in a topology is honoured by Sync.
func TestSupportedFieldsAreReconciled(t *testing.T) {
	conformance.SupportedFieldsAreReconciled(t, conformance.Config{
		Provider: New(),
	})
}
