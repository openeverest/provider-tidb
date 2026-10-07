package provider

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/internal/common"
)

const (
	// scheduleStartingDeadline is how late a slot may still fire, covering
	// provider restarts and a previous run or the cluster blocking it. Older
	// slots are skipped, so adding a schedule never fires a past slot.
	scheduleStartingDeadline = 10 * time.Minute
	// scheduleRetryInterval re-checks a blocked slot while it is still in its deadline.
	scheduleRetryInterval = time.Minute
	// maxScheduledBackupNameLength keeps BR's "backup-<name>" Job name within
	// the 63-character label limit.
	maxScheduledBackupNameLength = 56
	scheduledBackupTimeFormat    = "200601021504"

	reasonScheduleInvalid = "BackupScheduleInvalid"
)

// BackupScheduler turns an Instance's .spec.backup.storages[].schedules into
// Backup CRs and prunes the ones a schedule's retention no longer keeps.
// TiDB Operator v2 ships the BackupSchedule CRD without a controller for it,
// so the provider runs the cron itself; each Backup it creates is then
// executed by SyncBackup like an on-demand one.
type BackupScheduler struct {
	client client.Client
	now    func() time.Time
}

// SetupBackupScheduler registers the BackupScheduler with the manager.
func SetupBackupScheduler(mgr ctrl.Manager) error {
	s := &BackupScheduler{client: mgr.GetClient(), now: time.Now}
	return ctrl.NewControllerManagedBy(mgr).
		Named(common.ProviderName + "-backup-scheduler").
		For(&corev1alpha1.Instance{}, builder.WithPredicates(predicate.NewPredicateFuncs(isProviderInstance))).
		Watches(&backupv1alpha1.Backup{}, handler.EnqueueRequestsFromMapFunc(instanceForScheduledBackup)).
		Complete(s)
}

func isProviderInstance(obj client.Object) bool {
	in, ok := obj.(*corev1alpha1.Instance)
	return ok && in.Spec.ProviderRef.Name == common.ProviderName
}

// instanceForScheduledBackup re-runs the scheduler when a scheduled backup
// changes, so a finished run unblocks the next slot and triggers pruning.
func instanceForScheduledBackup(_ context.Context, obj client.Object) []reconcile.Request {
	b, ok := obj.(*backupv1alpha1.Backup)
	if !ok || b.Spec.ScheduleName == "" || b.Spec.Origin.InstanceRef == nil {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: b.Namespace, Name: b.Spec.Origin.InstanceRef.Name}}}
}

// Reconcile fires every due schedule slot and applies retention, then requeues
// for the earliest upcoming slot.
func (s *BackupScheduler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	in := &corev1alpha1.Instance{}
	if err := s.client.Get(ctx, req.NamespacedName, in); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}
	if !in.DeletionTimestamp.IsZero() || in.Spec.Backup == nil || !in.Spec.Backup.Enabled {
		return reconcile.Result{}, nil
	}

	list := &backupv1alpha1.BackupList{}
	if err := s.client.List(ctx, list,
		client.InNamespace(in.Namespace),
		client.MatchingFields{controller.IndexBackupInstanceName: in.Name},
	); err != nil {
		return reconcile.Result{}, fmt.Errorf("list backups: %w", err)
	}

	now := s.now()
	var wake time.Time
	for _, storage := range in.Spec.Backup.Storages {
		for _, schedule := range storage.Schedules {
			if !schedule.Enabled {
				continue
			}
			// An invalid expression is surfaced on the Instance by Sync.
			cronSchedule, err := cron.ParseStandard(schedule.Cron)
			if err != nil {
				continue
			}
			backups := backupsOfSchedule(list.Items, schedule.Name)

			next, err := s.fireDueSlot(ctx, in, storage.StorageRef, schedule, cronSchedule, backups, now)
			if err != nil {
				return reconcile.Result{}, err
			}
			wake = earliest(wake, next)

			if err := s.applyRetention(ctx, schedule, backups, now); err != nil {
				return reconcile.Result{}, err
			}
		}
	}

	if wake.IsZero() {
		return reconcile.Result{}, nil
	}
	return reconcile.Result{RequeueAfter: wake.Sub(now)}, nil
}

