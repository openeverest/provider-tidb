package provider

import (
	"context"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbbrv1 "github.com/pingcap/tidb-operator/api/v2/br/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func brContext(t *testing.T, objs ...client.Object) (*controller.Context, client.Client) {
	t.Helper()
	// The types the runtime registers, plus exactly those the provider declares:
	// a type the provider applies but forgot to register fails here as it would
	// in the cluster.
	scheme := runtime.NewScheme()
	for _, install := range append([]func(*runtime.Scheme) error{
		corev1alpha1.AddToScheme, backupv1alpha1.AddToScheme, corev1.AddToScheme,
	}, New().SchemeFuncs...) {
		if err := install(scheme); err != nil {
			t.Fatal(err)
		}
	}
	base := []client.Object{
		testCluster(),
		&backupv1alpha1.BackupStorage{
			ObjectMeta: metav1.ObjectMeta{Name: "s3", Namespace: testNamespace},
			Spec: backupv1alpha1.BackupStorageSpec{
				Type: "s3",
				S3: &backupv1alpha1.BackupStorageS3Spec{
					Bucket: "bucket", Region: "us-east-1", EndpointURL: "http://s3:9000",
					CredentialsSecretRef: commonv1alpha1.SecretRef{Name: "s3-creds"},
				},
			},
		},
		&corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "s3-creds", Namespace: testNamespace},
			Data:       map[string][]byte{"AWS_ACCESS_KEY_ID": []byte("id"), "AWS_SECRET_ACCESS_KEY": []byte("key")},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(base, objs...)...).Build()
	return controller.NewContext(context.Background(), cl, testInstance("8.5.2"), common.ProviderName), cl
}

func testBackup(state backupv1alpha1.BackupState) *backupv1alpha1.Backup {
	backup := &backupv1alpha1.Backup{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly", Namespace: testNamespace, UID: "backup-uid"},
		Spec: backupv1alpha1.BackupSpec{
			Origin:     backupv1alpha1.BackupOrigin{Type: "Instance", InstanceRef: &commonv1alpha1.ObjectRef{Name: "orders"}},
			ClassRef:   commonv1alpha1.ObjectRef{Name: "br"},
			StorageRef: commonv1alpha1.ObjectRef{Name: "s3"},
		},
	}
	backup.Status.State = state
	return backup
}

// assertBackupManagerAccount checks the account the BR jobs run as exists with
// just the permissions tidb-backup-manager uses.
func assertBackupManagerAccount(t *testing.T, cl client.Client) {
	t.Helper()
	key := client.ObjectKey{Namespace: testNamespace, Name: backupManagerName}
	if err := cl.Get(context.Background(), key, &corev1.ServiceAccount{}); err != nil {
		t.Errorf("service account: %v", err)
	}
	role := &rbacv1.Role{}
	if err := cl.Get(context.Background(), key, role); err != nil {
		t.Fatalf("role: %v", err)
	}
	var granted []string
	for _, rule := range role.Rules {
		for _, res := range rule.Resources {
			granted = append(granted, res+":"+strings.Join(rule.Verbs, ","))
		}
	}
	slices.Sort(granted)
	want := []string{
		"backups/status:get,list,watch,update", "backups:get,list,watch,update", "events:create,patch",
		"restores/status:get,list,watch,update", "restores:get,list,watch,update",
	}
	if !slices.Equal(granted, want) {
		t.Errorf("role grants %v, want %v", granted, want)
	}
	binding := &rbacv1.RoleBinding{}
	if err := cl.Get(context.Background(), key, binding); err != nil {
		t.Fatalf("role binding: %v", err)
	}
	if binding.RoleRef.Name != backupManagerName || len(binding.Subjects) != 1 ||
		binding.Subjects[0].Name != backupManagerName || binding.Subjects[0].Namespace != testNamespace {
		t.Errorf("role binding = %+v", binding)
	}
}

func TestSyncBackupRunsBRAsProviderAccount(t *testing.T) {
	backup := testBackup("")
	c, cl := brContext(t, backup)

	// A second pass must apply cleanly over the first.
	for range 2 {
		if _, err := New().SyncBackup(c, backup); err != nil {
			t.Fatalf("SyncBackup: %v", err)
		}
	}

	assertBackupManagerAccount(t, cl)
	br := &tidbbrv1.Backup{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "nightly"}, br); err != nil {
		t.Fatal(err)
	}
	if br.Spec.ServiceAccount != backupManagerName {
		t.Errorf("BR backup runs as %q, want %q", br.Spec.ServiceAccount, backupManagerName)
	}
}

func TestSyncRestoreRunsBRAsProviderAccount(t *testing.T) {
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
	c, cl := brContext(t, testBackup(backupv1alpha1.BackupStateSucceeded), restore)

	if _, err := New().SyncRestore(c, restore); err != nil {
		t.Fatalf("SyncRestore: %v", err)
	}

	assertBackupManagerAccount(t, cl)
	br := &tidbbrv1.Restore{}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "rollback"}, br); err != nil {
		t.Fatal(err)
	}
	if br.Spec.ServiceAccount != backupManagerName {
		t.Errorf("BR restore runs as %q, want %q", br.Spec.ServiceAccount, backupManagerName)
	}
}
