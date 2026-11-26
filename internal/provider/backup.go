package provider

import (
	"errors"
	"fmt"
	"net/url"
	"path"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/contract/referents"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/backupclasses"
)

// backupClassName is the BackupClass the provider ships, defined in
// definition/backupclasses.
const backupClassName = "br"

// errBackupLocationUnknown is returned for a backup whose path in its storage
// was never recorded.
var errBackupLocationUnknown = errors.New("the backup's path in its storage is unknown")

// brSecretName is the BR-format credentials Secret derived from the OpenEverest
// BackupStorage credentials (BR expects access_key/secret_key keys).
func brSecretName(backupName string) string { return backupName + "-br-s3" }

// s3CompatibleProvider is BR's provider for S3-compatible storage other than
// AWS. TiDB Operator passes it to BR as is: it doesn't special-case it the way
// it does "ceph" (which must be plain http://).
const s3CompatibleProvider tidbbrv1.S3StorageProviderType = "other"

// brS3Provider tells BR what kind of S3 storage it writes to. As "aws", BR
// looks up the bucket's region and refuses to run unless it matches the
// configured one, which S3-compatible stores answer with their own names (Ceph
// RGW and Linode say "default"). So only Amazon S3 at an amazonaws.com
// endpoint is "aws"; anything else is an S3-compatible store, for which BR uses
// the endpoint and region as given.
func brS3Provider(implementation s3class.Implementation, endpoint string) tidbbrv1.S3StorageProviderType {
	if implementation != s3class.ImplementationAWS {
		return s3CompatibleProvider
	}
	if endpoint == "" {
		return tidbbrv1.S3StorageProviderTypeAWS
	}
	u, err := url.Parse(endpoint)
	if err == nil && (u.Hostname() == "amazonaws.com" || strings.HasSuffix(u.Hostname(), ".amazonaws.com")) {
		return tidbbrv1.S3StorageProviderTypeAWS
	}
	return s3CompatibleProvider
}

// brStorage is an s3 BackupStorage rendered for BR.
type brStorage struct {
	s3          *tidbbrv1.S3StorageProvider
	credentials s3class.Credentials
}

// renderBRStorage renders an s3 BackupStorage as BR's S3 storage writing
// under prefix in its bucket, reading credentials from the BR-format Secret
// secretName. A storage BR cannot use is an error wrapping the runtime's
// sentinel, for RejectBackupStorage.
func renderBRStorage(c *controller.Context, bs *backupv1alpha1.BackupStorage, prefix, secretName string) (brStorage, error) {
	params, credentials, err := c.BackupStorageS3(bs)
	if err != nil {
		return brStorage{}, err
	}
	implementation := params.EffectiveImplementation()
	if implementation == s3class.ImplementationGCS {
		return brStorage{}, fmt.Errorf("BR cannot use GCS through its S3 client: %w", controller.ErrImplementationUnsupported)
	}
	if _, err := c.BackupStorageTLS(bs, backupclasses.TLSSupport(backupClassName, s3class.ClassName)); err != nil {
		return brStorage{}, err
	}
	return brStorage{
		s3: &tidbbrv1.S3StorageProvider{
			Provider:   brS3Provider(implementation, params.EndpointURL),
			Region:     params.Region,
			Bucket:     bs.Spec.Location.ObjectStore.Bucket,
			Endpoint:   params.EndpointURL,
			Prefix:     prefix,
			SecretName: secretName,
		},
		credentials: credentials,
	}, nil
}

// isAPIError reports whether err is a failed read of the cluster, which is
// retried, rather than a storage BR cannot use.
func isAPIError(err error) bool {
	var status apierrors.APIStatus
	return errors.As(err, &status)
}

// backupDir is the folder of backup's snapshot in its storage: a folder named
// after the backup under the path the runtime recorded when it started.
func backupDir(c *controller.Context, backup *backupv1alpha1.Backup, bs *backupv1alpha1.BackupStorage) (string, error) {
	location := c.BackupLocation(backup, bs)
	if location == "" {
		return "", errBackupLocationUnknown
	}
	if backup.Spec.Origin.External != nil {
		return location, nil
	}
	return path.Join(location, backup.Name), nil
}

