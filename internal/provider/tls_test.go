package provider

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

func tlsTopology(clusterTLS, clientTLS bool) cluster.ClusterTopologyParameters {
	return cluster.ClusterTopologyParameters{TLS: &cluster.TLS{Cluster: clusterTLS, Client: clientTLS}}
}

func tlsContext(t *testing.T, params any, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install, corev1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, monitoredInstance(t, params), common.ProviderName), cl
}

func testCluster() *tidbcorev1.Cluster {
	return &tidbcorev1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace, UID: "cluster-uid"}}
}

func parseCert(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestEnsureTLSSecrets(t *testing.T) {
	c, cl := tlsContext(t, tlsTopology(true, true))
	owner := testCluster()

	if err := ensureTLSSecrets(c, owner); err != nil {
		t.Fatalf("ensureTLSSecrets: %v", err)
	}

	caSecret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders-tls-ca"}, caSecret); err != nil {
		t.Fatalf("CA secret: %v", err)
	}
	ca := parseCert(t, caSecret.Data["ca.crt"])
	if !ca.IsCA {
		t.Errorf("CA certificate is not a CA")
	}
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	type want struct {
		dnsName string // a name the certificate must be valid for; empty for client-only
		usage   x509.ExtKeyUsage
	}
	wants := map[string]want{
		"orders-pd-cluster-secret":      {"orders-pd-0.orders-pd-peer.db.svc", x509.ExtKeyUsageServerAuth},
		"orders-tikv-cluster-secret":    {"orders-tikv.db.svc", x509.ExtKeyUsageServerAuth},
		"orders-tidb-cluster-secret":    {"orders-tidb", x509.ExtKeyUsageClientAuth},
		"orders-tiflash-cluster-secret": {"orders-tiflash-abc.orders-tiflash-peer.db", x509.ExtKeyUsageServerAuth},
		"orders-cluster-client-secret":  {"", x509.ExtKeyUsageClientAuth},
		"orders-tidb-server-secret":     {"orders-tidb.db", x509.ExtKeyUsageServerAuth},
	}
	for name, w := range wants {
		secret := &corev1.Secret{}
		if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, secret); err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if secret.Type != corev1.SecretTypeTLS || string(secret.Data["ca.crt"]) != string(caSecret.Data["ca.crt"]) || len(secret.Data["tls.key"]) == 0 {
			t.Errorf("%s: type %s, keys %v, want a TLS secret carrying the CA", name, secret.Type, slices.Sorted(func(yield func(string) bool) {
				for k := range secret.Data {
					if !yield(k) {
						return
					}
				}
			}))
		}
		owner := metav1.GetControllerOf(secret)
		if owner == nil || owner.Kind != "Cluster" || owner.Name != "orders" {
			t.Errorf("%s: controller owner = %+v, want the Cluster", name, owner)
		}
		cert := parseCert(t, secret.Data["tls.crt"])
		if _, err := cert.Verify(x509.VerifyOptions{Roots: roots, DNSName: w.dnsName, KeyUsages: []x509.ExtKeyUsage{w.usage}}); err != nil {
			t.Errorf("%s: does not verify for %q as %v: %v", name, w.dnsName, w.usage, err)
		}
		if cert.NotAfter.Sub(cert.NotBefore) < 9*365*24*60*60*1e9 {
			t.Errorf("%s: valid %s..%s, want about ten years", name, cert.NotBefore, cert.NotAfter)
		}
	}
	if pd := parseCert(t, mustSecret(t, cl, "orders-pd-cluster-secret").Data["tls.crt"]); pd.Subject.CommonName != "TiDB" {
		t.Errorf("cluster certificate CN = %q, want TiDB", pd.Subject.CommonName)
	}

	// A second pass issues nothing new.
	before := mustSecret(t, cl, "orders-tikv-cluster-secret").Data["tls.crt"]
	if err := ensureTLSSecrets(c, owner); err != nil {
		t.Fatalf("second ensureTLSSecrets: %v", err)
	}
	if after := mustSecret(t, cl, "orders-tikv-cluster-secret").Data["tls.crt"]; string(after) != string(before) {
		t.Error("second pass reissued the TiKV certificate")
	}
}

func mustSecret(t *testing.T, cl client.Client, name string) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, secret); err != nil {
		t.Fatalf("get %s: %v", name, err)
	}
	return secret
}

func TestEnsureTLSSecretsSelection(t *testing.T) {
	tests := []struct {
		name       string
		params     any
		wantSecret []string
	}{
		{name: "no parameters", params: nil},
		{name: "both off", params: tlsTopology(false, false)},
		{name: "client only", params: tlsTopology(false, true),
			wantSecret: []string{"orders-tidb-server-secret", "orders-tls-ca"}},
		{name: "cluster only", params: tlsTopology(true, false),
			wantSecret: []string{"orders-cluster-client-secret", "orders-pd-cluster-secret", "orders-tidb-cluster-secret",
				"orders-tiflash-cluster-secret", "orders-tikv-cluster-secret", "orders-tls-ca"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := tlsContext(t, tt.params)
			if err := ensureTLSSecrets(c, testCluster()); err != nil {
				t.Fatal(err)
			}
			list := &corev1.SecretList{}
			if err := cl.List(context.Background(), list); err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, s := range list.Items {
				got = append(got, s.Name)
			}
			slices.Sort(got)
			if !slices.Equal(got, tt.wantSecret) {
				t.Errorf("secrets = %v, want %v", got, tt.wantSecret)
			}
		})
	}
}

