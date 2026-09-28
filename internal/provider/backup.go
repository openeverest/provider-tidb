package provider

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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

// backupPrefix is the object-key prefix within the bucket, unique per backup so
// backups sharing a bucket never collide.
func backupPrefix(namespace, instance, backupName string) string {
	return fmt.Sprintf("%s/%s/%s", namespace, instance, backupName)
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

	accessKeyID, secretAccessKey, err := c.BackupStorageCredentials(storage)
	if err != nil {
		return controller.BackupExecutionStatus{}, err
	}

	// BR reads S3 credentials from a Secret keyed access_key/secret_key, which
	// differ from OpenEverest's AWS_* keys — translate into a BR-format Secret.
	brSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: brSecretName(backup.Name), Namespace: backup.Namespace},
		Type:       corev1.SecretTypeOpaque,
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brSecret, func() error {
		brSecret.Data = map[string][]byte{
			"access_key": []byte(accessKeyID),
			"secret_key": []byte(secretAccessKey),
		}
		return controllerutil.SetControllerReference(backup, brSecret, c.Client().Scheme())
	}); err != nil {
		return controller.BackupExecutionStatus{}, fmt.Errorf("ensure BR credentials secret: %w", err)
	}

	brBackup := &tidbbrv1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: backup.Name, Namespace: backup.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brBackup, func() error {
		brBackup.Spec.Type = tidbbrv1.BackupTypeFull
		brBackup.Spec.Mode = tidbbrv1.BackupModeSnapshot
		brBackup.Spec.CleanPolicy = cleanPolicyFor(backup.Spec.DeletionPolicy)
		brBackup.Spec.S3 = &tidbbrv1.S3StorageProvider{
			Provider:   tidbbrv1.S3StorageProviderTypeAWS,
			Region:     storage.Spec.S3.Region,
			Bucket:     storage.Spec.S3.Bucket,
			Endpoint:   storage.Spec.S3.EndpointURL,
			Prefix:     backupPrefix(c.Namespace(), c.Name(), backup.Name),
			SecretName: brSecret.Name,
		}
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
	if ds.Type != backupv1alpha1.DataSourceTypeBackup || ds.Backup == nil {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStateFailed,
			Message: "Only Backup data sources are supported (point-in-time recovery is not yet implemented)",
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
	accessKeyID, secretAccessKey, err := c.BackupStorageCredentials(storage)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}

	// BR reads the backup from the same object-key prefix SyncBackup wrote it to,
	// derived from the SOURCE backup's identity (so clones read the source data).
	prefix := backupPrefix(srcBackup.Namespace, srcBackup.Spec.Origin.InstanceRef.Name, srcBackup.Name)

	brSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: brSecretName(restore.Name), Namespace: restore.Namespace},
		Type:       corev1.SecretTypeOpaque,
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brSecret, func() error {
		brSecret.Data = map[string][]byte{
			"access_key": []byte(accessKeyID),
			"secret_key": []byte(secretAccessKey),
		}
		return controllerutil.SetControllerReference(restore, brSecret, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("ensure BR credentials secret: %w", err)
	}

	brRestore := &tidbbrv1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: restore.Name, Namespace: restore.Namespace},
	}
	if _, err := controllerutil.CreateOrUpdate(c.Context(), c.Client(), brRestore, func() error {
		brRestore.Spec.Type = tidbbrv1.BackupTypeFull
		brRestore.Spec.Mode = tidbbrv1.RestoreModeSnapshot
		brRestore.Spec.S3 = &tidbbrv1.S3StorageProvider{
			Provider:   tidbbrv1.S3StorageProviderTypeAWS,
			Region:     storage.Spec.S3.Region,
			Bucket:     storage.Spec.S3.Bucket,
			Endpoint:   storage.Spec.S3.EndpointURL,
			Prefix:     prefix,
			SecretName: brSecret.Name,
		}
		brRestore.Spec.BR = &tidbbrv1.BRConfig{
			Cluster:          c.Name(),
			ClusterNamespace: c.Namespace(),
		}
		return controllerutil.SetControllerReference(restore, brRestore, c.Client().Scheme())
	}); err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create or update BR restore: %w", err)
	}

	return brRestoreExecutionStatus(brRestore), nil
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