// sourceBackupDir is where the snapshot of srcBackup is: the folder its BR
// Backup wrote to while that object exists, which also holds for a backup
// taken before paths were recorded, otherwise the recorded path.
func sourceBackupDir(c *controller.Context, srcBackup *backupv1alpha1.Backup, bs *backupv1alpha1.BackupStorage) (string, error) {
	br := &tidbbrv1.Backup{}
	found, err := c.Exists(br, srcBackup.Name)
	if err != nil {
		return "", fmt.Errorf("get BR backup: %w", err)
	}
	if found && br.Spec.S3 != nil && br.Spec.S3.Prefix != "" {
		return br.Spec.S3.Prefix, nil
	}
	return backupDir(c, srcBackup, bs)
}

// ensureBRCredentials writes the BackupStorage credentials into the Secret
// name, owned by owner. BR reads S3 credentials from access_key/secret_key,
// which differ from OpenEverest's AWS_* keys.
func ensureBRCredentials(c *controller.Context, owner client.Object, name string, credentials s3class.Credentials) error {
	accessKeyID, secretAccessKey := credentials.AccessKeyID, credentials.SecretAccessKey
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: c.Namespace()},
		Type:       corev1.SecretTypeOpaque,
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), secret, func() error {
		secret.Data = map[string][]byte{
			"access_key": []byte(accessKeyID),
			"secret_key": []byte(secretAccessKey),
		}
		return controllerutil.SetControllerReference(owner, secret, c.Client().Scheme())
	}); err != nil {
		return fmt.Errorf("ensure BR credentials secret: %w", err)
	}
	return nil
}

// SyncBackup reconciles a Backup CR into a br.pingcap.com Backup (full snapshot)
// with inlined S3 storage and a BR-format credentials Secret.
func (p *Provider) SyncBackup(c *controller.Context, backup *backupv1alpha1.Backup) (controller.BackupExecutionStatus, error) {
	// The runtime syncs a Backup in every state. Once BR has its job, its
	// storage stays as created: a backup taken before its path was recorded
	// keeps its folder, and a later edit of the storage doesn't fail it.
	existing := &tidbbrv1.Backup{}
	found, err := c.Exists(existing, backup.Name)
	if err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("get BR backup: %w", err)
	}
	if found {
		// deletionPolicy may change on a live Backup. Keeping BR's copy current
		// also covers deletions that bypass CleanupBackup, such as the namespace's.
		if existing.DeletionTimestamp.IsZero() {
			if err := alignCleanPolicy(c, backup, existing); err != nil {
				return controller.BackupExecutionStatus{}, err
			}
		}
		return brBackupExecutionStatus(existing), nil
	}

	cluster := &tidbcorev1.Cluster{}
	if err := c.Get(cluster, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.BackupExecutionStatus{
				State:   backupv1alpha1.BackupStatePending,
				Message: "Waiting for TiDB cluster to exist",
			}, nil
		}
		return controller.BackupExecutionStatus{}, fmt.Errorf("get cluster: %w", err)
	}

	storage, err := c.BackupStorage(backup.Spec.StorageRef.Name)
	if err != nil {
		return controller.BackupExecutionStatus{}, err
	}
	dir, err := backupDir(c, backup, storage)
	if err != nil {
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStatePending,
			Message: fmt.Sprintf("Waiting for the path of backup storage %q to be recorded", storage.Name),
		}, nil
	}
	rendered, err := renderBRStorage(c, storage, dir, brSecretName(backup.Name))
	if err != nil {
		if isAPIError(err) {
			return controller.BackupExecutionStatus{}, err
		}
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateFailed,
			Message: fmt.Sprintf("BackupStorage %q cannot be used: %v", storage.Name, err),
		}, nil
	}

	if err := ensureBRCredentials(c, backup, brSecretName(backup.Name), rendered.credentials); err != nil {
		return controller.BackupExecutionStatus{}, err
	}

	if err := ensureBackupManagerRBAC(c, backup.Namespace); err != nil {
		return controller.BackupExecutionStatus{}, err
	}

	if err := recordBackupRootPassword(c, backup); err != nil {
		return controller.BackupExecutionStatus{}, err
	}

	brBackup := &tidbbrv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: backup.Name, Namespace: backup.Namespace},
		Spec: tidbbrv1.BackupSpec{
			ServiceAccount:  backupManagerName,
			Type:            tidbbrv1.BackupTypeFull,
			Mode:            tidbbrv1.BackupModeSnapshot,
			CleanPolicy:     cleanPolicyFor(c, backup),
			StorageProvider: tidbbrv1.StorageProvider{S3: rendered.s3},
			BR:              &tidbbrv1.BRConfig{Cluster: c.Name(), ClusterNamespace: c.Namespace()},
		},
	}
	if err := controllerutil.SetControllerReference(backup, brBackup, c.Client().Scheme()); err != nil {
		return controller.BackupExecutionStatus{}, err
	}
	if err := c.Client().Create(c.Context(), brBackup); err != nil && !apierrors.IsAlreadyExists(err) {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create BR backup: %w", err)
	}

	return brBackupExecutionStatus(brBackup), nil
}

