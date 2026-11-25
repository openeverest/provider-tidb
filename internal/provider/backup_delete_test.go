package provider

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
)

// completedBRBackup is the BR Backup of testBackup as TiDB Operator leaves it
// after the snapshot, created under cleanPolicy.
func completedBRBackup(cleanPolicy tidbbrv1.CleanPolicyType) *tidbbrv1.Backup {
	br := &tidbbrv1.Backup{ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testNamespace}}
	br.Spec.Mode = tidbbrv1.BackupModeSnapshot
	br.Spec.CleanPolicy = cleanPolicy
	if cleanPolicy == tidbbrv1.CleanPolicyTypeDelete {
		br.Finalizers = []string{brFinalizer}
	}
	br.Spec.S3 = &tidbbrv1.S3StorageProvider{Bucket: "bucket", Prefix: testBackupPath + "/nightly"}
	br.Status.Phase = tidbbrv1.BackupComplete
	br.Status.BackupPath = "s3://bucket/" + testBackupPath + "/nightly"
	return br
}

func backupWithPolicy(policy backupv1alpha1.BackupDeletionPolicy) *backupv1alpha1.Backup {
	backup := testBackup(backupv1alpha1.BackupStateSucceeded)
	backup.Spec.DeletionPolicy = policy
	return backup
}

func getBRBackup(t *testing.T, cl client.Client) (*tidbbrv1.Backup, bool) {
	t.Helper()
	br := &tidbbrv1.Backup{}
	err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "nightly"}, br)
	if client.IgnoreNotFound(err) != nil {
		t.Fatal(err)
	}
	return br, err == nil
}

// deletionPolicy may change after the backup was taken: deleting the Backup
// follows the policy it has then, not the one it was created with.
func TestCleanupBackupFollowsTheCurrentDeletionPolicy(t *testing.T) {
	for _, tt := range []struct {
		name      string
		createdAs tidbbrv1.CleanPolicyType
		policy    backupv1alpha1.BackupDeletionPolicy
	}{
		{"delete", tidbbrv1.CleanPolicyTypeDelete, backupv1alpha1.BackupDeletionPolicyDelete},
		{"retain", tidbbrv1.CleanPolicyTypeRetain, backupv1alpha1.BackupDeletionPolicyRetain},
		{"switched to retain", tidbbrv1.CleanPolicyTypeDelete, backupv1alpha1.BackupDeletionPolicyRetain},
		{"switched to delete", tidbbrv1.CleanPolicyTypeRetain, backupv1alpha1.BackupDeletionPolicyDelete},
	} {
		t.Run(tt.name, func(t *testing.T) {
			backup := backupWithPolicy(tt.policy)
			c, cl := brContext(t, backup, completedBRBackup(tt.createdAs))

			done, err := New().CleanupBackup(c, backup)
			if err != nil || done {
				t.Fatalf("CleanupBackup = %v, %v; want to wait for the BR backup to go", done, err)
			}
			br, found := getBRBackup(t, cl)
			if tt.policy == backupv1alpha1.BackupDeletionPolicyRetain {
				if found {
					t.Fatalf("BR backup still exists (cleanPolicy %s, finalizers %v): the data would not be retained",
						br.Spec.CleanPolicy, br.Finalizers)
				}
			} else {
				if !found {
					t.Fatal("BR backup is gone before TiDB Operator could delete its data")
				}
				if br.DeletionTimestamp.IsZero() || br.Spec.CleanPolicy != tidbbrv1.CleanPolicyTypeDelete ||
					!controllerutil.ContainsFinalizer(br, brFinalizer) {
					t.Errorf("BR backup: deleting %v, cleanPolicy %s, finalizers %v; want deleted, Delete, held for the clean job",
						!br.DeletionTimestamp.IsZero(), br.Spec.CleanPolicy, br.Finalizers)
				}
			}

			// Done once the BR backup is gone.
			if found {
				controllerutil.RemoveFinalizer(br, brFinalizer)
				if err := cl.Update(context.Background(), br); err != nil {
					t.Fatal(err)
				}
			}
			if done, err := New().CleanupBackup(c, backup); err != nil || !done {
				t.Errorf("CleanupBackup = %v, %v; want done", done, err)
			}
		})
	}
}

