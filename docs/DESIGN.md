# sharedfs design (v1alpha1)

## Problem

A static PersistentVolume binds to exactly one PersistentVolumeClaim. FSx for Lustre and FSx for OpenZFS are shared file systems, but their dynamic provisioners create a new file system per claim, so letting a second namespace mount an existing one means a second static PersistentVolume pointing at it. By hand that is: read `dnsname` / `mountname` / `volumeHandle` / capacity off the existing PV, render a PV and a PVC, reserve the PV with `claimRef` (an unreserved static PV can be bound by any namespace that names it in `volumeName`), and, when the PVC is deleted and recreated, clear the old claim UID that keeps the `Retain` PV `Released`.

## Measured before the design was fixed

On EKS 1.35 with an FSx for Lustre (4800 GiB) and an FSx for OpenZFS (256 GiB) file system, CPU nodes only.

| # | Observation | How |
|---|---|---|
| M1 | Both drivers build the mount source from `volumeAttributes`; `volumeHandle` is an identifier only | driver source, `pkg/driver/node.go` (aws-fsx-csi-driver 5ede23e, aws-fsx-openzfs-csi-driver 93c640d) |
| M2 | Both drivers reject `ReadOnlyMany` (`Volume capability not supported`); they accept `SINGLE_NODE_WRITER` and `MULTI_NODE_MULTI_WRITER` only | a real mount, and `controller.go` `volumeCaps` |
| M3 | `ReadWriteMany` with the `ro` mount option mounts read-only (`Read-only file system` on write), while a read-write mount of the same file system on the same node keeps writing | two pods on one node, both drivers |
| M4 | A PVC in another namespace naming a reserved PV in `volumeName` stays Pending (`already bound to a different claim`) | real cluster |
| M5 | Deleting and recreating a PVC of the same name leaves the PV `Released` and the new PVC Pending | real cluster |
| M6 | Removing only `claimRef.uid` and `claimRef.resourceVersion` rebinds the new PVC within 5 s | real cluster |
| M7 | A cluster-scoped object with an ownerReference to a namespaced owner is not garbage collected (`OwnerRefInvalidNamespace`) | real cluster |
| M8 | Both CSIDrivers have `fsGroupPolicy: ReadWriteOnceWithFSType`, so a pod's `fsGroup` does not trigger a recursive ownership change on these `ReadWriteMany` volumes | `kubectl get csidriver` |

## API

Group `sharedfs.littlemex.github.io`, version `v1alpha1`. Both kinds have the `status` subresource and a finalizer `sharedfs.littlemex.github.io/cleanup`.

- **SharedFilesystem** (cluster-scoped): `spec.persistentVolumeRef` (immutable, CEL `self == oldSelf`) and `spec.grants[]` of `{namespaceSelector, access}`. A namespace selected by several grants gets the strongest access; no grants means no namespace. Status: `resolved` (informational copy of the source), `volumes` (generated PVs pointing at it), `Ready`.
- **SharedFilesystemClaim** (namespaced): `spec.filesystemRef` and `spec.access` (default `ReadOnly`); the whole spec is immutable. Status: `persistentVolumeName`, `Accepted`, `Bound`, `Ready`.

One SharedFilesystem per file system, with a list of grants, rather than one object per access level: the object is named after what it shares.

## Invariant

A generated volume exists only while a SharedFilesystem exists and grants its namespace the access the volume has.

