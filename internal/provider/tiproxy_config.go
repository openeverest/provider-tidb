package provider

import (
	"bytes"

	"github.com/BurntSushi/toml"
)

// withOptionalClientCerts makes TiProxy accept MySQL clients that present no
// certificate, like TiDB does. TiDB Operator always gives TiProxy's server TLS
// the CA (ca.crt) of its MySQL Secret, and TiProxy then requires a client
// certificate unless security.server-tls.skip-ca is set; with it, TiProxy still
// verifies the certificates clients do present. An explicit skip-ca in the
// user's config wins, and config that does not parse is returned unchanged for
// the operator to report.
func withOptionalClientCerts(cfg string) string {
	parsed := map[string]any{}
	if _, err := toml.Decode(cfg, &parsed); err != nil {
		return cfg
	}
	security, ok := subTable(parsed, "security")
	if !ok {
		return cfg
	}
	serverTLS, ok := subTable(security, "server-tls")
	if !ok {
		return cfg
	}
	if _, set := serverTLS["skip-ca"]; set {
		return cfg
	}
	serverTLS["skip-ca"] = true

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(parsed); err != nil {
		return cfg
	}
	return buf.String()
}

// subTable returns the table under key, adding an empty one if it is missing;
// ok is false when key holds something other than a table.
func subTable(parent map[string]any, key string) (map[string]any, bool) {
	value, found := parent[key]
	if !found {
		table := map[string]any{}
		parent[key] = table
		return table, true
	}
	table, ok := value.(map[string]any)
	return table, ok
}
