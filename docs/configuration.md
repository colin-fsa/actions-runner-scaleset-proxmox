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

With paced `host_available` admission enabled, `pool.boots_per_node` is
**submissions per rolling `pool.boot_start_interval` window**, per Proxmox
node across profiles, operating systems and scale sets. For example, `2` and
`15s` permit two cold clone POSTs in a rolling 15 seconds and, independently,
two cold power POSTs in a rolling 15 seconds. These are separate operation
clocks, not a single combined allowance. Clone/configuration task duration and
guest readiness never retain a slot. Ambiguous submissions consume their
window entry; refusal or cancellation before submission does not. This budget
is local to one elected controller, not multiple independent controllers.

Warm promotions retain their intentional cold-timer bypass. They share the
serialized POST boundary and fresh host pressure/fencing checks, not an
outstanding-boot semaphore. After one host-sized priority burst, warm requests
yield to a due cold request. `windows_boots_per_node` remains a compatibility
alias; do not set both names. Defaults are `0` and `0s`. Without both positive
pacing settings, the legacy concurrent/readiness slot behavior is unchanged.
Per-profile runner ceilings remain independent.

Paced host admission uses a fresh `/nodes/{node}/status` memory reading:
explicit `memory.available` when present, otherwise validated
`memory.total - memory.used`. It does not use guest allocations, swap usage,
or subtract pending/started claim RAM. The existing host reserve is a
**physical floor**, not another allocation: with `reserve_fraction: 0.15`,
starts stop at or below 15% available (85% used), unless a larger byte reserve
is configured. Configured guest size is not subtracted again. Measured host
CPU similarly replaces speculative launch CPU charges in host-usage mode.
This policy can overshoot between samples or as already-running jobs grow;
it is an admission threshold, not a hard resource cap or guaranteed reserve.

This endpoint arithmetic is explicit; it does not prove which GUI widget
uses the identical cache/ballooning definition. No future balloon reclaim is
assumed. If the API and GUI disagree, compare their contemporaneous raw
counters rather than describing pending reservations as actual host usage.
Ownership/task claims and exact-generation retirement evidence remain tracked
for diagnostics and recovery, but do not debit paced admission headroom.
Allocated and nonpaced host modes retain their historical accounting.

A waiting cold timer cannot block a warm promotion. After one host-sized priority burst,
warm requests explicitly yield to a registered cold request whose POST clock
is due, even if another warm caller acquires the lock first. A refused cold
admission consumes that turn too. A stopped cold worker stays registered and
retries outside the lock on a coalesced per-node cadence until its existing
deadline/cancellation,
so pressure cannot silently reclassify it as a warm contender. Cancellation
removes the waiter and parks an unsubmitted guest without a boot failure.
Pool boot admission fetches and rechecks Proxmox outside the POST mutex. Cold
workers share one node probe; warm workers have an independent coalesced probe
so one slow cold read cannot serialize warm requests behind it. Pressure
refusals use a one-second retry cadence; read failures back off for 1, 2, 4 and
at most 5 seconds per node/lane. Each probe is bounded to one second. Only a
prepared, ready cold request owns a due cold turn; I/O/backoff never does.
A prepared sample creates no charge. At the POST boundary, the accountant
rejects samples older than the smaller of the refresh interval and one second,
or older than a host observation that retired a charge, then atomically checks
actual host pressure and ownership. Cold coalesced callers remain bounded by
the submission window, not by readiness or speculative configured memory.
Cancellation returns promptly without aborting peers' bounded read. No stale
cache fallback authorizes a POST. A running LXC JIT restart still checks actual host
floors without buying a second guest allocation; a stopped LXC needs fresh
start admission. Defaults and configurations without positive pace and limit,
or with idle eviction enabled, keep their previous behavior.

Online runners remain immediately eligible for jobs. Controller deferrals and
GitHub's choice of which online runner receives a job are separate; this policy
does not enforce GitHub assignment order.

A submitted launch retains its ownership claim until Proxmox confirms task
success and two direct readings see the same running guest process with fresh
host readings, or until destruction is confirmed. Ambiguous claims do not
expire automatically. These proofs protect identity and restart recovery;
in paced mode they do not withhold RAM or occupy a concurrency slot. Missing
task recovery still requires positive visibility of the dedicated controller
token's task archive, not merely an empty task response.

On replacement, a paced standalone single-node deployment with a dedicated,
unchanged Proxmox API token and destroy-mode recycling validates the
controller's own active/archive task stream, exact adopted guests and fresh
host capacity before reopening admission. The previous instance must be
stopped, and other operators must not manage the controller's VMID ranges.
The same node status privilege used by live capacity allows the clock read;
own task visibility does not require datacenter-wide auditing. Missing or
ambiguous task evidence logs a reason and blocks new clones and power
starts, while existing listeners and tracked-job reconciliation continue.
A scale set waiting for an exclusive GitHub message session can contribute
read-only recovery observations. It cannot run workers or cleanup until it
owns that session. A missing scale set can be created after shared proof;
on a first guarded install, the same prior Proxmox proof requirements apply.
Pristine, already-powered Windows runners can accept jobs without a new
power operation. If the same inherited running process remains observable
but its readiness cannot be confirmed, it stays unavailable for new work.
Its exact process is present in the paired direct host measurements, so
a paced host does not also deduct its configured footprint or consume a
concurrent boot slot. No timer clears blocked restart proof. If a matching old clone first
appears after initial adoption, it is recorded without any power or delete
request and the complete proof runs again. An expected-name, in-range,
untagged guest can occupy an excluded ID after old tasks drain and two
direct fresh observations confirm it is stopped, unlocked and not HA
managed. An exact same-token clone log provides attribution when still
available; an older or missing log never confers ownership. The excluded
ID consumes no runner slot or host RAM reservation and requires operator
review. The controller never tags, starts, or deletes it. A new install or token
with no prior matching archive has no restart proof: establish a clean
handoff under operator control before enabling this guarded mode. Other
topologies with these gates do not automatically resume on this path.

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

After restart, a busy runner or ambiguous guest power or GitHub state is
adopted as protected, not returned to the warm or hot pool. The controller
logs `adopt: guest remains protected` and waits for a known power state and a
fresh, matching, nonbusy runner observation before resolving it. If GitHub
data stays missing or inconsistent, the guest can remain protected
indefinitely and requires operator investigation. Failed owned-guest discovery
is retried before orphan cleanup starts.
