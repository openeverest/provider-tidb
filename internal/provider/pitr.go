package provider

import (
	"fmt"
	"sort"
	"strconv"
	"time"

	"golang.org/x/mod/semver"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"
)

const (
	// logBackupSuffix names an Instance's BR log backup "<instance>-pitr". With
	// the 37-character Instance limit, BR's longest Job name for it,
	// "backup-<name>-log-truncate", stays within 63 characters.
	logBackupSuffix = "-pitr"
	// annotationLogBackupStorage records the BackupStorage a log backup writes to.
	annotationLogBackupStorage = "tidb.openeverest.io/backup-storage"
	// annotationPITRBaseBackup records the Backup a point-in-time restore starts from.
	annotationPITRBaseBackup = "tidb.openeverest.io/pitr-base-backup"
	// logBackupMaxLag is how far the checkpoint may trail before the log backup
	// is reported as lagging. TiKV advances it every few seconds and TiDB
	// Operator reads it once a minute.
	logBackupMaxLag = 15 * time.Minute
	// brRestoreWithLogBackupVersion is the first BR that restores into a
	// cluster while a log backup runs on it.
	brRestoreWithLogBackupVersion = "v8.5.5"
	// brFinalizer is TiDB Operator's finalizer on BR Backups.
	brFinalizer = "pingcap.com/finalizer"

	reasonPITRStorageInvalid = "PITRStorageInvalid"
)

// Reasons published on InstanceBackupStoragePITRStatus.
const (
	pitrReasonWindowAvailable = "WindowAvailable"
	pitrReasonLagging         = "LogBackupLagging"
	pitrReasonStarting        = "LogBackupStarting"
	pitrReasonFailed          = "LogBackupFailed"
	pitrReasonNoBaseBackup    = "NoBaseBackup"
)

var _ controller.InstanceBackupStatusReporter = (*Provider)(nil)

func logBackupName(instance string) string { return instance + logBackupSuffix }

// logBackupPrefix is where an Instance's log backup lives in the bucket. A
// Kubernetes name can't contain "_", so no snapshot backup's
// "<namespace>/<instance>/<backup>" prefix collides with it.
func logBackupPrefix(namespace, instance string) string {
	return fmt.Sprintf("%s/%s/_log", namespace, instance)
}

// pitrStorage returns the storage with point-in-time recovery enabled, if any.
// The BackupClass allows one: TiDB runs one log backup per cluster.
func pitrStorage(backup *corev1alpha1.InstanceBackupSpec) *corev1alpha1.InstanceBackupStorage {
	if backup == nil || !backup.Enabled {
		return nil
	}
	for i := range backup.Storages {
		if s := &backup.Storages[i]; s.PITR != nil && s.PITR.Enabled {
			return s
		}
	}
	return nil
}

// syncLogBackup runs a BR log backup while a storage of the Instance has
// point-in-time recovery enabled. Turning it off, or moving it to another
// storage, deletes the log backup: TiDB Operator stops it and deletes its data,
// which nothing can restore from once the stream is no longer enabled.
func syncLogBackup(c *controller.Context) error {
	storage := pitrStorage(c.Instance().Spec.Backup)
	name := logBackupName(c.Name())
	logBackup := &tidbbrv1.Backup{}
	found, err := c.Exists(logBackup, name)
	if err != nil {
		return fmt.Errorf("get log backup: %w", err)
	}
	switch {
	case found && !logBackup.DeletionTimestamp.IsZero():
		// The stopping stream still owns its prefix; the next one starts once it's gone.
		return nil
	case found && (storage == nil || logBackup.Annotations[annotationLogBackupStorage] != storage.StorageRef.Name):
		return c.Delete(logBackup)
	case storage == nil:
		return deleteSecretIfExists(c, brSecretName(name))
	case !found && !readyForLogBackup(c):
		return nil
	}

	bs, err := c.BackupStorage(storage.StorageRef.Name)
	if apierrors.IsNotFound(err) {
		return &controller.BackupConfigError{Reason: reasonPITRStorageInvalid,
			Message: fmt.Sprintf("BackupStorage %q for point-in-time recovery not found", storage.StorageRef.Name)}
	}
	if err != nil {
		return err
	}
	if bs.Spec.S3 == nil {
		return &controller.BackupConfigError{Reason: reasonPITRStorageInvalid,
			Message: fmt.Sprintf("BackupStorage %q is not S3-backed", bs.Name)}
	}
	if err := ensureBRCredentials(c, c.Instance(), brSecretName(name), bs); err != nil {
		return err
	}
	if err := ensureBackupManagerRBAC(c, c.Namespace()); err != nil {
		return err
	}
	return c.Apply(buildLogBackup(c, bs))
}

