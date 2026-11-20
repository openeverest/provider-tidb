package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	s3class "github.com/openeverest/openeverest/v2/backupstorageclasses/s3"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"

	"github.com/openeverest/provider-tidb/definition/backupclasses"
)

func TestBRS3Provider(t *testing.T) {
	for _, tt := range []struct {
		implementation s3class.Implementation
		endpoint       string
		want           tidbbrv1.S3StorageProviderType
	}{
		{s3class.ImplementationAWS, "", tidbbrv1.S3StorageProviderTypeAWS},
		{s3class.ImplementationAWS, "https://s3.eu-central-1.amazonaws.com", tidbbrv1.S3StorageProviderTypeAWS},
		{s3class.ImplementationAWS, "https://s3.amazonaws.com", tidbbrv1.S3StorageProviderTypeAWS},
		// Unset implementation means Amazon S3, but BR's region check only
		// passes against an amazonaws.com endpoint.
		{s3class.ImplementationAWS, "https://de-fra-1.linodeobjects.com", s3CompatibleProvider},
		{s3class.ImplementationAWS, "https://amazonaws.com.evil.example", s3CompatibleProvider},
		{s3class.ImplementationMinIO, "https://minio.example.com", s3CompatibleProvider},
		{s3class.ImplementationCeph, "http://rgw.ceph.svc", s3CompatibleProvider},
		{s3class.ImplementationOther, "http://seaweedfs.s3.svc:8333", s3CompatibleProvider},
		{s3class.ImplementationOther, "https://s3.amazonaws.com", s3CompatibleProvider},
	} {
		if got := brS3Provider(tt.implementation, tt.endpoint); got != tt.want {
			t.Errorf("brS3Provider(%q, %q) = %q, want %q", tt.implementation, tt.endpoint, got, tt.want)
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
	// Each backup gets a folder under the Instance's path in the storage.
	if want := testBackupPath + "/nightly"; br.Spec.S3.Prefix != want || br.Spec.S3.Bucket != "bucket" {
		t.Errorf("BR backup writes to %s/%s, want bucket/%s", br.Spec.S3.Bucket, br.Spec.S3.Prefix, want)
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
	if want := testBackupPath + "/nightly"; brr.Spec.S3.Prefix != want {
		t.Errorf("BR restore reads %s, want %s", brr.Spec.S3.Prefix, want)
	}
}

// A backup whose path was never recorded can't be found in its storage.
func TestRestoreOfABackupWithoutAPathFails(t *testing.T) {
	src := testBackup(backupv1alpha1.BackupStateSucceeded)
	src.Status.Path = ""
	restore := &backupv1alpha1.Restore{
		ObjectMeta: metav1.ObjectMeta{Name: "clone", Namespace: testNamespace},
		Spec: backupv1alpha1.RestoreSpec{
			InstanceRef: commonv1alpha1.ObjectRef{Name: "orders"},
			DataSource: backupv1alpha1.DataSource{
				Type:   backupv1alpha1.DataSourceTypeBackup,
				Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
			},
		},
	}
	c, cl := brContext(t, src, restore)
	exec, err := New().SyncRestore(c, restore)
	if err != nil {
		t.Fatal(err)
	}
	if exec.State != backupv1alpha1.RestoreStateFailed || !strings.Contains(exec.Message, "BackupLocationUnknown") {
		t.Errorf("got %s %q, want Failed with BackupLocationUnknown", exec.State, exec.Message)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "clone"}, &tidbbrv1.Restore{}); err == nil {
		t.Error("BR restore created")
	}
}

// The runtime claims the class the chart ships; it names the s3 storage
// class without GCS, and no TLS option a BR job could not honour.
func TestShippedBackupClass(t *testing.T) {
	p := New()
	classes := p.ShippedBackupClasses()
	if len(classes) != 1 || classes[0].Name != backupClassName {
		t.Fatalf("shipped classes = %v, want %s", classes, backupClassName)
	}
	class := classes[0]
	if got := class.Spec.ExecutorProvider(); got != p.Name() {
		t.Errorf("executor = %q, want %q", got, p.Name())
	}
	entry := class.Spec.FindBackupStorageClass(s3class.ClassName)
	if entry == nil || entry.ParametersSchema == nil {
		t.Fatalf("s3 entry = %+v, want one with a parameters schema", entry)
	}
	enum := entry.ParametersSchema.OpenAPIV3Schema.Properties["implementation"].Enum
	if len(enum) == 0 || slices.ContainsFunc(enum, func(v apiextensionsv1.JSON) bool { return string(v.Raw) == `"gcs"` }) {
		t.Errorf("implementation enum = %v, want one without gcs", enum)
	}
	if got := backupclasses.TLSSupport(backupClassName, s3class.ClassName); got != (controller.TLSSupport{}) {
		t.Errorf("TLS support = %+v, want none", got)
	}
}
