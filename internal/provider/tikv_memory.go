package provider

import (
	"bytes"
	"fmt"

	"github.com/BurntSushi/toml"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	"github.com/openeverest/openeverest/v2/provider-runtime/controller"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

const mib = 1 << 20

var (
	// tikvRuntimeReserve is memory TiKV holds but does not budget for. At
	// startup it resolves a backtrace ahead of time for its panic hook, which
	// decompresses and caches the binary's debug info (about 1.8 GiB in the
	// official images) for the life of the process.
	tikvRuntimeReserve = resource.MustParse("2Gi")
	// tikvMinMemory is the smallest TiKV memory limit accepted for new and
	// changed Instances: below it TiKV is OOM-killed under light load.
	tikvMinMemory = resource.MustParse("4Gi")
	// tikvMemorySizingThreshold is the limit below which the provider sizes
	// TiKV's memory itself. From here up, TiKV's own defaults (block cache 45%
	// and memory-usage-limit 75% of the limit) leave room for the reserve, and
	// leaving them alone avoids restarting larger clusters.
	tikvMemorySizingThreshold = resource.MustParse("8Gi")
)

// tikvMemoryKeys are the TiKV settings the provider sizes. If the user's
// config sets any of them, the provider sets none, so they stay consistent.
var tikvMemoryKeys = [][]string{
	{"storage", "block-cache", "capacity"},
	{"rocksdb", "write-buffer-limit"},
	{"memory-usage-limit"},
}

// tikvConfig returns the TiKV config file for a group: the user's config,
// plus memory settings that fit a small memory limit. TiKV sizes its caches
// from the whole limit without leaving room for tikvRuntimeReserve, so on a
// small limit it is OOM-killed under light load. The caches are sized from
// what remains after the reserve instead, with TiKV's own ratios:
//
//   - storage.block-cache.capacity: 45% of the remainder
//   - rocksdb.write-buffer-limit: 15% of the remainder, a cap on all memtables
//   - memory-usage-limit: the reserve plus 75% of the remainder
func tikvConfig(userConfig string, limits corev1.ResourceList) string {
	limit, ok := limits[corev1.ResourceMemory]
	if !ok || limit.Cmp(tikvMinMemory) < 0 || limit.Cmp(tikvMemorySizingThreshold) >= 0 {
		return userConfig
	}

	cfg := map[string]any{}
	if _, err := toml.Decode(userConfig, &cfg); err != nil {
		// TiKV reports an invalid config itself; don't hide it behind ours.
		return userConfig
	}
	for _, key := range tikvMemoryKeys {
		if hasKey(cfg, key) {
			return userConfig
		}
	}

	remainder := (limit.Value() - tikvRuntimeReserve.Value()) / mib
	setKey(cfg, tikvMemoryKeys[0], fmt.Sprintf("%dMiB", remainder*45/100))
	setKey(cfg, tikvMemoryKeys[1], fmt.Sprintf("%dMiB", remainder*15/100))
	setKey(cfg, tikvMemoryKeys[2], fmt.Sprintf("%dMiB", tikvRuntimeReserve.Value()/mib+remainder*75/100))

	var buf bytes.Buffer
	enc := toml.NewEncoder(&buf)
	enc.Indent = ""
	if err := enc.Encode(cfg); err != nil {
		return userConfig
	}
	return buf.String()
}

func hasKey(cfg map[string]any, key []string) bool {
	for i, k := range key {
		v, ok := cfg[k]
		if !ok {
			return false
		}
		if i == len(key)-1 {
			return true
		}
		if cfg, ok = v.(map[string]any); !ok {
			return false
		}
	}
	return false
}

func setKey(cfg map[string]any, key []string, value string) {
	for _, k := range key[:len(key)-1] {
		next, ok := cfg[k].(map[string]any)
		if !ok {
			next = map[string]any{}
			cfg[k] = next
		}
		cfg = next
	}
	cfg[key[len(key)-1]] = value
}

// validateTiKVMemory rejects a TiKV memory limit below tikvMinMemory. An
// Instance that already runs with a smaller limit keeps it, so it can still be
// edited.
func validateTiKVMemory(c *controller.Context) error {
	comp := c.Instance().Spec.Components[common.ComponentTiKV]
	if comp.Resources == nil {
		return nil
	}
	limit, ok := comp.Resources.Limits[corev1.ResourceMemory]
	if !ok || limit.Cmp(tikvMinMemory) >= 0 {
		return nil
	}

	tikv := &tidbcorev1.TiKVGroup{}
	found, err := c.Exists(tikv, c.Name())
	if err != nil {
		return fmt.Errorf("reading current tikv resources: %w", err)
	}
	if found {
		if current := tikv.Spec.Template.Spec.Resources.Memory; current != nil && current.Cmp(limit) == 0 {
			return nil
		}
	}
	return fmt.Errorf("tikv memory limit %s is below the minimum of %s: TiKV holds about %s for its own runtime "+
		"and is OOM-killed under light load with less", limit.String(), tikvMinMemory.String(), tikvRuntimeReserve.String())
}
