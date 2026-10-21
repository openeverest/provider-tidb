package provider

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/topologies/cluster"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	// tlsCASecretSuffix names the Secret holding the Instance's CA and its key.
	// It is never mounted into pods; the provider signs the leaf certificates
	// with it.
	tlsCASecretSuffix = "-tls-ca"

	// tlsCertValidity is the lifetime of the CA and the leaf certificates. The
	// provider does not rotate them: TiDB Operator never reloads its own client
	// certificate, and TiDB needs ALTER INSTANCE RELOAD TLS for a new server
	// certificate.
	tlsCertValidity = 10 * 365 * 24 * time.Hour
	tlsRSABits      = 2048

	tlsKeyCA     = "ca.crt"
	tlsKeyCAKey  = "ca.key"
	tlsKeyCert   = corev1.TLSCertKey
	tlsKeyKey    = corev1.TLSPrivateKeyKey
	tlsOrg       = "OpenEverest"
	tlsClusterCN = "TiDB"
)

// tlsSettings returns the Instance's TLS choices; absent means plaintext.
func tlsSettings(c *controller.Context) cluster.TLS {
	var params cluster.ClusterTopologyParameters
	if !c.TryDecodeTopologyParameters(&params) || params.TLS == nil {
		return cluster.TLS{}
	}
	return *params.TLS
}

func tlsCASecretName(instance string) string {
	return instance + tlsCASecretSuffix
}

// The operator mounts these Secrets by name: one per component group for the
// cluster certificates, one for its own (and BR's) client certificate, and one
// for TiDB's MySQL server certificate.
func clusterTLSSecretName(instance, component string) string {
	return instance + "-" + component + "-cluster-secret"
}

func clusterClientTLSSecretName(instance string) string {
	return instance + "-cluster-client-secret"
}

func mysqlServerTLSSecretName(instance string) string {
	return instance + "-tidb-server-secret"
}

// leafCert describes one certificate the provider issues into a Secret.
type leafCert struct {
	secretName string
	commonName string
	dnsNames   []string
	usages     []x509.ExtKeyUsage
}

// tlsLeaves lists the certificates the Instance's TLS settings need.
func tlsLeaves(instance, namespace string, settings cluster.TLS) []leafCert {
	var leaves []leafCert
	if settings.Cluster {
		for _, component := range []string{common.ComponentPD, common.ComponentTiKV, common.ComponentTiDB, common.ComponentTiFlash} {
			svc := instance + "-" + component
			leaves = append(leaves, leafCert{
				secretName: clusterTLSSecretName(instance, component),
				commonName: tlsClusterCN,
				dnsNames:   append(serviceDNSNames(svc, namespace), peerDNSNames(svc+"-peer", namespace)...),
				usages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
			})
		}
		leaves = append(leaves, leafCert{
			secretName: clusterClientTLSSecretName(instance),
			commonName: tlsClusterCN,
			usages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		})
	}
	if settings.Client {
		svc := instance + "-" + common.ComponentTiDB
		leaves = append(leaves, leafCert{
			secretName: mysqlServerTLSSecretName(instance),
			commonName: svc,
			dnsNames:   append(serviceDNSNames(svc, namespace), peerDNSNames(svc+"-peer", namespace)...),
			usages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
	}
	return leaves
}

func serviceDNSNames(svc, namespace string) []string {
	return []string{svc, svc + "." + namespace, svc + "." + namespace + ".svc", svc + "." + namespace + ".svc.cluster.local"}
}

// peerDNSNames covers the headless service and every pod behind it.
func peerDNSNames(svc, namespace string) []string {
	names := serviceDNSNames(svc, namespace)
	for _, name := range serviceDNSNames(svc, namespace) {
		names = append(names, "*."+name)
	}
	return names
}

// ensureTLSSecrets creates the CA and every certificate the TLS settings need,
// owned by the Cluster so they outlive the pods during the operator's ordered
// teardown. Existing Secrets are never replaced: certificates are issued once.
func ensureTLSSecrets(c *controller.Context, owner *tidbcorev1.Cluster) error {
	settings := tlsSettings(c)
	leaves := tlsLeaves(c.Name(), c.Namespace(), settings)
	if len(leaves) == 0 {
		return nil
	}
	if usesCertManager(settings) {
		return ensureCertManagerCertificates(c, owner, settings, leaves)
	}

	ca, err := ensureCA(c, owner, leaves)
	if err != nil {
		return err
	}
	for _, leaf := range leaves {
		found, err := c.Exists(&corev1.Secret{}, leaf.secretName)
		if err != nil {
			return fmt.Errorf("get TLS secret %q: %w", leaf.secretName, err)
		}
		if found {
			continue
		}
		certPEM, keyPEM, err := ca.issue(leaf)
		if err != nil {
			return fmt.Errorf("issue certificate for %q: %w", leaf.secretName, err)
		}
		secret := &corev1.Secret{
			ObjectMeta: c.ObjectMeta(leaf.secretName),
			Type:       corev1.SecretTypeTLS,
			Data:       map[string][]byte{tlsKeyCA: ca.certPEM, tlsKeyCert: certPEM, tlsKeyKey: keyPEM},
		}
		if err := createOwnedByCluster(c, owner, secret); err != nil {
			return err
		}
	}
	return nil
}

// ensureCA reads the Instance's CA, generating it on first use. A missing CA
// next to issued certificates is not regenerated: a new CA would no longer
// verify the certificates the pods already use.
func ensureCA(c *controller.Context, owner *tidbcorev1.Cluster, leaves []leafCert) (*certAuthority, error) {
	name := tlsCASecretName(c.Name())
	secret := &corev1.Secret{}
	err := c.Get(secret, name)
	if err == nil {
		ca, err := parseCertAuthority(secret.Data[tlsKeyCA], secret.Data[tlsKeyCAKey])
		if err != nil {
			return nil, fmt.Errorf("read the CA from secret %q: %w", name, err)
		}
		return ca, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get CA secret %q: %w", name, err)
	}

	for _, leaf := range leaves {
		found, err := c.Exists(&corev1.Secret{}, leaf.secretName)
		if err != nil {
			return nil, fmt.Errorf("get TLS secret %q: %w", leaf.secretName, err)
		}
		if found {
			return nil, fmt.Errorf("CA secret %q is missing but certificate secret %q exists; restore the CA secret "+
				"or delete the certificate secrets to issue new ones", name, leaf.secretName)
		}
	}

	ca, err := newCertAuthority(c.Name())
	if err != nil {
		return nil, fmt.Errorf("generate the CA: %w", err)
	}
	caSecret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(name),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{tlsKeyCA: ca.certPEM, tlsKeyCAKey: ca.keyPEM()},
	}
	if err := createOwnedByCluster(c, owner, caSecret); err != nil {
		return nil, err
	}
	return ca, nil
}

// createOwnedByCluster creates obj owned by the Cluster. It never overwrites:
// a Secret the cache does not show yet fails with AlreadyExists, and the next
// reconcile reads it instead of issuing a second certificate.
func createOwnedByCluster(c *controller.Context, owner *tidbcorev1.Cluster, secret *corev1.Secret) error {
	if err := controllerutil.SetControllerReference(owner, secret, c.Client().Scheme()); err != nil {
		return fmt.Errorf("set cluster owner: %w", err)
	}
	if err := c.Client().Create(c.Context(), secret); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("secret %q was created concurrently; retrying", secret.Name)
		}
		return fmt.Errorf("create secret %q: %w", secret.Name, err)
	}
	return nil
}

