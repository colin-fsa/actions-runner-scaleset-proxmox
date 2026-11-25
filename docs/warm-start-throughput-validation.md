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
  Priority yields after one host-sized burst of attempts, including refusals.
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
facts. This work did not query production or run a production experiment.

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
A cold power refusal returns its stopped clone to Warm for a later promotion.
The baseline deliberately begins with both cold clones owning the old slots.

| Modeled event (seconds) | Base policy | Proposed policy |
| --- | --- | --- |
| Cold clone submissions | 0, 0 | 0, 15 |
| Four existing warm power submissions | 90, 105, 150, 165 | 1, 1, 19, 19 |
| Cold runner power submissions | 30, 45 | 37, 45 |
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
