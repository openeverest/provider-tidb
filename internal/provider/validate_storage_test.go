package provider

import (
	"testing"

	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func TestCheckNoShrink(t *testing.T) {
	current := []tidbcorev1.Volume{{Name: dataVolumeName, Storage: resource.MustParse("20Gi")}}
	storage := func(size string) *corev1alpha1.Storage {
		return &corev1alpha1.Storage{Size: resource.MustParse(size)}
	}
	tests := []struct {
		name      string
		current   []tidbcorev1.Volume
		requested *corev1alpha1.Storage
		wantErr   string
	}{
		{"expand", current, storage("50Gi"), ""},
		{"unchanged", current, storage("20Gi"), ""},
		{"same size in other units", current, storage("20480Mi"), ""},
		{"shrink", current, storage("15Gi"), "shrinking tikv storage from 20Gi to 15Gi is not supported"},
		{"size removed falls back to a smaller default", current, nil, "shrinking tikv storage from 20Gi to 10Gi"},
		{"group without a data volume", nil, storage("5Gi"), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertError(t, checkNoShrink(common.ComponentTiKV, tt.current, tt.requested), tt.wantErr)
		})
	}
}

func TestValidateTiDBStorageChanges(t *testing.T) {
	tests := []struct {
		name     string
		existing bool
		pdSize   string
		tikvSize string
		wantErr  string
	}{
		{"new instance", false, "5Gi", "5Gi", ""},
		{"expand tikv", true, "10Gi", "40Gi", ""},
		{"shrink tikv", true, "10Gi", "10Gi", "shrinking tikv storage from 20Gi to 10Gi is not supported"},
		{"shrink pd", true, "5Gi", "20Gi", "shrinking pd storage from 10Gi to 5Gi is not supported"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			setStorage(in, common.ComponentPD, tt.pdSize)
			setStorage(in, common.ComponentTiKV, tt.tikvSize)

			objs := []client.Object{testProvider()}
			if tt.existing {
				objs = append(objs, groupsWithStorage("10Gi", "20Gi")...)
			}
			assertError(t, ValidateTiDB(validationContext(t, in, objs...)), tt.wantErr)
		})
	}
}

func setStorage(in *corev1alpha1.Instance, component, size string) {
	comp := in.Spec.Components[component]
	comp.Storage = &corev1alpha1.Storage{Size: resource.MustParse(size)}
	in.Spec.Components[component] = comp
}

func groupsWithStorage(pdSize, tikvSize string) []client.Object {
	meta := metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}
	pd := &tidbcorev1.PDGroup{ObjectMeta: meta}
	pd.Spec.Template.Spec.Version = testVersion
	pd.Spec.Template.Spec.Volumes = []tidbcorev1.Volume{dataVolume(&corev1alpha1.Storage{Size: resource.MustParse(pdSize)}, tidbcorev1.VolumeMountTypePDData)}
	tikv := &tidbcorev1.TiKVGroup{ObjectMeta: meta}
	tikv.Spec.Template.Spec.Version = testVersion
	tikv.Spec.Template.Spec.Volumes = []tidbcorev1.Volume{dataVolume(&corev1alpha1.Storage{Size: resource.MustParse(tikvSize)}, tidbcorev1.VolumeMountTypeTiKVData)}
	return []client.Object{pd, tikv}
}
