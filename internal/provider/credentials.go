package provider

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

const (
	rootUser              = "root"
	rootPasswordKey       = "password"
	rootPasswordLength    = 24
	bootstrapSQLConfigKey = "bootstrap-sql"

	// annotationRootFromRestore and annotationRootRestoredAt record on the root
	// password Secret which restore its password was adopted from, so an older
	// restore that reconciles again cannot overwrite a newer one.
	annotationRootFromRestore = "tidb.openeverest.io/root-password-from-restore"
	annotationRootRestoredAt  = "tidb.openeverest.io/root-password-restored-at"
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

// currentSecret reads a Secret the provider creates and owns by ownerUID, nil
// if there is none. A Secret controlled by another object was left behind by a
// deleted Instance of the same name and is about to be garbage-collected:
// reusing it would leave the new Instance with Secrets that then disappear (or
// a root password the new cluster never had), so it is deleted and the
// reconcile waits until it is gone.
func currentSecret(c *controller.Context, ownerUID types.UID, name string) (*corev1.Secret, error) {
	secret := &corev1.Secret{}
	if err := c.Get(secret, name); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get secret %q: %w", name, err)
	}
	ref := metav1.GetControllerOf(secret)
	if ref == nil || ref.UID == ownerUID {
		return secret, nil
	}
	if secret.DeletionTimestamp.IsZero() {
		if err := c.Delete(secret); err != nil && !apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("delete secret %q of a previous %s: %w", name, ref.Kind, err)
		}
	}
	return nil, controller.WaitFor(fmt.Sprintf("secret %q belongs to a previous %s %q; waiting for it to be removed", name, ref.Kind, ref.Name))
}

// ensureRootPassword returns the cluster's root password, generating and
// persisting one in a Secret on first call. The password is generated once and
// reused across reconciles so it stays stable for the life of the instance.
func ensureRootPassword(c *controller.Context) (string, error) {
	secret, err := currentSecret(c, c.Instance().UID, rootSecretName(c.Name()))
	if err != nil {
		return "", err
	}
	if secret != nil {
		if pw := secret.Data[rootPasswordKey]; len(pw) > 0 {
			return string(pw), nil
		}
	}

	password, err := newRootPassword()
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
	if ref := metav1.GetControllerOf(secret); ref != nil && ref.UID != c.Instance().UID {
		return ""
	}
	return string(secret.Data[rootPasswordKey])
}

// backupRootSecretName is the Secret, owned by a Backup, that holds the root
// password of the backed-up Instance at the time the backup was taken.
func backupRootSecretName(backup string) string {
	return backup + "-tidb-root"
}

// recordBackupRootPassword stores the Instance's root password next to a
// backup. A BR snapshot includes mysql.user, so whoever restores it gets this
// password for root. The Secret is written once, when the backup starts.
func recordBackupRootPassword(c *controller.Context, backup *backupv1alpha1.Backup) error {
	err := c.Get(&corev1.Secret{}, backupRootSecretName(backup.Name))
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return fmt.Errorf("reading backup root password secret: %w", err)
	}
	password := readRootPassword(c)
	if password == "" {
		return nil
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupRootSecretName(backup.Name),
			Namespace: backup.Namespace,
			Labels:    c.ObjectMeta("").Labels,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{rootPasswordKey: []byte(password)},
	}
	if err := controllerutil.SetControllerReference(backup, secret, c.Client().Scheme()); err != nil {
		return fmt.Errorf("owning backup root password secret: %w", err)
	}
	if err := c.Client().Create(c.Context(), secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating backup root password secret: %w", err)
	}
	return nil
}

// restoredRootPassword returns the root password the data of a backup carries:
// the one recorded with the backup or, for backups taken before that was
// recorded, the source Instance's current one. Empty if neither is known.
func restoredRootPassword(c *controller.Context, backup *backupv1alpha1.Backup) (string, error) {
	names := []string{backupRootSecretName(backup.Name)}
	if ref := backup.Spec.Origin.InstanceRef; ref != nil {
		names = append(names, rootSecretName(ref.Name))
	}
	for _, name := range names {
		secret := &corev1.Secret{}
		if err := c.Get(secret, name); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return "", fmt.Errorf("reading secret %s: %w", name, err)
		}
		if pw := secret.Data[rootPasswordKey]; len(pw) > 0 {
			return string(pw), nil
		}
	}
	return "", nil
}

// adoptRestoredRootPassword makes the Instance's root password Secret, and so
// its connection Secret, hold the root password the restored mysql.user has.
// Restores are ordered by completion time: one that completed no later than
// the restore the password last came from leaves it alone. It reports false
// when the backup's root password is unknown.
func adoptRestoredRootPassword(c *controller.Context, restore *backupv1alpha1.Restore, backup *backupv1alpha1.Backup, completed time.Time) (bool, error) {
	password, err := restoredRootPassword(c, backup)
	if err != nil || password == "" {
		return false, err
	}
	current := &corev1.Secret{}
	if err := c.Get(current, rootSecretName(c.Name())); err != nil && !apierrors.IsNotFound(err) {
		return false, fmt.Errorf("reading root password secret: %w", err)
	}
	if prev, perr := time.Parse(time.RFC3339, current.Annotations[annotationRootRestoredAt]); perr == nil && !completed.Truncate(time.Second).After(prev) {
		return true, nil
	}
	secret := &corev1.Secret{
		ObjectMeta: c.ObjectMeta(rootSecretName(c.Name())),
		Type:       corev1.SecretTypeOpaque,
		Data:       map[string][]byte{rootPasswordKey: []byte(password)},
	}
	secret.Annotations = map[string]string{
		annotationRootFromRestore: restore.Name,
		annotationRootRestoredAt:  completed.UTC().Format(time.RFC3339),
	}
	if err := c.Apply(secret); err != nil {
		return false, fmt.Errorf("updating root password secret: %w", err)
	}
	return true, nil
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

// newRootPassword is a variable so tests can make Sync's output deterministic.
var newRootPassword = func() (string, error) { return generatePassword(rootPasswordLength) }

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
