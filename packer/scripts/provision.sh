#!/usr/bin/env bash
# Main provisioning + hardening pass for the runner template.
#
# Run as root by Packer's shell provisioner after Subiquity autoinstall
# completes. Sourced env vars (set by Packer):
#   RUNNER_VERSION   - GitHub Actions runner release (e.g. 2.323.0)
#   RUNNER_ARCH      - runner architecture (x64 / arm64)
#   RUNNER_USER      - local user the runner runs as
#   BUILD_USERNAME   - the Subiquity build user (locked at the end)
#
# This script is intentionally linear and idempotent-on-replay so a
# Packer build failure mid-way can be re-run safely. Every section logs
# what it's doing so build output is auditable.

set -euo pipefail

RUNNER_VERSION="${RUNNER_VERSION:?missing}"
RUNNER_ARCH="${RUNNER_ARCH:?missing}"
RUNNER_USER="${RUNNER_USER:?missing}"
BUILD_USERNAME="${BUILD_USERNAME:?missing}"
SHARED_CACHE_TAG="${SHARED_CACHE_TAG:-}"
SHARED_CACHE_MOUNT="${SHARED_CACHE_MOUNT:-}"
RUNNER_UID="${RUNNER_UID:-}"
PLAYWRIGHT_VERSION="${PLAYWRIGHT_VERSION:-}"
PLAYWRIGHT_BROWSERS="${PLAYWRIGHT_BROWSERS:-chromium webkit}"
TOOLCACHE_PYTHON_VERSIONS="${TOOLCACHE_PYTHON_VERSIONS:-}"
PREPULL_DOCKER_IMAGES="${PREPULL_DOCKER_IMAGES:-}"

STAGE_DIR="/tmp/scaleset-files"

log() { printf '[provision] %s\n' "$*"; }

# -----------------------------------------------------------------------------
# Wait for cloud-init / apt to settle.
# -----------------------------------------------------------------------------
log "waiting for cloud-init to finish"
cloud-init status --wait || true

log "waiting for apt locks"
while fuser /var/lib/dpkg/lock-frontend >/dev/null 2>&1 \
   || fuser /var/lib/apt/lists/lock >/dev/null 2>&1; do
    sleep 2
done

export DEBIAN_FRONTEND=noninteractive

# -----------------------------------------------------------------------------
# Base packages.
# -----------------------------------------------------------------------------
log "updating apt cache + installing baseline packages"
apt-get update

# Bake the updates in here rather than paying for them on every clone's
# first boot. Clones deliberately run no apt at boot (cloud-init apt is
# disabled below, ciupgrade=0 on the template, apt-daily masked), so the
# template rebuild is the only place updates land.
log "upgrading base image packages"
apt-get dist-upgrade -y

apt-get install -y --no-install-recommends \
    qemu-guest-agent \
    ca-certificates \
    curl \
    git \
    jq \
    sudo \
    iproute2 \
    iputils-ping \
    netcat-openbsd \
    unzip \
    xz-utils \
    libicu-dev   # required by the Actions runner's .NET runtime

systemctl enable --now qemu-guest-agent
log "qemu-guest-agent active"

# Toolchain the GitHub-hosted ubuntu images ship, so workflows written for them run unchanged:
# build tools, python, zip/wget/rsync, Node LTS (+ npm; pnpm/action-setup and others call npm from PATH) and gh.
log "installing GitHub-hosted-runner toolchain"
apt-get install -y --no-install-recommends \
    build-essential python3 python3-pip python3-venv zip wget rsync patch gnupg
curl -fsSL https://deb.nodesource.com/setup_lts.x | bash -
apt-get install -y --no-install-recommends nodejs
install -d -m 0755 /etc/apt/keyrings
curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg -o /etc/apt/keyrings/githubcli.gpg
echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli.gpg] https://cli.github.com/packages stable main" \
    > /etc/apt/sources.list.d/github-cli.list
apt-get update
apt-get install -y --no-install-recommends gh
node --version && npm --version && gh --version | head -1

