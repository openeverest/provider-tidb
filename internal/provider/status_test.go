package provider

import (
	"reflect"
	"slices"
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

// The cluster converges once every group runs what is asked; meanwhile the
// message says what is waited for.
func TestEvaluateStatusConverged(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 2
	got := evaluateStatus([]groupRollout{convergedGroup("pd", 3), tikv, convergedGroup("tidb", 2)}, false)
	if got.Converged || !strings.HasPrefix(got.Message, "Waiting for tikv (2/3 ready") {
		t.Errorf("got converged=%v %q, want waiting for tikv", got.Converged, got.Message)
	}
	got = evaluateStatus([]groupRollout{convergedGroup("pd", 3), convergedGroup("tikv", 3), convergedGroup("tidb", 2)}, false)
	if !got.Converged || got.Message != "" || got.Available != controller.Available {
		t.Errorf("got %+v, want converged and available", got)
	}
}

func TestEvaluateStatusReportsPendingComponents(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 1
	tikv.problem = componentProblem{message: "instance orders-abc: pod of the instance is not ready"}
	tidb := convergedGroup("tidb", 2)
	tidb.groupStatus.ReadyReplicas = 0
	tidb.commonStatus.Conditions = []metav1.Condition{
		{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "not all instances are ready"},
	}

	got := evaluateStatus([]groupRollout{convergedGroup("pd", 3), tikv, tidb}, false)

	wantMessage := "Waiting for tikv (1/3 ready: instance orders-abc: pod of the instance is not ready), " +
		"tidb (0/2 ready: not all instances are ready)"
	if got.Message != wantMessage {
		t.Errorf("message = %q, want %q", got.Message, wantMessage)
	}
	if len(got.Problems) != 0 {
		t.Errorf("problems = %+v, want none: a pod that isn't ready yet is waited for", got.Problems)
	}
}

func TestEvaluateStatusReportsStuckInstance(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.groupStatus.ReadyReplicas = 2
	tikv.problem = componentProblem{message: "instance orders-abc: main container tikv is waiting", reason: "CrashLoopBackOff"}

	got := evaluateStatus([]groupRollout{convergedGroup("pd", 3), tikv, convergedGroup("tidb", 2)}, false)

	want := []controller.Problem{{Reason: "CrashLoopBackOff", Message: "tikv is failing: instance orders-abc: main container tikv is waiting"}}
	if !reflect.DeepEqual(got.Problems, want) {
		t.Errorf("problems = %+v, want %+v", got.Problems, want)
	}
	// One TiKV node down of three: clients still connect.
	if got.Available != controller.Available {
		t.Errorf("available = %q, want True", got.Available)
	}
}

func TestEvaluateStatusAvailability(t *testing.T) {
	ready := func(g groupRollout, n int32) groupRollout { g.groupStatus.ReadyReplicas = n; return g }
	// joining scales a converged group to desired before its new pods exist.
	joining := func(g groupRollout, desired int32) groupRollout { g.replicas = &desired; return g }
	tests := []struct {
		name       string
		groups     []groupRollout
		tiproxy    bool
		want       controller.Availability
		wantReason string
	}{
		{name: "rolling restart of one pod per group",
			groups: []groupRollout{ready(convergedGroup("pd", 3), 2), ready(convergedGroup("tikv", 3), 2), ready(convergedGroup("tidb", 2), 1)},
			want:   controller.Available},
		{name: "no tidb ready",
			groups: []groupRollout{convergedGroup("pd", 3), convergedGroup("tikv", 3), ready(convergedGroup("tidb", 2), 0)},
			want:   controller.Unavailable, wantReason: reasonNoSQLServerReady},
		{name: "clients connect through tiproxy",
			groups:  []groupRollout{convergedGroup("pd", 3), convergedGroup("tikv", 3), convergedGroup("tidb", 2), ready(convergedGroup("tiproxy", 2), 0)},
			tiproxy: true, want: controller.Unavailable, wantReason: reasonNoSQLServerReady},
		{name: "pd lost quorum",
			groups: []groupRollout{ready(convergedGroup("pd", 3), 1), convergedGroup("tikv", 3), convergedGroup("tidb", 2)},
			want:   controller.Unavailable, wantReason: reasonNoPDQuorum},
		{name: "no tikv ready",
			groups: []groupRollout{convergedGroup("pd", 3), ready(convergedGroup("tikv", 3), 0), convergedGroup("tidb", 2)},
			want:   controller.Unavailable, wantReason: reasonNoTiKVReady},
		{name: "two tikv nodes down",
			groups: []groupRollout{convergedGroup("pd", 3), ready(convergedGroup("tikv", 5), 3), convergedGroup("tidb", 2)},
			want:   controller.AvailabilityUnknown, wantReason: reasonTiKVNodesDown},
		{name: "new tikv nodes joining hold no data yet",
			groups: []groupRollout{convergedGroup("pd", 3), joining(convergedGroup("tikv", 1), 4), convergedGroup("tidb", 2)},
			want:   controller.Available},
		{name: "new pd members joining",
			groups: []groupRollout{joining(convergedGroup("pd", 1), 3), convergedGroup("tikv", 3), convergedGroup("tidb", 2)},
			want:   controller.Available},
		{name: "single node cluster",
			groups: []groupRollout{convergedGroup("pd", 1), convergedGroup("tikv", 1), convergedGroup("tidb", 1)},
			want:   controller.Available},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := evaluateStatus(tt.groups, tt.tiproxy)
			if got.Available != tt.want || got.AvailableReason != tt.wantReason {
				t.Errorf("available = %q (%s), want %q (%s)", got.Available, got.AvailableReason, tt.want, tt.wantReason)
			}
		})
	}
}

