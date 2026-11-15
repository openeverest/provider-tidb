package provider

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller/validationtest"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	providerSpecPath = "../../charts/provider-tidb/generated/provider-spec.yaml"
	instanceFixture  = "../../examples/instance-simple.yaml"
)

func suspendedInstance(suspended bool) *corev1alpha1.Instance {
	in := testInstance("8.5.2")
	in.Spec.Suspend = &corev1alpha1.SuspendSpec{Suspended: suspended}
	return in
}

// The suspend action is rendered only while a suspend is requested, so on
// resume server-side apply removes it and TiDB Operator starts the pods.
func TestApplySuspend(t *testing.T) {
	for _, tt := range []struct {
		name string
		in   *corev1alpha1.Instance
		want bool
	}{
		{name: "not requested", in: testInstance("8.5.2")},
		{name: "suspended", in: suspendedInstance(true), want: true},
		{name: "resumed", in: suspendedInstance(false)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &tidbcorev1.Cluster{}
			applySuspend(validationContext(t, tt.in), cluster)
			got := cluster.Spec.SuspendAction != nil && cluster.Spec.SuspendAction.SuspendCompute
			if got != tt.want {
				t.Errorf("suspendCompute = %v, want %v", got, tt.want)
			}
		})
	}
}

func suspendingCluster(status metav1.ConditionStatus, observed int64) *tidbcorev1.Cluster {
	cluster := testCluster()
	cluster.Generation = 2
	cluster.Spec.SuspendAction = &tidbcorev1.SuspendAction{SuspendCompute: true}
	meta.SetStatusCondition(&cluster.Status.Conditions, metav1.Condition{
		Type: tidbcorev1.ClusterCondSuspended, Status: status, ObservedGeneration: observed,
		Reason: tidbcorev1.ClusterSuspendReason, Message: "Cluster is suspending",
	})
	return cluster
}

func tiproxyGroup(suspended bool) *tidbcorev1.TiProxyGroup {
	group := &tidbcorev1.TiProxyGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
	status := metav1.ConditionFalse
	if suspended {
		status = metav1.ConditionTrue
	}
	group.Status.Conditions = []metav1.Condition{{Type: tidbcorev1.CondSuspended, Status: status, Reason: tidbcorev1.ReasonSuspending}}
	return group
}

func TestStatusWhileSuspended(t *testing.T) {
	notApplied := testCluster()
	for _, tt := range []struct {
		name    string
		objs    []client.Object
		want    corev1alpha1.InstancePhase
		message string
	}{
		{name: "no cluster", want: corev1alpha1.InstancePhaseSuspending},
		{name: "action not applied yet", objs: []client.Object{notApplied}, want: corev1alpha1.InstancePhaseSuspending},
		{name: "condition not observed yet", objs: []client.Object{suspendingCluster(metav1.ConditionTrue, 1)},
			want: corev1alpha1.InstancePhaseSuspending},
		{name: "pods stopping", objs: []client.Object{suspendingCluster(metav1.ConditionFalse, 2)},
			want: corev1alpha1.InstancePhaseSuspending, message: "TiDB Operator: Cluster is suspending"},
		{name: "suspended", objs: []client.Object{suspendingCluster(metav1.ConditionTrue, 2)},
			want: corev1alpha1.InstancePhaseSuspended},
		{name: "tiproxy still running", objs: []client.Object{suspendingCluster(metav1.ConditionTrue, 2), tiproxyGroup(false)},
			want: corev1alpha1.InstancePhaseSuspending, message: "waiting for the TiProxy pods to stop"},
		{name: "tiproxy suspended", objs: []client.Object{suspendingCluster(metav1.ConditionTrue, 2), tiproxyGroup(true)},
			want: corev1alpha1.InstancePhaseSuspended},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := StatusTiDB(validationContext(t, suspendedInstance(true), tt.objs...))
			if err != nil {
				t.Fatal(err)
			}
			if got.Phase != tt.want || (tt.message != "" && got.Message != tt.message) {
				t.Errorf("status = %+v, want phase %s message %q", got, tt.want, tt.message)
			}
		})
	}
}

// The runtime rejects other spec changes in the update that suspends or
// resumes an Instance and while it is suspended, once the Provider declares
// suspend support.
func TestValidateSuspend(t *testing.T) {
	v := validationtest.New(t, New(), providerSpecPath)
	suspend := func(suspended bool) func(*corev1alpha1.Instance) {
		return func(in *corev1alpha1.Instance) { in.Spec.Suspend = &corev1alpha1.SuspendSpec{Suspended: suspended} }
	}
	scaleTiDB := func(in *corev1alpha1.Instance) {
		tidb := in.Spec.Components[common.ComponentTiDB]
		tidb.Replicas = ptr.To(*tidb.Replicas + 1)
		in.Spec.Components[common.ComponentTiDB] = tidb
	}
	type change = []func(*corev1alpha1.Instance)
	for _, tt := range []struct {
		name    string
		old     change
		updated change
		wantErr bool
	}{
		{name: "suspend", updated: change{suspend(true)}},
		{name: "suspend with another change", updated: change{suspend(true), scaleTiDB}, wantErr: true},
		{name: "change while suspended", old: change{suspend(true)}, updated: change{scaleTiDB}, wantErr: true},
		{name: "resume", old: change{suspend(true)}, updated: change{suspend(false)}},
		{name: "resume with another change", old: change{suspend(true)}, updated: change{suspend(false), scaleTiDB}, wantErr: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			old := validationtest.Load(t, instanceFixture)
			for _, f := range tt.old {
				f(old)
			}
			if got := v.Run(old, nil); len(got.Errors) > 0 {
				t.Fatalf("fixture is invalid: %v", got.Errors)
			}
			updated := old.DeepCopy()
			for _, f := range tt.updated {
				f(updated)
			}
			if got := v.Run(updated, old); (len(got.Errors) > 0) != tt.wantErr {
				t.Errorf("errors = %v, want error: %v", got.Errors, tt.wantErr)
			}
		})
	}
}
