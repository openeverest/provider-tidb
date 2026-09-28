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

// SyncRestore is not implemented yet — restore is tracked separately.
func (p *Provider) SyncRestore(_ *controller.Context, _ *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	return controller.RestoreExecutionStatus{
		State:   backupv1alpha1.RestoreStateFailed,
		Message: "Restore is not yet supported by the TiDB provider",
	}, nil
}

// CleanupRestore is a no-op: no restore resources are created yet.
func (p *Provider) CleanupRestore(_ *controller.Context, _ *backupv1alpha1.Restore) (bool, error) {
	return true, nil
}

// BackupWatches routes BR Backup status changes back to the owning Backup CR.
func (p *Provider) BackupWatches() []controller.WatchConfig {
	return []controller.WatchConfig{
		controller.WatchOwned(&tidbbrv1.Backup{}),
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
