package provider

import (
	"os"
	"testing"

	"sigs.k8s.io/yaml"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
)

// TestPresetsPinDefaultVersion keeps the chart's presets on the default version
// bundle: the UI shows an Instance's version from spec.version, which a preset
// fills in only when it pins one.
func TestPresetsPinDefaultVersion(t *testing.T) {
	var generated struct {
		Spec corev1alpha1.ProviderSpec `json:"spec"`
	}
	readYAML(t, "../../charts/provider-tidb/generated/provider-spec.yaml", &generated)
	var values struct {
		Presets []struct {
			Name string                          `json:"name"`
			Spec corev1alpha1.InstancePresetSpec `json:"spec"`
		} `json:"presets"`
	}
	readYAML(t, "../../charts/provider-tidb/values.yaml", &values)

	want := generated.Spec.DefaultVersion
	if want == "" || len(values.Presets) == 0 {
		t.Fatalf("default version %q, %d presets: expected both to be set", want, len(values.Presets))
	}
	for _, p := range values.Presets {
		if p.Spec.Version != want {
			t.Errorf("preset %q pins version %q, want the default bundle %q", p.Name, p.Spec.Version, want)
		}
	}
}

func readYAML(t *testing.T, path string, out any) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	if err := yaml.Unmarshal(data, out); err != nil {
		t.Fatalf("parsing %s: %v", path, err)
	}
}
