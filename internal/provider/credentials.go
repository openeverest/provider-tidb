package provider

import (
	"crypto/rand"
	"fmt"
	"math/big"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	rootUser              = "root"
	rootPasswordKey       = "password"
	rootPasswordLength    = 24
	bootstrapSQLConfigKey = "bootstrap-sql"
)

// rootSecretName is the Secret holding the generated root password.
func rootSecretName(instance string) string {
	return instance + "-tidb-root"
}

// bootstrapConfigMapName is the ConfigMap holding the bootstrap SQL that sets
// the root password on first cluster bootstrap.
func bootstrapConfigMapName(instance string) string {
	return instance + "-tidb-bootstrap-sql"
}

// ensureRootPassword returns the cluster's root password, generating and
// persisting one in a Secret on first call. The password is generated once and
// reused across reconciles so it stays stable for the life of the instance.
func ensureRootPassword(c *controller.Context) (string, error) {
	secret := &corev1.Secret{}
	err := c.Get(secret, rootSecretName(c.Name()))
	if err == nil {
		if pw := secret.Data[rootPasswordKey]; len(pw) > 0 {
			return string(pw), nil
		}
	} else if !apierrors.IsNotFound(err) {
		return "", fmt.Errorf("reading root password secret: %w", err)
	}

	password, err := generatePassword(rootPasswordLength)
	if err != nil {
		return "", err
	}
	secret = &corev1.Secret{
		ObjectMeta: c.ObjectMeta(rootSecretName(c.Name())),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{rootPasswordKey: []byte(password)},
	}
	if err := c.Apply(secret); err != nil {
		return "", fmt.Errorf("creating root password secret: %w", err)
	}
	return password, nil
}

// readRootPassword returns the stored root password, or empty string if it does
// not exist yet.
func readRootPassword(c *controller.Context) string {
	secret := &corev1.Secret{}
	if err := c.Get(secret, rootSecretName(c.Name())); err != nil {
		return ""
	}
	return string(secret.Data[rootPasswordKey])
}

// buildBootstrapSQLConfigMap builds the ConfigMap TiDB runs once at first
// bootstrap to set the root password. The password is alphanumeric, so it needs
// no SQL escaping.
func buildBootstrapSQLConfigMap(c *controller.Context, password string) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: c.ObjectMeta(bootstrapConfigMapName(c.Name())),
		Data: map[string]string{
			bootstrapSQLConfigKey: fmt.Sprintf("SET PASSWORD FOR '%s'@'%%' = '%s';\n", rootUser, password),
		},
	}
}

// generatePassword returns a cryptographically random alphanumeric string.
func generatePassword(n int) (string, error) {
	const charset = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, n)
	for i := range b {
		idx, err := rand.Int(rand.Reader, big.NewInt(int64(len(charset))))
		if err != nil {
			return "", fmt.Errorf("generating password: %w", err)
		}
		b[i] = charset[idx.Int64()]
	}
	return string(b), nil
}
