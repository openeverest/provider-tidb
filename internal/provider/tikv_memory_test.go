package provider

import (
	"testing"

	"github.com/BurntSushi/toml"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	tidbcorev1 "github.com/pingcap/tidb-operator/api/v2/core/v1alpha1"

	"github.com/openeverest/provider-tidb/internal/common"
)

func memoryLimit(q string) corev1.ResourceList {
	return corev1.ResourceList{corev1.ResourceMemory: resource.MustParse(q)}
}

type tikvMemorySettings struct {
	MemoryUsageLimit string `toml:"memory-usage-limit"`
	Storage          struct {
		BlockCache struct {
			Capacity string `toml:"capacity"`
		} `toml:"block-cache"`
	} `toml:"storage"`
	RocksDB struct {
		WriteBufferLimit string `toml:"write-buffer-limit"`
	} `toml:"rocksdb"`
}

func TestTiKVConfigSizesSmallLimits(t *testing.T) {
	tests := []struct {
		limit                      string
		blockCache, buffers, usage string
	}{
		// 2 GiB is left after the runtime reserve.
		{limit: "4Gi", blockCache: "921MiB", buffers: "307MiB", usage: "3584MiB"},
		{limit: "6Gi", blockCache: "1843MiB", buffers: "614MiB", usage: "5120MiB"},
	}
	for _, tt := range tests {
		t.Run(tt.limit, func(t *testing.T) {
			var got tikvMemorySettings
			if _, err := toml.Decode(tikvConfig("", memoryLimit(tt.limit)), &got); err != nil {
				t.Fatal(err)
			}
			if got.Storage.BlockCache.Capacity != tt.blockCache || got.RocksDB.WriteBufferLimit != tt.buffers ||
				got.MemoryUsageLimit != tt.usage {
				t.Errorf("tikvConfig(%s) = %+v, want block cache %s, write buffers %s, memory usage limit %s",
					tt.limit, got, tt.blockCache, tt.buffers, tt.usage)
			}
		})
	}
}

func TestTiKVConfigKeepsTheUsersConfig(t *testing.T) {
	user := "[raftstore]\napply-pool-size = 2\n\n[storage]\nreserve-space = \"1GiB\"\n"
	var got struct {
		tikvMemorySettings
		Raftstore struct {
			ApplyPoolSize int `toml:"apply-pool-size"`
		} `toml:"raftstore"`
		Storage struct {
			ReserveSpace string `toml:"reserve-space"`
			BlockCache   struct {
				Capacity string `toml:"capacity"`
			} `toml:"block-cache"`
		} `toml:"storage"`
	}
	if _, err := toml.Decode(tikvConfig(user, memoryLimit("4Gi")), &got); err != nil {
		t.Fatal(err)
	}
	if got.Raftstore.ApplyPoolSize != 2 || got.Storage.ReserveSpace != "1GiB" {
		t.Errorf("the user's settings were lost: %+v", got)
	}
	if got.Storage.BlockCache.Capacity != "921MiB" || got.MemoryUsageLimit != "3584MiB" {
		t.Errorf("memory settings missing next to the user's config: %+v", got)
	}
}

func TestTiKVConfigLeavesTiKVDefaults(t *testing.T) {
	user := "[raftstore]\napply-pool-size = 2\n"
	tests := []struct {
		name   string
		config string
		limits corev1.ResourceList
	}{
		{name: "no memory limit", config: user},
		{name: "large limit", config: user, limits: memoryLimit("8Gi")},
		{name: "below the minimum", config: user, limits: memoryLimit("2Gi")},
		{name: "user sets the block cache", limits: memoryLimit("4Gi"),
			config: "[storage.block-cache]\ncapacity = \"1GiB\"\n"},
		{name: "user sets the memory usage limit", limits: memoryLimit("4Gi"),
			config: "memory-usage-limit = \"3GiB\"\n"},
		{name: "user sets the write buffer limit", limits: memoryLimit("4Gi"),
			config: "[rocksdb]\nwrite-buffer-limit = \"256MiB\"\n"},
		{name: "invalid config", limits: memoryLimit("4Gi"), config: "[storage\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tikvConfig(tt.config, tt.limits); got != tt.config {
				t.Errorf("tikvConfig = %q, want the user's config unchanged %q", got, tt.config)
			}
		})
	}
}

func TestValidateTiKVMemory(t *testing.T) {
	tests := []struct {
		name    string
		limit   string // empty: no limit
		current string // TiKVGroup memory; empty: no cluster yet
		wantErr string
	}{
		{name: "no limit"},
		{name: "at the minimum", limit: "4Gi"},
		{name: "new cluster below the minimum", limit: "2Gi",
			wantErr: "tikv memory limit 2Gi is below the minimum of 4Gi"},
		{name: "lowered below the minimum", limit: "3Gi", current: "8Gi", wantErr: "below the minimum of 4Gi"},
		{name: "existing cluster keeps a small limit", limit: "2Gi", current: "2Gi"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			in := testInstance("8.5.2")
			if tt.limit != "" {
				tikv := in.Spec.Components[common.ComponentTiKV]
				tikv.Resources = &corev1.ResourceRequirements{Limits: memoryLimit(tt.limit)}
				in.Spec.Components[common.ComponentTiKV] = tikv
			}
			objs := []client.Object{testProvider()}
			if tt.current != "" {
				group := &tidbcorev1.TiKVGroup{ObjectMeta: metav1.ObjectMeta{Name: "orders", Namespace: testNamespace}}
				mem := resource.MustParse(tt.current)
				group.Spec.Template.Spec.Resources.Memory = &mem
				objs = append(objs, group)
			}
			assertError(t, ValidateTiDB(validationContext(t, in, objs...)), tt.wantErr)
		})
	}
}
