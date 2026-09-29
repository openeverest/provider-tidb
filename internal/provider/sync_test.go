package provider

import (
	"context"
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

// Clearing a group's CPU and memory leaves an empty resources struct. Sending it
// as {} makes the API server store null after SSA prunes the old values, which
// the CRD rejects, so it must be left out of the apply body.
func TestApplyOwnedByClusterDropsImplicitEmptyStructs(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := tidbcorev1.Install(scheme); err != nil {
		t.Fatalf("building scheme: %v", err)
	}
	var body map[string]any
	cl := fake.NewClientBuilder().WithScheme(scheme).WithInterceptorFuncs(interceptor.Funcs{
		Apply: func(_ context.Context, _ client.WithWatch, obj runtime.ApplyConfiguration, _ ...client.ApplyOption) error {
			data, err := json.Marshal(obj)
			if err != nil {
				return err
			}
			return json.Unmarshal(data, &body)
		},
	}).Build()
	in := testInstance(testVersion)
	c := controller.NewContext(context.Background(), cl, in, common.ProviderName)
	cluster := &tidbcorev1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: in.Name, Namespace: testNamespace, UID: "cluster-uid"}}

	if err := applyOwnedByCluster(c, cluster, buildTiKVGroup(c, in.Spec.Components[common.ComponentTiKV], "")); err != nil {
		t.Fatalf("apply: %v", err)
	}

	spec, _ := body["spec"].(map[string]any)
	template, _ := spec["template"].(map[string]any)
	templateSpec, _ := template["spec"].(map[string]any)
	if _, ok := templateSpec["resources"]; ok {
		t.Errorf("apply body has spec.template.spec.resources = %v, want it omitted", templateSpec["resources"])
	}
	if _, ok := templateSpec["volumes"]; !ok {
		t.Errorf("apply body lost spec.template.spec.volumes: %v", templateSpec)
	}
}

func TestToResources(t *testing.T) {
	if got := toResources(nil); got.CPU != nil || got.Memory != nil {
		t.Fatalf("nil resources should map to empty, got %+v", got)
	}

	r := &corev1.ResourceRequirements{
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("2"),
			corev1.ResourceMemory: resource.MustParse("4Gi"),
		},
	}
	got := toResources(r)
	if got.CPU == nil || got.CPU.String() != "2" {
		t.Errorf("CPU = %v, want 2", got.CPU)
	}
	if got.Memory == nil || got.Memory.String() != "4Gi" {
		t.Errorf("Memory = %v, want 4Gi", got.Memory)
	}
}

func TestDataVolume(t *testing.T) {
	// Defaults when storage is unset.
	v := dataVolume(nil, tidbcorev1.VolumeMountTypePDData)
	if v.Name != "data" {
		t.Errorf("Name = %q, want data", v.Name)
	}
	if len(v.Mounts) != 1 || v.Mounts[0].Type != tidbcorev1.VolumeMountTypePDData {
		t.Errorf("Mounts = %+v, want single data mount", v.Mounts)
	}
	if v.Storage.String() != "10Gi" {
		t.Errorf("Storage = %v, want 10Gi default", v.Storage)
	}
	if v.StorageClassName != nil {
		t.Errorf("StorageClassName = %v, want nil", v.StorageClassName)
	}

	// User-supplied size and class win.
	sc := "fast"
	size := resource.MustParse("50Gi")
	v = dataVolume(&corev1alpha1.Storage{Size: size, StorageClass: &sc}, tidbcorev1.VolumeMountTypeTiKVData)
	if v.Storage.String() != "50Gi" {
		t.Errorf("Storage = %v, want 50Gi", v.Storage)
	}
	if v.StorageClassName == nil || *v.StorageClassName != "fast" {
		t.Errorf("StorageClassName = %v, want fast", v.StorageClassName)
	}
}

func TestReplicasOrDefault(t *testing.T) {
	if got := replicasOrDefault(nil, 3); got == nil || *got != 3 {
		t.Errorf("nil replicas should fall back to default 3, got %v", got)
	}
	five := int32(5)
	if got := replicasOrDefault(&five, 3); got == nil || *got != 5 {
		t.Errorf("explicit replicas should win, got %v", got)
	}
}
