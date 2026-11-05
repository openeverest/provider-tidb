package provider

import (
	"context"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

var pitrNow = time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)

func pitrBackupSpec(pitrStorage string) *corev1alpha1.InstanceBackupSpec {
	spec := &corev1alpha1.InstanceBackupSpec{Enabled: true, ClassRef: commonv1alpha1.ObjectRef{Name: "br"}}
	for _, name := range []string{"s3", "other"} {
		s := corev1alpha1.InstanceBackupStorage{StorageRef: commonv1alpha1.ObjectRef{Name: name}}
		if name == pitrStorage {
			s.PITR = &corev1alpha1.InstanceBackupStoragePITR{Enabled: true}
		}
		spec.Storages = append(spec.Storages, s)
	}
	return spec
}

// pitrContext is brContext for an Instance named instance whose backup spec is backup.
func pitrContext(t *testing.T, instance string, backup *corev1alpha1.InstanceBackupSpec, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	_, cl := brContext(t, objs...)
	in := testInstance("8.5.2")
	in.Name, in.UID = instance, types.UID("uid-"+instance)
	in.Spec.Backup = backup
	return controller.NewContext(context.Background(), cl, in, common.ProviderName), cl
}

func readyGroups(instance string) []client.Object {
	one := int32(1)
	meta := metav1.ObjectMeta{Name: instance, Namespace: testNamespace}
	pd := &tidbcorev1.PDGroup{ObjectMeta: meta}
	pd.Spec.Replicas, pd.Status.ReadyReplicas = &one, 1
	tikv := &tidbcorev1.TiKVGroup{ObjectMeta: meta}
	tikv.Spec.Replicas, tikv.Status.ReadyReplicas = &one, 1
	tidb := &tidbcorev1.TiDBGroup{ObjectMeta: meta}
	tidb.Spec.Replicas, tidb.Status.ReadyReplicas = &one, 1
	return []client.Object{pd, tikv, tidb}
}

func getLogBackup(t *testing.T, cl client.Client, instance string) (*tidbbrv1.Backup, bool) {
	t.Helper()
	b := &tidbbrv1.Backup{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: logBackupName(instance)}, b)
	if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return b, err == nil
}

// runningLogBackup is the log backup of instance to storage, started at start
// and checkpointed at checkpoint.
func runningLogBackup(instance, storage string, start, checkpoint time.Time) *tidbbrv1.Backup {
	b := &tidbbrv1.Backup{ObjectMeta: metav1.ObjectMeta{
		Name: logBackupName(instance), Namespace: testNamespace,
		Annotations: map[string]string{annotationLogBackupStorage: storage},
	}}
	b.Spec.Mode = tidbbrv1.BackupModeLog
	b.Status.Phase = tidbbrv1.BackupRunning
	b.Status.CommitTs = tsString(start)
	b.Status.LogCheckpointTs = tsString(checkpoint)
	return b
}

func tsString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return strconv.FormatUint(tidbbrv1.GoTimeToTS(t), 10)
}

// fullBackup is a completed snapshot of instance on storage whose snapshot is at ts.
func fullBackup(name, instance, storage string, ts time.Time) []client.Object {
	b := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{Type: "Instance", InstanceRef: &commonv1alpha1.ObjectRef{Name: instance}},
			ClassRef:   commonv1alpha1.ObjectRef{Name: "br"},
			StorageRef: commonv1alpha1.ObjectRef{Name: storage},
		},
	}
	br := &tidbbrv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	br.Spec.Mode = tidbbrv1.BackupModeSnapshot
	br.Status.Phase = tidbbrv1.BackupComplete
	br.Status.CommitTs = tsString(ts)
	return []client.Object{b, br}
}

