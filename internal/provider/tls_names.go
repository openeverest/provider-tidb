package provider

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition"
	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	// tlsNamesAnnotation records on the Cluster the additional names the
	// MySQL certificates are issued for, so a change waits for maintenance
	// approval on a running instance.
	tlsNamesAnnotation = "tidb.openeverest.io/tls-additional-names"

	// tidbTLSNamesAnnotation is a TiDB pod annotation that changes with the
	// additional names TiDB's certificate carries, which restarts TiDB to load
	// it: TiDB reads its MySQL certificate only at startup. TiProxy reloads
	// its own by itself.
	tidbTLSNamesAnnotation = "tidb.openeverest.io/tls-names"
)

// appliedAdditionalNames returns the additional names to issue the MySQL
// certificates for. A new instance takes them as they are; on a running one a
// change reissues the certificates and restarts TiDB, so it waits for
// maintenance approval and keeps the recorded names until then.
func appliedAdditionalNames(c *controller.Context, existing *tidbcorev1.Cluster, params definition.TLSParameters) ([]string, error) {
	desired := additionalNames(params)
	recorded := splitNames(existing.Annotations[tlsNamesAnnotation])
	if slices.Equal(desired, recorded) {
		return desired, nil
	}
	found, err := c.Exists(&tidbcorev1.TiDBGroup{}, c.Name())
	if err != nil {
		return nil, fmt.Errorf("get tidb group: %w", err)
	}
	if !found {
		return desired, nil
	}
	description := "Reissue the MySQL certificates of TiDB and TiProxy without additional names and restart TiDB"
	if len(desired) > 0 {
		description = fmt.Sprintf("Reissue the MySQL certificates of TiDB and TiProxy for the additional names %s and restart TiDB",
			strings.Join(desired, ", "))
	}
	if c.RequestMaintenance("tls-names-"+namesHash(desired), description, controller.MaintenanceRollingRestart) {
		return desired, nil
	}
	return recorded, nil
}

func splitNames(value string) []string {
	if value == "" {
		return nil
	}
	return strings.Split(value, ",")
}

func namesHash(names []string) string {
	sum := sha256.Sum256([]byte(strings.Join(names, ",")))
	return hex.EncodeToString(sum[:])[:10]
}

// addTLSNamesRestart sets the TiDB pod annotation from the additional names in
// TiDB's MySQL certificate, so TiDB restarts once the certificate carrying
// new names is in its Secret. It reports whether that certificate does not
// carry the applied names yet: cert-manager reissues it asynchronously.
// Without additional names there is no annotation, so instances without them
// don't restart when the provider is upgraded.
func addTLSNamesRestart(c *controller.Context, group *tidbcorev1.TiDBGroup, applied []string) (pending bool, err error) {
	if !tlsEnabled(tlsParameters(c)) {
		return false, nil
	}
	served := applied
	secret := &corev1.Secret{}
	found, err := c.Exists(secret, mysqlServerTLSSecretName(c.Name()))
	if err != nil {
		return false, fmt.Errorf("get secret %q: %w", mysqlServerTLSSecretName(c.Name()), err)
	}
	if found {
		if served, err = extraDNSNames(c, secret.Data[tlsKeyCert]); err != nil {
			return false, fmt.Errorf("read the certificate in secret %q: %w", secret.Name, err)
		}
	}
	if len(served) > 0 {
		pod := group.Spec.Template.Spec.Overlay.Pod
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[tidbTLSNamesAnnotation] = namesHash(served)
	}
	return !slices.Equal(served, applied), nil
}

// extraDNSNames returns the names in TiDB's MySQL certificate besides those
// of its Services, sorted.
func extraDNSNames(c *controller.Context, certPEM []byte) ([]string, error) {
	cert, err := parseCertificate(certPEM)
	if err != nil {
		return nil, err
	}
	own := mysqlServerDNSNames(c.Name()+"-"+common.ComponentTiDB, c.Namespace(), nil)
	var extra []string
	for _, name := range cert.DNSNames {
		if !slices.Contains(own, name) {
			extra = append(extra, name)
		}
	}
	slices.Sort(extra)
	return slices.Compact(extra), nil
}
