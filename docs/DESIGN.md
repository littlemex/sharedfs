# sharedfs design (v1alpha1)

## Problem

LLM workloads share one large file system in a few fixed shapes:

| Use | What is needed |
|---|---|
| Training data | One tree, read-only, to many training namespaces |
| Model weights for inference | One tree, read-only, to many serving namespaces |
| Checkpoints | A directory per team, read-write for that team only |
| Shared scratch | One tree, read-write, for a group of namespaces |

The FSx for Lustre CSI driver provisions a new file system per claim. A static PersistentVolume of an existing file system binds to one claim and exposes the whole file system. The work by hand is one static PersistentVolume per namespace and directory: copy the mount attributes, append the directory to the mount name, reserve the volume with `claimRef`, create the directory with the right owner, and remember to remove all of it when the team leaves. sharedfs does this as an external provisioner.

## Measured

On EKS 1.35, with FSx for Lustre (Lustre 2.12 client) and FSx for OpenZFS file systems, CPU nodes.

| # | Observation | How |
|---|---|---|
| M1 | Both drivers build the mount source from `volumeAttributes`; `volumeHandle` is an identifier only | driver source, `pkg/driver/node.go` (aws-fsx-csi-driver 5ede23e, aws-fsx-openzfs-csi-driver 93c640d) |
| M2 | Both drivers accept only `SINGLE_NODE_WRITER` and `MULTI_NODE_MULTI_WRITER`; `ReadOnlyMany` as the first access mode is refused | a real mount, and `controller.go` `volumeCaps` |
| M3 | `ReadWriteMany` with the `ro` mount option mounts read-only, while a read-write mount of the same file system on the same node keeps writing | two pods on one node |
| M8 | Both CSIDrivers have `fsGroupPolicy: ReadWriteOnceWithFSType` and only the `Persistent` lifecycle mode | `kubectl get csidriver` |
| M9 | `volumeAttributes.mountname: <mountname>/<dir>` mounts only that directory: its content appears at the mount root | a real mount |
| M10 | A missing directory fails the mount; the pod stays ContainerCreating | a real mount |
| M11 | A symlink as the last path component fails the mount (relative and absolute targets) | a real mount |
| M12 | A symlink as an intermediate component also fails the mount | a real mount |
| M13 | The OpenZFS driver refuses any `VolumePath` other than the root for `ResourceType: filesystem` | a real mount |
| M14 | A claim of a class with `allowVolumeExpansion: false` cannot be resized; the API server refuses the request | e2e |
| M15 | The Lustre driver's CreateVolume always creates a file system; there is no mode for a directory of an existing one | driver source |
| M16 | `nosuid` and `nodev` are not applied on FSx for Lustre mounts | `/proc/mounts` in a pod |
| M17 | A PersistentVolume annotated `pv.kubernetes.io/provisioned-by` with another provisioner gets no finalizer from the FSx provisioner, and the PV controller leaves it Released for its provisioner to delete | e2e: finalizers of a provisioned PV are `[kubernetes.io/pv-protection]` |
| M18 | As UID 0 with only `CAP_CHOWN`, a directory created with mode 0000 cannot be opened (no `CAP_DAC_OVERRIDE`); one created with mode 0700 can be given its mode and then its owner through its file descriptor | the preparer's tests, run in a container with `--cap-drop ALL --cap-add CHOWN` |

## API

Group `sharedfs.littlemex.github.io`, version `v1alpha1`.

**FilesystemShare** (cluster-scoped, created by the admin) is one path on one file system:

| Field | Meaning |
|---|---|
| `spec.persistentVolumeRef` | The existing PersistentVolume of the file system. It must be bound or reserved |
| `spec.path` | `"/"`, or relative segments of `[A-Za-z0-9._-]` (not starting with `.` or `-`), or `{namespace}` as a whole segment. At most 32 segments of 255 bytes, 1024 bytes in all |
| `spec.grants[]` | `{namespaceSelector, access}`. A claim needs one grant that selects its namespace and allows the access of its class. Selectors must not be empty |
| `spec.provision` | Optional `{uid, gid, mode}`: create the directory on first use |
| `status.storageClasses` | `<name>-rw` and `<name>-ro`, owned by the share |
| `status.volumes` | Provisioned volumes |

Everything but the grants is immutable (CEL), so a share never changes under the volumes it has provisioned. To change a path, create another share.

**FilesystemShareDirectory** (cluster-scoped, created by the controller) records one directory of a share: which share (name and UID), which namespace and namespace UID, which expanded path, and, with `provision`, whether it is Prepared. Its spec is immutable. An admin deletes it to let a recreated namespace take the directory.

The path rules are implemented twice, in CEL for admission and in Go (`internal/sharepath`) for the controller and the preparer. One list of examples (`sharepath.ValidExamples`, `InvalidExamples`) is run against both.

## Reconcilers

| Reconciler | Keyed on | Writes | Responsibility |
|---|---|---|---|
| Share | FilesystemShare | its status and finalizer; its StorageClasses (create) | Decide whether the share is safe to provision from; own the classes; hold the share while volumes remain |
| Provisioner | PersistentVolumeClaim | tenant PersistentVolumes; directory records (create); deadline annotation; claim deletion | Provision Pending claims of its classes; withdraw bound claims whose grant is gone |
| Directory | FilesystemShareDirectory | its status; preparer pod, claim and volume | Prepare the directory once, report the result, clean up |
| Reclaim | PersistentVolume | deletes tenant PersistentVolumes | Delete Released volumes provisioned by sharedfs |

Each field has one writer. The PV controller does the binding: a provisioned volume carries a `claimRef` with the claim's UID, as any dynamically provisioned volume does.

