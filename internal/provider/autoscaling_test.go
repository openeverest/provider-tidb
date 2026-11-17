package provider

import (
	"context"
	"encoding/json"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// withAutoscaling gives TiDB replicas, a CPU limit unless cpu is empty, and
// the given parameters unless params is empty.
func withAutoscaling(in *corev1alpha1.Instance, replicas int32, cpu, params string) *corev1alpha1.Instance {
	comp := corev1alpha1.ComponentSpec{Type: common.ComponentTiDB, Replicas: &replicas}
	if cpu != "" {
		comp.Resources = &corev1.ResourceRequirements{Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse(cpu)}}
	}
	if params != "" {
		comp.Parameters = &runtime.RawExtension{Raw: []byte(params)}
	}
	in.Spec.Components[common.ComponentTiDB] = comp
	return in
}

func TestValidateAutoscaling(t *testing.T) {
	tests := []struct {
		name     string
		replicas int32
		cpu      string
		params   string
		wantErr  string
	}{
		{"off", 2, "", `{"config":""}`, ""},
		{"tidb", 2, "1", `{"autoscaling":{"maxReplicas":4,"targetCPUUtilization":70}}`, ""},
		{"max equal to replicas", 2, "1", `{"autoscaling":{"maxReplicas":2}}`, ""},
		{"max below replicas", 3, "1", `{"autoscaling":{"maxReplicas":2}}`,
			"tidb autoscaling.maxReplicas (2) must be at least its replicas (3)"},
		{"no cpu limit", 2, "", `{"autoscaling":{"maxReplicas":4}}`, "tidb autoscaling needs a CPU limit"},
		{"target too low", 2, "1", `{"autoscaling":{"maxReplicas":4,"targetCPUUtilization":0}}`,
			"targetCPUUtilization must be between 1 and 100"},
		{"target too high", 2, "1", `{"autoscaling":{"maxReplicas":4,"targetCPUUtilization":150}}`,
			"targetCPUUtilization must be between 1 and 100"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := withAutoscaling(testInstance("8.5.2"), tt.replicas, tt.cpu, tt.params)
			assertError(t, validateAutoscaling(validationContext(t, in)), tt.wantErr)
		})
	}
}

func autoscalingClient(t *testing.T, funcs interceptor.Funcs, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range append([]func(*runtime.Scheme) error{corev1alpha1.AddToScheme}, New().SchemeFuncs...) {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
}

func applyTiDB(cl client.Client, in *corev1alpha1.Instance) error {
	c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
	owner := &tidbcorev1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: testNamespace, UID: "cluster-uid"}}
	comp := in.Spec.Components[common.ComponentTiDB]
	return applyAutoscalable(c, owner, common.ComponentTiDB, comp, buildTiDBGroup(c, comp, ""), &tidbcorev1.TiDBGroup{},
		func(g *tidbcorev1.TiDBGroup) **int32 { return &g.Spec.Replicas })
}

func tidbReplicas(t *testing.T, cl client.Client) int32 {
	t.Helper()
	g := &tidbcorev1.TiDBGroup{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders"}, g); err != nil {
		t.Fatal(err)
	}
	if g.Spec.Replicas == nil {
		t.Fatal("tidb group has no replicas")
	}
	return *g.Spec.Replicas
}

// scaleTiDB changes the replica count as the autoscaler does.
func scaleTiDB(t *testing.T, cl client.Client, replicas int32) {
	t.Helper()
	g := &tidbcorev1.TiDBGroup{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders"}, g); err != nil {
		t.Fatal(err)
	}
	g.Spec.Replicas = &replicas
	if err := cl.Update(context.Background(), g); err != nil {
		t.Fatal(err)
	}
}

