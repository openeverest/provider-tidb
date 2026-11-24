package provider

import (
	"context"
	"slices"
	"testing"
	"time"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta/testrestmapper"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition"
	"github.com/openeverest/provider-tidb/internal/common"
)

func issuerTLS(name string, kind definition.IssuerKind, group string) *definition.TLSParameters {
	return &definition.TLSParameters{Mode: definition.TLSModePrefer,
		Issuer: &definition.IssuerReference{Name: name, Kind: kind, Group: group}}
}

// certManagerContext builds a context whose client knows cert-manager's types;
// withCRDs controls whether its REST mapper serves them, as an installed
// cert-manager would.
func certManagerContext(t *testing.T, tls *definition.TLSParameters, withCRDs bool, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	installs := []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, tidbcorev1.Install, corev1.AddToScheme}
	scheme := runtime.NewScheme()
	mapperScheme := runtime.NewScheme()
	for _, install := range append(installs, cmapi.AddToScheme) {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	if withCRDs {
		installs = append(installs, cmapi.AddToScheme)
	}
	for _, install := range installs {
		if err := install(mapperScheme); err != nil {
			t.Fatal(err)
		}
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).
		WithRESTMapper(testrestmapper.TestOnlyStaticRESTMapper(mapperScheme)).
		WithObjects(objs...).Build()
	return controller.NewContext(context.Background(), cl, tlsInstance(t, tls), common.ProviderName), cl
}

func listCertificates(t *testing.T, cl client.Client) map[string]cmapi.Certificate {
	t.Helper()
	list := &cmapi.CertificateList{}
	if err := cl.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	certs := map[string]cmapi.Certificate{}
	for _, cert := range list.Items {
		certs[cert.Name] = cert
	}
	return certs
}

func listIssuers(t *testing.T, cl client.Client) []string {
	t.Helper()
	list := &cmapi.IssuerList{}
	if err := cl.List(context.Background(), list); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, issuer := range list.Items {
		names = append(names, issuer.Name)
	}
	slices.Sort(names)
	return names
}

func TestCertManagerSelfSignedCA(t *testing.T) {
	// Managed certificates go through cert-manager because it is installed.
	c, cl := certManagerContext(t, tlsMode(definition.TLSModePrefer), true)
	if err := issueTLS(c, testCluster()); err != nil {
		t.Fatalf("ensureTLSSecrets: %v", err)
	}

	if got, want := listIssuers(t, cl), []string{"orders-tls-ca", "orders-tls-selfsigned"}; !slices.Equal(got, want) {
		t.Errorf("issuers = %v, want %v", got, want)
	}
	certs := listCertificates(t, cl)
	ca, ok := certs["orders-tls-ca"]
	if !ok || !ca.Spec.IsCA || ca.Spec.IssuerRef.Name != "orders-tls-selfsigned" || ca.Spec.SecretName != "orders-tls-ca" {
		t.Errorf("CA certificate = %+v, want a CA signed by the self-signed issuer", ca.Spec)
	}

	wantLeaves := []string{
		"orders-cluster-client-secret", "orders-pd-cluster-secret", "orders-tidb-cluster-secret",
		"orders-tidb-server-secret", "orders-tiflash-cluster-secret", "orders-tikv-cluster-secret",
	}
	for _, name := range wantLeaves {
		cert, ok := certs[name]
		if !ok {
			t.Errorf("no certificate %s", name)
			continue
		}
		if cert.Spec.SecretName != name || cert.Spec.IssuerRef.Name != "orders-tls-ca" || cert.Spec.IssuerRef.Kind != "Issuer" {
			t.Errorf("%s: secret %q issuer %+v, want its secret signed by the CA issuer", name, cert.Spec.SecretName, cert.Spec.IssuerRef)
		}
		if owner := metav1.GetControllerOf(&cert); owner == nil || owner.Kind != "Cluster" {
			t.Errorf("%s: controller owner = %+v, want the Cluster", name, owner)
		}
	}
	if len(certs) != len(wantLeaves)+1 {
		t.Errorf("got %d certificates, want %d", len(certs), len(wantLeaves)+1)
	}

	pd := certs["orders-pd-cluster-secret"].Spec
	if pd.CommonName != "TiDB" || !slices.Contains(pd.DNSNames, "*.orders-pd-peer.db.svc") ||
		!slices.Equal(pd.IPAddresses, []string{"127.0.0.1", "::1"}) ||
		!slices.Equal(pd.Usages, []cmapi.KeyUsage{cmapi.UsageDigitalSignature, cmapi.UsageKeyEncipherment, cmapi.UsageServerAuth, cmapi.UsageClientAuth}) ||
		pd.Duration == nil || pd.Duration.Duration < 9*365*24*time.Hour {
		t.Errorf("pd certificate = %+v", pd)
	}
	clientCert := certs["orders-cluster-client-secret"].Spec
	if len(clientCert.DNSNames) != 0 || len(clientCert.IPAddresses) != 0 ||
		!slices.Equal(clientCert.Usages, []cmapi.KeyUsage{cmapi.UsageDigitalSignature, cmapi.UsageKeyEncipherment, cmapi.UsageClientAuth}) {
		t.Errorf("client certificate = %+v", clientCert)
	}

	// The provider issues nothing itself.
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders-pd-cluster-secret"}, &corev1.Secret{}); err == nil {
		t.Error("the provider created a certificate Secret itself")
	}
}