## Decisions

| Decision | Alternatives considered | Why |
|---|---|---|
| Tenants claim with plain PVCs of the share's StorageClasses | a namespaced claim kind | Charts, StatefulSets, ephemeral volumes and training operators take a `storageClassName`. A later in-driver mode is a StorageClass swap |
| One share is one path | one share with many paths and per-grant paths | Grants never compose across paths; the list of shares is the catalogue |
| Two classes per share, always created | classes only for the access levels granted | A claim of a share that is not Ready, or of an access no grant allows, is told why (`ShareNotReady`, `NotGranted`) instead of waiting on a class that does not exist |
| Class parameters name the share and its UID; the class must be controlled by the share | trust the parameters | Anyone who can create StorageClasses could otherwise point a class with this provisioner name at a share |
| PV name `sfs-<claim UID>`, handle `sharedfs:<claim UID>` | the source handle | Names are deterministic, so a retry finds the volume it created. A handle that is not a file system ID cannot be mistaken for one by any controller of the driver |
| PV `reclaimPolicy: Delete`, annotated `provisioned-by` sharedfs | `Retain` | Released volumes are removed by sharedfs alone (M17); the file system is never asked for anything |
| PV access modes: the driver's mode first, then the claim's | the claim's modes | kubelet passes the first mode to the driver, which accepts multi-node-multi-writer only (M2); the PV controller requires the PV to include every mode the claim asks for |
| No `fsType` on provisioned volumes | copy it from the source | With `ReadWriteOnceWithFSType` (M8), a claim asking for RWO plus an fsType would let kubelet chown the shared tree for a pod's `fsGroup` |
| Refuse `selector`, `Block`, `dataSource`, `ReadWriteOncePod`, `volumeAttributesClassName` | ignore them | The PV controller leaves selectors to the provisioner; ignoring a field binds a volume the claim did not ask for |
| Refuse CSIDrivers with `fsGroupPolicy: File` or the `Ephemeral` lifecycle | trust the driver | The first chowns the shared tree for any pod; the second lets pods mount the file system inline without a claim |
| Require the source to be bound or reserved, for every path | only for sub-paths | An unreserved source can be bound by any namespace by name, which is root access to the file system |
| Overlap guard: a share whose directory may strictly contain another's, granting ReadWrite, is in conflict; the share without volumes, then the newer, then the greater UID, gives way | stop the outer share; compare only fixed paths | A writer of the outer path can rename the inner directory and plant files. The order never stops a share in use, and both shares reach the same verdict from facts, not from earlier verdicts. `{namespace}` compares as any segment; paths are compared from the file system root, including a source's own `mountname` directory; DNS names are compared case-insensitively. Equal paths only warn: a read-write and a read-only share of one directory is the normal dataset pattern |
| Revocation deadline on the PersistentVolume | on the claim | Tenants can edit their claims; only admins can edit PersistentVolumes |
| A share that is not Ready stops new claims only | withdraw existing volumes | A source that is briefly unreadable must not take every tenant's volume away. Only a removed grant withdraws |
| Directory records as a cluster-scoped kind | a list in the share's status | Status is not restored from backups, and a lost record would silently hand a directory to a recreated namespace. A record is removed by deleting it |
| Preparer: a pod per directory, UID 0 with only `CAP_CHOWN`, no token, mounting only the parent | a Job; the controller mounting Lustre | The controller stays free of file system access. A non-root process with an added capability loses it at exec, so the pod is root without any other capability (M18), which Pod Security `baseline` allows. A pod is retried by the controller with backoff (30s doubling to an hour) |
| Create under a temporary name, `fchmod` then `fchown`, check, then rename without replacing | `mkdir` with the final mode; mode 0000 as a marker | No half-made directory is ever visible under its final name, a crash leaves only an empty temporary directory that the next run removes, and an existing directory is never touched. Mode before owner needs no `CAP_FOWNER`. `renameat2(RENAME_NOREPLACE)` is used where supported; otherwise the parent, writable only by root, leaves no other writer |
| The controller trusts only the preparer's JSON report, with a known outcome | the exit code | A pod that ends without a report failed; a report from a pod the record does not control is ignored |
| No admission webhook or policy | a webhook for cross-object rules | Per-object rules are in the schema. Rules that depend on other objects change after admission, so they are evaluated in reconcile and shown in conditions. Internal volumes are reserved by claim UID, so no other claim can bind them |
| Driver table in code | configuration | Each row asserts how the driver mounts, checked against its source; making it configurable invites a driver that mounts from `volumeHandle` |

## Threat model

The boundary is the mount: a volume from sharedfs mounts only its share's directory. It does not stand against:

- pods that are privileged, use `hostPath`, or hold capabilities beyond Pod Security `baseline`;
- anyone who can create PersistentVolumes;
- anyone who can set the namespace labels the grants select on;
- writers of the same path. Readers trust them: `nosuid` and `nodev` do not apply (M16), so a writer can leave setuid binaries. Tenants should run at Pod Security `restricted`, with root squash on;
- `uid`, `gid` and `mode`, which hold only when tenants cannot pick `runAsUser` and root squash is on.

The controller namespace runs only the controller and its preparer pods; nothing else should run there or be granted pod access there. Exhausting capacity or inodes is not prevented.

## Not in v1alpha1

Project quotas (they need Lustre 2.15), paths deeper than the share, a directory per claim, csi-driver-nfs, EFS, eviction of running pods on revocation, and conversion between versions. An explicit sub-path volume attribute in aws-fsx-csi-driver would let sharedfs stop relying on how the driver joins `mountname`.