func TestSyncLogBackupStartsOnceTheClusterIsReady(t *testing.T) {
	c, cl := pitrContext(t, "orders", pitrBackupSpec("s3"))
	if err := syncLogBackup(c); err != nil {
		t.Fatalf("syncLogBackup: %v", err)
	}
	if _, found := getLogBackup(t, cl, "orders"); found {
		t.Fatal("log backup started before the cluster was ready")
	}

	c, cl = pitrContext(t, "orders", pitrBackupSpec("s3"), readyGroups("orders")...)
	// A second pass must apply cleanly over the first.
	for range 2 {
		if err := syncLogBackup(c); err != nil {
			t.Fatalf("syncLogBackup: %v", err)
		}
	}
	b, found := getLogBackup(t, cl, "orders")
	if !found {
		t.Fatal("no log backup")
	}
	if b.Spec.Mode != tidbbrv1.BackupModeLog || b.Spec.LogSubcommand != tidbbrv1.LogStartCommand {
		t.Errorf("mode %q subcommand %q, want a started log backup", b.Spec.Mode, b.Spec.LogSubcommand)
	}
	if b.Spec.S3 == nil || b.Spec.S3.Bucket != "bucket" || b.Spec.S3.Prefix != "db/orders/_log" ||
		b.Spec.S3.SecretName != "orders-pitr-br-s3" {
		t.Errorf("S3 = %+v", b.Spec.S3)
	}
	if b.Spec.BR == nil || b.Spec.BR.Cluster != "orders" || b.Spec.ServiceAccount != backupManagerName {
		t.Errorf("BR = %+v, service account %q", b.Spec.BR, b.Spec.ServiceAccount)
	}
	if b.Spec.CleanPolicy != tidbbrv1.CleanPolicyTypeDelete {
		t.Errorf("clean policy %q, want Delete", b.Spec.CleanPolicy)
	}
	if b.Annotations[annotationLogBackupStorage] != "s3" {
		t.Errorf("storage annotation = %q", b.Annotations[annotationLogBackupStorage])
	}
	if ref := metav1.GetControllerOf(b); ref == nil || ref.Kind != "Instance" {
		t.Errorf("log backup owner = %+v, want the Instance", ref)
	}
	secret := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "orders-pitr-br-s3"}, secret); err != nil {
		t.Fatalf("BR credentials: %v", err)
	}
	if string(secret.Data["access_key"]) != "id" {
		t.Errorf("access_key = %q", secret.Data["access_key"])
	}
	assertBackupManagerAccount(t, cl)
}

// A clone is seeded by a BR restore, which can't run while a log backup does.
func TestSyncLogBackupWaitsForTheDataSource(t *testing.T) {
	c, cl := pitrContext(t, "orders", pitrBackupSpec("s3"), readyGroups("orders")...)
	c.SetDataSourceStatus(controller.DataSourceStatus{Done: false, State: controller.DataSourceStateRestoring})
	if err := syncLogBackup(c); err != nil {
		t.Fatalf("syncLogBackup: %v", err)
	}
	if _, found := getLogBackup(t, cl, "orders"); found {
		t.Error("log backup started while the Instance was being seeded")
	}
}

func TestSyncLogBackupStopsWhenDisabledOrMoved(t *testing.T) {
	for name, backup := range map[string]*corev1alpha1.InstanceBackupSpec{
		"disabled":        pitrBackupSpec(""),
		"backup disabled": {Enabled: false, Storages: pitrBackupSpec("s3").Storages},
		"moved":           pitrBackupSpec("other"),
	} {
		t.Run(name, func(t *testing.T) {
			running := runningLogBackup("orders", "s3", pitrNow, pitrNow)
			c, cl := pitrContext(t, "orders", backup, append(readyGroups("orders"), running)...)
			if err := syncLogBackup(c); err != nil {
				t.Fatalf("syncLogBackup: %v", err)
			}
			if _, found := getLogBackup(t, cl, "orders"); found {
				t.Error("log backup still running")
			}
		})
	}
}

func TestSyncLogBackupRemovesCredentialsOnceStopped(t *testing.T) {
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "orders-pitr-br-s3", Namespace: testNamespace}}
	c, cl := pitrContext(t, "orders", pitrBackupSpec(""), secret)
	if err := syncLogBackup(c); err != nil {
		t.Fatalf("syncLogBackup: %v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(secret), &corev1.Secret{}); err == nil {
		t.Error("BR credentials of the stopped log backup remain")
	}
}

func TestSyncLogBackupRejectsAMissingStorage(t *testing.T) {
	backup := pitrBackupSpec("")
	backup.Storages = append(backup.Storages, corev1alpha1.InstanceBackupStorage{
		StorageRef: commonv1alpha1.ObjectRef{Name: "missing"},
		PITR:       &corev1alpha1.InstanceBackupStoragePITR{Enabled: true},
	})
	c, _ := pitrContext(t, "orders", backup, readyGroups("orders")...)
	err := syncLogBackup(c)
	if bce := controller.AsBackupConfigError(err); bce == nil || bce.Reason != reasonPITRStorageInvalid {
		t.Fatalf("syncLogBackup = %v, want a %s backup configuration error", err, reasonPITRStorageInvalid)
	}
}

