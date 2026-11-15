#!/usr/bin/env bash
# lxc-runner-bootstrap.sh - one-shot GitHub Actions runner launcher for
# LXC containers managed by actions-runner-scaleset-proxmox.
#
# Install inside the container template as /usr/local/sbin/lxc-runner-bootstrap
# (mode 0755, owned by root) and enable lxc-runner-bootstrap.service.
#
# Contract with the orchestrator (guest_type: lxc):
#   * A job is handed over by setting the container's Proxmox `env` option to
#     ACTIONS_RUNNER_JIT_CONFIG=<encoded JIT config> and restarting the
#     container. PVE passes it to the container's init process, so it shows up
#     in /proc/1/environ (readable by root only).
#   * Without that variable the container is an idle hot/warm runner: exit 0
#     and wait for the next restart.
#   * With it: run the runner exactly once as the unprivileged runner user, then
#     power the container off whatever the outcome. The orchestrator's power
#     poller treats "stopped" as job completion and destroys the container -
#     the same contract as gh-runner.service on the QEMU Linux template.
#
# The JIT config is a credential. This script never prints it, never enables
# xtrace, and passes it only on the runner's command line (as the QEMU Linux
# path does) with a minimal environment that does not contain it.
#
# Optional overrides in /etc/default/lxc-runner-bootstrap:
#   RUNNER_DIR   runner installation directory (default /opt/actions-runner)
#   RUNNER_USER  account the runner runs as     (default runner)
set -euo pipefail
set +x

RUNNER_DIR=/opt/actions-runner
RUNNER_USER=runner
if [[ -r /etc/default/lxc-runner-bootstrap ]]; then
    # shellcheck disable=SC1091
    . /etc/default/lxc-runner-bootstrap
fi

log() { printf 'lxc-runner-bootstrap: %s\n' "$*" >&2; }

if [[ $(id -u) -ne 0 ]]; then
    log "must run as root (reads /proc/1/environ)"
    exit 1
fi

JIT=""
while IFS= read -r -d '' kv; do
    case "$kv" in
        ACTIONS_RUNNER_JIT_CONFIG=*) JIT=${kv#ACTIONS_RUNNER_JIT_CONFIG=} ;;
    esac
done </proc/1/environ

if [[ -z "$JIT" ]]; then
    log "no JIT config in the container environment; idling until the orchestrator assigns a job"
    exit 0
fi

# From here on the JIT has been handed to this container and is single-use:
# whatever happens, power off so the orchestrator reaps the container instead
# of leaving it idle with a consumed (or unusable) credential.
trap 'log "powering off"; systemctl --no-block poweroff' EXIT

# The orchestrator only sends base64; refuse anything else rather than pass it on.
if [[ ! "$JIT" =~ ^[A-Za-z0-9+/=_-]+$ ]]; then
    log "JIT config has an unexpected format; not starting the runner"
    exit 1
fi
if [[ ! -x "$RUNNER_DIR/run.sh" ]]; then
    log "runner not found at $RUNNER_DIR/run.sh"
    exit 1
fi
if ! id -u "$RUNNER_USER" >/dev/null 2>&1; then
    log "runner user $RUNNER_USER does not exist"
    exit 1
fi

runner_home=$(getent passwd "$RUNNER_USER" | cut -d: -f6)
log "starting runner as $RUNNER_USER from $RUNNER_DIR"
rc=0
cd "$RUNNER_DIR"
runuser -u "$RUNNER_USER" -- env -i \
    HOME="${runner_home:-/home/$RUNNER_USER}" \
    USER="$RUNNER_USER" LOGNAME="$RUNNER_USER" \
    PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin \
    LANG="${LANG:-C.UTF-8}" \
    "$RUNNER_DIR/run.sh" --jitconfig "$JIT" || rc=$?
JIT=""
log "runner exited with status $rc"
exit "$rc"
