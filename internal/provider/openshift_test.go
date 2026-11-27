package provider

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func TestOpenShiftSlowLog(t *testing.T) {
	for _, openShift := range []bool{false, true} {
		scheme := runtime.NewScheme()
		if err := tidbcorev1.Install(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
		mapper := meta.NewDefaultRESTMapper(nil)
		if openShift {
			mapper.Add(sccKind.WithVersion("v1"), meta.RESTScopeRoot)
		}
		cl := fake.NewClientBuilder().WithScheme(scheme).WithRESTMapper(mapper).Build()
		in := testInstance(testVersion)
		c := controller.NewContext(context.Background(), cl, in, common.ProviderName)

		group := buildTiDBGroup(c, in.Spec.Components[common.ComponentTiDB], "")
		if err := addOpenShiftSlowLog(c, group); err != nil {
			t.Fatalf("openShift=%v: %v", openShift, err)
		}
		// On OpenShift TiDB runs under a namespace UID that can't write to /.
		if got := group.Spec.Template.Spec.SlowLog != nil; got != openShift {
			t.Errorf("openShift=%v: slowLog set = %v, want %v", openShift, got, openShift)
		}
	}
}
