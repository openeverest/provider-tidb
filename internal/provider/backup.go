package provider

import (
	"fmt"
	"net/url"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

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
// RGW and Linode say "default"). Any other endpoint is an S3-compatible store,
// for which BR uses the endpoint and region as given.
func brS3Provider(endpoint string) tidbbrv1.S3StorageProviderType {
	if endpoint == "" {
		return tidbbrv1.S3StorageProviderTypeAWS
	}
	u, err := url.Parse(endpoint)
	if err == nil && (u.Hostname() == "amazonaws.com" || strings.HasSuffix(u.Hostname(), ".amazonaws.com")) {
		return tidbbrv1.S3StorageProviderTypeAWS
	}
	return s3CompatibleProvider
}

// backupPrefix is the object-key prefix within the bucket, unique per backup so
// backups sharing a bucket never collide.
func backupPrefix(namespace, instance, backupName string) string {
	return fmt.Sprintf("%s/%s/%s", namespace, instance, backupName)
}

// brS3Storage points BR at prefix in an OpenEverest S3 BackupStorage, reading
// credentials from the BR-format Secret secretName.
func brS3Storage(s3 *backupv1alpha1.BackupStorageS3Spec, prefix, secretName string) *tidbbrv1.S3StorageProvider {
	return &tidbbrv1.S3StorageProvider{
		Provider:   brS3Provider(s3.EndpointURL),
		Region:     s3.Region,
		Bucket:     s3.Bucket,
		Endpoint:   s3.EndpointURL,
		Prefix:     prefix,
		SecretName: secretName,
	}
}

// ensureBRCredentials writes the BackupStorage credentials into the Secret
// name, owned by owner. BR reads S3 credentials from access_key/secret_key,
// which differ from OpenEverest's AWS_* keys.
func ensureBRCredentials(c *controller.Context, owner client.Object, name string, storage *backupv1alpha1.BackupStorage) error {
	accessKeyID, secretAccessKey, err := c.BackupStorageCredentials(storage)
	if err != nil {
		return err
	}
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
	if storage.Spec.S3 == nil {
		return controller.BackupExecutionStatus{
			State:   backupv1alpha1.BackupStateFailed,
			Message: fmt.Sprintf("BackupStorage %q is not S3-backed", storage.Name),
		}, nil
	}

	if err := ensureBRCredentials(c, backup, brSecretName(backup.Name), storage); err != nil {
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
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brBackup, func() error {
		brBackup.Spec.ServiceAccount = backupManagerName
		brBackup.Spec.Type = tidbbrv1.BackupTypeFull
		brBackup.Spec.Mode = tidbbrv1.BackupModeSnapshot
		brBackup.Spec.CleanPolicy = cleanPolicyFor(backup.Spec.DeletionPolicy)
		brBackup.Spec.S3 = brS3Storage(storage.Spec.S3, backupPrefix(c.Namespace(), c.Name(), backup.Name), brSecretName(backup.Name))
		brBackup.Spec.BR = &tidbbrv1.BRConfig{
			Cluster:          c.Name(),
			ClusterNamespace: c.Namespace(),
		}
		return controllerutil.SetControllerReference(backup, brBackup, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("create or update BR backup: %w", err)
	}

	return brBackupExecutionStatus(brBackup), nil
}

// CleanupBackup deletes the BR Backup CR. Whether the underlying S3 data is
// removed is governed by the CleanPolicy set at creation from the Backup's
// DeletionPolicy.
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
		if err := c.Delete(brBackup); err != nil {
			return false, fmt.Errorf("delete BR backup: %w", err)
		}
	}
	return false, nil
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
	if err := controller.ValidateBackupSucceeded(srcBackup); err != nil {
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

	started, err := c.Exists(&tidbbrv1.Restore{}, restore.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get BR restore: %w", err)
	}
	if !started {
		blocked, isBlocked, err := restoreBlockedByLogBackup(c)
		if err != nil || isBlocked {
			return blocked, err
		}
	}

	storage, err := c.BackupStorage(srcBackup.Spec.StorageRef.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if storage.Spec.S3 == nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("BackupStorage %q is not S3-backed", storage.Name),
		}, nil
	}
	// BR reads the backup from the same object-key prefix SyncBackup wrote it to,
	// derived from the SOURCE backup's identity (so clones read the source data).
	prefix := backupPrefix(srcBackup.Namespace, srcBackup.Spec.Origin.InstanceRef.Name, srcBackup.Name)

	if err := ensureBRCredentials(c, restore, brSecretName(restore.Name), storage); err != nil {
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
		brRestore.Spec.S3 = brS3Storage(storage.Spec.S3, prefix, brSecretName(restore.Name))
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

func cleanPolicyFor(policy backupv1alpha1.BackupDeletionPolicy) tidbbrv1.CleanPolicyType {
	if policy == backupv1alpha1.BackupDeletionPolicyRetain {
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