// readClientCA returns the CA that signed TiDB's MySQL certificate, or nil
// before it is issued. The Secret carries it whoever issued the certificate.
func readClientCA(c *controller.Context) ([]byte, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, mysqlServerTLSSecretName(c.Name())); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return secret.Data[tlsKeyCA], nil
}

type certAuthority struct {
	cert    *x509.Certificate
	certPEM []byte
	key     *rsa.PrivateKey
}

func newCertAuthority(instance string) (*certAuthority, error) {
	key, err := rsa.GenerateKey(rand.Reader, tlsRSABits)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: instance + " TiDB CA", Organization: []string{tlsOrg}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(tlsCertValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &certAuthority{cert: cert, certPEM: pemEncode("CERTIFICATE", der), key: key}, nil
}

func parseCertAuthority(certPEM, keyPEM []byte) (*certAuthority, error) {
	certBlock, _ := pem.Decode(certPEM)
	if certBlock == nil {
		return nil, errors.New("no PEM certificate")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse certificate: %w", err)
	}
	keyBlock, _ := pem.Decode(keyPEM)
	if keyBlock == nil {
		return nil, errors.New("no PEM private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse private key: %w", err)
	}
	return &certAuthority{cert: cert, certPEM: certPEM, key: key}, nil
}

func (ca *certAuthority) keyPEM() []byte {
	return pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(ca.key))
}

// issue signs a leaf certificate valid as long as the CA.
func (ca *certAuthority) issue(leaf leafCert) (certPEM, keyPEM []byte, err error) {
	key, err := rsa.GenerateKey(rand.Reader, tlsRSABits)
	if err != nil {
		return nil, nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: leaf.commonName, Organization: []string{tlsOrg}},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     ca.cert.NotAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  leaf.usages,
		DNSNames:     leaf.dnsNames,
	}
	if len(leaf.dnsNames) > 0 {
		tmpl.IPAddresses = []net.IP{net.IPv4(127, 0, 0, 1), net.IPv6loopback}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	return pemEncode("CERTIFICATE", der), pemEncode("RSA PRIVATE KEY", x509.MarshalPKCS1PrivateKey(key)), nil
}

func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func pemEncode(blockType string, der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: blockType, Bytes: der})
}

// validateTLS rejects changing the TLS settings of an existing cluster: the
// operator only takes them when the cluster is created.
func validateTLS(c *controller.Context) error {
	requested := tlsSettings(c)
	if err := validateCertManager(c, requested); err != nil {
		return err
	}

	cluster := &tidbcorev1.Cluster{}
	found, err := c.Exists(cluster, c.Name())
	if err != nil {
		return fmt.Errorf("get cluster: %w", err)
	}
	if found {
		current := cluster.Spec.TLSCluster != nil && cluster.Spec.TLSCluster.Enabled
		if requested.Cluster != current {
			return fmt.Errorf("tls.cluster cannot be changed after the Instance is created: it is %s", onOff(current))
		}
		// A new CA would not verify the certificates the pods already use.
		currentSource := currentTLSSource(cluster, current || requested.Client)
		if want := tlsSource(requested); want != "" && currentSource != "" && want != currentSource {
			return fmt.Errorf("the TLS certificate source cannot be changed after the Instance is created: it is %s", currentSource)
		}
	}

	tidb := &tidbcorev1.TiDBGroup{}
	found, err = c.Exists(tidb, c.Name())
	if err != nil {
		return fmt.Errorf("get tidb group: %w", err)
	}
	if found {
		sec := tidb.Spec.Template.Spec.Security
		current := sec != nil && sec.TLS != nil && sec.TLS.MySQL != nil && sec.TLS.MySQL.Enabled
		if requested.Client != current {
			return fmt.Errorf("tls.client cannot be changed after the Instance is created: it is %s", onOff(current))
		}
	}
	return nil
}

func onOff(enabled bool) string {
	if enabled {
		return "on"
	}
	return "off"
}
