package provider

import (
	"crypto/x509"
	"fmt"
	"net"

	cmapi "github.com/cert-manager/cert-manager/pkg/apis/certmanager/v1"
	cmmeta "github.com/cert-manager/cert-manager/pkg/apis/meta/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	// tlsSourceAnnotation records on the Cluster where its certificates come
	// from, so a change of source after creation can be rejected.
	tlsSourceAnnotation = "tidb.openeverest.io/tls-certificates"
	tlsSourceProvider   = "provider"
	tlsSourceCertMgr    = "cert-manager"

	issuerKindIssuer        = "Issuer"
	issuerKindClusterIssuer = "ClusterIssuer"
)

// usesCertManager reports whether cert-manager issues the certificates.
func usesCertManager(settings cluster.TLS) bool {
	cm := settings.CertManager
	return cm != nil && (cm.Enabled || (cm.IssuerRef != nil && cm.IssuerRef.Name != ""))
}

// tlsSource names where the certificates come from, or "" without TLS:
// "provider", "cert-manager" (a CA the provider sets up in cert-manager) or
// "cert-manager:<Kind>/<name>" (the user's issuer).
func tlsSource(settings cluster.TLS) string {
	switch {
	case !settings.Cluster && !settings.Client:
		return ""
	case !usesCertManager(settings):
		return tlsSourceProvider
	}
	if ref := settings.CertManager.IssuerRef; ref != nil && ref.Name != "" {
		return fmt.Sprintf("%s:%s/%s", tlsSourceCertMgr, issuerKind(ref), ref.Name)
	}
	return tlsSourceCertMgr
}

// currentTLSSource reads the source recorded on an existing Cluster. Clusters
// created before it was recorded issued their certificates in the provider.
func currentTLSSource(cl *tidbcorev1.Cluster, hasTLS bool) string {
	if source := cl.Annotations[tlsSourceAnnotation]; source != "" {
		return source
	}
	if hasTLS {
		return tlsSourceProvider
	}
	return ""
}

func issuerKind(ref *cluster.IssuerRef) string {
	if ref.Kind == "" {
		return issuerKindIssuer
	}
	return ref.Kind
}

func selfSignedIssuerName(instance string) string {
	return instance + "-tls-selfsigned"
}

func caIssuerName(instance string) string {
	return instance + "-tls-ca"
}

// validateCertManager checks the cert-manager settings: they need TLS, an
// issuer kind cert-manager knows, and cert-manager's CRDs.
func validateCertManager(c *controller.Context, settings cluster.TLS) error {
	if !usesCertManager(settings) {
		return nil
	}
	if !settings.Cluster && !settings.Client {
		return fmt.Errorf("tls.certManager issues certificates for tls.cluster or tls.client; turn on at least one")
	}
	if ref := settings.CertManager.IssuerRef; ref != nil && ref.Name != "" {
		if kind := issuerKind(ref); kind != issuerKindIssuer && kind != issuerKindClusterIssuer {
			return fmt.Errorf("tls.certManager.issuerRef.kind %q must be %s or %s", ref.Kind, issuerKindIssuer, issuerKindClusterIssuer)
		}
	}
	gvk := cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind)
	if _, err := c.Client().RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return fmt.Errorf("tls.certManager requires cert-manager: the Certificate CRD (%s) is not installed", gvk.GroupVersion())
		}
		return fmt.Errorf("look up the Certificate CRD: %w", err)
	}
	return nil
}

// ensureCertManagerCertificates has cert-manager issue every certificate into
// the Secrets the operator mounts, signed by the user's issuer or by a CA the
// provider sets up in cert-manager for the Instance.
func ensureCertManagerCertificates(c *controller.Context, owner *tidbcorev1.Cluster, settings cluster.TLS, leaves []leafCert) error {
	issuer, err := ensureCertManagerIssuer(c, owner, settings.CertManager.IssuerRef)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		if err := applyOwnedByCluster(c, owner, buildCertificate(c, leaf, issuer)); err != nil {
			return fmt.Errorf("apply certificate %q: %w", leaf.secretName, err)
		}
		if err := adoptSecret(c, owner, leaf.secretName); err != nil {
			return err
		}
	}
	return nil
}