// readyForLogBackup reports whether the log backup may start: BR needs PD and
// TiKV serving, and can't restore .spec.dataSource while a log backup runs.
func readyForLogBackup(c *controller.Context) bool {
	if ds := c.GetDataSourceStatus(); ds != nil && !ds.Done {
		return false
	}
	return clusterReady(c)
}

func buildLogBackup(c *controller.Context, storage *backupv1alpha1.BackupStorage) *tidbbrv1.Backup {
	name := logBackupName(c.Name())
	return &tidbbrv1.Backup{
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   c.Namespace(),
			Annotations: map[string]string{annotationLogBackupStorage: storage.Name},
		},
		Spec: tidbbrv1.BackupSpec{
			Mode:            tidbbrv1.BackupModeLog,
			LogSubcommand:   tidbbrv1.LogStartCommand,
			CleanPolicy:     tidbbrv1.CleanPolicyTypeDelete,
			ServiceAccount:  backupManagerName,
			StorageProvider: tidbbrv1.StorageProvider{S3: brS3Storage(storage.Spec.S3, logBackupPrefix(c.Namespace(), c.Name()), brSecretName(name))},
			BR:              &tidbbrv1.BRConfig{Cluster: c.Name(), ClusterNamespace: c.Namespace()},
		},
	}
}

// stopLogBackup deletes the Instance's log backup and reports whether it is
// gone. TiDB Operator stops the stream through PD, so the cluster must outlive it.
func stopLogBackup(c *controller.Context) (bool, error) {
	logBackup := &tidbbrv1.Backup{}
	found, err := c.Exists(logBackup, logBackupName(c.Name()))
	if err != nil {
		return false, fmt.Errorf("get log backup: %w", err)
	}
	if !found {
		return true, nil
	}
	if logBackup.DeletionTimestamp.IsZero() {
		if err := c.Delete(logBackup); err != nil {
			return false, fmt.Errorf("delete log backup: %w", err)
		}
		return false, nil
	}
	// Deleting the namespace takes the cluster down alongside, and TiDB Operator
	// can't stop a log backup without it: the task went with PD, so release it.
	clusterFound, err := c.Exists(&tidbcorev1.Cluster{}, c.Name())
	if err != nil {
		return false, fmt.Errorf("get cluster: %w", err)
	}
	if !clusterFound && controllerutil.RemoveFinalizer(logBackup, brFinalizer) {
		if err := c.Client().Update(c.Context(), logBackup); client.IgnoreNotFound(err) != nil {
			return false, fmt.Errorf("release log backup: %w", err)
		}
	}
	return false, nil
}

func deleteSecretIfExists(c *controller.Context, name string) error {
	secret := &corev1.Secret{}
	found, err := c.Exists(secret, name)
	if err != nil || !found {
		return err
	}
	return c.Delete(secret)
}

// restoreBlockedByLogBackup reports whether the log backup of the Instance
// keeps BR from restoring into its cluster, and the status that says so.
func restoreBlockedByLogBackup(c *controller.Context) (controller.RestoreExecutionStatus, bool, error) {
	logBackup := &tidbbrv1.Backup{}
	found, err := c.Exists(logBackup, logBackupName(c.Name()))
	if err != nil || !found {
		return controller.RestoreExecutionStatus{}, false, err
	}
	if !logBackup.DeletionTimestamp.IsZero() {
		return controller.RestoreExecutionStatus{
			State:   backupv1alpha1.RestoreStatePending,
			Message: fmt.Sprintf("Waiting for the log backup of Instance %q to stop", c.Name()),
		}, true, nil
	}
	tikv := &tidbcorev1.TiKVGroup{}
	if err := c.Get(tikv, c.Name()); client.IgnoreNotFound(err) != nil {
		return controller.RestoreExecutionStatus{}, false, fmt.Errorf("get tikv group: %w", err)
	}
	if v := tikv.Spec.Template.Spec.Version; semver.IsValid(v) && semver.Compare(v, brRestoreWithLogBackupVersion) >= 0 {
		return controller.RestoreExecutionStatus{}, false, nil
	}
	return controller.RestoreExecutionStatus{
		State: backupv1alpha1.RestoreStateFailed,
		Message: fmt.Sprintf("Turn off point-in-time recovery on Instance %q before restoring into it: "+
			"BR before TiDB %s can't restore into a cluster while its log backup runs", c.Name(), brRestoreWithLogBackupVersion),
	}, true, nil
}

