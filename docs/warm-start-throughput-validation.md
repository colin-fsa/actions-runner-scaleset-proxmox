# Warm runner startup throughput validation

Base: `integration/windows-host-memory` at
`e986ff266b6cb9138529db342435f02e04a21b82`.
Work is restricted to the isolated `fix/warm-runner-start-throughput-20261125`
branch in the Colin fork. Fork PR #6 (`fix/warm-promotion-admission`,
`bec4f6d8aee4b1770e275896524b7a29156bc504`) remains open; its basic warm
admission and stopped-clone accounting changes are already present in this
integration base. This change extends that implementation; it does not
cherry-pick or replace #6, and does not modify its branch.

## Behavior

With the existing positive host start limit, positive start interval,
`host_available` admission and idle eviction disabled:

- New clone POSTs use a separate per-host clock at `boot_start_interval`.
  Both QEMU and LXC gate the actual POST; task waiting happens outside it.
  LXC retries pass through the same gate.
- Cold power POSTs and LXC JIT power operations retain their existing clock.
  An ambiguous POST response still advances its clock.
- Already-provisioned warm promotions bypass that clock and get priority over
  waiting cold submissions. A cold timer sleeps outside the submission lock.
  Priority explicitly yields after one host-sized burst of attempts, including
  refusals, to a registered cold waiter whose POST clock is due. A warm caller
  acquiring the mutex first cannot bypass this turn. Cold refusals also consume
  their turn; canceled waiters are removed. Future cold timers never block warm.
- Clone/configuration tasks have a separate per-host semaphore using the same
  positive `boots_per_node` limit, shared across all managers/scale sets and
  released before readiness. Task waits do not hold the submission lock.
- Cloning and readiness no longer consume host start slots in this mode.
  The existing shared accountant bounds outstanding starts and atomically
  charges fresh memory/CPU before the power POST. Per-manager worker limits,
  profile limits and all actual capacity floors remain in force.
- The existing task/process/host evidence retires a charge independently of
  readiness. An ambiguous submitted start cannot simply expire; exact cleanup
  or verified recovery remains required. Running JIT restarts still check host
  floors without charging a second new allocation. No new accounting policy.
- Submission rechecks the recovery fence and exact worker/generation identity.
  Cold clones also recheck their provisioning generation after pacing.
- Defaults and legacy/unpaced/idle-eviction paths retain their slot policy.
  No config-only zero workaround, new knob or change to recovery eligibility.

Online runners remain eligible immediately. GitHub runner assignment order
is outside this controller's scheduling policy.

## Observed local checks versus supplied deployment facts

The shared two-slot limit and 15-second power spacing were supplied as live
facts. No production workload experiment or deployment was performed. The
follow-up made only read-only infrastructure observations (see below).

Local tests exercise the real Go dispatch gate and accountant with fake HTTP
servers. Thirty-two simultaneous warm admission requests admit exactly two
with abundant RAM, or one with only one shape above the reserve. The blocked
clone scenario demonstrates a warm promotion reaching readiness while cloning
is still blocked and the legacy readiness slot is occupied. QEMU/LXC tests
verify that a rejected clone gate sends no POST. Priority, independent spacing,
leader fence, cancellation and replaced-generation cleanup are covered.

One scaled local gate replay used an 80 ms cold interval: two warm POST
callbacks completed in 21.292 microseconds after the first cold callback;
the next cold callback ran at 80.341 ms. This is a local callback timing,
not a VM startup or GitHub queue measurement; host scheduling changes timings.
Repeated tests assert ordering and minimum spacing, not this exact duration.

## Deterministic queue model

Run `python3 scripts/replay/warm_start_queue.py`.
This is a discrete-time model of the scheduling policy, separate from the Go
execution checks above. It assumes two cold arrivals at t=0 and four existing
warm arrivals at t=1, 30-second clone/configuration work, 60-second readiness,
18 seconds to reliable accounting, two outstanding starts, and 15-second
spacing. It assumes sufficient actual memory/CPU and immediate reconcile ticks.
A cold power refusal retains its stopped worker in the cold queue until admission,
cancellation or the existing deadline.
The baseline deliberately begins with both cold clones owning the old slots.

| Modeled event (seconds) | Base policy | Proposed policy |
| --- | --- | --- |
| Cold clone submissions | 0, 0 | 0, 15 |
| Four existing warm power submissions | 90, 105, 150, 165 | 1, 1, 19, 19 |
| Cold runner power submissions | 30, 45 | 37, 52 |
| Maximum occupied start slots/charges | 2 | 2 |