// fireDueSlot creates the Backup for the schedule's latest slot if it is still
// within the starting deadline, and returns when the scheduler should next run.
// A slot is held back while the cluster is not serving or a previous run of the
// same schedule is still in progress.
func (s *BackupScheduler) fireDueSlot(
	ctx context.Context,
	in *corev1alpha1.Instance,
	storageRef commonv1alpha1.ObjectRef,
	schedule corev1alpha1.InstanceBackupSchedule,
	cronSchedule cron.Schedule,
	backups []backupv1alpha1.Backup,
	now time.Time,
) (time.Time, error) {
	next := cronSchedule.Next(now)
	slot := latestSlot(cronSchedule, now, scheduleStartingDeadline)
	if slot.IsZero() {
		return next, nil
	}
	name := scheduledBackupName(in.Name, schedule.Name, slot)
	for i := range backups {
		if backups[i].Name == name {
			return next, nil
		}
	}

	logger := log.FromContext(ctx).WithValues("schedule", schedule.Name, "slot", slot)
	if !instanceCanBackUp(in) {
		logger.V(1).Info("Holding scheduled backup until the Instance is serving", "phase", in.Status.Phase)
		return earliest(next, now.Add(scheduleRetryInterval)), nil
	}
	if running := inProgressBackup(backups); running != "" {
		logger.V(1).Info("Holding scheduled backup until the previous run finishes", "running", running)
		return earliest(next, now.Add(scheduleRetryInterval)), nil
	}

	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: in.Namespace},
		Spec: backupv1alpha1.BackupSpec{
			Origin: backupv1alpha1.BackupOrigin{
				Type:        backupv1alpha1.BackupOriginTypeInstance,
				InstanceRef: &commonv1alpha1.ObjectRef{Name: in.Name},
			},
			ClassRef:     in.Spec.Backup.ClassRef,
			StorageRef:   storageRef,
			ScheduleName: schedule.Name,
			Parameters:   schedule.Parameters.DeepCopy(),
		},
	}
	if err := s.client.Create(ctx, backup); err != nil && !apierrors.IsAlreadyExists(err) {
		return time.Time{}, fmt.Errorf("create scheduled backup %q: %w", name, err)
	}
	logger.Info("Created scheduled backup", "backup", name)
	return next, nil
}

// applyRetention deletes the scheduled backups the schedule's retention no
// longer keeps. Each Backup's own deletionPolicy decides whether its data in
// storage is removed too.
func (s *BackupScheduler) applyRetention(
	ctx context.Context,
	schedule corev1alpha1.InstanceBackupSchedule,
	backups []backupv1alpha1.Backup,
	now time.Time,
) error {
	expired, err := expiredBackups(backups, schedule.Retention, now)
	if err != nil {
		log.FromContext(ctx).Error(err, "Skipping retention", "schedule", schedule.Name)
		return nil
	}
	for _, b := range expired {
		if err := s.client.Delete(ctx, b); client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("delete expired backup %q: %w", b.Name, err)
		}
		log.FromContext(ctx).Info("Deleted backup past retention", "schedule", schedule.Name, "backup", b.Name)
	}
	return nil
}

// validateBackupSchedules reports every schedule whose cron expression the
// scheduler cannot run.
func validateBackupSchedules(backup *corev1alpha1.InstanceBackupSpec) error {
	if backup == nil || !backup.Enabled {
		return nil
	}
	var errs []error
	for _, storage := range backup.Storages {
		for _, schedule := range storage.Schedules {
			if _, err := cron.ParseStandard(schedule.Cron); err != nil {
				errs = append(errs, fmt.Errorf("schedule %q: invalid cron %q: %w", schedule.Name, schedule.Cron, err))
			}
		}
	}
	if len(errs) == 0 {
		return nil
	}
	return &controller.BackupConfigError{Reason: reasonScheduleInvalid, Message: errors.Join(errs...).Error()}
}

// latestSlot returns the most recent activation of the schedule within window
// before now, or the zero time when there is none.
func latestSlot(schedule cron.Schedule, now time.Time, window time.Duration) time.Time {
	var slot time.Time
	for t := schedule.Next(now.Add(-window - time.Second)); !t.IsZero() && !t.After(now); t = schedule.Next(t) {
		slot = t
	}
	return slot
}

// scheduledBackupName derives a deterministic, DNS-safe name per schedule slot,
// so creating the same slot twice is idempotent.
func scheduledBackupName(instance, schedule string, slot time.Time) string {
	suffix := "-" + slot.UTC().Format(scheduledBackupTimeFormat)
	raw := instance + "-" + schedule
	base := dnsLabel(raw)
	if base == raw && len(base)+len(suffix) <= maxScheduledBackupNameLength {
		return base + suffix
	}
	// Sanitising or truncating could make two schedules collide; a hash of the
	// original keeps them apart.
	h := fnv.New32a()
	_, _ = h.Write([]byte(raw))
	hash := "-" + strconv.FormatUint(uint64(h.Sum32()), 16)
	if limit := maxScheduledBackupNameLength - len(suffix) - len(hash); len(base) > limit {
		base = strings.TrimRight(base[:limit], "-")
	}
	return base + hash + suffix
}