// logStream is a running log backup, its positions as TSOs.
type logStream struct {
	start          uint64
	truncatedUntil uint64
	checkpoint     uint64
}

// openLogStream reads the log backup instance writes to storage. Without a
// stream to read it returns the PITR status that says why.
func openLogStream(c *controller.Context, instance, storage string) (*logStream, *corev1alpha1.InstanceBackupStoragePITRStatus, error) {
	logBackup := &tidbbrv1.Backup{}
	found, err := c.Exists(logBackup, logBackupName(instance))
	if err != nil {
		return nil, nil, fmt.Errorf("get log backup: %w", err)
	}
	switch {
	case !found:
		return nil, unavailablePITR(pitrReasonStarting, "The log backup starts once the cluster is ready"), nil
	case !logBackup.DeletionTimestamp.IsZero() || logBackup.Annotations[annotationLogBackupStorage] != storage:
		return nil, unavailablePITR(pitrReasonStarting, fmt.Sprintf("The log backup is moving to storage %q", storage)), nil
	case logBackup.Status.Phase == tidbbrv1.BackupFailed || logBackup.Status.Phase == tidbbrv1.BackupInvalid:
		return nil, unavailablePITR(pitrReasonFailed, brFailureMessage(logBackup)), nil
	}
	stream := &logStream{}
	if stream.start, err = tidbbrv1.ParseTSString(logBackup.Status.CommitTs); err != nil {
		return nil, nil, fmt.Errorf("log backup %s start: %w", logBackup.Name, err)
	}
	if stream.checkpoint, err = tidbbrv1.ParseTSString(logBackup.Status.LogCheckpointTs); err != nil {
		return nil, nil, fmt.Errorf("log backup %s checkpoint: %w", logBackup.Name, err)
	}
	if stream.truncatedUntil, err = tidbbrv1.ParseTSString(logBackup.Status.LogSuccessTruncateUntil); err != nil {
		return nil, nil, fmt.Errorf("log backup %s truncation: %w", logBackup.Name, err)
	}
	if stream.start == 0 || stream.checkpoint == 0 {
		return nil, unavailablePITR(pitrReasonStarting, "The log backup is starting"), nil
	}
	return stream, nil, nil
}

// pitrBase is a full backup a point-in-time restore can start from.
type pitrBase struct {
	name string
	ts   uint64
}

// pitrBases returns the full backups of instance on storage that the stream
// covers onward, oldest first: BR restores one, then replays the log from its
// snapshot. Older backups predate the log and the start of the window.
func pitrBases(c *controller.Context, instance, storage string, stream *logStream) ([]pitrBase, error) {
	list := &backupv1alpha1.BackupList{}
	if err := c.List(list); err != nil {
		return nil, fmt.Errorf("list backups: %w", err)
	}
	floor := max(stream.start, stream.truncatedUntil)
	var bases []pitrBase
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.Origin.InstanceRef == nil || b.Spec.Origin.InstanceRef.Name != instance || b.Spec.StorageRef.Name != storage {
			continue
		}
		snapshot := &tidbbrv1.Backup{}
		if err := c.Get(snapshot, b.Name); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, fmt.Errorf("get BR backup %s: %w", b.Name, err)
		}
		if snapshot.Spec.Mode == tidbbrv1.BackupModeLog || snapshot.Status.Phase != tidbbrv1.BackupComplete {
			continue
		}
		ts, err := tidbbrv1.ParseTSString(snapshot.Status.CommitTs)
		if err != nil || ts < floor {
			continue
		}
		bases = append(bases, pitrBase{name: b.Name, ts: ts})
	}
	sort.Slice(bases, func(i, j int) bool { return bases[i].ts < bases[j].ts })
	return bases, nil
}