// CleanupBackup deletes the BR Backup CR and reports done once it is gone,
// following the Backup's deletionPolicy as it is now:
//
//   - Delete: TiDB Operator's finalizer holds the BR Backup until its clean job
//     has removed the snapshot from the storage.
//   - Retain: the finalizer is released, so the BR Backup goes and the
//     snapshot stays.
func (p *Provider) CleanupBackup(c *controller.Context, backup *backupv1alpha1.Backup) (bool, error) {
	brBackup := &tidbbrv1.Backup{}
	err := c.Get(brBackup, backup.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get BR backup for cleanup: %w", err)
	}
	if brBackup.DeletionTimestamp.IsZero() {
		if err := alignCleanPolicy(c, backup, brBackup); err != nil {
			return false, err
		}
		if err := c.Delete(brBackup); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("delete BR backup: %w", err)
		}
		return false, nil
	}
	if c.ShouldRetainBackupData(backup) {
		// TiDB Operator only releases a BR Backup it cleans, which it no longer
		// does once the policy said Retain.
		if controllerutil.RemoveFinalizer(brBackup, brFinalizer) {
			if err := c.Client().Update(c.Context(), brBackup); client.IgnoreNotFound(err) != nil {
				return false, fmt.Errorf("release BR backup: %w", err)
			}
		}
		return false, nil
	}
	if tidbbrv1.IsBackupCleanFailed(brBackup) {
		// TiDB Operator doesn't retry: the Backup stays until the data is
		// removed by hand and the BR Backup's finalizer released.
		return false, fmt.Errorf("deleting the data of BR backup %s failed: %s", brBackup.Name, brCleanFailureMessage(brBackup))
	}
	return false, nil
}

// alignCleanPolicy makes TiDB Operator delete brBackup's data with it exactly
// when backup's deletionPolicy asks to. Switching to Delete also sets the
// operator's finalizer, which the operator adds only on its next pass, so a
// BR Backup deleted right after the switch still holds for the clean job.
// Under Retain it drops the finalizer, which the operator would never release.
func alignCleanPolicy(c *controller.Context, backup *backupv1alpha1.Backup, brBackup *tidbbrv1.Backup) error {
	want := cleanPolicyFor(c, backup)
	switch {
	case want == tidbbrv1.CleanPolicyTypeRetain:
		changed := controllerutil.RemoveFinalizer(brBackup, brFinalizer)
		if !changed && brBackup.Spec.CleanPolicy == want {
			return nil
		}
	case brBackup.Spec.CleanPolicy == want:
		return nil
	default:
		controllerutil.AddFinalizer(brBackup, brFinalizer)
	}
	brBackup.Spec.CleanPolicy = want
	if err := c.Client().Update(c.Context(), brBackup); err != nil {
		return fmt.Errorf("update clean policy of BR backup: %w", err)
	}
	return nil
}

// brCleanFailureMessage is why TiDB Operator failed to delete a backup's data.
func brCleanFailureMessage(b *tidbbrv1.Backup) string {
	if _, cond := tidbbrv1.GetBackupCondition(&b.Status, tidbbrv1.BackupCleanFailed); cond != nil && cond.Message != "" {
		return cond.Message
	}
	return "the clean job failed"
}

