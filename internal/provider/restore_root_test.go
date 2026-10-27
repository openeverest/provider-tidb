package provider

import (
	"context"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
)

func rootSecret(name, password string) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace},
		Data:       map[string][]byte{rootPasswordKey: []byte(password)},
	}
}

func secretPassword(t *testing.T, cl client.Client, name string) (string, map[string]string) {
	t.Helper()
	s := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: name}, s); err != nil {
		t.Fatalf("get secret %s: %v", name, err)
	}
	return string(s.Data[rootPasswordKey]), s.Annotations
}

// restoreFrom returns a Restore of backupName into the test Instance, and the
// BR Restore the operator finished at completed.
func restoreFrom(name, backupName string, completed time.Time) (*backupv1alpha1.Restore, *tidbbrv1.Restore) {
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, UID: types.UID("uid-" + name)},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "orders"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: backupName}},
			},
		},
	}
	br := &tidbbrv1.Restore{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace}}
	br.Status.Phase = tidbbrv1.RestoreComplete
	br.Status.TimeCompleted = metav1.NewTime(completed)
	return restore, br
}

func succeededBackup(name, origin string) *backupv1alpha1.Backup {
	b := testBackup(backupv1alpha1.BackupStateSucceeded)
	b.Name, b.UID = name, types.UID("uid-"+name)
	b.Spec.Origin.InstanceRef = &commonv1alpha1.ObjectRef{Name: origin}
	return b
}

func TestSyncBackupRecordsRootPasswordOnce(t *testing.T) {
	backup := testBackup("")
	c, cl := brContext(t, backup, rootSecret(rootSecretName("orders"), "at-backup"))

	if _, err := New().SyncBackup(c, backup); err != nil {
		t.Fatalf("SyncBackup: %v", err)
	}
	// A password change after the backup started must not leak into its record.
	if err := cl.Update(context.Background(), rootSecret(rootSecretName("orders"), "changed-later")); err != nil {
		t.Fatal(err)
	}
	if _, err := New().SyncBackup(c, backup); err != nil {
		t.Fatalf("SyncBackup: %v", err)
	}

	if pw, _ := secretPassword(t, cl, backupRootSecretName("nightly")); pw != "at-backup" {
		t.Errorf("recorded root password = %q, want %q", pw, "at-backup")
	}
	s := &corev1.Secret{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: backupRootSecretName("nightly")}, s); err != nil {
		t.Fatal(err)
	}
	if ref := metav1.GetControllerOf(s); ref == nil || ref.Kind != "Backup" || ref.Name != "nightly" {
		t.Errorf("record is owned by %+v, want the Backup so it goes away with it", ref)
	}
}

func TestSyncRestoreAdoptsTheBackupRootPassword(t *testing.T) {
	completed := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	restore, br := restoreFrom("clone", "nightly", completed)
	c, cl := brContext(t, succeededBackup("nightly", "source"), restore, br,
		rootSecret(rootSecretName("orders"), "generated-for-target"),
		rootSecret(backupRootSecretName("nightly"), "from-source"))

	exec, err := New().SyncRestore(c, restore)
	if err != nil {
		t.Fatalf("SyncRestore: %v", err)
	}
	if exec.State != backupv1alpha1.RestoreStateSucceeded || exec.Message != "" {
		t.Errorf("restore state = %q, message %q", exec.State, exec.Message)
	}
	pw, ann := secretPassword(t, cl, rootSecretName("orders"))
	if pw != "from-source" {
		t.Errorf("root password = %q, want the restored %q", pw, "from-source")
	}
	if ann[annotationRootFromRestore] != "clone" || ann[annotationRootRestoredAt] != completed.Format(time.RFC3339) {
		t.Errorf("annotations = %v", ann)
	}
	if got := readRootPassword(c); got != "from-source" {
		t.Errorf("connection password = %q", got)
	}
}

func TestOlderRestoreDoesNotOverrideNewerRootPassword(t *testing.T) {
	t1 := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	older, olderBR := restoreFrom("older", "monday", t1)
	newer, newerBR := restoreFrom("newer", "tuesday", t1.Add(time.Hour))
	c, cl := brContext(t, succeededBackup("monday", "source"), succeededBackup("tuesday", "source"),
		older, olderBR, newer, newerBR,
		rootSecret(rootSecretName("orders"), "generated"),
		rootSecret(backupRootSecretName("monday"), "monday-pw"),
		rootSecret(backupRootSecretName("tuesday"), "tuesday-pw"))

	for _, r := range []*backupv1alpha1.Restore{newer, older, newer, older} {
		if _, err := New().SyncRestore(c, r); err != nil {
			t.Fatalf("SyncRestore %s: %v", r.Name, err)
		}
	}
	if pw, _ := secretPassword(t, cl, rootSecretName("orders")); pw != "tuesday-pw" {
		t.Errorf("root password = %q, want the newer restore's %q", pw, "tuesday-pw")
	}
}

func TestRestoreFallsBackToTheSourceInstancePassword(t *testing.T) {
	restore, br := restoreFrom("clone", "legacy", time.Now())
	c, cl := brContext(t, succeededBackup("legacy", "source"), restore, br,
		rootSecret(rootSecretName("orders"), "generated"),
		rootSecret(rootSecretName("source"), "source-current"))

	if _, err := New().SyncRestore(c, restore); err != nil {
		t.Fatalf("SyncRestore: %v", err)
	}
	if pw, _ := secretPassword(t, cl, rootSecretName("orders")); pw != "source-current" {
		t.Errorf("root password = %q, want %q", pw, "source-current")
	}
}

func TestRestoreWithUnknownRootPasswordSaysSo(t *testing.T) {
	restore, br := restoreFrom("clone", "orphan", time.Now())
	c, cl := brContext(t, succeededBackup("orphan", "deleted-instance"), restore, br,
		rootSecret(rootSecretName("orders"), "generated"))

	exec, err := New().SyncRestore(c, restore)
	if err != nil {
		t.Fatalf("SyncRestore: %v", err)
	}
	if exec.State != backupv1alpha1.RestoreStateSucceeded || !strings.Contains(exec.Message, "root password") {
		t.Errorf("restore state = %q, message %q", exec.State, exec.Message)
	}
	if pw, _ := secretPassword(t, cl, rootSecretName("orders")); pw != "generated" {
		t.Errorf("root password = %q, want it untouched", pw)
	}
}