# Playwright's OS-level libraries (fonts, GStreamer, GTK, Xvfb, ...), so e2e jobs do not apt-install ~300
# packages on every ephemeral VM. Browser binaries are NOT installed here: they are cached per version on
# the shared cache. A later Playwright bump that needs one more library just installs it at job time.
if [ -n "${PLAYWRIGHT_VERSION}" ]; then
    log "installing Playwright ${PLAYWRIGHT_VERSION} system dependencies (${PLAYWRIGHT_BROWSERS})"
    # EGL/GLES/Vulkan loaders that the e2e workflow also installs explicitly.
    apt-get install -y --no-install-recommends libgbm1 libgl1-mesa-dri mesa-vulkan-drivers
    # shellcheck disable=SC2086  # PLAYWRIGHT_BROWSERS is a space-separated list
    DEBIAN_FRONTEND=noninteractive npx --yes "playwright@${PLAYWRIGHT_VERSION}" install-deps ${PLAYWRIGHT_BROWSERS}
fi

# -----------------------------------------------------------------------------
# Runner user.
# -----------------------------------------------------------------------------
log "creating runner user ${RUNNER_USER}"
if ! id "${RUNNER_USER}" >/dev/null 2>&1; then
    # RUNNER_UID, when set, makes the guest uid/gid equal to the owner of the shared cache on the host
    # (virtiofs passes ids through unmapped), so the cache dirs the LXC runners created stay writable.
    if [ -n "${RUNNER_UID}" ]; then
        groupadd --gid "${RUNNER_UID}" "${RUNNER_USER}"
        useradd --create-home --shell /bin/bash --uid "${RUNNER_UID}" --gid "${RUNNER_UID}" "${RUNNER_USER}"
    else
        useradd --create-home --shell /bin/bash "${RUNNER_USER}"
    fi
fi
# Lock the password so console / SSH password login is impossible.
passwd -l "${RUNNER_USER}" || true

# The runner needs to run sudo for `actions/setup-*` recipes and similar.
# Restricted to NOPASSWD because no human ever logs in.
install -m 0440 /dev/stdin /etc/sudoers.d/10-runner <<EOF
${RUNNER_USER} ALL=(ALL) NOPASSWD: ALL
EOF

# -----------------------------------------------------------------------------
# Docker (Ubuntu's docker.io, no third-party repo). Membership of the docker
# group is root-equivalent, which is acceptable here: the VM is single-use and
# the runner already has passwordless sudo.
# -----------------------------------------------------------------------------
log "installing docker"
apt-get install -y --no-install-recommends docker.io docker-buildx docker-compose-v2
systemctl enable docker
usermod -aG docker "${RUNNER_USER}"

# -----------------------------------------------------------------------------
# Shared cache (optional): a host directory attached to each clone as virtiofs
# (qm set <vmid> --virtiofs0 <tag>; see packer/README.md). nofail keeps clones
# booting if it is not attached.
# pnpm's store is SQLite and virtiofs file locks are not coherent between guests (or with the LXC
# runners), so a shared store can corrupt. Bind a per-runner local directory over <share>/pnpm-store:
# jobs that point pnpm at the shared path get a writable local store and the share's store is never
# touched. pip and ESLint caches stay shared (file-based, per-runner names).
# -----------------------------------------------------------------------------
if [ -n "${SHARED_CACHE_MOUNT}" ]; then
    log "configuring shared cache ${SHARED_CACHE_TAG} at ${SHARED_CACHE_MOUNT}"
    mkdir -p "${SHARED_CACHE_MOUNT}"
    echo "${SHARED_CACHE_TAG} ${SHARED_CACHE_MOUNT} virtiofs defaults,nofail 0 0" >> /etc/fstab
    install -d -o "${RUNNER_USER}" -g "${RUNNER_USER}" -m 0755 /var/lib/pnpm-store
    PNPM_UNIT="$(systemd-escape --path --suffix=mount "${SHARED_CACHE_MOUNT}/pnpm-store")"
    install -m 0644 /dev/stdin "/etc/systemd/system/${PNPM_UNIT}" <<EOF
