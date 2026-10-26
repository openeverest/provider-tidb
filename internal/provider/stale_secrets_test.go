package provider

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

// secretOwnedBy is a Secret controlled by a Cluster or Instance with the given UID.
func secretOwnedBy(name, kind string, uid types.UID, data map[string][]byte) *corev1.Secret {
	yes := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: testNamespace,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: kind, Name: "orders", UID: uid, Controller: &yes}},
		},
		Data: data,
	}
}

// An Instance deleted and created again under the same name finds the previous
// cluster's TLS Secrets still waiting for garbage collection. Reusing them left
// the new cluster with a CA that then disappeared and no PD certificate.
func TestTLSSecretsOfAPreviousClusterAreReplaced(t *testing.T) {
	stale := map[string][]byte{"ca.crt": []byte("old")}
	c, cl := tlsContext(t, tlsTopology(true, true),
		secretOwnedBy("orders-tls-ca", "Cluster", "old-cluster-uid", stale),
		secretOwnedBy("orders-pd-cluster-secret", "Cluster", "old-cluster-uid", stale))
	owner := testCluster()

	err := ensureTLSSecrets(c, owner)
	if !controller.IsWaitError(err) {
		t.Fatalf("ensureTLSSecrets = %v, want a wait while the stale Secrets go", err)
	}
	for _, name := range []string{"orders-tls-ca"} {
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Errorf("stale %s still there: %v", name, err)
		}
	}

	// Once the stale Secrets are gone, everything is issued for the new cluster.
	_ = cl.Delete(context.Background(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "orders-pd-cluster-secret", Namespace: testNamespace}})
	if err := ensureTLSSecrets(c, owner); err != nil {
		t.Fatalf("ensureTLSSecrets: %v", err)
	}
	for _, name := range []string{"orders-tls-ca", "orders-pd-cluster-secret", "orders-tikv-cluster-secret"} {
		s := &corev1.Secret{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, s); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if ref := metav1.GetControllerOf(s); ref == nil || ref.UID != owner.UID {
			t.Errorf("%s is owned by %+v, want the current Cluster", name, ref)
		}
	}
}

func TestRootPasswordOfAPreviousInstanceIsNotReused(t *testing.T) {
	c, cl := brContext(t, secretOwnedBy(rootSecretName("orders"), "Instance", "old-instance-uid",
		map[string][]byte{rootPasswordKey: []byte("previous")}))

	if got := readRootPassword(c); got != "" {
		t.Errorf("connection password = %q, want none from the previous Instance", got)
	}
	if _, err := ensureRootPassword(c); !controller.IsWaitError(err) {
		t.Fatalf("ensureRootPassword = %v, want a wait while the stale Secret goes", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: rootSecretName("orders")}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Errorf("stale root password Secret still there: %v", err)
	}
	pw, err := ensureRootPassword(c)
	if err != nil || pw == "" || pw == "previous" {
		t.Errorf("ensureRootPassword = %q, %v; want a new password", pw, err)
	}
}
