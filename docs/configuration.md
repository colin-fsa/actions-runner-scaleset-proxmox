# Configuration

[config.example.yaml](../config.example.yaml) is the canonical reference —
every key is present there with inline documentation. This page explains the
shape of the file and the rules that aren't obvious from reading it.

The orchestrator loads `config.yaml` from the path given by `--config`
(default `config.yaml`).

## Top-level blocks

| Block | Purpose | Detail |
| --- | --- | --- |
| `github:` | Auth mode (PAT or App), scope, REST-reconciler timings | [getting-started.md](getting-started.md#3-set-up-github-credentials) |
| `scaleset:` | Name, labels, runner group, concurrency ceiling | below |
| `scalesets:` | Multi-scale-set alternative to `scaleset:` | [multi-scaleset.md](multi-scaleset.md) |
| `proxmox:` | Endpoint, API token, template VMID, VMID range, storage, network | [getting-started.md](getting-started.md#4-write-a-minimal-configyaml) |
| `nodes:` | Node-placement strategy and affinity rules | [node-placement.md](node-placement.md) |
| `pool:` | Default hot/warm sizing and lifecycle timings | [profiles-and-scaling.md](profiles-and-scaling.md) |
| `profiles:` | Named hardware/label/template bundles | [profiles-and-scaling.md](profiles-and-scaling.md) |
| `quotas:` / `priority:` | Per-org/repo caps and priority classes | [profiles-and-scaling.md](profiles-and-scaling.md#multi-tenancy-quotas-and-priority) |
| `observability:` | Metrics/health address, log level, OTLP tracing | [operations.md](operations.md) |
| `admin_api:` | Optional admin HTTP API | [operations.md](operations.md#admin-api) |
| `cluster:` | `standalone` or `raft` leader election | [deployment.md](deployment.md#high-availability-with-raft) |

## Environment variable overrides

Every key is overridable by an environment variable named with the YAML key
path uppercased, dots replaced by underscores, and prefixed with `SCALESET_`.
`snake_case` YAML keys are preserved as single tokens:

| YAML key | Environment variable |
| --- | --- |
| `github.pat.token` | `SCALESET_GITHUB_PAT_TOKEN` |
| `proxmox.auth.token_secret` | `SCALESET_PROXMOX_AUTH_TOKEN_SECRET` |
| `admin_api.shared_secret` | `SCALESET_ADMIN_API_SHARED_SECRET` |
| `pool.hot_size` | `SCALESET_POOL_HOT_SIZE` |

Note that `pool.hot_size` is one key — it is **not** nested as `hot.size`.

Unknown `SCALESET_*`-prefixed variables are logged at startup, so a typo like
`SCALESET_POOL_HOTSIZE` is loud rather than a silent no-op.

## Secrets

Secrets must come from the environment (or another secure config-loader
source). Do not write them into YAML. A typical PAT deployment needs:

- `SCALESET_GITHUB_PAT_TOKEN`
- `SCALESET_PROXMOX_AUTH_TOKEN_SECRET`
- `SCALESET_ADMIN_API_SHARED_SECRET` — required when `admin_api.http_addr` is
  set

## Validation

Config is validated fully before anything connects, so misconfiguration is a
startup error rather than a runtime surprise. Notable rules:

- `github.auth_mode` must be `app` or `pat`, and the matching block must be
  present.
- `github.scope` must set exactly one of `org` or `repo`.
- Every profile must advertise exactly its scale set's labels. Profiles in a
  scale set must use the same `guest_os`; separate Linux and Windows into
  different scale sets. See [Windows runners](windows-runners.md).
- `guest_type` is `qemu` (default) or `lxc`. An `lxc` profile must be Linux,
  must set its own `template_vmid`, and cannot have a `network:` block. It
  cannot share a scale set with `qemu` profiles under
  `pool.recycle_mode: snapshot_rollback`. A scale set made only of LXC
  profiles always uses destroy recycling. See [LXC runners](lxc-runners.md).
- `nodes.affinity` rules naming an undeclared profile or node are rejected.
- With more than one scale set declared, every entry must carry its own
  `vmid_range` and the ranges must be pairwise disjoint.
- Mixing the singular `scaleset:` form with the plural `scalesets:` form is
  rejected, with an error naming the offending legacy keys.

## GitHub Enterprise Server

Two mutually exclusive overrides point the orchestrator at a GHES instance.
Both live under `github.pat:` or `github.app:` depending on your auth mode:

- **`config_url`** — a full URL with the org/repo baked in. Use this for a
  single scale set. It is rejected in multi-scale-set deployments, since every
  scale set would be forced to handshake against the same scope.
- **`config_base_url`** — scheme and host only. The orchestrator joins it with
  each scale set's scope at runtime, so per-scale-set clients hit the right
  `/<org>` path on the shared host. This is the required form for
  multi-scale-set GHES.

For App auth, `rest_base_url` additionally overrides the base URL used for the
installation-token fetch and subsequent REST calls (typically
`https://ghes.example.com/api/v3/`).

Leave all of these empty when running against github.com — the default
per-scope URL derivation already targets the right endpoint.

### Shared host boot pacing

`pool.boots_per_node` limits concurrent boots per Proxmox node across all
profiles, operating systems, and scale sets in this controller. The default is
`0` (unlimited). `pool.boot_start_interval` is the minimum interval between
starts on the same node; its default is `0s` (unlimited). The old
`windows_boots_per_node` setting is accepted as an alias but now covers all
operating systems; do not set both names. A conservative initial setting is
`boots_per_node: 2` and `boot_start_interval: 15s`. The existing per-profile
runner limit, including a configured Windows ceiling of 25, remains separate.
Slots are held through guest readiness or failure; saturation defers pool
refill. The budget is local to one elected controller, not a lock between
independently deployed controllers.

With capacity enabled and `memory_mode: host_available`, start reservations
withhold the configured RAM (scaled by `memory_overcommit`) for at least 15
minutes after the start request, or `clone_inflight_grace` if longer. This is a
bounded settling assumption, not a measurement of individual guest RAM. It can
temporarily double count memory that has already appeared in host usage.
After each leader election, start pacing and host-memory admission pause for
the same window to let requests from the previous leader settle. This initial
quiet period is intentional even on a fresh controller: with pacing enabled,
new starts wait at least 15 minutes after election; host-available admission
also waits when capacity accounting is enabled. Queued work can remain queued
throughout this period and LXC JIT delivery can wait for the budget, increasing
listener response time. Plan the isolated canary around that delay rather than
interpreting it as a failed start. This policy assumes Proxmox start tasks and
memory ramp complete within that window; raise `clone_inflight_grace` if that
is not conservative enough. Keep host reserve for other workloads.

### Staged canary and rollback

In an isolated test environment with representative Linux and Windows profiles,
set the two controls to `2` and `15s`, enable host-available admission with the
normal reserve, and retain the existing Windows runner ceiling. First establish
baseline free RAM, start duration and queued-job latency. Then:

1. Submit a small mixed burst that requires cold clones and warm promotions on
   one node; verify no more than two boots are active and start timestamps are
   at least 15 seconds apart. Repeat across two nodes to check independent budgets.
2. During a controlled start, compare Proxmox host free-memory samples and
   capacity admission. A powered-on guest whose memory has not yet appeared in
   the host reading must still consume a reservation; further work waits.
3. With an isolated assigned or running test job, briefly interrupt the runner's
   GitHub connectivity, restore it within the grace, and verify the job survives.
   Separately confirm sustained offline status is cleaned up after grace, and
   repeat after a controller restart to check the timer begins again.

Watch `scaleset_pool_size`, `scaleset_acquire_duration_seconds`,
`scaleset_boot_duration_seconds`, `scaleset_startup_stage_duration_seconds`,
`scaleset_reconcile_errors_total`, host MemAvailable, Proxmox task timestamps,
and controller logs for deferred admission, reservation expiry and force
destroy. An offline runner still reported busy is deliberately not reaped;
watch for `runner offline but still reported busy` warnings and investigate
these ambiguous cases using authoritative job and runner evidence. Stop the
canary if memory pressure, failed jobs, excessive waiting,
or unexpected destroys appear. Roll back by restoring the previous controller
image and configuration together, then confirm the old leader has stopped and
inspect in-flight Proxmox tasks before resuming demand. A rollback removes these
protections; do not use it as a substitute for controlling incoming load.