func TestCertManagerIssuer(t *testing.T) {
	tests := []struct {
		name        string
		tls         *definition.TLSParameters
		wantIssuer  string
		wantKind    string
		wantGroup   string
		wantIssuers []string
	}{
		{name: "managed", tls: tlsMode(definition.TLSModePrefer),
			wantIssuer: "orders-tls-ca", wantKind: "Issuer", wantGroup: "cert-manager.io",
			wantIssuers: []string{"orders-tls-ca", "orders-tls-selfsigned"}},
		{name: "issuer with defaults", tls: issuerTLS("corp-ca", "", ""),
			wantIssuer: "corp-ca", wantKind: "Issuer", wantGroup: "cert-manager.io"},
		{name: "cluster issuer", tls: issuerTLS("corp-ca", definition.IssuerKindClusterIssuer, ""),
			wantIssuer: "corp-ca", wantKind: "ClusterIssuer", wantGroup: "cert-manager.io"},
		{name: "external issuer", tls: issuerTLS("pca", "AWSPCAClusterIssuer", "awspca.cert-manager.io"),
			wantIssuer: "pca", wantKind: "AWSPCAClusterIssuer", wantGroup: "awspca.cert-manager.io"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, cl := certManagerContext(t, tt.tls, true)
			if err := issueTLS(c, testCluster()); err != nil {
				t.Fatal(err)
			}
			cert, ok := listCertificates(t, cl)["orders-pd-cluster-secret"]
			if !ok {
				t.Fatal("no PD certificate")
			}
			if ref := cert.Spec.IssuerRef; ref.Name != tt.wantIssuer || ref.Kind != tt.wantKind || ref.Group != tt.wantGroup {
				t.Errorf("issuerRef = %+v, want %s.%s %s", ref, tt.wantKind, tt.wantGroup, tt.wantIssuer)
			}
			if got := listIssuers(t, cl); !slices.Equal(got, tt.wantIssuers) {
				t.Errorf("issuers = %v, want %v", got, tt.wantIssuers)
			}
		})
	}
}

func TestCertManagerAdditionalNames(t *testing.T) {
	tls := issuerTLS("corp-ca", definition.IssuerKindClusterIssuer, "")
	tls.AdditionalNames = []string{"db.example.com", "*.db.example.com"}
	c, cl := certManagerContext(t, tls, true)
	withTiProxy(c.Instance(), 2, "")
	if err := issueTLS(c, testCluster()); err != nil {
		t.Fatal(err)
	}
	certs := listCertificates(t, cl)
	for _, name := range []string{"orders-tidb-server-secret", "orders-tiproxy-server-secret"} {
		names := certs[name].Spec.DNSNames
		if !slices.Contains(names, "db.example.com") || !slices.Contains(names, "*.db.example.com") {
			t.Errorf("%s: dnsNames %v lack the additional names", name, names)
		}
	}
	if names := certs["orders-pd-cluster-secret"].Spec.DNSNames; slices.Contains(names, "db.example.com") {
		t.Errorf("cluster certificate carries the additional names: %v", names)
	}
}

