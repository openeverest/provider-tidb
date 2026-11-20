package provider

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func TestEnabledTiFlash(t *testing.T) {
	replicas := func(n int32) *int32 { return &n }
	tests := []struct {
		name  string
		comps map[string]corev1alpha1.ComponentSpec
		want  bool
	}{
		{"absent", map[string]corev1alpha1.ComponentSpec{}, false},
		{"zero replicas", map[string]corev1alpha1.ComponentSpec{common.ComponentTiFlash: {Replicas: replicas(0)}}, false},
		{"replicas left to the default", map[string]corev1alpha1.ComponentSpec{common.ComponentTiFlash: {}}, true},
		{"explicit replicas", map[string]corev1alpha1.ComponentSpec{common.ComponentTiFlash: {Replicas: replicas(2)}}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, got := enabledTiFlash(tt.comps); got != tt.want {
				t.Errorf("enabledTiFlash() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestSyncTiFlashDeletesGroupOnceDisabled(t *testing.T) {
	in := withTiFlash(testInstance("8.5.2"), 0, "")
	c := validationContext(t, in, &tidbcorev1.TiFlashGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}})

	assertError(t, syncTiFlash(c, nil, nil), "")

	if found, err := c.Exists(&tidbcorev1.TiFlashGroup{}, "orders"); err != nil || found {
		t.Fatalf("TiFlash group still exists (err %v)", err)
	}
}

func TestEvaluateStatusWhileRemovingTiFlash(t *testing.T) {
	tiflash := convergedGroup(common.ComponentTiFlash, 2)
	tiflash.removing = true

	got := evaluateStatus([]groupRollout{convergedGroup(common.ComponentPD, 3), convergedGroup(common.ComponentTiKV, 3),
		convergedGroup(common.ComponentTiDB, 2), tiflash}, false)

	if got.Converged || !strings.Contains(got.Message, "tiflash (removing)") || got.Available != controller.Available {
		t.Fatalf("status = %+v, want not converged and available while tiflash is removed", got)
	}
}

func TestStatusTiDBTiFlash(t *testing.T) {
	tiflashGroup := func(ready int32) client.Object {
		g := &tidbcorev1.TiFlashGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
		g.Spec.Template.Spec.Version = testVersion
		g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		g.Status.ReadyReplicas = ready
		return g
	}
	tests := []struct {
		name          string
		replicas      int32
		group         client.Object
		wantConverged bool
		wantMessage   string
	}{
		{"enabled before the group exists", 1, nil, false, "tiflash (0/1 ready)"},
		{"enabled and starting", 1, tiflashGroup(0), false, "tiflash (0/1 ready)"},
		{"enabled and ready", 1, tiflashGroup(1), true, ""},
		{"disabled and still running", 0, tiflashGroup(1), false, "tiflash (removing)"},
		{"disabled and gone", 0, nil, true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(convergedCoreGroups(), testProvider())
			if tt.group != nil {
				objs = append(objs, tt.group)
			}
			in := withTiFlash(testInstance("8.5.2"), tt.replicas, "")
			c := validationContext(t, in, objs...)

			got, err := StatusTiDB(c)
			assertError(t, err, "")
			if got.Converged != tt.wantConverged || !strings.Contains(got.Message, tt.wantMessage) {
				t.Errorf("status = converged %v %q, want %v containing %q", got.Converged, got.Message, tt.wantConverged, tt.wantMessage)
			}
			// TiFlash serves analytics only: clients connect while it starts.
			if got.Available != controller.Available {
				t.Errorf("available = %q, want True", got.Available)
			}
		})
	}
}

func TestValidateTiDBTiFlashVersion(t *testing.T) {
	tests := []struct {
		name     string
		replicas int32
		version  string
		wantErr  string
	}{
		{"same version as the cluster", 1, "v8.5.2", ""},
		{"newer than pd", 1, "v8.5.3", "tiflash version v8.5.3 is newer than pd version v8.5.2"},
		{"older than tikv", 1, "v8.5.1", "tikv version v8.5.2 is newer than tiflash version v8.5.1"},
		{"disabled tiflash is not checked", 0, "v8.5.3", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validationContext(t, withTiFlash(testInstance("8.5.2"), tt.replicas, tt.version), testProvider())
			assertError(t, validateAll(c), tt.wantErr)
		})
	}
}

func withTiFlash(in *corev1alpha1.Instance, replicas int32, version string) *corev1alpha1.Instance {
	in.Spec.Components[common.ComponentTiFlash] = corev1alpha1.ComponentSpec{
		Type: common.ComponentTiFlash, Replicas: &replicas, Version: version,
	}
	return in
}

// convergedCoreGroups returns PD, TiKV and TiDB groups that have finished rolling out.
func convergedCoreGroups() []client.Object {
	groups := runningGroups(testVersion)
	for _, obj := range groups {
		switch g := obj.(type) {
		case *tidbcorev1.PDGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		case *tidbcorev1.TiKVGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		case *tidbcorev1.TiDBGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		}
	}
	return groups
}