// A BR backup already being deleted whose Backup now says Retain is released:
// TiDB Operator would hold it for a clean that must not happen.
func TestCleanupBackupReleasesABRBackupBeingDeletedWhenRetained(t *testing.T) {
	backup := backupWithPolicy(backupv1alpha1.BackupDeletionPolicyRetain)
	c, cl := brContext(t, backup, completedBRBackup(tidbbrv1.CleanPolicyTypeDelete))
	br, _ := getBRBackup(t, cl)
	if err := cl.Delete(context.Background(), br); err != nil {
		t.Fatal(err)
	}

	if _, err := New().CleanupBackup(c, backup); err != nil {
		t.Fatal(err)
	}
	if _, found := getBRBackup(t, cl); found {
		t.Error("BR backup is still held")
	}
}

func TestCleanupBackupWaitsForTheCleanJob(t *testing.T) {
	backup := backupWithPolicy(backupv1alpha1.BackupDeletionPolicyDelete)
	c, cl := brContext(t, backup, completedBRBackup(tidbbrv1.CleanPolicyTypeDelete))
	br, _ := getBRBackup(t, cl)
	if err := cl.Delete(context.Background(), br); err != nil {
		t.Fatal(err)
	}

	if done, err := New().CleanupBackup(c, backup); err != nil || done {
		t.Fatalf("CleanupBackup = %v, %v; want to wait", done, err)
	}
	if br, found := getBRBackup(t, cl); !found || !controllerutil.ContainsFinalizer(br, brFinalizer) {
		t.Error("BR backup was released before its data was deleted")
	}

	// TiDB Operator doesn't retry a failed clean: say why the Backup stays.
	br, _ = getBRBackup(t, cl)
	br.Status.Conditions = append(br.Status.Conditions, metav1.Condition{
		Type: string(tidbbrv1.BackupCleanFailed), Status: metav1.ConditionTrue, Reason: "CleanBackupDataFailed",
		Message: "access denied",
	})
	if err := cl.Update(context.Background(), br); err != nil {
		t.Fatal(err)
	}
	done, err := New().CleanupBackup(c, backup)
	if done || err == nil || !strings.Contains(err.Error(), "access denied") {
		t.Errorf("CleanupBackup = %v, %v; want the clean failure", done, err)
	}
}

// Deleting a namespace deletes the BR backups without CleanupBackup, so they
// carry the Backup's current policy all along.
func TestSyncBackupKeepsTheCleanPolicyCurrent(t *testing.T) {
	backup := backupWithPolicy(backupv1alpha1.BackupDeletionPolicyRetain)
	c, cl := brContext(t, backup, completedBRBackup(tidbbrv1.CleanPolicyTypeDelete))
	if _, err := New().SyncBackup(c, backup); err != nil {
		t.Fatal(err)
	}
	br, _ := getBRBackup(t, cl)
	if br.Spec.CleanPolicy != tidbbrv1.CleanPolicyTypeRetain || controllerutil.ContainsFinalizer(br, brFinalizer) {
		t.Errorf("cleanPolicy %s, finalizers %v; want Retain without the operator's finalizer", br.Spec.CleanPolicy, br.Finalizers)
	}

	backup.Spec.DeletionPolicy = backupv1alpha1.BackupDeletionPolicyDelete
	if _, err := New().SyncBackup(c, backup); err != nil {
		t.Fatal(err)
	}
	br, _ = getBRBackup(t, cl)
	if br.Spec.CleanPolicy != tidbbrv1.CleanPolicyTypeDelete || !controllerutil.ContainsFinalizer(br, brFinalizer) {
		t.Errorf("cleanPolicy %s, finalizers %v; want Delete with the operator's finalizer", br.Spec.CleanPolicy, br.Finalizers)
	}
}