func TestCleanupStopsTheLogBackupFirst(t *testing.T) {
	logBackup := runningLogBackup("orders", "s3", pitrNow, pitrNow)
	logBackup.Finalizers = []string{"pingcap.com/finalizer"}
	c, cl := cleanupContext(t, testCluster(), logBackup)

	if err := New().Cleanup(c); !controller.IsWaitError(err) {
		t.Fatalf("Cleanup = %v, want to wait for the log backup to stop", err)
	}
	if found, _ := c.Exists(&tidbcorev1.Cluster{}, "orders"); !found {
		t.Error("Cleanup deleted the Cluster while the log backup still needed it")
	}

	stopping, _ := getLogBackup(t, cl, "orders")
	stopping.Finalizers = nil
	if err := cl.Update(context.Background(), stopping); err != nil {
		t.Fatal(err)
	}
	if err := New().Cleanup(c); !controller.IsWaitError(err) {
		t.Fatalf("Cleanup = %v, want to wait for the cluster teardown", err)
	}
	if found, _ := c.Exists(&tidbcorev1.Cluster{}, "orders"); found {
		t.Error("Cleanup did not delete the Cluster once the log backup stopped")
	}
}

// Deleting the namespace removes the cluster alongside, after which TiDB
// Operator can't stop the log backup, so Cleanup releases it.
func TestCleanupReleasesALogBackupWithoutItsCluster(t *testing.T) {
	logBackup := runningLogBackup("orders", "s3", pitrNow, pitrNow)
	logBackup.Finalizers = []string{brFinalizer}
	logBackup.DeletionTimestamp = &metav1.Time{Time: pitrNow}
	c, cl := cleanupContext(t, logBackup)

	if err := New().Cleanup(c); !controller.IsWaitError(err) {
		t.Fatalf("Cleanup = %v, want to wait for the release to land", err)
	}
	if _, found := getLogBackup(t, cl, "orders"); found {
		t.Error("log backup still held by the operator's finalizer")
	}
	if err := New().Cleanup(c); err != nil {
		t.Fatalf("Cleanup = %v, want done", err)
	}
}

func TestPITRWindow(t *testing.T) {
	start := pitrNow.Add(-6 * time.Hour)
	failed := runningLogBackup("orders", "s3", time.Time{}, time.Time{})
	failed.Status.Phase = tidbbrv1.BackupFailed
	failed.Status.Conditions = []metav1.Condition{{Type: "Failed", Status: metav1.ConditionTrue, Message: "access denied"}}

	tests := map[string]struct {
		objs                []client.Object
		wantState           corev1alpha1.PITRState
		wantReason, wantMsg string
		earliest, latest    time.Time
	}{
		"not started": {
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonStarting,
		},
		"starting": {
			objs:      []client.Object{runningLogBackup("orders", "s3", time.Time{}, time.Time{})},
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonStarting,
		},
		"failed": {
			objs:      []client.Object{failed},
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonFailed, wantMsg: "access denied",
		},
		"writing to another storage": {
			objs:      []client.Object{runningLogBackup("orders", "other", start, pitrNow)},
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonStarting,
		},
		"only full backups older than the log": {
			objs: append(fullBackup("before", "orders", "s3", start.Add(-time.Hour)),
				runningLogBackup("orders", "s3", start, pitrNow)),
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonNoBaseBackup, wantMsg: "Take a full backup",
		},
		"window": {
			objs: concat(
				fullBackup("before", "orders", "s3", start.Add(-time.Hour)),
				fullBackup("first", "orders", "s3", start.Add(time.Hour)),
				fullBackup("second", "orders", "s3", start.Add(3*time.Hour)),
				fullBackup("elsewhere", "orders", "other", start.Add(30*time.Minute)),
				fullBackup("of-another-instance", "billing", "s3", start.Add(30*time.Minute)),
				[]client.Object{runningLogBackup("orders", "s3", start, pitrNow.Add(-time.Minute))},
			),
			wantState: corev1alpha1.PITRStateAvailable, wantReason: pitrReasonWindowAvailable,
			earliest: start.Add(time.Hour), latest: pitrNow.Add(-time.Minute),
		},
		"lagging": {
			objs: append(fullBackup("first", "orders", "s3", start.Add(time.Hour)),
				runningLogBackup("orders", "s3", start, pitrNow.Add(-time.Hour))),
			wantState: corev1alpha1.PITRStateAvailable, wantReason: pitrReasonLagging, wantMsg: "1h0m0s behind",
			earliest: start.Add(time.Hour), latest: pitrNow.Add(-time.Hour),
		},
		"log behind the only full backup": {
			objs: append(fullBackup("first", "orders", "s3", pitrNow.Add(-time.Minute)),
				runningLogBackup("orders", "s3", start, pitrNow.Add(-2*time.Minute))),
			wantState: corev1alpha1.PITRStateUnavailable, wantReason: pitrReasonStarting,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			c, _ := pitrContext(t, "orders", pitrBackupSpec("s3"), tc.objs...)
			got, err := pitrWindow(c, "s3", pitrNow)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != tc.wantState || got.Reason != tc.wantReason || !strings.Contains(got.Message, tc.wantMsg) {
				t.Errorf("got %s/%s %q, want %s/%s containing %q", got.State, got.Reason, got.Message, tc.wantState, tc.wantReason, tc.wantMsg)
			}
			if !tc.earliest.IsZero() && (got.EarliestRestorableTime == nil || !got.EarliestRestorableTime.Time.Equal(tc.earliest)) {
				t.Errorf("earliest = %v, want %v", got.EarliestRestorableTime, tc.earliest)
			}
			if !tc.latest.IsZero() && (got.LatestRestorableTime == nil || !got.LatestRestorableTime.Time.Equal(tc.latest)) {
				t.Errorf("latest = %v, want %v", got.LatestRestorableTime, tc.latest)
			}
		})
	}
}

