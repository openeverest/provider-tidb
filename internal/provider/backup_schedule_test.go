package provider

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/robfig/cron/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	"github.com/openeverest/provider-tidb/internal/common"
)

var scheduleNow = time.Date(2026, 10, 1, 10, 3, 0, 0, time.UTC)

func TestLatestSlot(t *testing.T) {
	tests := []struct {
		name string
		cron string
		now  time.Time
		want time.Time
	}{
		{"slot just passed", "0 10 * * *", scheduleNow, time.Date(2026, 10, 1, 10, 0, 0, 0, time.UTC)},
		{"latest of several slots in the window", "*/2 * * * *", scheduleNow, time.Date(2026, 10, 1, 10, 2, 0, 0, time.UTC)},
		{"slot exactly now", "3 10 * * *", scheduleNow, scheduleNow},
		{"slot past the deadline is skipped", "0 9 * * *", scheduleNow, time.Time{}},
		{"schedule that never fires", "0 0 30 2 *", scheduleNow, time.Time{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, err := cron.ParseStandard(tt.cron)
			if err != nil {
				t.Fatal(err)
			}
			if got := latestSlot(sched, tt.now, scheduleStartingDeadline); !got.Equal(tt.want) {
				t.Fatalf("latestSlot = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestScheduledBackupName(t *testing.T) {
	slot := time.Date(2026, 10, 1, 2, 0, 0, 0, time.UTC)
	if got := scheduledBackupName("orders", "daily", slot); got != "orders-daily-202610010200" {
		t.Fatalf("simple name = %q", got)
	}

	long := strings.Repeat("a", 37)
	names := map[string]bool{}
	for _, schedule := range []string{"nightly-full-backup-to-s3", "nightly-full-backup-to-gcs", "Nightly_Full", "nightly-full"} {
		name := scheduledBackupName(long, schedule, slot)
		if len(name) > maxScheduledBackupNameLength {
			t.Fatalf("%q is %d chars, want <= %d", name, len(name), maxScheduledBackupNameLength)
		}
		if dnsLabel(name) != name || strings.Contains(name, "--") {
			t.Fatalf("%q is not a clean DNS label", name)
		}
		if names[name] {
			t.Fatalf("schedules collide on %q", name)
		}
		names[name] = true
	}

	if a, b := scheduledBackupName("orders", "Daily", slot), scheduledBackupName("orders", "daily", slot); a == b {
		t.Fatalf("sanitised schedule name collides with an existing one: %q", a)
	}
}

func TestExpiredBackups(t *testing.T) {
	backups := []backupv1alpha1.Backup{
		finishedBackup("b6", 1, backupv1alpha1.BackupStateRunning),
		finishedBackup("b5", 2, backupv1alpha1.BackupStateFailed),
		finishedBackup("b4", 3, backupv1alpha1.BackupStateSucceeded),
		finishedBackup("b3", 4, backupv1alpha1.BackupStateFailed),
		finishedBackup("b2", 5, backupv1alpha1.BackupStateSucceeded),
		finishedBackup("b1", 6, backupv1alpha1.BackupStateSucceeded),
	}
	tests := []struct {
		name      string
		backups   []backupv1alpha1.Backup
		retention *corev1alpha1.BackupScheduleRetention
		want      []string
		wantErr   string
	}{
		{"no retention keeps everything", backups, nil, nil, ""},
		{"count keeps the newest successful and newer failed", backups, countRetention(1), []string{"b3", "b2", "b1"}, ""},
		{"count spans failed runs", backups, countRetention(2), []string{"b1"}, ""},
		{"fewer successes than count", backups, countRetention(5), nil, ""},
		{"time expires backups older than the window", backups, timeRetention("3d"), []string{"b3", "b2", "b1"}, ""},
		{
			"time keeps the newest success even when expired",
			[]backupv1alpha1.Backup{
				finishedBackup("new-failed", 1, backupv1alpha1.BackupStateFailed),
				finishedBackup("old-ok", 30, backupv1alpha1.BackupStateSucceeded),
				finishedBackup("older-ok", 31, backupv1alpha1.BackupStateSucceeded),
			},
			timeRetention("1w"),
			[]string{"older-ok"},
			"",
		},
		{"invalid duration", backups, timeRetention("3y"), nil, "invalid retention duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := expiredBackups(tt.backups, tt.retention, scheduleNow)
			assertError(t, err, tt.wantErr)
			if names := backupNames(got); strings.Join(names, ",") != strings.Join(tt.want, ",") {
				t.Fatalf("expired = %v, want %v", names, tt.want)
			}
		})
	}
}

func TestRetentionCutoffMonths(t *testing.T) {
	got, err := retentionCutoff("2m", scheduleNow)
	assertError(t, err, "")
	if want := time.Date(2026, 8, 1, 10, 3, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("cutoff = %v, want %v", got, want)
	}
}

func TestValidateBackupSchedules(t *testing.T) {
	spec := scheduledBackupSpec(corev1alpha1.InstanceBackupSchedule{Name: "ok", Cron: "@daily"},
		corev1alpha1.InstanceBackupSchedule{Name: "broken", Cron: "61 * * * *"})
	err := validateBackupSchedules(spec)
	var bce *controller.BackupConfigError
	if !errors.As(err, &bce) || bce.Reason != reasonScheduleInvalid || !strings.Contains(bce.Message, `schedule "broken"`) {
		t.Fatalf("error = %v, want a %s BackupConfigError naming the broken schedule", err, reasonScheduleInvalid)
	}

	spec.Enabled = false
	assertError(t, validateBackupSchedules(spec), "")
}

func TestBackupSchedulerReconcile(t *testing.T) {
	hourly := corev1alpha1.InstanceBackupSchedule{Name: "hourly", Enabled: true, Cron: "0 * * * *"}
	dueName := "orders-hourly-202610011000"

	tests := []struct {
		name        string
		phase       corev1alpha1.InstancePhase
		schedule    corev1alpha1.InstanceBackupSchedule
		existing    []client.Object
		wantBackups []string
		wantRequeue time.Duration
	}{
		{
			name:        "due slot creates a backup",
			phase:       corev1alpha1.InstancePhaseReady,
			schedule:    hourly,
			wantBackups: []string{dueName},
			wantRequeue: 57 * time.Minute,
		},
		{
			name:        "slot already taken is not repeated",
			phase:       corev1alpha1.InstancePhaseReady,
			schedule:    hourly,
			existing:    []client.Object{scheduledBackup(dueName, "hourly", 0, backupv1alpha1.BackupStateRunning)},
			wantBackups: []string{dueName},
			wantRequeue: 57 * time.Minute,
		},
		{
			name:        "held while the instance is not serving",
			phase:       corev1alpha1.InstancePhaseProvisioning,
			schedule:    hourly,
			wantRequeue: scheduleRetryInterval,
		},
		{
			name:        "held while the previous run is in progress",
			phase:       corev1alpha1.InstancePhaseReady,
			schedule:    hourly,
			existing:    []client.Object{scheduledBackup("orders-hourly-202610010900", "hourly", 1, backupv1alpha1.BackupStateRunning)},
			wantBackups: []string{"orders-hourly-202610010900"},
			wantRequeue: scheduleRetryInterval,
		},
		{
			name:     "disabled schedule does nothing",
			phase:    corev1alpha1.InstancePhaseReady,
			schedule: corev1alpha1.InstanceBackupSchedule{Name: "hourly", Cron: "0 * * * *"},
		},
		{
			name: "retention prunes older backups of the schedule only",
			phase: corev1alpha1.InstancePhaseReady,
			schedule: corev1alpha1.InstanceBackupSchedule{
				Name: "hourly", Enabled: true, Cron: "0 * * * *", Retention: countRetention(1),
			},
			existing: []client.Object{
				scheduledBackup(dueName, "hourly", 0, backupv1alpha1.BackupStateSucceeded),
				scheduledBackup("orders-hourly-202610010900", "hourly", 1, backupv1alpha1.BackupStateSucceeded),
				scheduledBackup("on-demand", "", 2, backupv1alpha1.BackupStateSucceeded),
			},
			wantBackups: []string{"on-demand", dueName},
			wantRequeue: 57 * time.Minute,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			in.Spec.ProviderRef.Name = common.ProviderName
			in.Spec.Backup = scheduledBackupSpec(tt.schedule)
			in.Status.Phase = tt.phase

			cl := schedulerClient(t, append([]client.Object{in}, tt.existing...)...)
			s := &BackupScheduler{client: cl, now: func() time.Time { return scheduleNow }}
			res, err := s.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: in.Name}})
			assertError(t, err, "")
			if res.RequeueAfter != tt.wantRequeue {
				t.Fatalf("RequeueAfter = %v, want %v", res.RequeueAfter, tt.wantRequeue)
			}

			list := &backupv1alpha1.BackupList{}
			assertError(t, cl.List(context.Background(), list, client.InNamespace(testNamespace)), "")
			var got []string
			for _, b := range list.Items {
				got = append(got, b.Name)
			}
			if strings.Join(got, ",") != strings.Join(tt.wantBackups, ",") {
				t.Fatalf("backups = %v, want %v", got, tt.wantBackups)
			}
		})
	}
}

