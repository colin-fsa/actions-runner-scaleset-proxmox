# LXC runners

Linux runners can run as unprivileged Proxmox LXC containers instead of QEMU
VMs. A container has a fixed cgroup memory limit, so Proxmox auto-ballooning
cannot shrink it during a job. Memory is only used as the job needs it, and a
container starts in seconds. Windows runners stay on QEMU VMs.

Set `guest_type: lxc` on a profile:

```yaml
scalesets:
  - name: linux-x64
    labels: [self-hosted, linux, x64, proxmox-linux]
    vmid_range: { min: 10000, max: 10999 }
    max_concurrent_runners: 8
    profiles:
      - name: linux
        guest_type: lxc        # default: qemu
        labels: [self-hosted, linux, x64, proxmox-linux]
        template_vmid: 9200    # an LXC container template; required for lxc
        cpu: 4                 # container cores
        memory_mb: 8192        # hard memory limit; swap is always set to 0
        disk_gb: 40            # optional absolute rootfs resize
        hot_size: 1
```

[`examples/lxc-linux.yaml`](../examples/lxc-linux.yaml) shows a full
configuration with LXC Linux runners next to QEMU Windows runners.

## Requirements and validation

- **Proxmox VE 9.x.** The JIT config is delivered through the container `env`
  option, which older releases do not have.
- **Linux only.** `guest_type: lxc` with `guest_os: windows` is rejected.
- **Explicit `template_vmid`.** The global `proxmox.template_vmid` is treated
  as a QEMU template. An LXC profile must name its own container template.
  A `canary_template_vmid` on an LXC profile must also be a container
  template.
- **No `network:` block.** Profile NIC overrides write QEMU `net<N>` strings,
  and IPAM writes the QEMU cloud-init `ipconfig0` key. Neither applies to a
  container, so configure the network interfaces on the container template.
  When `proxmox.firewall` is enabled, `firewall=1` is still added to every
  template NIC.
- **Destroy recycling.** Snapshot rollback is not implemented for containers.
  A scale set made only of LXC profiles always uses destroy recycling, even
  when `pool.recycle_mode: snapshot_rollback` is set for QEMU scale sets.
  An LXC profile in the same scale set as a QEMU profile is rejected under
  `snapshot_rollback`.
- **API token.** The token needs the same privileges as for QEMU VMs, on
  containers. Setting `env` needs only `VM.Config.Options`.

## How it works

| Step | QEMU VM | LXC container |
|------|---------|---------------|
| Clone | `POST /qemu/{tmpl}/clone` | `POST /lxc/{tmpl}/clone` with `hostname`, `pool`, and `full=0` for linked clones |
| Shape | `cores`, `memory`, tags | `cores`, `memory`, `swap=0`, the same owner, scale set, profile and template tags |
| Firewall | `/qemu/{id}/firewall` | `/lxc/{id}/firewall`; `firewall=1` on each `netN` |
| Ready | qemu-guest-agent responds | container status is `running` (containers have no guest agent) |
| JIT hand-off | guest-agent file write | `env=ACTIONS_RUNNER_JIT_CONFIG=<jit>`, then reboot, or start if stopped |
| Job done | runner unit powers the VM off | bootstrap powers the container off |
| Destroy | `DELETE /qemu/{id}` | shut down, then `DELETE /lxc/{id}?purge=1&force=1` |

The orchestrator finds containers and VMs with the same owner-tag and
name-prefix/VMID-range rules. Containers are listed for every scale set,
including QEMU-only ones. If a profile changes from `lxc` back to `qemu`,
containers left behind are still found and removed.

### JIT delivery through the container environment

Containers have no guest agent, so the hand-off uses the PVE container option
`env`. Proxmox writes this option as `lxc.environment.runtime` and passes it to
the container's init process at start. When a job is assigned to a hot
container, `InjectJITConfig` does the following:

1. Checks the JIT. It must be base64 and decode to a JSON object, and it must
   not contain NUL, newline or other control characters.
2. `PUT /lxc/{id}/config env=ACTIONS_RUNNER_JIT_CONFIG=<jit>`. On a running
   container, Proxmox stores the change as pending.
3. `POST /lxc/{id}/status/reboot`, or `status/start` if the container is
   stopped. Proxmox stops the container, applies the pending change and starts
   it again with the new environment.
4. Waits until the container reports `running`.

During the reboot, `PowerState` reports the container as `running`. This stops
the power poller from treating the brief stop as job completion.

On restart, the bootstrap in the container reads the variable from
`/proc/1/environ`, runs the runner once, and powers the container off. The
power poller sees `stopped` and destroys the container. `ReadJITConfig` reads
the value back from the container config. During adoption, a Hot container
whose `env` already contains a JIT is classified as Assigned, so it never gets
a second JIT (that would reboot it during a job).

Security notes:

- The JIT config is a single-use credential. It is never logged, and API error
  messages that might echo it are redacted.
- Inside the container, `/proc/1/environ` is readable only by root. The
  bootstrap starts the runner with a minimal environment that does not contain
  the variable. As on the QEMU path, the runner gets the JIT as a command-line
  argument.
- On the Proxmox side, the value is stored in the container config. Any API
  user with `VM.Audit` on the container can read it until the container is
  destroyed.

### Warm and hot pools

A container starts in seconds, so warm pools matter much less than for VMs.
A small `hot_size` (or none) is usually enough. Capacity admission counts LXC
rows like VMs. A container's `memory` is a limit, not a reservation, so
admission is conservative for containers.

## Building an LXC template

1. Create an **unprivileged** container from a Debian/Ubuntu template, with:
   - `features: nesting=1` (add `keyctl=1` if you run Docker inside),
   - `swap: 0`,
   - a NIC on the runner network, e.g.
     `net0: name=eth0,bridge=vmbr0,ip=dhcp,firewall=1`.
2. Inside the container, install the GitHub Actions runner in
   `/opt/actions-runner` and its dependencies (`bin/installdependencies.sh`).
   Create an unprivileged `runner` user that owns the directory. Install any
   toolchain and Docker you need. Do **not** configure or register the runner.
3. Install the bootstrap from this repository:

   ```sh
   install -m 0755 scripts/linux/lxc-runner-bootstrap.sh /usr/local/sbin/lxc-runner-bootstrap
   install -m 0644 scripts/linux/lxc-runner-bootstrap.service /etc/systemd/system/
   systemctl enable lxc-runner-bootstrap.service
   ```

   To change the defaults (`RUNNER_DIR=/opt/actions-runner`,
   `RUNNER_USER=runner`), set them in `/etc/default/lxc-runner-bootstrap`.
4. Shut the container down, check that `pct config <id>` has no `env:` line,
   and convert it to a template (`pct template <id>`). Use its VMID as the
   profile's `template_vmid`.

When the container boots without the variable, the bootstrap exits right away
and the container idles. This is the hot/warm state.