func TestEvaluateStatusReportsUpgrade(t *testing.T) {
	tikv := convergedGroup("tikv", 3)
	tikv.version = "v8.5.8"
	got := evaluateStatus([]groupRollout{convergedGroup("pd", 3), tikv, convergedGroup("tidb", 2)}, false)
	want := []controller.Operation{{Type: corev1alpha1.ConditionUpgrading, Reason: reasonRollingOutVersion, Message: "Rolling out tikv to v8.5.8"}}
	if !reflect.DeepEqual(got.Operations, want) || got.Converged {
		t.Errorf("operations = %+v converged=%v, want %+v", got.Operations, got.Converged, want)
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
		name       string
		conds      []metav1.Condition
		wantFound  bool
		wantReason string
	}{
		{"healthy", []metav1.Condition{
			{Type: tidbcorev1.CondRunning, Status: metav1.ConditionTrue},
			{Type: tidbcorev1.CondReady, Status: metav1.ConditionTrue},
		}, false, ""},
		{"running but not ready", []metav1.Condition{
			{Type: tidbcorev1.CondRunning, Status: metav1.ConditionTrue},
			{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "pod of the instance is not ready"},
		}, true, ""},
		{"early crash loop while dependencies bootstrap",
			notRunning("reason: CrashLoopBackOff, message: back-off 20s restarting failed container=tikv"), true, ""},
		{"crash loop at max back-off",
			notRunning("reason: CrashLoopBackOff, message: back-off 5m0s restarting failed container=tikv"), true, "CrashLoopBackOff"},
		{"image cannot be pulled", notRunning("reason: ImagePullBackOff, message: Back-off pulling image"), true, "ImagePullBackOff"},
		{"missing config", notRunning("reason: CreateContainerConfigError, message: secret not found"), true, "CreateContainerConfigError"},
		{"still creating", notRunning("reason: ContainerCreating, message: "), true, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, found := instanceProblem("orders-abc", tt.conds)
			if found != tt.wantFound || got.reason != tt.wantReason {
				t.Errorf("instanceProblem() = (%+v, %v), want found=%v reason=%q", got, found, tt.wantFound, tt.wantReason)
			}
			if found && !strings.HasPrefix(got.message, "instance orders-abc: ") {
				t.Errorf("message %q should name the instance", got.message)
			}
		})
	}
}

func TestRecordProblemPrefersStuck(t *testing.T) {
	notReady := []metav1.Condition{{Type: tidbcorev1.CondReady, Status: metav1.ConditionFalse, Message: "not ready"}}
	crashLooping := []metav1.Condition{{
		Type: tidbcorev1.CondRunning, Status: metav1.ConditionFalse,
		Message: "reason: CrashLoopBackOff, message: back-off 5m0s restarting",
	}}

	problems := map[string]componentProblem{}
	recordProblem(problems, "tikv", "orders-a", notReady)
	recordProblem(problems, "tikv", "orders-b", crashLooping)
	recordProblem(problems, "tikv", "orders-c", notReady)

	if got := problems["tikv"]; got.reason != "CrashLoopBackOff" || !strings.Contains(got.message, "orders-b") {
		t.Errorf("problem = %+v, want the crash loop of orders-b", got)
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
	if len(got.Problems) != 1 || got.Problems[0].Reason != "CrashLoopBackOff" ||
		!strings.Contains(got.Problems[0].Message, "tikv is failing: instance orders-abc") {
		t.Errorf("problems = %+v, want the operator's crash loop of orders-abc", got.Problems)
	}
	if got.Available != controller.Unavailable || got.AvailableReason != reasonNoTiKVReady {
		t.Errorf("available = %q (%s), want False: the only TiKV is down", got.Available, got.AvailableReason)
	}
	if got.Converged {
		t.Error("converged with TiKV down")
	}
	want := []controller.ComponentReplicas{{Component: "pd", Desired: 1}, {Component: "tidb", Desired: 1}, {Component: "tikv", Desired: 1}}
	desired := slices.Clone(got.DesiredReplicas)
	slices.SortFunc(desired, func(a, b controller.ComponentReplicas) int { return strings.Compare(a.Component, b.Component) })
	if !reflect.DeepEqual(desired, want) {
		t.Errorf("desired replicas = %+v, want %+v", desired, want)
	}
}

func convergedReplicas(n int32) (*int32, tidbcorev1.GroupStatus) {
	return &n, tidbcorev1.GroupStatus{Version: testVersion, Replicas: n, ReadyReplicas: n, UpdatedReplicas: n, CurrentReplicas: n}
}