// SyncRestore reconciles a Restore CR into a br.pingcap.com Restore that reads
// the source backup's data from S3 and applies it to the target cluster. It
// serves both explicit restores and initial seeding (.spec.dataSource), which
// the runtime routes through the same Restore CR.
func (p *Provider) SyncRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	ds := restore.Spec.DataSource
	switch {
	case ds.Type == backupv1alpha1.DataSourceTypePointInTime && ds.PointInTime != nil:
		return syncPointInTimeRestore(c, restore)
	case ds.Type != backupv1alpha1.DataSourceTypeBackup || ds.Backup == nil:
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("Unsupported data source type %q", ds.Type),
		}, nil
	}

	srcBackup := &backupv1alpha1.Backup{}
	if err := c.Get(srcBackup, ds.Backup.BackupRef.Name); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStateFailed,
				Message: fmt.Sprintf("source backup %q not found", ds.Backup.BackupRef.Name),
			}, nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get source backup: %w", err)
	}
	if err := referents.ValidateBackupSucceeded(srcBackup); err != nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: "Waiting for source backup to succeed",
		}, nil
	}
	if srcBackup.Spec.Origin.InstanceRef == nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "source backup has no instance origin",
		}, nil
	}

	cluster := &tidbcorev1.Cluster{}
	if err := c.Get(cluster, c.Name()); err != nil {
		if apierrors.IsNotFound(err) {
			return controller.RestoreExecutionStatus{
				State:   backupv1alpha1.RestoreStatePending,
				Message: "Waiting for TiDB cluster to exist",
			}, nil
		}
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get cluster: %w", err)
	}

	existing := &tidbbrv1.Restore{}
	started, err := c.Exists(existing, restore.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get BR restore: %w", err)
	}
	if started {
		// Its storage stays as created, whatever happens to the BackupStorage since.
		return restoreExecutionStatus(c, restore, existing, srcBackup)
	}
	if blocked, isBlocked, err := restoreBlockedByLogBackup(c); err != nil || isBlocked {
		return blocked, err
	}

	storage, err := c.BackupStorage(srcBackup.Spec.StorageRef.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	// BR reads the backup from the folder SyncBackup wrote it to, under the
	// path the source backup recorded, so it resolves after the source
	// Instance is gone.
	dir, err := sourceBackupDir(c, srcBackup, storage)
	if isAPIError(err) {
		return controller.RestoreExecutionStatus{}, err
	}
	if err != nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("BackupLocationUnknown: backup %q: %v", srcBackup.Name, err),
		}, nil
	}
	rendered, err := renderBRStorage(c, storage, dir, brSecretName(restore.Name))
	if err != nil {
		if isAPIError(err) {
			return controller.RestoreExecutionStatus{}, err
		}
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("BackupStorage %q cannot be used: %v", storage.Name, err),
		}, nil
	}

	if err := ensureBRCredentials(c, restore, brSecretName(restore.Name), rendered.credentials); err != nil {
		return controller.RestoreExecutionStatus{}, err
	}

	if err := ensureBackupManagerRBAC(c, restore.Namespace); err != nil {
		return controller.RestoreExecutionStatus{}, err
	}

	brRestore := &tidbbrv1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: restore.Name, Namespace: restore.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brRestore, func() error {
		brRestore.Spec.ServiceAccount = backupManagerName
		brRestore.Spec.Type = tidbbrv1.BackupTypeFull
		brRestore.Spec.Mode = tidbbrv1.RestoreModeSnapshot
		brRestore.Spec.S3 = rendered.s3
		brRestore.Spec.BR = &tidbbrv1.BRConfig{
			Cluster:          c.Name(),
			ClusterNamespace: c.Namespace(),
		}
		return controllerutil.SetControllerReference(restore, brRestore, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create or update BR restore: %w", err)
	}

	return restoreExecutionStatus(c, restore, brRestore, srcBackup)
}

// restoreExecutionStatus reports the BR Restore's progress. Once it succeeded
// the Instance's root password follows the one srcBackup carries: the restore
// brought back the source's mysql.user, root password included.
func restoreExecutionStatus(c *controller.Context, restore *backupv1alpha1.Restore, brRestore *tidbbrv1.Restore,
	srcBackup *backupv1alpha1.Backup) (controller.RestoreExecutionStatus, error) {
	exec := brRestoreExecutionStatus(brRestore)
	if exec.State != backupv1alpha1.RestoreStateSucceeded {
		return exec, nil
	}
	completed := brRestore.Status.TimeCompleted.Time
	if completed.IsZero() {
		completed = restore.CreationTimestamp.Time
	}
	known, err := adoptRestoredRootPassword(c, restore, srcBackup, completed)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if !known {
		exec.Message = fmt.Sprintf("The root password of backup %q is unknown, so the connection Secret may not log in: "+
			"root now has the password the backed-up cluster had", srcBackup.Name)
	}
	return exec, nil
}

// CleanupRestore deletes the BR Restore CR.
func (p *Provider) CleanupRestore(c *controller.Context, restore *backupv1alpha1.Restore) (bool, error) {
	brRestore := &tidbbrv1.Restore{}
	err := c.Get(brRestore, restore.Name)
	if apierrors.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("get BR restore for cleanup: %w", err)
	}
	if brRestore.DeletionTimestamp.IsZero() {
		if err := c.Delete(brRestore); err != nil {
			return false, fmt.Errorf("delete BR restore: %w", err)
		}
	}
	return false, nil
}