The claim reconciler enforces it. Every reconcile evaluates the grants first, before any lifecycle check, so a grant removed while the SharedFilesystem is being deleted still withdraws access. `NamespaceNotGranted`, `AccessNotGranted` and `FilesystemNotFound` withdraw access; every other rejection (`FilesystemDeleting`, the SharedFilesystem's own reasons such as `SourceNotFound`, `PVCConflict`, ...) is possibly temporary and leaves existing volumes in place, so a source PV that is briefly unreadable does not take every tenant's volume away.

Withdrawal is not immediate. When a claim stops being granted, `status.withdrawAfter` is set to now plus `--revocation-grace-period` (30 s, one-second resolution) and cleared when the grant comes back. The start is kept in its own field rather than taken from the `Accepted` condition's `lastTransitionTime`, which does not move when a claim that was already rejected for another reason loses its grant. Once `withdrawAfter` passes, the controller re-reads the namespace and the SharedFilesystem from the API server, bypassing the cache, and only then deletes. A label that disappears for a moment during a GitOps sync does not cost a running workload its volume, and a stale cache cannot either. Withdrawal deletes the PVC; pods already running keep their mount until they stop (pvc-protection), new pods cannot start, and the claim's `Ready` message names the pods that remain. Claims that hold no volume skip the live reads.

## Reconcilers

| Reconciler | Keyed on | Writes | Responsibility |
|---|---|---|---|
| SharedFilesystem | SharedFilesystem | its status and finalizer | Report whether the source can be distributed; keep the SharedFilesystem until no generated PV points at it (counted from PVs, read live before the finalizer is removed, so a rejected claim does not block the admin) |
| SharedFilesystemClaim | claim | PV (server-side apply), PVC (create only), claim status and finalizer | Admit, create, heal, withdraw, release on deletion |
| Orphan | generated PV | deletes PVC and PV | Release volumes whose claim no longer exists (finalizer removed by hand, claim restored with a new UID); sweeps every PV once at startup |

The claim reconciler reads the source PersistentVolume itself rather than the SharedFilesystem status, so there is no copy between the two controllers to go stale. Releasing is one function, `releaseVolumes`, used by deletion, withdrawal and the orphan reconciler: PVC first, PV only once the PVC is gone, every read live.

## Decisions

| Decision | Alternatives considered | Why |
|---|---|---|
| PV name `sfc-<claim UID>` | `<namespace>-<name>` | Injective (`a-b`+`c` and `a`+`b-c` collide when joined), and the orphan reconciler can read the claim UID back from a name only cluster admins can change |
| `volumeHandle` = source handle + `-sfc-` + claim UID | the source handle | kubelet identifies a CSI volume by driver and handle (M1 makes a per-PV handle safe); the source handle is limited to 87 bytes so the result fits the CSI limit of 128 |
| Ownership of a PVC is `spec.volumeName == sfc-<claim UID>` | ownerReference, labels | Tenants can edit ownerReferences and labels on their PVCs; `volumeName` is what the binder treats as authoritative, and it cannot reach anything not granted because the PV is reserved for this namespace and name |
| PV by server-side apply, `claimRef` namespace and name only | CreateOrUpdate | `claimRef` is a granular struct: sharedfs owns namespace and name, the PV controller owns uid and resourceVersion, and neither overwrites the other. An unchanged apply is not a write (no `resourceVersion` change, checked by the e2e) |
| PVC created once, never updated | apply, CreateOrUpdate | Both would silently adopt someone else's PVC of the same name. Nothing in a PVC spec changes after creation |
| PVC requests `min(1Gi, source capacity)` | the source capacity | A static PV binds when its capacity covers the request; charging a namespace's storage ResourceQuota the whole file system would block it |
| Heal only a `Released` PV, with a JSON-patch `test` on `resourceVersion` | clear whenever the UID differs | A cached PVC with an old UID would otherwise unbind a correctly Bound volume |
| CSI drift keeps the old CSI source and still applies the mutable fields | stop reconciling the PV | A PV's volume source cannot change; the `ro` option and the reservation must stay enforced |
| Grant selectors must not be empty (CEL) | `{}` means every namespace | A selector whose fields were dropped by a typo or a templating gap would otherwise grant every namespace |
| No admission webhook | validating webhook for cross-object rules | Every per-object rule is in the schema (enum, default, CEL immutability). Rules that depend on other objects change after admission, so they are evaluated in reconcile and shown in conditions |
| Driver table in code | configuration | Each row asserts a property of the driver's mount checked against its source; making it configurable invites EFS, which mounts from `volumeHandle` |
| PVC watch maps any PVC to the claim of the same name | `Owns` (ownerReference) | A foreign PVC whose deletion should clear `PVCConflict` has no ownerReference, and tenants can remove ours |

## Measured behaviour

On the clusters above, a claim is Ready about 25 seconds after it is created, for both drivers. Binding a PersistentVolume reserved by `claimRef` waits for the PV controller's periodic sync of the PVC; sharedfs does not try to shorten it.

## Not in v1alpha1

EFS, sub-directory mounts, adopting hand-made PVs, following source changes automatically, eviction of running pods on withdrawal, conversion and multiple versions.
