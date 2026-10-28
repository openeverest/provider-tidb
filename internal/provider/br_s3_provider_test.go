package provider

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"
)

func TestBRS3Provider(t *testing.T) {
	for endpoint, want := range map[string]tidbbrv1.S3StorageProviderType{
		"":                                      tidbbrv1.S3StorageProviderTypeAWS,
		"https://s3.eu-central-1.amazonaws.com": tidbbrv1.S3StorageProviderTypeAWS,
		"https://s3.amazonaws.com":              tidbbrv1.S3StorageProviderTypeAWS,
		"https://de-fra-1.linodeobjects.com":    s3CompatibleProvider,
		"http://seaweedfs.s3.svc:8333":          s3CompatibleProvider,
		"https://minio.example.com":             s3CompatibleProvider,
		"https://amazonaws.com.evil.example":    s3CompatibleProvider,
	} {
		if got := brS3Provider(endpoint); got != want {
			t.Errorf("brS3Provider(%q) = %q, want %q", endpoint, got, want)
		}
	}
}

// Linode Object Storage, like Ceph RGW, answers a bucket region lookup with
// "default"; as "aws" BR refused to back up to it.
func TestBackupAndRestoreToS3CompatibleStorage(t *testing.T) {
	backup := testBackup("")
	c, cl := brContext(t, backup)
	if _, err := New().SyncBackup(c, backup); err != nil {
		t.Fatalf("SyncBackup: %v", err)
	}
	br := &tidbbrv1.Backup{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "nightly"}, br); err != nil {
		t.Fatal(err)
	}
	if br.Spec.S3.Provider != s3CompatibleProvider {
		t.Errorf("BR backup provider = %q, want an S3-compatible provider for endpoint %q", br.Spec.S3.Provider, br.Spec.S3.Endpoint)
	}

	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "clone", Namespace: testNamespace, UID: "restore-uid"},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "orders"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
			},
		},
	}
	c, cl = brContext(t, testBackup(backupv1alpha1.BackupStateSucceeded), restore)
	if _, err := New().SyncRestore(c, restore); err != nil {
		t.Fatalf("SyncRestore: %v", err)
	}
	brr := &tidbbrv1.Restore{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "clone"}, brr); err != nil {
		t.Fatal(err)
	}
	if brr.Spec.S3.Provider != s3CompatibleProvider {
		t.Errorf("BR restore provider = %q, want an S3-compatible provider", brr.Spec.S3.Provider)
	}
}