// BackupWatches routes BR Backup status changes back to the owning Backup CR.
func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&tidbbrv1.Backup{}),
	}
}

// RestoreWatches routes BR Restore status changes back to the owning Restore CR.
func (p *Provider) RestoreWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&tidbbrv1.Restore{}),
	}
}

// cleanPolicyFor is the BR clean policy for backup's deletionPolicy.
func cleanPolicyFor(c *controller.Context, backup *backupv1alpha1.Backup) tidbbrv1.CleanPolicyType {
	if c.ShouldRetainBackupData(backup) {
		return tidbbrv1.CleanPolicyTypeRetain
	}
	return tidbbrv1.CleanPolicyTypeDelete
}

// brBackupExecutionStatus maps the BR Backup phase onto the runtime's execution status.
func brBackupExecutionStatus(b *tidbbrv1.Backup) controller.BackupExecutionStatus {
	exec := controller.BackupExecutionStatus{
		OperatorBackupRef: &commonv1alpha1.TypedObjectRef{
			Group: tidbbrv1.GroupName,
			Kind:  "Backup",
			Name:  b.Name,
		},
	}
	if !b.Status.TimeStarted.IsZero() {
		started := b.Status.TimeStarted
		exec.StartedAt = &started
	}

	switch b.Status.Phase {
	case tidbbrv1.BackupComplete:
		exec.State = backupv1alpha1.BackupStateSucceeded
		if !b.Status.TimeCompleted.IsZero() {
			completed := b.Status.TimeCompleted
			exec.CompletedAt = &completed
		}
		if b.Status.BackupSizeReadable != "" {
			size := b.Status.BackupSizeReadable
			exec.Size = &size
		}
	case tidbbrv1.BackupFailed, tidbbrv1.BackupInvalid, tidbbrv1.BackupCleanFailed:
		exec.State = backupv1alpha1.BackupStateFailed
		exec.Message = brFailureMessage(b)
	case tidbbrv1.BackupScheduled, tidbbrv1.BackupRunning, tidbbrv1.BackupPrepare:
		exec.State = backupv1alpha1.BackupStateRunning
	case "":
		exec.State = backupv1alpha1.BackupStatePending
	default:
		exec.State = backupv1alpha1.BackupStateRunning
	}
	return exec
}

// brFailureMessage extracts a human-readable failure reason from the BR Backup conditions.
func brFailureMessage(b *tidbbrv1.Backup) string {
	for i := len(b.Status.Conditions) - 1; i >= 0; i-- {
		cond := b.Status.Conditions[i]
		if cond.Status == metav1.ConditionTrue && cond.Message != "" {
			return cond.Message
		}
	}
	return "BR backup failed"
}

// brRestoreExecutionStatus maps the BR Restore phase onto the runtime's execution status.
func brRestoreExecutionStatus(r *tidbbrv1.Restore) controller.RestoreExecutionStatus {
	exec := controller.RestoreExecutionStatus{
		OperatorRestoreRef: &commonv1alpha1.TypedObjectRef{
			Group: tidbbrv1.GroupName,
			Kind:  "Restore",
			Name:  r.Name,
		},
	}
	if !r.Status.TimeStarted.IsZero() {
		started := r.Status.TimeStarted
		exec.StartedAt = &started
	}

	switch r.Status.Phase {
	case tidbbrv1.RestoreComplete:
		exec.State = backupv1alpha1.RestoreStateSucceeded
		if !r.Status.TimeCompleted.IsZero() {
			completed := r.Status.TimeCompleted
			exec.CompletedAt = &completed
		}
	case tidbbrv1.RestoreFailed, tidbbrv1.RestoreInvalid:
		exec.State = backupv1alpha1.RestoreStateFailed
		exec.Message = brRestoreFailureMessage(r)
	case tidbbrv1.RestoreScheduled, tidbbrv1.RestoreRunning:
		exec.State = backupv1alpha1.RestoreStateRunning
	case "":
		exec.State = backupv1alpha1.RestoreStatePending
	default:
		exec.State = backupv1alpha1.RestoreStateRunning
	}
	return exec
}

// brRestoreFailureMessage extracts a human-readable failure reason from the BR Restore conditions.
func brRestoreFailureMessage(r *tidbbrv1.Restore) string {
	for i := len(r.Status.Conditions) - 1; i >= 0; i-- {
		cond := r.Status.Conditions[i]
		if cond.Status == metav1.ConditionTrue && cond.Message != "" {
			return cond.Message
		}
	}
	return "BR restore failed"
}
