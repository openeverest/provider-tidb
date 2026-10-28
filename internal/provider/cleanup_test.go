package provider

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func cleanupContext(t *testing.T, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, testInstance(testVersion), common.ProviderName), cl
}

// The Instance, and with it its name, is only released once TiDB Operator has
// torn the cluster down, so a new Instance with the same name can't be caught
// by the old cluster's teardown.
func TestCleanupWaitsForTheClusterToBeTornDown(t *testing.T) {
	tikv := &tidbcorev1.TiKV{ObjectMeta: metav1.ObjectMeta{Name: "orders-abc123", Namespace: testNamespace,
		Labels: map[string]string{tidbcorev1.LabelKeyCluster: "orders"}}}
	c, cl := cleanupContext(t, testCluster(), tikv)

	if err := New().Cleanup(c); !controller.IsWaitError(err) {
		t.Fatalf("Cleanup = %v, want to wait for the teardown", err)
	}
	if found, _ := c.Exists(&tidbcorev1.Cluster{}, "orders"); found {
		t.Error("Cleanup did not delete the Cluster")
	}

	// The Cluster is gone, but the operator is still removing a TiKV.
	if err := New().Cleanup(c); !controller.IsWaitError(err) {
		t.Fatalf("Cleanup = %v, want to wait while TiKV objects remain", err)
	}

	if err := cl.Delete(context.Background(), tikv); err != nil {
		t.Fatal(err)
	}
	if err := New().Cleanup(c); err != nil {
		t.Fatalf("Cleanup = %v, want done once the cluster is gone", err)
	}
}