This illustrates the tradeoff: warm service advances and cold work still
progresses, while new clone submissions retain a floor. These are assumed
scenario results, not measured production latency, a throughput guarantee,
or a prediction of GitHub pickup order. Actual accounting, API latency and
reconcile cadence determine the next admitted burst.

## Proposed post-deploy comparison (not performed or authorized here)

After separately approved deployment, compare equivalent demand windows at
pinned controller/config SHAs, with matched starting warm/hot populations,
profile mix, host reserve and available RAM/CPU. Correlate by node, exact VM
generation/name, runner ID and task UPID:

1. Record demand time, clone POST/return, power POST/return, successful task,
   both process/host observations, admission/deferral reason, charge retirement,
   guest readiness, runner online and GitHub job start.
2. Compare warm demand-to-power and demand-to-online p50/p95, cold completion
   latency, maximum outstanding charges, observed clone POST gaps and overlap.
3. Verify no power below actual capacity floors, no charge released from stale
   observations/reused IDs, no growth in unresolved failed starts, and eventual
   cold progress. Examine leader transitions separately with the same proof gate.
4. Report controller deferrals separately from online-to-job latency and GitHub
   pickup order. Do not attribute out-of-order job assignment to this fix.

No production changes, rollout, upstream push or merge were made.

## Validation results

Toolchain: existing local Go 1.26.3 with race detector (macOS).

- `go vet ./...`, formatting and `git diff --check`: pass.
- New pool priority, clone overlap, cancellation/fencing and generation tests:
  pass with `-race -count=10`.
- New accountant 32-request burst tests: pass with `-race -count=10`.
- Real QEMU/LXC clone submission seam tests: pass with `-race`.
- Targeted `go test -race ./internal/pool ./internal/nodecap
  ./internal/provisioner -skip
  'TestPromote_StartFailureReleasesClaim|TestProfiles_AdoptRoutesByProfileTag'`:
  pass. The exclusions are existing failures, not passing checks.
- Full `go test -race ./...`: fails only
  `TestPromote_StartFailureReleasesClaim` and
  `TestProfiles_AdoptRoutesByProfileTag`. Both reproduce on an untouched
  archive of the exact base SHA with the same toolchain. The former expects
  immediate release after a failed legacy start, whereas the base preserves
  ambiguous issued starts; the latter expects Warm from an inherited stopped
  guest where the base protects ambiguous adoption. Their expected results
  were left unchanged in this focused fix.
- Full `go test -race -tags=e2e ./test/e2e/...`: fails the two subtests of
  `TestE2E_CapacityAdmission_HostAvailablePlansAgainstFreeMemory` with
  orchestrator-readiness timeouts. Both reproduce on the exact untouched base.
  Their fixture does not meet the base's scoped restart-recovery requirements.
- `go test -race -tags=e2e ./test/e2e -skip
  TestE2E_CapacityAdmission_HostAvailablePlansAgainstFreeMemory`: passes
  (69.852 seconds); the excluded scenario is the reproduced base failure.
- The deterministic Python model passes its assertions.

No CI run was triggered. The existing workflow targets `main`, so a PR to
this integration branch does not automatically supply normal PR CI.
The named Colin fork was verified as **public** via GitHub. Colin explicitly
authorized pushing this branch and opening the draft PR there; upstream
publication remains prohibited.
No upstream push, deployment, rollout or merge is authorized by this change.


## Independent review follow-up

The review of `1b8d5b68` identified that the old `warmBurst` condition only made
cold requests eligible; it did not stop another warm caller taking the lock.
The follow-up adds the explicit due-cold turn and a separate shared clone-task
bound. It also keeps a refused, already-provisioned cold worker registered in
that queue, retrying fresh admission outside the lock on a bounded/coalesced node cadence until its
existing deadline/cancellation. Reclassifying it as Warm on every refusal lost
its cold turn and repeated mixed runs reproduced cross-profile starvation.
No power/start charge or clone-task slot is held while waiting; its ordinary
per-manager worker slot remains bounded. Cancellation before submission parks
the stopped guest Warm without counting a boot failure. Warm-promotion refusals
still return Warm immediately. Recovery fencing and identity changes still end
the wait immediately. It keeps warm priority, existing accounting, the cold
clocks and defaults.

`TestMixedBacklogBoundsPressureAndMakesColdProgress` integrates two real pool
managers (Linux and Windows), the shared gate and the real node accountant.
It submits four cold clones with overlapping task barriers and continually adds
warm runners while all readiness probes remain blocked. It uses real successful
receipts, exact running-process identity and repeated fresh host observations;
it does not release claims by calling `Release` or manufacturing a ready guest.
The existing five-second probe throttle remains intact. It proves:

