# provider-tidb — Roadmap

OpenEverest provider for [TiDB](https://github.com/pingcap/tidb), built on **TiDB Operator v2** (pinned to **v2.0.1**).

This document defines the MVP and the phased plan to grow the provider beyond it. It is the
source of truth for scope; update it as milestones land.

---

## 1. Context & design constraints

- **Operator:** TiDB Operator **v2** (`main` line, tag `v2.0.1`). This is a control-plane redesign,
  not a new database — it manages standard TiDB releases (examples use TiDB LTS `v8.5.2`).
- **API module to import:** `github.com/pingcap/tidb-operator/api/v2` (a standalone, lightweight Go
  module — the provider depends on the CRD types only, not the whole operator).
- **Two API groups the provider touches:**
  - `core.pingcap.com/v1alpha1` — `Cluster` + per-component `*Group` CRDs (`PDGroup`, `TiKVGroup`,
    `TiDBGroup`, `TiFlashGroup`, `TiCDCGroup`, `TiProxyGroup`, …).
  - `br.pingcap.com/v1alpha1` — `Backup`, `Restore`, `BackupSchedule` (+ `CompactBackup`).
- **v2 model in one line:** a cluster is **not** one CR. It's a top-level `Cluster` (shared config,
  name ≤ 37 chars) plus one `*Group` CR per component, each carrying an immutable
  `spec.cluster.name` back-reference.
- **OpenEverest contract:** the provider implements `controller.ProviderInterface`
  (`Validate` / `Sync` / `Status` / `Cleanup`) from `openeverest/v2/provider-runtime`, translating a
  single `Instance` (`core.openeverest.io`) into the set of TiDB v2 CRs, and reporting phase +
  connection details back onto `Instance.status`. The core UI/API are schema-driven from the
  generated `Provider` CR — every UI-schema `path` must be reconciled by `Sync` (enforced by the
  conformance harness).

### Instance → TiDB v2 CR mapping (the heart of the provider)

| OpenEverest `Instance` | TiDB Operator v2 |
|---|---|
| the `Instance` itself | one `Cluster` (shared name injected into every group) |
| component `pd` | `PDGroup` (+ `data` volume, required) |
| component `tikv` | `TiKVGroup` (+ `data` volume, required) |
| component `tidb` | `TiDBGroup` (stateless, no volume) |
| component `tiflash` *(later)* | `TiFlashGroup` (+ `data` volume) |
| component `ticdc` *(later)* | `TiCDCGroup` |
| component `tiproxy` *(later)* | `TiProxyGroup` (own version line) |
| `component.replicas` | `*Group.spec.replicas` |
| `component.resources` (cpu/mem) | `template.spec.resources` (v2 sets requests==limits) |
| `component.storage` (size/class) | `template.spec.volumes[]` (`data` mount) |
| `component.version` / `Instance.spec.version` | `template.spec.version` (per group; doubles as image tag) |
| `component.parameters.config` | `template.spec.config` (inline TOML) |
| anything unmapped | `template.spec.overlay` (raw K8s patch escape hatch) |
| `Instance.spec.backup` *(later)* | `br.pingcap.com` `Backup` / `BackupSchedule` + `Restore` |

---

## 2. MVP

**Goal:** a user creates an `Instance` with `providerRef: tidb`, picks a version and per-component
size, and gets a healthy, connectable TiDB cluster — provisioned, updated, and deleted through
OpenEverest. Installable via Helm, testable in CI, runnable in the Tilt/k3d dev loop.

### MVP scope

**Components (minimal viable cluster):** `pd` + `tikv` + `tidb`.
- These are the mandatory trio for any working TiDB cluster. TiFlash/TiCDC/TiProxy are excluded.

**Topology:** a single `cluster` topology (standard distributed TiDB).
- Exposes per-component: `replicas`, `resources` (cpu/memory), `storage` (size + storageClass for
  pd/tikv), `version`, and raw `config` (TOML). PD/TiKV get a required `data` volume; TiDB is
  stateless.
- Sensible defaults (e.g. PD 3 / TiKV 3 / TiDB 2) and validation (odd PD count, min resources,
  required storage for stateful components).

**Version bundles:** curate 1–2 TiDB LTS lines (e.g. `v8.5.x`, and one older LTS) as version
bundles that pin pd/tikv/tidb to the same TiDB version. Exactly one `default: true`.

**Lifecycle (`ProviderInterface`):**
- `Validate` — spec sanity (name length ≤ 37 for the Cluster, resource minimums, replica rules).
- `Sync` — generate the shared `Cluster` name; create/patch `Cluster` + `PDGroup` + `TiKVGroup` +
  `TiDBGroup`; resolve version → image; apply with owner references.
- `Status` — aggregate the groups + `Cluster` conditions into an `Instance` phase; expose MySQL
  connection details (host = TiDB service, port 4000, credentials).
- `Cleanup` — delete the cluster resources (respecting `DeletionPolicy`).

**Watches:** `WatchOwned` the TiDB v2 CRs so group/cluster status changes re-trigger reconcile.

**Packaging & delivery:**
- Helm chart `charts/provider-tidb` that ships the provider Deployment + generated `Provider` CR,
  and bundles the **tidb-operator v2 chart** as a subchart dependency (or documents installing it
  alongside).
- `definition/` → `provider-spec.yaml` via `provider-sdk generate`; RBAC via controller-gen markers
  for `core.pingcap.com` (+ base OpenEverest groups).