// BackupStorageStatuses publishes the health of the log backup and the
// restorable window on the Instance's point-in-time recovery storage.
func (p *Provider) BackupStorageStatuses(c *controller.Context) ([]corev1alpha1.InstanceBackupStorageStatus, error) {
	backup := c.Instance().Spec.Backup
	if backup == nil || !backup.Enabled {
		return nil, nil
	}
	statuses := make([]corev1alpha1.InstanceBackupStorageStatus, 0, len(backup.Storages))
	for _, s := range backup.Storages {
		status := corev1alpha1.InstanceBackupStorageStatus{Name: s.StorageRef.Name}
		if s.PITR != nil && s.PITR.Enabled {
			window, err := pitrWindow(c, s.StorageRef.Name, time.Now())
			if err != nil {
				return nil, err
			}
			status.PITR = window
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// pitrWindow spans from the oldest full backup the log covers to the log
// backup's checkpoint: every point in between is restorable.
func pitrWindow(c *controller.Context, storage string, now time.Time) (*corev1alpha1.InstanceBackupStoragePITRStatus, error) {
	stream, unavailable, err := openLogStream(c, c.Name(), storage)
	if err != nil || unavailable != nil {
		return unavailable, err
	}
	bases, err := pitrBases(c, c.Name(), storage, stream)
	if err != nil {
		return nil, err
	}
	if len(bases) == 0 {
		return unavailablePITR(pitrReasonNoBaseBackup, fmt.Sprintf(
			"Take a full backup to storage %q: point-in-time recovery starts from one taken after the log backup started at %s",
			storage, formatTS(stream.start))), nil
	}
	if stream.checkpoint < bases[0].ts {
		return unavailablePITR(pitrReasonStarting, fmt.Sprintf("The log backup hasn't reached backup %q yet", bases[0].name)), nil
	}
	earliest := metav1.NewTime(tsTime(bases[0].ts))
	latest := metav1.NewTime(tsTime(stream.checkpoint))
	status := &corev1alpha1.InstanceBackupStoragePITRStatus{
		EarliestRestorableTime: &earliest,
		LatestRestorableTime:   &latest,
		State:                  corev1alpha1.PITRStateAvailable,
		Reason:                 pitrReasonWindowAvailable,
	}
	if lag := now.Sub(latest.Time); lag > logBackupMaxLag {
		status.Reason = pitrReasonLagging
		status.Message = fmt.Sprintf("The log backup checkpoint is %s behind; check the TiKV logs and `br log status`",
			lag.Round(time.Minute))
	}
	return status, nil
}

func unavailablePITR(reason, message string) *corev1alpha1.InstanceBackupStoragePITRStatus {
	return &corev1alpha1.InstanceBackupStoragePITRStatus{
		State:   corev1alpha1.PITRStateUnavailable,
		Reason:  reason,
		Message: message,
	}
}

// tsTime is the wall-clock time of a TiDB TSO, whose upper bits are Unix milliseconds.
func tsTime(ts uint64) time.Time {
	return time.UnixMilli(int64(ts >> 18)).UTC()
}

func formatTS(ts uint64) string {
	return tsTime(ts).Format(time.RFC3339)
}

// syncPointInTimeRestore restores a point of an Instance's log backup into
// this Instance's cluster: BR restores the newest full backup at or before the
// point, then replays the log up to it.
func syncPointInTimeRestore(c *controller.Context, restore *backupv1alpha1.Restore) (controller.RestoreExecutionStatus, error) {
	source := controller.RestoreStreamInstanceName(restore)
	if source == c.Name() {
		return controller.RestoreExecutionStatus{
			State: backupv1alpha1.RestoreStateFailed,
			Message: fmt.Sprintf("TiDB restores a point in time only into an empty cluster that runs no log backup, "+
				"so Instance %q can't be rolled back in place. Create a new Instance from it with "+
				"spec.dataSource.pointInTime.source.instanceRef.name: %s", c.Name(), c.Name()),
		}, nil
	}

	brRestore := &tidbbrv1.Restore{}
	found, err := c.Exists(brRestore, restore.Name)
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get BR restore: %w", err)
	}
	if !found {
		return startPointInTimeRestore(c, restore, source)
	}

	// The restored mysql.user is the base backup's plus what the log replays,
	// so root has the password recorded with the base backup unless it was
	// changed in between.
	base := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: brRestore.Annotations[annotationPITRBaseBackup], Namespace: restore.Namespace},
	}
	base.Spec.Origin.InstanceRef = &commonv1alpha1.ObjectRef{Name: source}
	return restoreExecutionStatus(c, restore, brRestore, base)
}

