package provider

import (
	"reflect"
	"testing"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

const testVersion = "v8.5.2"

// convergedGroup is a group whose operator-reported status matches its spec.
func convergedGroup(component string, replicas int32) groupRollout {
	return groupRollout{
		component:    component,
		replicas:     &replicas,
		version:      testVersion,
		commonStatus: tidbcorev1.CommonStatus{CurrentRevision: "rev-1", UpdateRevision: "rev-1"},
		groupStatus: tidbcorev1.GroupStatus{
			Version:         testVersion,
			Replicas:        replicas,
			ReadyReplicas:   replicas,
			UpdatedReplicas: replicas,
			CurrentReplicas: replicas,
		},
	}
}

func TestGroupRolloutConverged(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*groupRollout)
		want   bool
	}{
		{"all replicas ready and up to date", func(*groupRollout) {}, true},
		{"nil desired replicas", func(g *groupRollout) { g.replicas = nil }, false},
		{"zero desired replicas", func(g *groupRollout) { zero := int32(0); g.replicas = &zero }, false},
		{"replica not ready", func(g *groupRollout) { g.groupStatus.ReadyReplicas-- }, false},
		{"scale out pending", func(g *groupRollout) { five := int32(5); g.replicas = &five }, false},
		{"scale in pending", func(g *groupRollout) { two := int32(2); g.replicas = &two }, false},
		{"rolling to new revision", func(g *groupRollout) {
			g.commonStatus.UpdateRevision = "rev-2"
			g.groupStatus.UpdatedReplicas = 1
		}, false},
		{"revision switched but not yet current", func(g *groupRollout) { g.commonStatus.UpdateRevision = "rev-2" }, false},
		{"version bump not yet observed", func(g *groupRollout) { g.version = "v8.5.3" }, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := convergedGroup("tikv", 3)
			tt.mutate(&g)
			if got := g.converged(); got != tt.want {
				t.Errorf("converged() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnconvergedComponents(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 2
	tidb := convergedGroup("tidb", 2)
	tidb.version = "v8.5.3"

	got := unconvergedComponents([]groupRollout{convergedGroup("pd", 3), tikv, tidb})
	if want := []string{"tikv", "tidb"}; !reflect.DeepEqual(got, want) {
		t.Errorf("unconvergedComponents() = %v, want %v", got, want)
	}
	if got := unconvergedComponents([]groupRollout{convergedGroup("pd", 3)}); len(got) != 0 {
		t.Errorf("unconvergedComponents() = %v, want none", got)
	}
}

func TestRolloutStatus(t *testing.T) {
	tests := []struct {
		previous corev1alpha1.InstancePhase
		want     corev1alpha1.InstancePhase
	}{
		{"", corev1alpha1.InstancePhaseProvisioning},
		{corev1alpha1.InstancePhaseProvisioning, corev1alpha1.InstancePhaseProvisioning},
		{corev1alpha1.InstancePhaseFailed, corev1alpha1.InstancePhaseProvisioning},
		{corev1alpha1.InstancePhaseReady, corev1alpha1.InstancePhaseUpdating},
		{corev1alpha1.InstancePhaseUpdating, corev1alpha1.InstancePhaseUpdating},
	}
	for _, tt := range tests {
		got := rolloutStatus(tt.previous, []string{"tikv", "tidb"})
		if got.Phase != tt.want {
			t.Errorf("previous %q: phase = %q, want %q", tt.previous, got.Phase, tt.want)
		}
		if got.Message == "" {
			t.Errorf("previous %q: expected a message naming the pending components", tt.previous)
		}
	}
}