[Unit]
Description=Per-runner local pnpm store over the shared cache path
RequiresMountsFor=${SHARED_CACHE_MOUNT}
[Mount]
What=/var/lib/pnpm-store
Where=${SHARED_CACHE_MOUNT}/pnpm-store
Type=none
Options=bind
[Install]
WantedBy=multi-user.target
EOF
    systemctl enable "${PNPM_UNIT}"
fi

# -----------------------------------------------------------------------------
# Speed over durability: these are single-use VMs, so a crash mid-install only fails one CI run.
# dpkg skips fsync on every file it unpacks (large apt installs in jobs get much faster).
# -----------------------------------------------------------------------------
echo "force-unsafe-io" > /etc/dpkg/dpkg.cfg.d/force-unsafe-io

# -----------------------------------------------------------------------------
# GitHub Actions runner binary.
# -----------------------------------------------------------------------------
log "installing GitHub Actions runner v${RUNNER_VERSION} (${RUNNER_ARCH})"
install -d -o "${RUNNER_USER}" -g "${RUNNER_USER}" -m 0755 /opt/actions-runner
cd /opt/actions-runner

TARBALL="actions-runner-linux-${RUNNER_ARCH}-${RUNNER_VERSION}.tar.gz"
URL="https://github.com/actions/runner/releases/download/v${RUNNER_VERSION}/${TARBALL}"

curl --fail --silent --show-error --location --proto '=https' --tlsv1.2 \
     -o "/tmp/${TARBALL}" "${URL}"
tar -xzf "/tmp/${TARBALL}" -C /opt/actions-runner
rm -f "/tmp/${TARBALL}"
chown -R "${RUNNER_USER}:${RUNNER_USER}" /opt/actions-runner

# The runner ships with a script that pulls in libs it needs (.NET deps).
log "installing runner OS dependencies"
/opt/actions-runner/bin/installdependencies.sh

# The orchestrator writes the JIT config as a systemd env file at this
# path on each clone. The path itself must NOT exist in the template.
test ! -e /opt/actions-runner/jitconfig.env