- `cmd/provider/main.go` wired to `reconciler.New` + validation server.

**Quality gates:**
- Unit tests for the mapping (`Sync` builds correct CRs) and validation.
- Provider-runtime **conformance** test (every UI-schema path is reconciled).
- One integration suite (chainsaw): create Instance → cluster becomes Ready → connect → delete.
- Tilt/k3d dev loop (`dev/`) working against a local core + operator.

**Definition of Done (MVP):**
- [x] Repo scaffolded via `provider-sdk init` (module, chart, CI, dev loop).
- [x] `pd` / `tikv` / `tidb` components + `cluster` topology defined and generating a valid Provider CR.
- [x] `Validate` / `Sync` / `Status` / `Cleanup` implemented, mapping the Instance onto `Cluster` + PD/TiKV/TiDB groups.
- [x] Unit tests, and the provider-runtime conformance suite (UI paths reconciled + supported fields honoured) pass; `go build`, `go vet`, `golangci-lint`, and generated-file output are clean.
- [x] Chainsaw integration suite passes on a live k3d cluster: create Instance → provider produces schema-valid TiDB v2 CRs (`Cluster` + PD/TiKV/TiDB groups) → simulated group readiness → Instance reports `Ready` with a MySQL connection secret → delete cascades. (Operator readiness is simulated; no real TiDB pods.)
- [ ] Full end-to-end against the real TiDB Operator provisioning actual pods.
- [ ] Update paths (resize / scale / version bump) covered by tests.
- [ ] README compatibility + capability tables reviewed for the MVP surface.

### Explicitly OUT of MVP
- TiFlash, TiCDC, TiProxy, PD micro-services (TSO/Scheduling/Router/ResourceManager), DM.
- Backups / restore / PITR / scheduled backups.
- Monitoring integration (PMM/TiDB dashboard wiring).
- TLS between components, external access (LoadBalancer/NodePort), advanced scheduling.
- Presets, secrets/configmaps custom types, upgrade preflight hooks.
- Multiple topologies / heterogeneous groups (e.g. TP + AP TiDB groups).

---

## 3. Post-MVP phases

Ordered by value/effort; each phase is independently shippable.

### Phase 1 — Operational hardening
- **Upgrade orchestration:** respect v2 ordered rolling upgrade (PD → TiProxy → TiFlash → TiKV → TiDB);
  surface `Updating` phase; add `UpgradeProvider.CheckUpgrade` preflight hook (Helm pre-upgrade).
- **Maintenance/rolling-restart** signalling via the runtime's maintenance API.
- **Storage resize** and graceful scale-in (leader eviction relies on operator; validate our flow).
- **Richer status:** per-component status, conditions, more precise phase transitions.

### Phase 2 — Backups & restore
- **BackupClass** (`ProviderManaged`) backed by `br.pingcap.com`:
  - Snapshot `Backup` to S3 / GCS / Azblob (inlined `StorageProvider` + credential secret).
  - `Restore` from a backup; data-source seeding on new Instances.
  - `BackupSchedule` (cron) via the runtime's schedule/mirror plumbing.
- **PITR:** `backupMode: log` + `restoreMode: pitr`; report restorable-time window via
  `InstanceBackupStatusReporter`.

### Phase 3 — HTAP & connectivity components
- **TiFlash** component (`TiFlashGroup`) — columnar/HTAP, optional in the topology.
- **TiProxy** component (`TiProxyGroup`) — connection proxy / session migration (own version line).
- **TiCDC** component (`TiCDCGroup`) — change-data-capture.
- **External access:** map `component.service` → v2 `server`/expose for LoadBalancer/NodePort.

### Phase 4 — Observability & security
- **Monitoring:** wire a `MonitoringConfig` reference; expose metrics endpoints; optional dashboard.
- **TLS:** cluster-internal mTLS (`Cluster.spec.tlsCluster`), client TLS for TiDB (MySQL).
- **Security Enhanced Mode**, auth-token config surfacing where appropriate.

### Phase 5 — Advanced topologies & tuning
- **Heterogeneous groups** (e.g. separate TP and AP `TiDBGroup`s).
- **PD micro-service mode** (`ms`: TSO / Scheduling / Router / ResourceManager).
- **PlacementPolicy** integration; advanced scheduling (affinity, topology spread, tolerations).
- **Presets** (`InstancePreset`) for common sizings; custom **secrets/configmaps** types.

### Later / under evaluation
- **DM** (Data Migration) components (`DMGroup` / `DMWorkerGroup`).
- **TiKV next-gen** remote workers (`TiKVWorkerGroup`).
- Compact backups, cross-region, and other BR advanced modes.

---

## 4. Risks & watch-items
- **v2 API stability:** pin to the `api/v2` module at tag `v2.0.1`; bump deliberately. Do not track
  `main` HEAD.
- **Docs mismatch:** most public PingCAP/tidb-in-kubernetes material describes **v1**
  (`TidbCluster`, `TidbMonitor`, Pump). Ground all mapping in the v2 type files, not online docs.
- **Multi-CR lifecycle:** provisioning is N resources (Cluster + groups); ordering, the immutable
  `spec.cluster.name` back-reference, and partial-failure handling need care in `Sync`/`Cleanup`.
- **Conformance:** keep the UI schema and `Sync` in lockstep — an unreconciled UI path fails CI.
