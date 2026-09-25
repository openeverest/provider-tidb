package provider

import (
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

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

func TestEvaluateStatusPhase(t *testing.T) {
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
		tikv := convergedGroup("tikv", 3)
		tikv.groupStatus.ReadyReplicas = 2
		got := evaluateStatus(tt.previous, []groupRollout{convergedGroup("pd", 3), tikv})
		if got.Phase != tt.want {
			t.Errorf("previous %q: phase = %q, want %q", tt.previous, got.Phase, tt.want)
		}
	}

	if got := evaluateStatus(corev1alpha1.InstancePhaseUpdating, []groupRollout{convergedGroup("pd", 3)}); got.Phase != corev1alpha1.InstancePhaseReady {
		t.Errorf("all groups converged: phase = %q, want Ready", got.Phase)
	}
}

func TestEvaluateStatusReportsComponents(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 1
	tikv.problem = componentProblem{message: "instance orders-abc: pod of the instance is not ready"}
	tidb := convergedGroup("tidb", 2)
	tidb.groupStatus.ReadyReplicas = 0
	tidb.commonStatus.Conditions = []metav1.Condition{
		{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "not all instances are ready"},
	}

	got := evaluateStatus("", []groupRollout{convergedGroup("pd", 3), tikv, tidb})

	wantMessage := "Waiting for tikv (1/3 ready: instance orders-abc: pod of the instance is not ready), " +
		"tidb (0/2 ready: not all instances are ready)"
	if got.Message != wantMessage {
		t.Errorf("message = %q, want %q", got.Message, wantMessage)
	}
	wantComponents := []controller.ComponentStatus{
		{Name: "pd", Ready: 3, Total: 3, State: "Ready"},
		{Name: "tikv", Ready: 1, Total: 3, State: "InProgress"},
		{Name: "tidb", Ready: 0, Total: 2, State: "InProgress"},
	}
	if !reflect.DeepEqual(got.Components, wantComponents) {
		t.Errorf("components = %+v, want %+v", got.Components, wantComponents)
	}
}

func TestEvaluateStatusFailsOnStuckInstance(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 2
	tikv.problem = componentProblem{message: "instance orders-abc: main container tikv is waiting", fatal: true}

	got := evaluateStatus(corev1alpha1.InstancePhaseReady, []groupRollout{convergedGroup("pd", 3), tikv})

	if got.Phase != corev1alpha1.InstancePhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Phase)
	}
	if want := "tikv is failing: instance orders-abc: main container tikv is waiting"; got.Message != want {
		t.Errorf("message = %q, want %q", got.Message, want)
	}
	if got.Components[1].State != "Error" {
		t.Errorf("tikv state = %q, want Error", got.Components[1].State)
	}
}

func TestInstanceProblem(t *testing.T) {
	notRunning := func(detail string) []metav1.Condition {
		return []metav1.Condition{{
			Type:    tidbcorev1.CondRunning,
			Status:  metav1.ConditionFalse,
			Message: "pod of the instance is not running, detail: main container tikv is waiting, " + detail,
		}}
	}
	tests := []struct {
		name      string
		conds     []metav1.Condition
		wantFound bool
		wantFatal bool
	}{
		{"healthy", []metav1.Condition{
			{Type: tidbcorev1.CondRunning, Status: metav1.ConditionTrue},
			{Type: tidbcorev1.CondReady, Status: metav1.ConditionTrue},
		}, false, false},
		{"running but not ready", []metav1.Condition{
			{Type: tidbcorev1.CondRunning, Status: metav1.ConditionTrue},
			{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "pod of the instance is not ready"},
		}, true, false},
		{"early crash loop while dependencies bootstrap",
			notRunning("reason: CrashLoopBackOff, message: back-off 20s restarting failed container=tikv"), true, false},
		{"crash loop at max back-off",
			notRunning("reason: CrashLoopBackOff, message: back-off 5m0s restarting failed container=tikv"), true, true},
		{"image cannot be pulled", notRunning("reason: ImagePullBackOff, message: Back-off pulling image"), true, true},
		{"missing config", notRunning("reason: CreateContainerConfigError, message: secret not found"), true, true},
		{"still creating", notRunning("reason: ContainerCreating, message: "), true, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := instanceProblem("orders-abc", tt.conds)
			if found != tt.wantFound || got.fatal != tt.wantFatal {
				t.Errorf("instanceProblem() = (%+v, %v), want found=%v fatal=%v", got, found, tt.wantFound, tt.wantFatal)
			}
			if found && !strings.HasPrefix(got.message, "instance orders-abc: ") {
				t.Errorf("message %q should name the instance", got.message)
			}
		})
	}
}

func TestRecordProblemPrefersFatal(t *testing.T) {
	notReady := []metav1.Condition{{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "not ready"}}
	crashLooping := []metav1.Condition{{
		Type: tidbcorev1.CondRunning, Status: metav1.ConditionFalse,
		Message: "reason: CrashLoopBackOff, message: back-off 5m0s restarting",
	}}

	problems := map[string]componentProblem{}
	recordProblem(problems, "tikv", "orders-a", notReady)
	recordProblem(problems, "tikv", "orders-b", crashLooping)
	recordProblem(problems, "tikv", "orders-c", notReady)

	if got := problems["tikv"]; !got.fatal || !strings.Contains(got.message, "orders-b") {
		t.Errorf("problem = %+v, want the fatal one from orders-b", got)
	}
}

// TestStatusTiDBSurfacesOperatorReason reads groups and instance CRs from the
// cluster, the way the reconciler calls Status.
func TestStatusTiDBSurfacesOperatorReason(t *testing.T) {
	groups := runningGroups(testVersion)
	for _, obj := range groups {
		switch g := obj.(type) {
		case *tidbcorev1.PDGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		case *tidbcorev1.TiKVGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
			g.Status.ReadyReplicas = 0
		case *tidbcorev1.TiDBGroup:
			g.Spec.Replicas, g.Status.GroupStatus = convergedReplicas(1)
		}
	}
	tikv := &tidbcorev1.TiKV{ObjectMeta: metav1.ObjectMeta{
		Name: "orders-abc", Namespace: testNamespace,
		Labels: map[string]string{tidbcorev1.LabelKeyCluster: "orders"},
	}}
	tikv.Status.Conditions = []metav1.Condition{{
		Type: tidbcorev1.CondRunning, Status: metav1.ConditionFalse, Reason: tidbcorev1.ReasonPodNotRunning,
		Message: "pod of the instance is not running, detail: main container tikv is waiting, " +
			"reason: CrashLoopBackOff, message: back-off 5m0s restarting failed container=tikv",
	}}

	c := validationContext(t, testInstance("8.5.2"), append(groups, testProvider(), tikv)...)
	got, err := StatusTiDB(c)
	if err != nil {
		t.Fatalf("StatusTiDB() error = %v", err)
	}
	if got.Phase != corev1alpha1.InstancePhaseFailed {
		t.Fatalf("phase = %q, want Failed", got.Phase)
	}
	if !strings.Contains(got.Message, "tikv is failing: instance orders-abc") || !strings.Contains(got.Message, "CrashLoopBackOff") {
		t.Errorf("message = %q, want the operator's crash-loop reason for orders-abc", got.Message)
	}
}

func convergedReplicas(n int32) (*int32, tidbcorev1.GroupStatus) {
	return &n, tidbcorev1.GroupStatus{Version: testVersion, Replicas: n, ReadyReplicas: n, UpdatedReplicas: n, CurrentReplicas: n}
}