func TestEnsureTLSSecretsKeepsIssuedCertificates(t *testing.T) {
	issued := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "orders-pd-cluster-secret", Namespace: testNamespace}}
	c, _ := tlsContext(t, tlsTopology(true, false), issued)

	err := ensureTLSSecrets(c, testCluster())
	if err == nil || !strings.Contains(err.Error(), `CA secret "orders-tls-ca" is missing but certificate secret "orders-pd-cluster-secret" exists`) {
		t.Errorf("error = %v, want the missing CA reported instead of a new CA", err)
	}
}

func TestSyncTLSFields(t *testing.T) {
	tests := []struct {
		name        string
		params      any
		existing    *tidbcorev1.Cluster
		wantCluster bool
		wantClient  bool
	}{
		{name: "plaintext", params: nil},
		{name: "both on", params: tlsTopology(true, true), wantCluster: true, wantClient: true},
		{name: "client only", params: tlsTopology(false, true), wantClient: true},
		{name: "existing cluster keeps its setting", params: tlsTopology(false, false),
			existing: func() *tidbcorev1.Cluster {
				cl := testCluster()
				cl.Spec.TLSCluster = &tidbcorev1.TLSCluster{Enabled: true}
				return cl
			}(), wantCluster: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			c, _ := tlsContext(t, tt.params, objs...)
			built, err := buildCluster(c)
			if err != nil {
				t.Fatal(err)
			}
			if got := built.Spec.TLSCluster != nil && built.Spec.TLSCluster.Enabled; got != tt.wantCluster {
				t.Errorf("tlsCluster = %v, want %v", got, tt.wantCluster)
			}
			tidb := buildTiDBGroup(c, c.Instance().Spec.Components[common.ComponentTiDB], "")
			sec := tidb.Spec.Template.Spec.Security
			if got := sec != nil && sec.TLS != nil && sec.TLS.MySQL != nil && sec.TLS.MySQL.Enabled; got != tt.wantClient {
				t.Errorf("mysql tls = %v, want %v", got, tt.wantClient)
			}
		})
	}
}

func TestValidateTiDBTLSImmutable(t *testing.T) {
	clusterWith := func(enabled bool) client.Object {
		cl := testCluster()
		if enabled {
			cl.Spec.TLSCluster = &tidbcorev1.TLSCluster{Enabled: true}
		}
		return cl
	}
	tidbWith := func(enabled bool) client.Object {
		g := &tidbcorev1.TiDBGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
		g.Spec.Template.Spec.Version = testVersion
		if enabled {
			g.Spec.Template.Spec.Security = &tidbcorev1.TiDBSecurity{TLS: &tidbcorev1.TiDBTLS{MySQL: &tidbcorev1.TLS{Enabled: true}}}
		}
		return g
	}
	tests := []struct {
		name    string
		params  any
		objs    []client.Object
		wantErr string
	}{
		{name: "new instance", params: tlsTopology(true, true)},
		{name: "unchanged", params: tlsTopology(true, true), objs: []client.Object{clusterWith(true), tidbWith(true)}},
		{name: "existing plaintext instance without the field", params: nil, objs: []client.Object{clusterWith(false), tidbWith(false)}},
		{name: "turning cluster tls on", params: tlsTopology(true, false), objs: []client.Object{clusterWith(false), tidbWith(false)},
			wantErr: "tls.cluster cannot be changed after the Instance is created: it is off"},
		{name: "turning cluster tls off", params: tlsTopology(false, true), objs: []client.Object{clusterWith(true), tidbWith(true)},
			wantErr: "tls.cluster cannot be changed after the Instance is created: it is on"},
		{name: "turning client tls off", params: tlsTopology(true, false), objs: []client.Object{clusterWith(true), tidbWith(true)},
			wantErr: "tls.client cannot be changed after the Instance is created: it is on"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := tlsContext(t, tt.params, append([]client.Object{testProvider()}, tt.objs...)...)
			assertError(t, ValidateTiDB(c), tt.wantErr)
		})
	}
}

func TestConnectionDetailsCarryTheCA(t *testing.T) {
	for _, clientTLS := range []bool{false, true} {
		c, _ := tlsContext(t, tlsTopology(true, clientTLS))
		if err := ensureTLSSecrets(c, testCluster()); err != nil {
			t.Fatal(err)
		}
		ca := connectionDetails(c).AdditionalProperties[tlsKeyCA]
		if clientTLS != strings.HasPrefix(ca, "-----BEGIN CERTIFICATE-----") {
			t.Errorf("client tls %v: ca.crt = %.30q", clientTLS, ca)
		}
	}
}