# -----------------------------------------------------------------------------
# Pre-seeded job dependencies: saves per-job downloads on ephemeral clones.
# -----------------------------------------------------------------------------
# actions/setup-python looks in the runner tool cache (<runner>/_work/_tool) before downloading;
# seed it with the exact builds it would fetch (actions/python-versions) for this Ubuntu release.
TOOL_CACHE=/opt/actions-runner/_work/_tool
install -d -o "${RUNNER_USER}" -g "${RUNNER_USER}" /opt/actions-runner/_work "${TOOL_CACHE}"
. /etc/os-release
for v in ${TOOLCACHE_PYTHON_VERSIONS}; do
    log "seeding setup-python tool cache: Python ${v} (ubuntu ${VERSION_ID})"
    url=$(curl -fsSL https://raw.githubusercontent.com/actions/python-versions/main/versions-manifest.json | python3 -c '
import json, sys
v, pv = sys.argv[1], sys.argv[2]
print(next(f["download_url"] for r in json.load(sys.stdin) if r["version"] == v
           for f in r["files"] if f["platform"] == "linux" and f["arch"] == "x64" and f["platform_version"] == pv))' "${v}" "${VERSION_ID}")
    d=$(mktemp -d)
    curl -fsSL "${url}" | tar -xz -C "${d}"
    # The build has no rpath for its libpython; without LD_LIBRARY_PATH it would load Ubuntu's same-version
    # libpython (Debian install scheme) and pip lands where this Python never looks. setup-python sets the
    # same variable at job time.
    lib="${TOOL_CACHE}/Python/${v}/x64/lib"
    (cd "${d}" && RUNNER_TOOL_CACHE="${TOOL_CACHE}" AGENT_TOOLSDIRECTORY="${TOOL_CACHE}" LD_LIBRARY_PATH="${lib}" bash ./setup.sh)
    rm -rf "${d}"
    test -e "${TOOL_CACHE}/Python/${v}/x64.complete"
    LD_LIBRARY_PATH="${lib}" "${TOOL_CACHE}/Python/${v}/x64/bin/python3" -m pip --version
done
chown -R "${RUNNER_USER}:${RUNNER_USER}" /opt/actions-runner/_work

# Service-container images: the runner still checks the registry per job, but layers are already local.
systemctl start docker
for img in ${PREPULL_DOCKER_IMAGES}; do
    log "pre-pulling ${img}"
    docker pull --quiet "${img}"
done

# PowerShell 7 (pwsh), on PATH like the GitHub-hosted images. The release tarball avoids the .deb's
# pinned libicu versions (this Ubuntu ships a newer ICU).
log "installing PowerShell 7 (pwsh)"
url=$(curl -fsSL https://api.github.com/repos/PowerShell/PowerShell/releases/latest | python3 -c '
import json, sys
print(next(a["browser_download_url"] for a in json.load(sys.stdin)["assets"]
           if a["name"].endswith("-linux-x64.tar.gz") and "musl" not in a["name"]))')
install -d /opt/microsoft/powershell/7
curl -fsSL "${url}" | tar -xz -C /opt/microsoft/powershell/7
chmod +x /opt/microsoft/powershell/7/pwsh
ln -sf /opt/microsoft/powershell/7/pwsh /usr/bin/pwsh
pwsh -NoProfile -Command '$PSVersionTable.PSVersion.ToString()'

# -----------------------------------------------------------------------------
# systemd units.
# -----------------------------------------------------------------------------
log "installing gh-runner.{path,service}"
install -m 0644 "${STAGE_DIR}/gh-runner.path"    /etc/systemd/system/gh-runner.path
install -m 0644 "${STAGE_DIR}/gh-runner.service" /etc/systemd/system/gh-runner.service
# OOM loudly, as the LXC fleet did: cap the runner's job tree below the VM's RAM (no swap) so a runaway job is
# killed promptly, and OOMPolicy=continue so only the offending process dies and the step fails visibly
# (exit 137, diagnostics still run) instead of systemd stopping the whole runner, which GitHub reports only as
# "The operation was canceled." Inside a VM a percentage resolves against the guest's own RAM. Docker service
# containers run under docker.service and are not covered.
install -d /etc/systemd/system/gh-runner.service.d
cat >/etc/systemd/system/gh-runner.service.d/oom.conf <<'EOF'
[Service]
MemoryMax=95%
MemorySwapMax=0
OOMPolicy=continue
EOF
# The workflows' memory diagnostics query github-runner.service (the LXC fleet's unit name).
ln -sf gh-runner.service /etc/systemd/system/github-runner.service
systemctl daemon-reload
# Only the path unit is enabled — it'll start the service when the
# orchestrator writes the jitconfig file.
systemctl enable gh-runner.path

# -----------------------------------------------------------------------------
# Kernel hardening.
# -----------------------------------------------------------------------------
log "installing sysctl hardening"
install -m 0644 "${STAGE_DIR}/99-scaleset-hardening.conf" /etc/sysctl.d/99-scaleset-hardening.conf

# -----------------------------------------------------------------------------
# Cloud-init: disable apt-update on first boot.
# -----------------------------------------------------------------------------
# By default cloud-init runs `apt-get update` (and optionally upgrade) on
# every clone's first boot. The template is built with current packages;
# burning ~10MB of bandwidth per clone for incremental refreshes is wasted
# and pointless (the VM lives for one job then dies).
#
# Workflows that need newer packages can run `apt-get update` themselves.
log "disabling cloud-init apt update/upgrade"
install -m 0644 /dev/stdin /etc/cloud/cloud.cfg.d/99-no-apt-update.cfg <<'CINIT'
# Disable cloud-init's first-boot apt refresh. The template is rebuilt
# regularly (security updates land via Packer re-runs, not via per-boot
# apt). Override per-VM if a specific workflow needs it.
package_update: false
package_upgrade: false
package_reboot_if_required: false
apt:
  preserve_sources_list: true
CINIT
# Defer sysctl --system to first boot via systemd; running it now in the
# build VM is unnecessary and may interact with the live ISO's network.

# -----------------------------------------------------------------------------
# SSH disable on first boot of every clone.
#
# We CAN'T remove openssh-server here — Packer is connected over the very
# SSH session we'd be terminating, so it dies mid-script and the build is
# torn down. Instead we install a oneshot systemd unit that runs once on
# the first boot of every clone (ConditionFirstBoot=yes) and removes SSH
# at that point. The template itself keeps SSH so we can re-Packer it.
# -----------------------------------------------------------------------------
log "installing firstboot SSH-removal oneshot"
install -m 0755 /dev/stdin /usr/local/sbin/firstboot-harden <<'HARDEN'
#!/bin/bash
# Runs once on first boot of a clone. Stops + masks openssh-server so
# the runner VM has no network-reachable shell, then self-disables.
#
# We DON'T purge openssh-server here — earlier attempts using
# `apt-get purge openssh-server && apt-get autoremove --purge` left
# the VM in a state where the Proxmox guest-agent endpoint returned
# "VM is not running" for ~30s, breaking JIT injection. Masking the
# service achieves the same security posture (no sshd ever runs)
# without touching apt or pulling network bandwidth.
set -eu
systemctl stop ssh.service ssh.socket 2>/dev/null || true
systemctl mask ssh.service ssh.socket 2>/dev/null || true
systemctl disable firstboot-harden.service 2>/dev/null || true
HARDEN

install -m 0644 /dev/stdin /etc/systemd/system/firstboot-harden.service <<'UNIT'
[Unit]
Description=First-boot hardening for ephemeral runner clone
ConditionFirstBoot=yes
After=multi-user.target
# NOTE: do NOT add `Before=gh-runner.path` here. gh-runner.path has
# WantedBy=multi-user.target, so adding Before=gh-runner.path creates
# an ordering cycle that systemd resolves by deleting gh-runner.path
# from the boot sequence. firstboot-harden running in parallel with
# the runner is fine — SSH removal is a defence-in-depth measure, not
# a precondition for the runner to start.

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/firstboot-harden
RemainAfterExit=true

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable firstboot-harden.service

# -----------------------------------------------------------------------------
# Service trimming.
# -----------------------------------------------------------------------------
log "disabling unnecessary services"
# snapd: not used; substantial attack surface for a single-job VM.
apt-get purge -y snapd || true
rm -rf /root/snap /home/*/snap /var/cache/snapd

# Pollyfilly services we don't need on an ephemeral runner.
for svc in \
    multipathd.service \
    iscsid.service iscsid.socket open-iscsi.service \
    ModemManager.service \
    unattended-upgrades.service apt-daily.service apt-daily-upgrade.service \
    apt-daily.timer apt-daily-upgrade.timer
do
    systemctl disable --now "${svc}" 2>/dev/null || true
    systemctl mask         "${svc}" 2>/dev/null || true
done

# -----------------------------------------------------------------------------
# Login hardening.
# -----------------------------------------------------------------------------
log "tightening login defaults"
sed -i \
    -e 's/^UMASK\s\+.*/UMASK\t\t027/' \
    -e 's/^PASS_MAX_DAYS\s\+.*/PASS_MAX_DAYS\t90/' \
    -e 's/^PASS_MIN_DAYS\s\+.*/PASS_MIN_DAYS\t1/' \
    -e 's/^PASS_WARN_AGE\s\+.*/PASS_WARN_AGE\t14/' \
    /etc/login.defs

# Disable core dumps via limits.conf (sysctl drop-in already disables suid_dumpable).
cat >/etc/security/limits.d/99-no-core.conf <<'EOF'
* hard core 0
* soft core 0
EOF

# -----------------------------------------------------------------------------
# File permissions.
# -----------------------------------------------------------------------------
log "tightening permissions on sensitive files"
chmod 0640 /etc/shadow /etc/gshadow
chmod 0644 /etc/passwd /etc/group
chmod 0700 /root
[ -d "/home/${RUNNER_USER}" ] && chmod 0750 "/home/${RUNNER_USER}"

# -----------------------------------------------------------------------------
# Disable swap.
# -----------------------------------------------------------------------------
# Ephemeral VMs are sized to fit their job; swap mostly just persists
# secrets to disk. Remove any swap entries Subiquity may have created.
log "disabling swap"
swapoff -a || true
sed -i.bak -E '/(^|\s)swap(\s|$)/d' /etc/fstab
rm -f /swap.img /swapfile

# -----------------------------------------------------------------------------
# Speed over hardening for one-job VMs.
# -----------------------------------------------------------------------------
# kdump-tools reserves 512 MB+ (crashkernel=) in every clone; nobody reads a runner's crash dump.
# mitigations=off: Skylake-class hosts pay heavily for CPU-vulnerability mitigations on syscall/IO-heavy
# builds; the guest runs one job then is destroyed, and the host keeps its own mitigations.
# Subiquity also leaves its "autoinstall ds=nocloud" arguments on the command line.
log "kernel command line: no crashkernel, mitigations=off, no boot menu wait"
# kdump-tools is a dependency of the ubuntu-server metapackage: pin what that metapackage pulled in as manually
# installed first, so the autoremove below cannot strip the server package set when the metapackage goes.
apt-mark manual $(apt-cache depends --installed ubuntu-server 2>/dev/null | awk '/(Depends|Recommends):/ && $2 !~ /^</ {print $2}') >/dev/null 2>&1 || true
apt-get purge -y kdump-tools kexec-tools || true
# lxd-installer is masked, not purged: purging it drops the ubuntu-server metapackage and autoremove below
# would then strip everything that metapackage pulled in.
systemctl disable lxd-installer.socket 2>/dev/null || true
systemctl mask lxd-installer.socket 2>/dev/null || true
sed -i -E \
    -e 's/^GRUB_CMDLINE_LINUX=.*/GRUB_CMDLINE_LINUX=""/' \
    -e 's/^GRUB_CMDLINE_LINUX_DEFAULT=.*/GRUB_CMDLINE_LINUX_DEFAULT="mitigations=off"/' \
    -e 's/^GRUB_TIMEOUT=.*/GRUB_TIMEOUT=0/' \
    /etc/default/grub
grep -rl crashkernel /etc/default/grub.d/ 2>/dev/null | xargs -r rm -f || true
update-grub
# No access-time writes on the root filesystem.
awk '$2 == "/" && $4 !~ /noatime/ { $4 = $4 ",noatime" } { print }' /etc/fstab > /etc/fstab.new && mv /etc/fstab.new /etc/fstab

# -----------------------------------------------------------------------------
# Cleanup for template conversion.
# -----------------------------------------------------------------------------
log "cleaning apt + tmp + logs"
apt-get autoremove --purge -y
apt-get clean
rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* "${STAGE_DIR}"

log "rotating + truncating journal"
journalctl --rotate || true
journalctl --vacuum-time=1s || true
find /var/log -type f -exec truncate -s 0 {} \;

# Clear shell history.
unset HISTFILE
rm -f /root/.bash_history /home/*/.bash_history

# Clear machine-id so each clone gets a fresh one on first boot.
# Empty file is required (not absent) so /etc/machine-id is regenerated.
log "clearing machine-id"
truncate -s 0 /etc/machine-id
test ! -L /var/lib/dbus/machine-id && ln -sf /etc/machine-id /var/lib/dbus/machine-id

# Reset cloud-init so clones perform first-boot routines.
log "resetting cloud-init"
cloud-init clean --logs --machine-id 2>/dev/null || cloud-init clean --logs || true

# -----------------------------------------------------------------------------
# Revoke build credentials (LAST step).
#
# After this point Packer will lose sudo on the next provisioner; we
# keep this script as the final shell provisioner in the build.
# -----------------------------------------------------------------------------
log "locking + de-privileging the build user (${BUILD_USERNAME})"
if id "${BUILD_USERNAME}" >/dev/null 2>&1; then
    # Lock the password so the account can't be used to log in.
    passwd -l "${BUILD_USERNAME}" || true
    # Remove the user from sudo group entirely.
    deluser "${BUILD_USERNAME}" sudo 2>/dev/null || true
fi

# TRIM the disk so Proxmox can compact the template.
log "fstrim"
fstrim -av || true

log "provisioning complete"
sync