- Clone/configure tasks stay at two across both scale sets, independently of
  readiness; actual clone POST gaps respect the configured clock.
- Warm promotion progresses while two clone tasks overlap and the original cold
  Linux/Windows guests remain powered on in readiness after accounting settles.
- Both additional cold clones eventually power on while new warm demand continues.
- Outstanding start charges stay at two; every running guest is charged its full
  shape in fake actual host RAM. Tightening fresh host availability to exactly
  the reserve prevents any further power POST despite the remaining warm backlog.

`TestExhaustedWarmBurstGivesDueColdAnExplicitTurn` holds the shared POST mutex
until a real cold waiter's timer is due, queues 16 new warm contenders, and
requires the cold callback first. Removing the explicit yield makes that test
fail (`expected cold`, `actual warm`). Cancellation, per-node isolation,
idempotent clone-slot release and clone failure release are covered separately.

These tests control arrivals and task/readiness barriers; goroutine scheduling
can change which equally eligible warm worker wins a charge. They establish the
stated bounds and eventual progress, not exact production latency or future
job-memory reservations. The older Python model remains illustrative and does
not model every semaphore, readiness-worker or scheduling interleaving here.

### Live GitOps build-ref finding

The independent reviewer verified CT105's active compose path builds this fork's
moving `integration/windows-host-memory` ref on rebuild. This supersedes the
previous incomplete MCP-only observation. It is reviewer-supplied verification;
this coding session did not independently re-read that compose path. A merge
changes what the next rebuild can build. No merge, rebuild, deployment, ref or
polling change was performed or authorized here.

Final exact-head checks and same-command results on the untouched base archive
are reported in PR #19. Inherited failures remain explicitly separated from the
new mixed-backlog and fairness checks.

The follow-up mixed/fairness/cancellation/failure group passed three consecutive
race runs (56.348 seconds aggregate). Mixed runs sustained 98–130 warm arrivals,
powered 8–10 guests while every readiness worker stayed blocked, and observed
maximums of two clone tasks, two outstanding starts and 56–76 GiB of full guest
shapes on the modeled 128 GiB host. Both queued cold runners progressed. Raising
foreign host pressure to leave exactly the 4 GiB reserve blocked further warm
POSTs. Removing the clone-task bound makes the integrated test fail with four
concurrent clones instead of two; removing the explicit cold turn makes the
lock-contention test fail with warm before cold. Those are local controlled
checks, not production workload measurements.


## Probe-pressure review follow-up

The follow-up to `99cecf1b` replaces the per-worker 100 ms retry and admission
GETs under the common POST lock. Pool boot admission now uses a direct,
bounded prepared sample outside that mutex, followed by a pure atomic ledger
check and reservation at the POST boundary. Prepared samples expire within one
second (or the configured shorter refresh), and an intervening authoritative
host observation that retired a charge invalidates older samples. Neither
preparation nor cooldown buys or retains a start/memory charge.

Cold workers share one in-flight read per node across scale sets. Warm workers
have an independent coalesced lane, so a slow cold GET cannot own the common
POST mutex or make warm requests wait for that cold read. Only a prepared cold
worker is eligible for its fair turn. Slot/pressure refusals have a one-second
node/lane retry cadence; direct read failures use 1/2/4/5-second backoff. Slot
saturation is reported separately from read failures and does not increment
the outage backoff. A newer denial cooldown cannot be erased by an older read
finishing later. Shared reads have a one-second timeout independent of caller
cancellation; canceling one worker promptly removes its queue entry without
canceling the probe needed by its peers.

Regression tests use actual pool workers and HTTP accountant calls. Twenty-four
pressured cold workers produced four cluster reads / eleven total PVE GETs over
about 2.2 seconds while a smaller warm runner successfully started. Twenty-four
workers receiving HTTP read errors produced two GETs in the same window. A
controlled cold GET stays held while an independent fresh warm read and POST
complete. Additional tests cover full unsettled slots and subsequent exact
cleanup, atomic admission from a shared sample, expiry/superseded evidence,
identity replacement during I/O, probe-owner cancellation, and late read versus
newer cooldown races. These are controlled local results, not production API
load measurements. Legacy/JIT accounting and recovery eligibility are unchanged.

The separate supplied destroy/storage observation is captured with an
ownership-safe diagnosis outline in `docs/teardown-vmid-reuse-followup.md`.
No teardown behavior was changed.
