package provider

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	backupv1alpha1 "github.com/openeverest/openeverest/v2/api/backup/v1alpha1"
	commonv1alpha1 "github.com/openeverest/openeverest/v2/api/common/v1alpha1"
	corev1alpha1 "github.com/openeverest/openeverest/v2/api/core/v1alpha1"
	"github.com/openeverest/openeverest/v2/pkg/common"
	"github.com/openeverest/openeverest/v2/provider-runtime/controller"
)

func userRootSecretInstance() *corev1alpha1.Instance {
	in := testInstance("8.5.2")
	in.Spec.UserSecretRef = &commonv1alpha1.SecretRef{Name: "my-root"}
	return in
}

func TestEnsureRootPasswordTakesUserSecretOnce(t *testing.T) {
	c, cl := tiproxyContext(t, userRootSecretInstance(), nil, rootSecret("my-root", `it's a \secret`))

	pw, err := ensureRootPassword(c)
	if err != nil {
		t.Fatal(err)
	}
	if pw != `it's a \secret` {
		t.Fatalf("root password = %q, want the user's", pw)
	}
	if stored, _ := secretPassword(t, cl, rootSecretName("orders")); stored != pw {
		t.Fatalf("stored root password = %q, want %q", stored, pw)
	}
	sql := buildBootstrapSQLConfigMap(c, pw).Data[bootstrapSQLConfigKey]
	if want := `SET PASSWORD FOR 'root'@'%' = 'it\'s a \\secret';` + "\n"; sql != want {
		t.Errorf("bootstrap SQL = %q, want %q", sql, want)
	}

	// The cluster bootstrapped with the first password: a later edit of the
	// user's Secret is not applied.
	if err := cl.Update(context.Background(), rootSecret("my-root", "changed")); err != nil {
		t.Fatal(err)
	}
	if pw, err := ensureRootPassword(c); err != nil || pw != `it's a \secret` {
		t.Errorf("after editing the user Secret: password %q, err %v; want the original", pw, err)
	}
}

func TestEnsureRootPasswordWaitsForUserSecret(t *testing.T) {
	c, cl := tiproxyContext(t, userRootSecretInstance(), nil)

	_, err := ensureRootPassword(c)
	if !controller.IsWaitError(err) {
		t.Fatalf("err = %v, want a wait for the Secret", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: rootSecretName("orders")}, &corev1.Secret{}); err == nil {
		t.Error("a root password Secret was created without the user's password")
	}
}

func TestValidateUserSecretRef(t *testing.T) {
	withRef := userRootSecretInstance()
	withoutRef := testInstance("8.5.2")
	withDataSource := userRootSecretInstance()
	withDataSource.Spec.DataSource = &backupv1alpha1.DataSource{
		Type:   backupv1alpha1.DataSourceTypeBackup,
		Backup: &backupv1alpha1.DataSourceBackup{BackupRef: commonv1alpha1.ObjectRef{Name: "nightly"}},
	}

	for _, tc := range []struct {
		name     string
		in, old  *corev1alpha1.Instance
		wantErrs string
	}{
		{"set on create", withRef, nil, ""},
		{"kept on update", withRef, withRef, ""},
		{"never set", withoutRef, withoutRef, ""},
		{"added on update", withRef, withoutRef, "only be set when the Instance is created"},
		{"with a data source", withDataSource, nil, "must not be set with spec.dataSource"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			errs := New().Validate(controller.ValidationInput{Instance: tc.in, Old: tc.old, Provider: testProvider()}).Errors
			if tc.wantErrs == "" {
				if len(errs) != 0 {
					t.Fatalf("errors = %v, want none", errs)
				}
				return
			}
			if len(errs) != 1 || errs[0].Field != "spec.userSecretRef" || !strings.Contains(errs[0].Detail, tc.wantErrs) {
				t.Fatalf("errors = %v, want spec.userSecretRef: %s", errs, tc.wantErrs)
			}
		})
	}
}

func TestValidateRootSecret(t *testing.T) {
	labelled := func(s *corev1.Secret, typ corev1.SecretType) *corev1.Secret {
		s.Labels = map[string]string{common.OpenEverestDefinitionLabel: rootSecretDefinition}
		s.Type = typ
		return s
	}
	provider := testProvider()
	provider.Spec.Secrets = map[string]corev1alpha1.SecretDefinition{rootSecretDefinition: {Type: corev1.SecretTypeOpaque}}

	for _, tc := range []struct {
		name    string
		objs    []client.Object
		wantErr string
	}{
		{"missing", nil, "not found"},
		{"no password", []client.Object{rootSecret("my-root", "")}, `key "password" is missing or empty`},
		{"control character", []client.Object{rootSecret("my-root", "a\nb")}, "control characters"},
		{"valid", []client.Object{rootSecret("my-root", "s3cret'\\")}, ""},
		{"fits the root definition", []client.Object{labelled(rootSecret("my-root", "s3cret"), corev1.SecretTypeOpaque)}, ""},
		{"does not fit the root definition", []client.Object{labelled(rootSecret("my-root", "s3cret"), corev1.SecretTypeBasicAuth)}, "does not match definition"},
		// Once the cluster has its password the user's Secret is not read again.
		{"already applied", []client.Object{rootSecret(rootSecretName("orders"), "taken")}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := tiproxyContext(t, userRootSecretInstance(), nil, append(tc.objs, provider)...)
			err := validateRootSecret(c)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("err = %v, want none", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateReferencesReportsMissingRootSecret(t *testing.T) {
	c, _ := tiproxyContext(t, userRootSecretInstance(), nil)

	errs, err := New().ValidateReferences(c)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range errs {
		if e.Field == "spec.userSecretRef.name" && strings.Contains(e.Detail, `"my-root" not found`) {
			return
		}
	}
	t.Fatalf("errors = %v, want the missing Secret at spec.userSecretRef.name", errs)
}
