package provider

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func versions(pd, tikv, tidb string) map[string]string {
	return map[string]string{common.ComponentPD: pd, common.ComponentTiKV: tikv, common.ComponentTiDB: tidb}
}

func TestCheckUpgradeOrder(t *testing.T) {
	tests := []struct {
		name    string
		target  map[string]string
		wantErr string
	}{
		{"all on one version", versions("v8.5.2", "v8.5.2", "v8.5.2"), ""},
		{"pd upgraded first", versions("v8.5.2", "v7.5.5", "v7.5.5"), ""},
		{"pd and tikv upgraded first", versions("v8.5.2", "v8.5.2", "v7.5.5"), ""},
		{"bundle-style versions without v prefix", versions("8.5.2", "8.5.2", "8.5.2"), ""},
		{"tikv ahead of pd", versions("v7.5.5", "v8.5.2", "v7.5.5"), "tikv version v8.5.2 is newer than pd version v7.5.5"},
		{"tidb ahead of tikv", versions("v8.5.2", "v7.5.5", "v8.5.2"), "tidb version v8.5.2 is newer than tikv version v7.5.5"},
		{"not a version", versions("latest", "v8.5.2", "v8.5.2"), `pd version "latest" is not a valid semantic version`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, checkUpgradeOrder(tt.target), tt.wantErr)
		})
	}
}

func TestCheckNoDowngrade(t *testing.T) {
	tests := []struct {
		name    string
		current map[string]string
		target  map[string]string
		wantErr string
	}{
		{"new cluster", map[string]string{}, versions("v7.5.5", "v7.5.5", "v7.5.5"), ""},
		{"unchanged", versions("v8.5.2", "v8.5.2", "v8.5.2"), versions("v8.5.2", "v8.5.2", "v8.5.2"), ""},
		{"minor upgrade", versions("v7.5.5", "v7.5.5", "v7.5.5"), versions("v8.5.2", "v8.5.2", "v8.5.2"), ""},
		{"minor downgrade", versions("v8.5.2", "v8.5.2", "v8.5.2"), versions("v7.5.5", "v7.5.5", "v7.5.5"),
			"downgrading pd from v8.5.2 to v7.5.5 is not supported"},
		{"patch downgrade of one component", versions("v8.5.2", "v8.5.2", "v8.5.2"), versions("v8.5.2", "v8.5.2", "v8.5.1"),
			"downgrading tidb from v8.5.2 to v8.5.1 is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, checkNoDowngrade(tt.current, tt.target), tt.wantErr)
		})
	}
}

// TestValidateTiDBVersionChanges drives Validate end to end: versions come from
// the Instance's bundle and are compared with the groups already in the cluster.
func TestValidateTiDBVersionChanges(t *testing.T) {
	tests := []struct {
		name           string
		runningVersion string
		bundle         string
		wantErr        string
	}{
		{"new instance", "", "7.5.5", ""},
		{"upgrade to newer bundle", "v7.5.5", "8.5.2", ""},
		{"re-apply same bundle", "v8.5.2", "8.5.2", ""},
		{"downgrade to older bundle", "v8.5.2", "7.5.5", "downgrading pd from v8.5.2 to v7.5.5 is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := []client.Object{testProvider()}
			if tt.runningVersion != "" {
				objs = append(objs, runningGroups(tt.runningVersion)...)
			}
			c := validationContext(t, testInstance(tt.bundle), objs...)
			assertError(t, ValidateTiDB(c), tt.wantErr)
		})
	}
}

func TestValidateTiDBExplicitComponentVersionWins(t *testing.T) {
	in := testInstance("8.5.2")
	tidb := in.Spec.Components[common.ComponentTiDB]
	tidb.Version = "v7.5.5"
	in.Spec.Components[common.ComponentTiDB] = tidb

	c := validationContext(t, in, append([]client.Object{testProvider()}, runningGroups("v8.5.2")...)...)
	assertError(t, ValidateTiDB(c), "downgrading tidb from v8.5.2 to v7.5.5 is not supported")
}

const testNamespace = "db"

func testInstance(bundle string) *corev1alpha1.Instance {
	one := int32(1)
	return &corev1alpha1.Instance{
		ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace},
		Spec: corev1alpha1.InstanceSpec{
			Version: bundle,
			Components: map[string]corev1alpha1.ComponentSpec{
				common.ComponentPD:   {Type: common.ComponentPD, Replicas: &one},
				common.ComponentTiKV: {Type: common.ComponentTiKV, Replicas: &one},
				common.ComponentTiDB: {Type: common.ComponentTiDB, Replicas: &one},
			},
		},
	}
}

func testProvider() *corev1alpha1.Provider {
	bundle := func(name, v string) corev1alpha1.VersionBundle {
		return corev1alpha1.VersionBundle{Name: name, Components: versions(v, v, v)}
	}
	return &corev1alpha1.Provider{
		ObjectMeta: metav1.ObjectMeta{Name: common.ProviderName},
		Spec: corev1alpha1.ProviderSpec{
			Versions: []corev1alpha1.VersionBundle{bundle("8.5.2", "v8.5.2"), bundle("7.5.5", "v7.5.5")},
		},
	}
}

func runningGroups(version string) []client.Object {
	meta := metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}
	pd := &tidbcorev1.PDGroup{ObjectMeta: meta}
	pd.Spec.Template.Spec.Version = version
	tikv := &tidbcorev1.TiKVGroup{ObjectMeta: meta}
	tikv.Spec.Template.Spec.Version = version
	tidb := &tidbcorev1.TiDBGroup{ObjectMeta: meta}
	tidb.Spec.Template.Spec.Version = version
	return []client.Object{pd, tikv, tidb}
}

func validationContext(t *testing.T, in *corev1alpha1.Instance, objs ...client.Object) *controller.Context {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install} {
		if err := install(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, in, common.ProviderName)
}

func assertError(t *testing.T, err error, want string) {
	t.Helper()
	if want == "" {
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}