func TestCertManagerSecretsAreAdopted(t *testing.T) {
	issued := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "orders-tidb-server-secret", Namespace: testNamespace},
		Type:       corev1.SecretTypeTLS,
		Data:       map[string][]byte{"ca.crt": []byte("ca"), "tls.crt": []byte("cert"), "tls.key": []byte("key")},
	}
	// The fake client's server-side apply assigns no UID; start from a
	// Certificate that already exists, as it does once cert-manager wrote it.
	existing := &cmapi.Certificate{ObjectMeta: metav1.ObjectMeta{Name: "orders-tidb-server-secret", Namespace: testNamespace, UID: "cert-uid"}}
	c, cl := certManagerContext(t, issuerTLS("corp-ca", "", ""), true, issued, existing)
	if err := issueTLS(c, testCluster()); err != nil {
		t.Fatal(err)
	}

	cert := &cmapi.Certificate{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders-tidb-server-secret"}, cert); err != nil {
		t.Fatal(err)
	}
	if cert.UID != "cert-uid" {
		t.Fatalf("certificate UID = %q, want the existing one", cert.UID)
	}
	secret := mustSecret(t, cl, "orders-tidb-server-secret")
	if len(secret.OwnerReferences) != 1 || secret.OwnerReferences[0].Kind != "Certificate" ||
		secret.OwnerReferences[0].Name != cert.Name || secret.OwnerReferences[0].UID != cert.UID {
		t.Errorf("owner references = %+v, want the Certificate %s (%s)", secret.OwnerReferences, cert.Name, cert.UID)
	}
	if c := secret.OwnerReferences[0].Controller; c != nil && *c {
		t.Error("adopted as controller; cert-manager may need that")
	}
	if string(secret.Data["tls.crt"]) != "cert" {
		t.Error("adopting the Secret changed its data")
	}
	if ca := connectionDetails(c).AdditionalProperties[tlsKeyCA]; ca != "ca" {
		t.Errorf("connection ca.crt = %q, want the CA cert-manager wrote", ca)
	}
}

func TestValidateTiDBIssuerNeedsCertManager(t *testing.T) {
	for _, withCRDs := range []bool{true, false} {
		c, _ := certManagerContext(t, issuerTLS("corp", definition.IssuerKindClusterIssuer, ""), withCRDs, testProvider())
		want := ""
		if !withCRDs {
			want = "issuer requires cert-manager: the Certificate CRD (cert-manager.io/v1) is not installed"
		}
		assertError(t, validateAll(c), want)
	}
}

func TestBuildClusterRecordsTLSSource(t *testing.T) {
	existingWith := func(source string) *tidbcorev1.Cluster {
		cl := testCluster()
		cl.Spec.TLSCluster = &tidbcorev1.TLSCluster{Enabled: true}
		if source != "" {
			cl.Annotations = map[string]string{tlsSourceAnnotation: source}
		}
		return cl
	}
	tests := []struct {
		name        string
		tls         *definition.TLSParameters
		certManager bool
		existing    client.Object
		want        string
	}{
		{name: "plaintext", tls: nil, want: ""},
		{name: "provider without cert-manager", tls: tlsMode(definition.TLSModePrefer), want: "provider"},
		{name: "cert-manager when installed", tls: tlsMode(definition.TLSModeRequire), certManager: true, want: "cert-manager"},
		{name: "user issuer", tls: issuerTLS("corp", definition.IssuerKindClusterIssuer, ""), certManager: true,
			want: "cert-manager:ClusterIssuer/corp"},
		{name: "external issuer", tls: issuerTLS("pca", "AWSPCAClusterIssuer", "awspca.cert-manager.io"), certManager: true,
			want: "cert-manager:AWSPCAClusterIssuer.awspca.cert-manager.io/pca"},
		{name: "existing cluster keeps its source", tls: tlsMode(definition.TLSModePrefer), certManager: true,
			existing: existingWith("provider"), want: "provider"},
		{name: "cluster from before the source was recorded", tls: tlsMode(definition.TLSModePrefer), certManager: true,
			existing: existingWith(""), want: "provider"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objs []client.Object
			if tt.existing != nil {
				objs = append(objs, tt.existing)
			}
			c, _ := certManagerContext(t, tt.tls, tt.certManager, objs...)
			built, err := buildCluster(c)
			if err != nil {
				t.Fatal(err)
			}
			if got := built.Annotations[tlsSourceAnnotation]; got != tt.want {
				t.Errorf("source = %q, want %q", got, tt.want)
			}
		})
	}
}
