package provider

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

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

	got := evaluateStatus(corev1alpha1.InstancePhaseReady, []groupRollout{convergedGroup(common.ComponentPD, 3), tiflash})

	if got.Phase != corev1alpha1.InstancePhaseUpdating || !strings.Contains(got.Message, "tiflash (removing)") {
		t.Fatalf("status = %s %q, want Updating while tiflash is removed", got.Phase, got.Message)
	}
	if c := got.Components[1]; c.Total != 0 || c.State != "InProgress" {
		t.Errorf("tiflash component = %+v, want 0 desired and InProgress", c)
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
		name        string
		previous    corev1alpha1.InstancePhase
		replicas    int32
		group       client.Object
		wantPhase   corev1alpha1.InstancePhase
		wantMessage string
	}{
		{"enabled before the group exists", "", 1, nil, corev1alpha1.InstancePhaseProvisioning, "tiflash (0/1 ready)"},
		{"enabled on a ready instance before the group exists", corev1alpha1.InstancePhaseReady, 1, nil, corev1alpha1.InstancePhaseUpdating, "tiflash (0/1 ready)"},
		{"enabled and starting", "", 1, tiflashGroup(0), corev1alpha1.InstancePhaseProvisioning, "tiflash (0/1 ready)"},
		{"enabled on a ready instance and starting", corev1alpha1.InstancePhaseReady, 1, tiflashGroup(0), corev1alpha1.InstancePhaseUpdating, "tiflash (0/1 ready)"},
		{"enabled and ready", "", 1, tiflashGroup(1), corev1alpha1.InstancePhaseReady, ""},
		{"disabled and still running", "", 0, tiflashGroup(1), corev1alpha1.InstancePhaseProvisioning, "tiflash (removing)"},
		{"disabled and gone", "", 0, nil, corev1alpha1.InstancePhaseReady, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			objs := append(convergedCoreGroups(), testProvider())
			if tt.group != nil {
				objs = append(objs, tt.group)
			}
			in := withTiFlash(testInstance("8.5.2"), tt.replicas, "")
			in.Status.Phase = tt.previous
			c := validationContext(t, in, objs...)

			got, err := StatusTiDB(c)
			assertError(t, err, "")
			if got.Phase != tt.wantPhase || !strings.Contains(got.Message, tt.wantMessage) {
				t.Errorf("status = %s %q, want %s containing %q", got.Phase, got.Message, tt.wantPhase, tt.wantMessage)
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
			assertError(t, ValidateTiDB(c), tt.wantErr)
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