func dnsLabel(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			return r
		case r >= 'A' && r <= 'Z':
			return r + ('a' - 'A')
		default:
			return '-'
		}
	}, s)
}

// instanceCanBackUp reports whether the cluster is serving and can take a backup.
func instanceCanBackUp(in *corev1alpha1.Instance) bool {
	return in.Status.Phase == corev1alpha1.InstancePhaseReady || in.Status.Phase == corev1alpha1.InstancePhaseUpdating
}

func backupsOfSchedule(all []backupv1alpha1.Backup, schedule string) []backupv1alpha1.Backup {
	var out []backupv1alpha1.Backup
	for _, b := range all {
		if b.Spec.ScheduleName == schedule {
			out = append(out, b)
		}
	}
	return out
}

func inProgressBackup(backups []backupv1alpha1.Backup) string {
	for _, b := range backups {
		if b.DeletionTimestamp.IsZero() && !backupFinished(&b) {
			return b.Name
		}
	}
	return ""
}

func backupFinished(b *backupv1alpha1.Backup) bool {
	return b.Status.State == backupv1alpha1.BackupStateSucceeded || b.Status.State == backupv1alpha1.BackupStateFailed
}

// expiredBackups returns the finished backups that fall outside retention.
// Retention is anchored on successful backups so failed runs never push good
// ones out:
//   - count N keeps the N newest successful backups and every newer failed one;
//   - time keeps everything created inside the window, plus the newest
//     successful backup so a stalled schedule never prunes its last good copy.
//
// In-progress backups are never expired.
func expiredBackups(backups []backupv1alpha1.Backup, retention *corev1alpha1.BackupScheduleRetention, now time.Time) ([]*backupv1alpha1.Backup, error) {
	if retention == nil {
		return nil, nil
	}
	var finished []*backupv1alpha1.Backup
	for i := range backups {
		b := &backups[i]
		if b.DeletionTimestamp.IsZero() && backupFinished(b) {
			finished = append(finished, b)
		}
	}
	sort.Slice(finished, func(i, j int) bool {
		ti, tj := finished[i].CreationTimestamp, finished[j].CreationTimestamp
		if !ti.Equal(&tj) {
			return tj.Before(&ti)
		}
		return finished[i].Name > finished[j].Name
	})

	switch retention.Type {
	case corev1alpha1.BackupScheduleRetentionTypeCount:
		if retention.Count == nil {
			return nil, errors.New("count retention without a count")
		}
		succeeded := 0
		for i, b := range finished {
			if b.Status.State != backupv1alpha1.BackupStateSucceeded {
				continue
			}
			succeeded++
			if succeeded == int(*retention.Count) {
				return finished[i+1:], nil
			}
		}
		return nil, nil
	case corev1alpha1.BackupScheduleRetentionTypeTime:
		cutoff, err := retentionCutoff(retention.Duration, now)
		if err != nil {
			return nil, err
		}
		newestSucceeded := true
		var expired []*backupv1alpha1.Backup
		for _, b := range finished {
			keep := !b.CreationTimestamp.Time.Before(cutoff)
			if b.Status.State == backupv1alpha1.BackupStateSucceeded && newestSucceeded {
				keep, newestSucceeded = true, false
			}
			if !keep {
				expired = append(expired, b)
			}
		}
		return expired, nil
	default:
		return nil, fmt.Errorf("unknown retention type %q", retention.Type)
	}
}

// retentionCutoff converts a "<n>d|w|m" window into the creation time before
// which backups expire.
func retentionCutoff(duration string, now time.Time) (time.Time, error) {
	if len(duration) < 2 {
		return time.Time{}, fmt.Errorf("invalid retention duration %q", duration)
	}
	n, err := strconv.Atoi(duration[:len(duration)-1])
	if err != nil || n < 1 {
		return time.Time{}, fmt.Errorf("invalid retention duration %q", duration)
	}
	switch duration[len(duration)-1] {
	case 'd':
		return now.AddDate(0, 0, -n), nil
	case 'w':
		return now.AddDate(0, 0, -7*n), nil
	case 'm':
		return now.AddDate(0, -n, 0), nil
	default:
		return time.Time{}, fmt.Errorf("invalid retention duration %q", duration)
	}
}

func earliest(a, b time.Time) time.Time {
	if a.IsZero() || (!b.IsZero() && b.Before(a)) {
		return b
	}
	return a
}
