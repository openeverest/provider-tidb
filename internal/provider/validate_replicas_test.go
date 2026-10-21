package provider

import (
	"encoding/json"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func TestValidateTiDBTiKVScaleIn(t *testing.T) {
	ptr := func(v int32) *int32 { return &v }
	tests := []struct {
		name      string
		current   *int32 // TiKVGroup replicas; nil means no cluster yet
		requested *int32 // Instance tikv replicas; nil falls back to the default 3
		pdConfig  string
		wantErr   string
	}{
		{name: "new single-node cluster", requested: ptr(1)},
		{name: "unchanged below max-replicas", current: ptr(1), requested: ptr(1)},
		{name: "scale out", current: ptr(3), requested: ptr(5)},
		{name: "scale in down to max-replicas", current: ptr(5), requested: ptr(3)},
		{name: "scale in to the default replicas", current: ptr(4), requested: nil},
		{name: "scale in below max-replicas", current: ptr(4), requested: ptr(2),
			wantErr: "scaling tikv in from 4 to 2 replicas is not supported: PD keeps 3 copies"},
		{name: "scale in a cluster already below max-replicas", current: ptr(2), requested: ptr(1),
			wantErr: "scaling tikv in from 2 to 1 replicas is not supported"},
		{name: "higher configured max-replicas", current: ptr(6), requested: ptr(4),
			pdConfig: "[replication]\nmax-replicas = 5\n",
			wantErr:  "PD keeps 5 copies of every region"},
		{name: "lower configured max-replicas", current: ptr(2), requested: ptr(1),
			pdConfig: "[replication]\nmax-replicas = 1\n"},
		{name: "other pd config keeps the default", current: ptr(3), requested: ptr(2),
			pdConfig: "[schedule]\nleader-schedule-limit = 4\n",
			wantErr:  "PD keeps 3 copies"},
		{name: "invalid pd config on scale-in", current: ptr(4), requested: ptr(3),
			pdConfig: "[replication\n", wantErr: "pd config is not valid TOML"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			tikv := in.Spec.Components[common.ComponentTiKV]
			tikv.Replicas = tt.requested
			in.Spec.Components[common.ComponentTiKV] = tikv
			if tt.pdConfig != "" {
				raw, err := json.Marshal(map[string]string{"config": tt.pdConfig})
				if err != nil {
					t.Fatal(err)
				}
				pd := in.Spec.Components[common.ComponentPD]
				pd.Parameters = &runtime.RawExtension{Raw: raw}
				in.Spec.Components[common.ComponentPD] = pd
			}

			objs := []client.Object{testProvider()}
			if tt.current != nil {
				group := &tidbcorev1.TiKVGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
				group.Spec.Replicas = tt.current
				objs = append(objs, group)
			}
			assertError(t, ValidateTiDB(validationContext(t, in, objs...)), tt.wantErr)
		})
	}
}