func TestBackupSchedulerCreatesBackupFromSchedule(t *testing.T) {
	in := testInstance("8.5.2")
	in.Spec.ProviderRef.Name = common.ProviderName
	in.Spec.Backup = scheduledBackupSpec(corev1alpha1.InstanceBackupSchedule{
		Name: "hourly", Enabled: true, Cron: "0 * * * *",
		Parameters: &runtime.RawExtension{Raw: []byte(`{"k":"v"}`)},
	})
	in.Status.Phase = corev1alpha1.InstancePhaseReady
	cl := schedulerClient(t, in)
	s := &BackupScheduler{client: cl, now: func() time.Time { return scheduleNow }}
	_, err := s.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Namespace: testNamespace, Name: in.Name}})
	assertError(t, err, "")

	b := &backupv1alpha1.Backup{}
	assertError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: testNamespace, Name: "orders-hourly-202610011000"}, b), "")
	if b.Spec.Origin.InstanceRef == nil || b.Spec.Origin.InstanceRef.Name != "orders" ||
		b.Spec.ClassRef.Name != "br" || b.Spec.StorageRef.Name != "s3-store" ||
		b.Spec.ScheduleName != "hourly" || b.Spec.Parameters == nil || string(b.Spec.Parameters.Raw) != `{"k":"v"}` {
		t.Fatalf("unexpected backup spec: %+v", b.Spec)
	}
}