func TestBackupStorageStatusesReportsPITRStoragesOnly(t *testing.T) {
	c, _ := pitrContext(t, "orders", pitrBackupSpec("other"))
	statuses, err := New().BackupStorageStatuses(c)
	if err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 2 || statuses[0].Name != "s3" || statuses[0].PITR != nil ||
		statuses[1].Name != "other" || statuses[1].PITR == nil {
		t.Errorf("statuses = %+v", statuses)
	}

	c, _ = pitrContext(t, "orders", nil)
	if statuses, err := New().BackupStorageStatuses(c); err != nil || statuses != nil {
		t.Errorf("without backups: %+v, %v", statuses, err)
	}
}

func concat(lists ...[]client.Object) []client.Object {
	var out []client.Object
	for _, l := range lists {
		out = append(out, l...)
	}
	return out
}

func pointInTimeRestore(name, target, source string, date *time.Time) *backupv1alpha1.Restore {
	pitr := &backupv1alpha1.DataSourcePointInTime{
		Source:         backupv1alpha1.StreamSource{StorageRef: commonv1alpha1.ObjectRef{Name: "s3"}},
		RecoveryTarget: backupv1alpha1.RecoveryTargetLatest,
	}
	if source != "" {
		pitr.Source.InstanceRef = &commonv1alpha1.ObjectRef{Name: source}
	}
	if date != nil {
		pitr.RecoveryTarget = backupv1alpha1.RecoveryTargetDate
		pitr.Date = &metav1.Time{Time: *date}
	}
	return &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name)},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: target},
			DataSource:  backupv1alpha1.DataSource{Type: backupv1alpha1.DataSourceTypePointInTime, PointInTime: pitr},
		},
	}
}

func cloneCluster() *tidbcorev1.Cluster {
	return &tidbcorev1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "clone", Namespace: testNamespace, UID: "clone-uid"}}
}

func TestPointInTimeRestoreRejectsTheStreamItsTargetWrites(t *testing.T) {
	for _, source := range []string{"", "orders"} {
		restore := pointInTimeRestore("rollback", "orders", source, nil)
		c, _ := pitrContext(t, "orders", pitrBackupSpec("s3"), restore)
		exec, err := New().SyncRestore(c, restore)
		if err != nil {
			t.Fatal(err)
		}
		if exec.State != backupv1alpha1.RestoreStateFailed || !strings.Contains(exec.Message, "Create a new Instance") {
			t.Errorf("source %q: got %s %q, want Failed pointing at a new Instance", source, exec.State, exec.Message)
		}
	}
}