// ensureCertManagerIssuer returns the issuer that signs the certificates. Without
// a user issuer it applies a self-signed issuer, a CA certificate signed by it,
// and a CA issuer backed by that certificate.
func ensureCertManagerIssuer(c *controller.Context, owner *tidbcorev1.Cluster, ref *cluster.IssuerRef) (cmmeta.IssuerReference, error) {
	if ref != nil && ref.Name != "" {
		return cmmeta.IssuerReference{Name: ref.Name, Kind: issuerKind(ref), Group: cmapi.SchemeGroupVersion.Group}, nil
	}

	selfSigned := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(selfSignedIssuerName(c.Name())),
		Spec:       cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{SelfSigned: &cmapi.SelfSignedIssuer{}}},
	}
	if err := applyOwnedByCluster(c, owner, selfSigned); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply self-signed issuer: %w", err)
	}
	ca := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(tlsCASecretName(c.Name())),
		Spec: cmapi.CertificateSpec{
			IsCA:       true,
			CommonName: c.Name() + " TiDB CA",
			Subject:    &cmapi.X509Subject{Organizations: []string{tlsOrg}},
			SecretName: tlsCASecretName(c.Name()),
			Duration:   &metav1.Duration{Duration: tlsCertValidity},
			IssuerRef: cmmeta.IssuerReference{
				Name: selfSignedIssuerName(c.Name()), Kind: issuerKindIssuer, Group: cmapi.SchemeGroupVersion.Group,
			},
		},
	}
	if err := applyOwnedByCluster(c, owner, ca); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA certificate: %w", err)
	}
	if err := adoptSecret(c, owner, tlsCASecretName(c.Name())); err != nil {
		return cmmeta.IssuerReference{}, err
	}
	caIssuer := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(caIssuerName(c.Name())),
		Spec:       cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{CA: &cmapi.CAIssuer{SecretName: tlsCASecretName(c.Name())}}},
	}
	if err := applyOwnedByCluster(c, owner, caIssuer); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA issuer: %w", err)
	}
	return cmmeta.IssuerReference{Name: caIssuerName(c.Name()), Kind: issuerKindIssuer, Group: cmapi.SchemeGroupVersion.Group}, nil
}

// buildCertificate asks for the same certificate the provider would issue
// itself. The long duration keeps renewals rare: TiDB Operator does not reload
// its own client certificate, and TiDB needs ALTER INSTANCE RELOAD TLS for a
// new MySQL certificate. An issuer that caps the duration renews sooner.
func buildCertificate(c *controller.Context, leaf leafCert, issuer cmmeta.IssuerReference) *cmapi.Certificate {
	usages := []cmapi.KeyUsage{cmapi.UsageDigitalSignature, cmapi.UsageKeyEncipherment}
	for _, usage := range leaf.usages {
		switch usage {
		case x509.ExtKeyUsageServerAuth:
			usages = append(usages, cmapi.UsageServerAuth)
		case x509.ExtKeyUsageClientAuth:
			usages = append(usages, cmapi.UsageClientAuth)
		}
	}
	cert := &cmapi.Certificate{
		ObjectMeta: c.ObjectMeta(leaf.secretName),
		Spec: cmapi.CertificateSpec{
			CommonName: leaf.commonName,
			Subject:    &cmapi.X509Subject{Organizations: []string{tlsOrg}},
			DNSNames:   leaf.dnsNames,
			SecretName: leaf.secretName,
			Duration:   &metav1.Duration{Duration: tlsCertValidity},
			Usages:     usages,
			IssuerRef:  issuer,
		},
	}
	if len(leaf.dnsNames) > 0 {
		cert.Spec.IPAddresses = []string{net.IPv4(127, 0, 0, 1).String(), net.IPv6loopback.String()}
	}
	return cert
}

// adoptSecret adds the Cluster as an owner of a Secret cert-manager wrote, so
// it is deleted with the Instance. cert-manager does not own its Secrets
// unless started with --enable-certificate-owner-ref.
func adoptSecret(c *controller.Context, owner *tidbcorev1.Cluster, name string) error {
	secret := &corev1.Secret{}
	found, err := c.Exists(secret, name)
	if err != nil {
		return fmt.Errorf("get certificate secret %q: %w", name, err)
	}
	if !found {
		return nil
	}
	for _, ref := range secret.OwnerReferences {
		if ref.UID == owner.UID {
			return nil
		}
	}
	gvk, err := c.Client().GroupVersionKindFor(owner)
	if err != nil {
		return err
	}
	ownerRef := metav1.NewControllerRef(owner, gvk)
	// Not a controller reference: cert-manager may hold that.
	ownerRef.Controller = nil
	patch := &unstructured.Unstructured{}
	patch.SetAPIVersion("v1")
	patch.SetKind("Secret")
	patch.SetName(name)
	patch.SetNamespace(c.Namespace())
	patch.SetOwnerReferences([]metav1.OwnerReference{*ownerRef})
	if err := c.Client().Apply(c.Context(), client.ApplyConfigurationFromUnstructured(patch),
		client.FieldOwner("provider-"+common.ProviderName)); err != nil {
		return fmt.Errorf("adopt certificate secret %q: %w", name, err)
	}
	return nil
}