// startPointInTimeRestore picks the point and the full backup to start from,
// once, and creates the BR Restore, or reports why it can't start yet or at all.
func startPointInTimeRestore(c *controller.Context, restore *backupv1alpha1.Restore, source string) (controller.RestoreExecutionStatus, error) {
	pitr := restore.Spec.DataSource.PointInTime
	storageName := pitr.Source.StorageRef.Name
	pending := func(format string, args ...any) controller.RestoreExecutionStatus {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStatePending, Message: fmt.Sprintf(format, args...)}
	}
	failed := func(format string, args ...any) controller.RestoreExecutionStatus {
		return controller.RestoreExecutionStatus{State: backupv1alpha1.RestoreStateFailed, Message: fmt.Sprintf(format, args...)}
	}

	found, err := c.Exists(&tidbcorev1.Cluster{}, c.Name())
	if err != nil {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("get cluster: %w", err)
	}
	if !found {
		return pending("Waiting for TiDB cluster to exist"), nil
	}
	if blocked, isBlocked, err := restoreBlockedByLogBackup(c); err != nil || isBlocked {
		return blocked, err
	}

	stream, unavailable, err := openLogStream(c, source, storageName)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if unavailable != nil {
		return pending("Waiting for the log backup of Instance %q: %s", source, unavailable.Message), nil
	}

	target := stream.checkpoint
	if pitr.RecoveryTarget == backupv1alpha1.RecoveryTargetDate {
		target = tidbbrv1.GoTimeToTS(pitr.Date.Time)
		if target > stream.checkpoint {
			return pending("Waiting for the log backup of Instance %q to reach %s; it is at %s",
				source, formatTS(target), formatTS(stream.checkpoint)), nil
		}
	}

	bases, err := pitrBases(c, source, storageName, stream)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	var base *pitrBase
	for i := range bases {
		if bases[i].ts <= target {
			base = &bases[i]
		}
	}
	switch {
	case base != nil:
	case pitr.RecoveryTarget != backupv1alpha1.RecoveryTargetDate:
		return pending("Waiting for a full backup of Instance %q on storage %q that the log backup covers", source, storageName), nil
	case len(bases) == 0:
		return failed("%s isn't restorable: Instance %q has no full backup on storage %q since its log backup started at %s",
			formatTS(target), source, storageName, formatTS(stream.start)), nil
	default:
		return failed("%s is before the restorable window of Instance %q on storage %q, which starts at %s",
			formatTS(target), source, storageName, formatTS(bases[0].ts)), nil
	}

	storage, err := c.BackupStorage(storageName)
	if err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if storage.Spec.S3 == nil {
		return failed("BackupStorage %q is not S3-backed", storage.Name), nil
	}
	secret := brSecretName(restore.Name)
	if err := ensureBRCredentials(c, restore, secret, storage); err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if err := ensureBackupManagerRBAC(c, restore.Namespace); err != nil {
		return controller.RestoreExecutionStatus{}, err
	}

	brRestore := &tidbbrv1.Restore{
		ObjectMeta: metav1.ObjectMeta{
			Name:        restore.Name,
			Namespace:   restore.Namespace,
			Annotations: map[string]string{annotationPITRBaseBackup: base.name},
		},
		Spec: tidbbrv1.RestoreSpec{
			Mode:            tidbbrv1.RestoreModePiTR,
			ServiceAccount:  backupManagerName,
			StorageProvider: tidbbrv1.StorageProvider{S3: brS3Storage(storage.Spec.S3, logBackupPrefix(restore.Namespace, source), secret)},
			// BR reads the full backup with the log storage's endpoint and credentials.
			PitrFullBackupStorageProvider: tidbbrv1.StorageProvider{
				S3: brS3Storage(storage.Spec.S3, backupPrefix(restore.Namespace, source, base.name), secret),
			},
			PitrRestoredTs: strconv.FormatUint(target, 10),
			BR:             &tidbbrv1.BRConfig{Cluster: c.Name(), ClusterNamespace: c.Namespace()},
		},
	}
	if err := controllerutil.SetControllerReference(restore, brRestore, c.Client().Scheme()); err != nil {
		return controller.RestoreExecutionStatus{}, err
	}
	if err := c.Client().Create(c.Context(), brRestore); err != nil && !apierrors.IsAlreadyExists(err) {
		return controller.RestoreExecutionStatus{}, fmt.Errorf("create BR restore: %w", err)
	}
	return brRestoreExecutionStatus(brRestore), nil
}