func TestPointInTimeRestore(t *testing.T) {
	start := pitrNow.Add(-6 * time.Hour)
	checkpoint := pitrNow.Add(-time.Minute)
	backups := concat(
		fullBackup("first", "orders", "s3", start.Add(time.Hour)),
		fullBackup("second", "orders", "s3", start.Add(3*time.Hour)),
		[]client.Object{runningLogBackup("orders", "s3", start, checkpoint), cloneCluster()},
	)
	at := func(d time.Duration) *time.Time { t := start.Add(d); return &t }

	tests := map[string]struct {
		date      *time.Time
		wantState backupv1alpha1.RestoreState
		wantMsg   string
		wantBase  string
		wantTS    time.Time
	}{
		"latest":               {wantState: backupv1alpha1.RestoreStatePending, wantBase: "second", wantTS: checkpoint},
		"between full backups": {date: at(2 * time.Hour), wantState: backupv1alpha1.RestoreStatePending, wantBase: "first", wantTS: start.Add(2 * time.Hour)},
		"at a full backup":     {date: at(3 * time.Hour), wantState: backupv1alpha1.RestoreStatePending, wantBase: "second", wantTS: start.Add(3 * time.Hour)},
		"before the window":    {date: at(30 * time.Minute), wantState: backupv1alpha1.RestoreStateFailed, wantMsg: "before the restorable window"},
		"after the checkpoint": {date: &pitrNow, wantState: backupv1alpha1.RestoreStatePending, wantMsg: "to reach"},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			restore := pointInTimeRestore("rewind", "clone", "orders", tc.date)
			c, cl := pitrContext(t, "clone", pitrBackupSpec(""), append(backups, restore)...)
			exec, err := New().SyncRestore(c, restore)
			if err != nil {
				t.Fatal(err)
			}
			if exec.State != tc.wantState || !strings.Contains(exec.Message, tc.wantMsg) {
				t.Errorf("got %s %q, want %s containing %q", exec.State, exec.Message, tc.wantState, tc.wantMsg)
			}
			br := &tidbbrv1.Restore{}
			err = cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "rewind"}, br)
			if tc.wantBase == "" {
				if err == nil {
					t.Error("BR restore created")
				}
				return
			}
			if err != nil {
				t.Fatalf("BR restore: %v", err)
			}
			if br.Spec.Mode != tidbbrv1.RestoreModePiTR || br.Spec.PitrRestoredTs != tsString(tc.wantTS) {
				t.Errorf("mode %q restored-ts %s, want pitr at %s", br.Spec.Mode, br.Spec.PitrRestoredTs, tsString(tc.wantTS))
			}
			if br.Spec.S3 == nil || br.Spec.S3.Prefix != "db/orders/_log" {
				t.Errorf("log storage = %+v", br.Spec.S3)
			}
			full := br.Spec.PitrFullBackupStorageProvider.S3
			if full == nil || full.Prefix != "db/orders/"+tc.wantBase || full.Bucket != "bucket" {
				t.Errorf("full backup storage = %+v, want backup %s", full, tc.wantBase)
			}
			if br.Spec.BR == nil || br.Spec.BR.Cluster != "clone" {
				t.Errorf("BR = %+v, want the clone's cluster", br.Spec.BR)
			}
			if br.Annotations[annotationPITRBaseBackup] != tc.wantBase {
				t.Errorf("base annotation = %q", br.Annotations[annotationPITRBaseBackup])
			}
		})
	}
}

// The point and the full backup are chosen once: later checkpoints and backups
// don't move a restore that already started.
func TestPointInTimeRestoreKeepsItsTarget(t *testing.T) {
	start := pitrNow.Add(-6 * time.Hour)
	logBackup := runningLogBackup("orders", "s3", start, pitrNow.Add(-time.Hour))
	restore := pointInTimeRestore("rewind", "clone", "orders", nil)
	c, cl := pitrContext(t, "clone", pitrBackupSpec(""),
		concat(fullBackup("first", "orders", "s3", start.Add(time.Hour)), []client.Object{logBackup, cloneCluster(), restore})...)
	if _, err := New().SyncRestore(c, restore); err != nil {
		t.Fatal(err)
	}

	logBackup.Status.LogCheckpointTs = tsString(pitrNow)
	if err := cl.Update(context.Background(), logBackup); err != nil {
		t.Fatal(err)
	}
	for _, obj := range fullBackup("second", "orders", "s3", start.Add(3*time.Hour)) {
		if err := cl.Create(context.Background(), obj); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := New().SyncRestore(c, restore); err != nil {
		t.Fatal(err)
	}

	br := &tidbbrv1.Restore{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "rewind"}, br); err != nil {
		t.Fatal(err)
	}
	if br.Spec.PitrRestoredTs != tsString(pitrNow.Add(-time.Hour)) || br.Annotations[annotationPITRBaseBackup] != "first" {
		t.Errorf("restore moved to %s from %s", br.Spec.PitrRestoredTs, br.Annotations[annotationPITRBaseBackup])
	}
}

