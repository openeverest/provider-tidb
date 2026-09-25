package provider

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

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
