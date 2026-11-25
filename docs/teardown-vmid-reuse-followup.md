# Separate follow-up: filesystem-in-use destroy failure and VMID reuse

Captured from the independent review supplied by Colin: Proxmox destroy reports
`filesystem in use`, and VMIDs are being reused. This report was not reproduced
or investigated live in the startup-throughput work. It is a separate lifecycle
and storage-ownership investigation, not a proposed change to teardown here.
CT105's compose build was also verified by the reviewer to use the fork's moving
`integration/windows-host-memory` ref on rebuild. No merge, rebuild, redeploy,
ref change or teardown remediation is authorized by this document.

## Ownership-safe, read-only diagnosis

1. Pin one failed destroy attempt by exact task UPID, node, API principal,
   request/return/completion times and authoritative task exit status/log.
   Capture the controller's matching destroy generation, name, profile,
   owner tags, original clone/start receipts, store state and allocator release
   events. Distinguish a failed task from a lost HTTP response and from later
   cleanup. Do not infer successful deletion from a missing config alone.
2. Separately capture the current generation occupying that numeric VMID:
   name, owner tags, profile, process start/PID, configuration fingerprint,
   creation/clone receipts and attached storage IDs. Correlate every disk by
   stable storage identity (volume UUID, device major/minor and backing chain
   where available), not only a `vm-<VMID>-disk-*` basename. The old task's VMID
   is not authority to operate on the replacement guest.
3. Reconstruct the timeline: when did the old guest stop, when did PVE remove
   its config or reject volume removal, when did the controller free the VMID,
   and when did a new clone claim it? Inspect idempotent not-found handling,
   delayed destroy work, retries, restart adoption and allocator/tombstone
   ownership for that exact pair of generations. Identify any interval where
   an old asynchronous delete could target a new guest.
4. Using an authorized read-only host route, map the exact old volume/device
   to mount tables, mount namespaces, open descriptors, holder devices,
   container/process identities and thin-volume metadata. Check snapshots,
   linked-clone/template backing dependencies and current configs before
   describing anything as orphaned. A reused name alone cannot distinguish
   a lingering old mount, a live replacement disk or a shared backing volume.
5. Report observations and hypotheses separately. Potential explanations
   include a lingering old namespace/reference, a storage task partially
   completing before reporting failure, or a delayed old-generation retry
   racing with reuse. None is established by the supplied symptom alone.
   Produce the ownership timeline and a narrowly reproduced lifecycle test
   before selecting a fix.

## Hold boundary

This work makes no allocator, not-found, retry, delete or storage-cleanup changes.
Do not clear locks, force-stop a guest, unmount anything, detach/delete a disk,
run `lvremove`, or reclaim volumes by numeric VMID/name. If later evidence proves
an old volume has no live ownership, mounts, process holders, snapshots or
backing dependencies, prepare an exact resource-specific cleanup/recovery plan
for separate approval. If ownership is ambiguous or belongs to the replacement
generation, preserve it and stop remediation.