func TestPointInTimeRestoreWaitsForTheLogBackup(t *testing.T) {
	restore := pointInTimeRestore("rewind", "clone", "orders", nil)
	c, _ := pitrContext(t, "clone", pitrBackupSpec(""), cloneCluster(), restore)
	exec, err := New().SyncRestore(c, restore)
	if err != nil {
		t.Fatal(err)
	}
	if exec.State != backupv1alpha1.RestoreStatePending || !strings.Contains(exec.Message, `log backup of Instance "orders"`) {
		t.Errorf("got %s %q, want Pending on the log backup", exec.State, exec.Message)
	}
}

// BR before v8.5.5 refuses to restore into a cluster whose log backup runs.
func TestRestoreIntoAnInstanceWithALogBackup(t *testing.T) {
	tikvAt := func(version string) *tidbcorev1.TiKVGroup {
		g := &tidbcorev1.TiKVGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
		g.Spec.Template.Spec.Version = version
		return g
	}
	stopping := runningLogBackup("orders", "s3", pitrNow, pitrNow)
	stopping.Finalizers = []string{"pingcap.com/finalizer"}
	stopping.DeletionTimestamp = &metav1.Time{Time: pitrNow}

	tests := map[string]struct {
		objs      []client.Object
		wantState backupv1alpha1.RestoreState
		wantMsg   string
	}{
		"log backup running": {
			objs:      []client.Object{runningLogBackup("orders", "s3", pitrNow, pitrNow), tikvAt("v8.5.2")},
			wantState: backupv1alpha1.RestoreStateFailed, wantMsg: "Turn off point-in-time recovery",
		},
		"log backup stopping": {
			objs:      []client.Object{stopping, tikvAt("v8.5.2")},
			wantState: backupv1alpha1.RestoreStatePending, wantMsg: "to stop",
		},
		"BR restores alongside the log backup": {
			objs:      []client.Object{runningLogBackup("orders", "s3", pitrNow, pitrNow), tikvAt("v8.5.5")},
			wantState: backupv1alpha1.RestoreStatePending,
		},
		"no log backup": {
			objs:      []client.Object{tikvAt("v8.5.2")},
			wantState: backupv1alpha1.RestoreStatePending,
		},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			restore := &backupv1alpha1.Restore{
				ObjectMeta: metav1.ObjectMeta{Name: "rollback", Namespace: testNamespace, UID: "restore-uid"},
				Spec: backupv1alpha1.RestoreSpec{
					InstanceRef: commonv1alpha1.ObjectRef{Name: "orders"},
					DataSource: backupv1alpha1.DataSource{
						Type:   backupv1alpha1.DataSourceTypeBackup,
						Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
					},
				},
			}
			c, _ := pitrContext(t, "orders", pitrBackupSpec("s3"),
				append(tc.objs, testBackup(backupv1alpha1.BackupStateSucceeded), restore)...)
			exec, err := New().SyncRestore(c, restore)
			if err != nil {
				t.Fatal(err)
			}
			if exec.State != tc.wantState || !strings.Contains(exec.Message, tc.wantMsg) {
				t.Errorf("got %s %q, want %s containing %q", exec.State, exec.Message, tc.wantState, tc.wantMsg)
			}
		})
	}
}

// After a point-in-time restore the root password is the one the base backup carries.
func TestPointInTimeRestoreAdoptsTheBaseBackupRootPassword(t *testing.T) {
	restore := pointInTimeRestore("rewind", "clone", "orders", nil)
	br := &tidbbrv1.Restore{ObjectMeta: metav1.ObjectMeta{
		Name: "rewind", Namespace: testNamespace,
		Annotations: map[string]string{annotationPITRBaseBackup: "first"},
	}}
	br.Status.Phase = tidbbrv1.RestoreComplete
	br.Status.TimeCompleted = metav1.NewTime(pitrNow)
	c, cl := pitrContext(t, "clone", pitrBackupSpec(""), cloneCluster(), restore, br,
		rootSecret(backupRootSecretName("first"), "at-first"), rootSecret(rootSecretName("clone"), "clone-own"))

	exec, err := New().SyncRestore(c, restore)
	if err != nil {
		t.Fatal(err)
	}
	if exec.State != backupv1alpha1.RestoreStateSucceeded {
		t.Fatalf("state %s", exec.State)
	}
	if pw, _ := secretPassword(t, cl, rootSecretName("clone")); pw != "at-first" {
		t.Errorf("root password = %q, want the base backup's", pw)
	}
}