func scheduledBackupSpec(schedules ...corev1alpha1.InstanceBackupSchedule) *corev1alpha1.InstanceBackupSpec {
	return &corev1alpha1.InstanceBackupSpec{
		Enabled:  true,
		ClassRef: commonv1alpha1.ObjectRef{Name: "br"},
		Storages: []corev1alpha1.InstanceBackupStorage{{
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3-store"},
			Schedules:  schedules,
		}},
	}
}

func schedulerClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, install := range []func(*runtime.Scheme) error{corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme} {
		if err := install(scheme); err != nil {
			t.Fatalf("building scheme: %v", err)
		}
	}
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).
		WithIndex(&backupv1alpha1.Backup{}, controller.IndexBackupInstanceName, func(obj client.Object) []string {
			if b, ok := obj.(*backupv1alpha1.Backup); ok && b.Spec.Origin.InstanceRef != nil {
				return []string{b.Spec.Origin.InstanceRef.Name}
			}
			return nil
		}).Build()
}

// scheduledBackup builds a Backup of the "orders" Instance created ageHours before scheduleNow.
func scheduledBackup(name, schedule string, ageHours int, state backupv1alpha1.BackupState) *backupv1alpha1.Backup {
	b := finishedBackup(name, 0, state)
	b.Namespace = testNamespace
	b.CreationTimestamp = metav1.NewTime(scheduleNow.Add(-time.Duration(ageHours) * time.Hour))
	b.Spec = backupv1alpha1.BackupSpec{
		Origin: backupv1alpha1.BackupOrigin{
			Type:        backupv1alpha1.BackupOriginTypeInstance,
			InstanceRef: &commonv1alpha1.ObjectRef{Name: "orders"},
		},
		ClassRef:     commonv1alpha1.ObjectRef{Name: "br"},
		StorageRef:   commonv1alpha1.ObjectRef{Name: "s3-store"},
		ScheduleName: schedule,
	}
	return &b
}

// finishedBackup builds a Backup created ageDays before scheduleNow.
func finishedBackup(name string, ageDays int, state backupv1alpha1.BackupState) backupv1alpha1.Backup {
	return backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, CreationTimestamp: metav1.NewTime(scheduleNow.AddDate(0, 0, -ageDays))},
		Status:     backupv1alpha1.BackupStatus{State: state},
	}
}

func countRetention(n int32) *corev1alpha1.BackupScheduleRetention {
	return &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeCount, Count: &n}
}

func timeRetention(d string) *corev1alpha1.BackupScheduleRetention {
	return &corev1alpha1.BackupScheduleRetention{Type: corev1alpha1.BackupScheduleRetentionTypeTime, Duration: d}
}

func backupNames(backups []*backupv1alpha1.Backup) []string {
	var names []string
	for _, b := range backups {
		names = append(names, b.Name)
	}
	return names
}
