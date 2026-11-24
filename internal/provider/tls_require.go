package provider

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/go-sql-driver/mysql"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/definition"
	"github.com/openeverest/provider-tidb/internal/common"
)

// requireSecureTransportAnnotation records on the Cluster that the provider
// turned on TiDB's require_secure_transport, so preferTLS turns it off only
// then and not when an administrator turned it on.
const requireSecureTransportAnnotation = "tidb.openeverest.io/require-secure-transport"

const sqlTimeout = 10 * time.Second

// tidbEndpoint is how the provider reaches TiDB's MySQL port over TLS.
type tidbEndpoint struct {
	addr       string
	serverName string
	password   string
	ca         []byte
}

// setRequireSecureTransport sets the global require_secure_transport, which
// TiDB keeps in the cluster and applies to every TiDB, also behind TiProxy.
// TiDB only takes it over a TLS connection. TiDB 8.5 ignores the
// security.require-secure-transport config item, and the bootstrap SQL runs
// without TLS, so it can only be set like this. A variable so tests can
// replace the connection.
var setRequireSecureTransport = func(ctx context.Context, ep tidbEndpoint, on bool) error {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ep.ca) {
		return errors.New("no CA certificate (ca.crt) to verify TiDB with")
	}
	cfg := mysql.NewConfig()
	cfg.User = rootUser
	cfg.Passwd = ep.password
	cfg.Net = "tcp"
	cfg.Addr = ep.addr
	cfg.TLS = &tls.Config{RootCAs: pool, ServerName: ep.serverName, MinVersion: tls.VersionTLS12}
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = sqlTimeout, sqlTimeout, sqlTimeout
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return err
	}
	db := sql.OpenDB(connector)
	defer func() { _ = db.Close() }()
	value := "OFF"
	if on {
		value = "ON"
	}
	_, err = db.ExecContext(ctx, "SET GLOBAL require_secure_transport = "+value)
	return err
}

// syncRequireSecureTransport turns require_secure_transport on for requireTLS
// and back off for preferTLS once TiDB serves TLS on every pod.
func syncRequireSecureTransport(c *controller.Context, cluster *tidbcorev1.Cluster) error {
	want := tlsParameters(c).Mode == definition.TLSModeRequire
	set := cluster.Annotations[requireSecureTransportAnnotation] == strconv.FormatBool(true)
	if want == set {
		return nil
	}
	tidb := &tidbcorev1.TiDBGroup{}
	found, err := c.Exists(tidb, c.Name())
	if err != nil {
		return fmt.Errorf("get tidb group: %w", err)
	}
	// The TiDB group watch brings Sync back once TiDB has rolled out.
	if !found || !tidbServesTLS(tidb) {
		return nil
	}
	ca, err := readClientCA(c)
	if err != nil {
		return fmt.Errorf("read the CA of TiDB's certificate: %w", err)
	}
	host := fmt.Sprintf("%s-%s.%s.svc", c.Name(), common.ComponentTiDB, c.Namespace())
	ep := tidbEndpoint{
		addr:       net.JoinHostPort(host, strconv.Itoa(tidbcorev1.DefaultTiDBPortClient)),
		serverName: host,
		password:   readRootPassword(c),
		ca:         ca,
	}
	ctx, cancel := context.WithTimeout(c.Context(), 2*sqlTimeout)
	defer cancel()
	if err := setRequireSecureTransport(ctx, ep, want); err != nil {
		return controller.WaitForDuration(fmt.Sprintf("set require_secure_transport %s in TiDB: %v", onOff(want), err), 30*time.Second)
	}
	patch := client.MergeFrom(cluster.DeepCopy())
	if want {
		if cluster.Annotations == nil {
			cluster.Annotations = map[string]string{}
		}
		cluster.Annotations[requireSecureTransportAnnotation] = strconv.FormatBool(true)
	} else {
		delete(cluster.Annotations, requireSecureTransportAnnotation)
	}
	if err := c.Client().Patch(c.Context(), cluster, patch); err != nil {
		return fmt.Errorf("record require_secure_transport on cluster: %w", err)
	}
	return nil
}

// tidbServesTLS reports whether every TiDB pod is ready with the MySQL TLS the
// group asks for, so a connection through the Service always gets TLS.
func tidbServesTLS(group *tidbcorev1.TiDBGroup) bool {
	sec := group.Spec.Template.Spec.Security
	if sec == nil || sec.TLS == nil || sec.TLS.MySQL == nil || !sec.TLS.MySQL.Enabled {
		return false
	}
	st := group.Status
	return group.Generation == st.ObservedGeneration && st.CurrentRevision == st.UpdateRevision &&
		groupIsReady(group.Spec.Replicas, st.ReadyReplicas) &&
		st.UpdatedReplicas == *group.Spec.Replicas && st.Replicas == *group.Spec.Replicas
}
