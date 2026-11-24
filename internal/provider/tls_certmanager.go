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

	"github.com/openeverest/provider-tidb/definition"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	// tlsSourceAnnotation records on the Cluster where its certificates come
	// from, chosen when it is created: "provider" (a CA the provider keeps in
	// a Secret), "cert-manager" (a CA the provider sets up in cert-manager)
	// or "cert-manager:<Kind>[.<group>]/<name>" (the user's issuer).
	tlsSourceAnnotation = "tidb.openeverest.io/tls-certificates"
	tlsSourceProvider   = "provider"
	tlsSourceCertMgr    = "cert-manager"
)

// newTLSSource picks the certificate source for a new Cluster: the issuer,
// otherwise a managed CA, through cert-manager when it is installed.
func newTLSSource(c *controller.Context, params definition.TLSParameters) (string, error) {
	switch {
	case !tlsEnabled(params):
		return "", nil
	case params.Issuer != nil:
		return issuerSource(*params.Issuer), nil
	}
	installed, err := certManagerInstalled(c)
	if err != nil {
		return "", err
	}
	if installed {
		return tlsSourceCertMgr, nil
	}
	return tlsSourceProvider, nil
}

// currentTLSSource reads the source recorded on an existing Cluster with TLS.
// Clusters created before it was recorded issued their certificates in the
// provider.
func currentTLSSource(cl *tidbcorev1.Cluster) string {
	if source := cl.Annotations[tlsSourceAnnotation]; source != "" {
		return source
	}
	return tlsSourceProvider
}

// issuerSource names the user's issuer. Issuers of cert-manager.io leave out
// the group, as the provider recorded them before external issuers.
func issuerSource(ref definition.IssuerReference) string {
	kind := issuerKind(ref)
	if group := issuerGroup(ref); group != cmapi.SchemeGroupVersion.Group {
		kind += "." + group
	}
	return fmt.Sprintf("%s:%s/%s", tlsSourceCertMgr, kind, ref.Name)
}

func issuerKind(ref definition.IssuerReference) string {
	if ref.Kind == "" {
		return string(definition.IssuerKindIssuer)
	}
	return string(ref.Kind)
}

func issuerGroup(ref definition.IssuerReference) string {
	if ref.Group == "" {
		return cmapi.SchemeGroupVersion.Group
	}
	return ref.Group
}

func selfSignedIssuerName(instance string) string {
	return instance + "-tls-selfsigned"
}

func caIssuerName(instance string) string {
	return instance + "-tls-ca"
}

// certManagerInstalled reports whether the cert-manager Certificate CRD is
// installed.
func certManagerInstalled(c *controller.Context) (bool, error) {
	gvk := cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind)
	if _, err := c.Client().RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version); err != nil {
		if meta.IsNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("look up the Certificate CRD: %w", err)
	}
	return true, nil
}

// validateCertManagerCRD checks that cert-manager is installed for the issuer.
func validateCertManagerCRD(c *controller.Context) error {
	installed, err := certManagerInstalled(c)
	if err != nil {
		return err
	}
	if !installed {
		return fmt.Errorf("issuer requires cert-manager: the Certificate CRD (%s) is not installed",
			cmapi.SchemeGroupVersion)
	}
	return nil
}

// ensureCertManagerCertificates has cert-manager issue every certificate into
// the Secrets the operator mounts, signed by the user's issuer or by a CA the
// provider sets up in cert-manager for the Instance.
func ensureCertManagerCertificates(c *controller.Context, owner *tidbcorev1.Cluster, ref *definition.IssuerReference, leaves []leafCert) error {
	issuer, err := ensureCertManagerIssuer(c, owner, ref)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		if err := applyOwnedByCluster(c, owner, buildCertificate(c, leaf, issuer)); err != nil {
			return fmt.Errorf("apply certificate %q: %w", leaf.secretName, err)
		}
		if err := adoptSecret(c, leaf.secretName); err != nil {
			return err
		}
	}
	return nil
}

// ensureCertManagerIssuer returns the issuer that signs the certificates. Without
// a user issuer it applies a self-signed issuer, a CA certificate signed by it,
// and a CA issuer backed by that certificate.
func ensureCertManagerIssuer(c *controller.Context, owner *tidbcorev1.Cluster, ref *definition.IssuerReference) (cmmeta.IssuerReference, error) {
	if ref != nil {
		return cmmeta.IssuerReference{Name: ref.Name, Kind: issuerKind(*ref), Group: issuerGroup(*ref)}, nil
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
				Name: selfSignedIssuerName(c.Name()), Kind: string(definition.IssuerKindIssuer), Group: cmapi.SchemeGroupVersion.Group,
			},
		},
	}
	if err := applyOwnedByCluster(c, owner, ca); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA certificate: %w", err)
	}
	if err := adoptSecret(c, tlsCASecretName(c.Name())); err != nil {
		return cmmeta.IssuerReference{}, err
	}
	caIssuer := &cmapi.Issuer{
		ObjectMeta: c.ObjectMeta(caIssuerName(c.Name())),
		Spec:       cmapi.IssuerSpec{IssuerConfig: cmapi.IssuerConfig{CA: &cmapi.CAIssuer{SecretName: tlsCASecretName(c.Name())}}},
	}
	if err := applyOwnedByCluster(c, owner, caIssuer); err != nil {
		return cmmeta.IssuerReference{}, fmt.Errorf("apply CA issuer: %w", err)
	}
	return cmmeta.IssuerReference{Name: caIssuerName(c.Name()), Kind: string(definition.IssuerKindIssuer), Group: cmapi.SchemeGroupVersion.Group}, nil
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

// adoptSecret makes the Secret cert-manager wrote for a Certificate (both named
// alike) owned by that Certificate, so it is deleted with it. cert-manager does
// not own its Secrets unless started with --enable-certificate-owner-ref. The
// Certificate, not the Cluster: it is owned by the Cluster and so outlives the
// pods during teardown, and once it is gone cert-manager cannot write the
// Secret again, as it can while a Secret is deleted before its Certificate.
func adoptSecret(c *controller.Context, name string) error {
	cert := &cmapi.Certificate{}
	found, err := c.Exists(cert, name)
	if err != nil || !found {
		return err
	}
	secret := &corev1.Secret{}
	found, err = c.Exists(secret, name)
	if err != nil {
		return fmt.Errorf("get certificate secret %q: %w", name, err)
	}
	if !found {
		return nil
	}
	for _, ref := range secret.OwnerReferences {
		if ref.UID == cert.UID {
			return nil
		}
	}
	ownerRef := metav1.NewControllerRef(cert, cmapi.SchemeGroupVersion.WithKind(cmapi.CertificateKind))
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