func TestApplyAutoscalable(t *testing.T) {
	const on = `{"autoscaling":{"maxReplicas":5}}`
	cl := autoscalingClient(t, interceptor.Funcs{})
	hpaKey := client.ObjectKey{Namespace: testNamespace, Name: "orders-tidb"}

	// A new group starts at the component's replicas, under an autoscaler.
	assertError(t, applyTiDB(cl, withAutoscaling(testInstance("8.5.2"), 2, "1", on)), "")
	if got := tidbReplicas(t, cl); got != 2 {
		t.Errorf("new group has %d replicas, want 2", got)
	}
	hpa := &autoscalingv2.HorizontalPodAutoscaler{}
	if err := cl.Get(context.Background(), hpaKey, hpa); err != nil {
		t.Fatalf("autoscaler: %v", err)
	}
	wantTarget := autoscalingv2.CrossVersionObjectReference{APIVersion: "core.pingcap.com/v1alpha1", Kind: "TiDBGroup", Name: "orders"}
	if hpa.Spec.ScaleTargetRef != wantTarget || *hpa.Spec.MinReplicas != 2 || hpa.Spec.MaxReplicas != 5 {
		t.Errorf("autoscaler = %+v, want %+v between 2 and 5", hpa.Spec, wantTarget)
	}
	if m := hpa.Spec.Metrics; len(m) != 1 || m[0].Resource == nil || m[0].Resource.Name != corev1.ResourceCPU ||
		*m[0].Resource.Target.AverageUtilization != defaultTargetCPUUtilization {
		t.Errorf("metrics = %+v, want CPU utilization at the default", hpa.Spec.Metrics)
	}

	steps := []struct {
		name     string
		scaleTo  int32
		replicas int32
		params   string
		want     int32
	}{
		{"keeps what the autoscaler chose", 4, 2, on, 4},
		{"raises to a higher minimum", 4, 5, on, 5},
		{"lowers to a lower maximum", 5, 2, `{"autoscaling":{"maxReplicas":3}}`, 3},
		{"returns to replicas once off", 3, 2, "", 2},
	}
	for _, s := range steps {
		scaleTiDB(t, cl, s.scaleTo)
		assertError(t, applyTiDB(cl, withAutoscaling(testInstance("8.5.2"), s.replicas, "1", s.params)), "")
		if got := tidbReplicas(t, cl); got != s.want {
			t.Errorf("%s: %d replicas, want %d", s.name, got, s.want)
		}
	}
	if err := cl.Get(context.Background(), hpaKey, hpa); !apierrors.IsNotFound(err) {
		t.Errorf("autoscaler after autoscaling is off: %v, want not found", err)
	}
}

// The group is applied at the resourceVersion its replicas were read at, and a
// conflict there is retried instead of failing the Instance.
func TestApplyAutoscalableDoesNotUndoConcurrentScaling(t *testing.T) {
	existing := &tidbcorev1.TiDBGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
	existing.Spec.Replicas = ptr.To[int32](3)
	var sent string
	cl := autoscalingClient(t, interceptor.Funcs{
		Apply: func(ctx context.Context, w client.WithWatch, obj runtime.ApplyConfiguration, opts ...client.ApplyOption) error {
			data, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			var body struct {
				Kind     string `json:"kind"`
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(data, &body); err != nil {
				return err
			}
			if body.Kind != "TiDBGroup" {
				return w.Apply(ctx, obj, opts...)
			}
			sent = body.Metadata.ResourceVersion
			return apierrors.NewConflict(schema.GroupResource{Group: "core.pingcap.com", Resource: "tidbgroups"}, "orders", nil)
		},
	}, existing)
	read := &tidbcorev1.TiDBGroup{}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(existing), read); err != nil {
		t.Fatal(err)
	}

	err := applyTiDB(cl, withAutoscaling(testInstance("8.5.2"), 2, "1", `{"autoscaling":{"maxReplicas":5}}`))

	if sent == "" || sent != read.ResourceVersion {
		t.Errorf("applied at resourceVersion %q, want the group's %q", sent, read.ResourceVersion)
	}
	if !controller.IsWaitError(err) {
		t.Errorf("err = %v, want a wait so the sync is retried", err)
	}
}
