# sharedfs design (v1alpha1)

## Problem

A static PersistentVolume binds to exactly one PersistentVolumeClaim. FSx for Lustre and FSx for
OpenZFS file systems are shared by nature, but their dynamic provisioners create a whole new file
system per claim, so letting a second namespace mount an existing file system means a second static
PV pointing at the same file system. Today this is done by hand:

- read `dnsname` / `mountname` / `volumeHandle` / capacity off the source PV with `kubectl get -o
  jsonpath`, render a PV and a PVC with `envsubst`, and apply them;
- pre-bind the PV with `claimRef`, because an unbound static PV can be taken by any namespace that
  names it in `volumeName`;
- when the PVC is deleted and recreated, the Retain PV turns `Released`, keeps the old claim UID in
  `claimRef`, and refuses the new PVC. A dedicated script exists to clear it.

## Measured on EKS 1.35 before writing code

| # | Observation | How |
|---|---|---|
| M1 | `fsx.csi.aws.com` and `fsx.openzfs.csi.aws.com` build the mount source from `volumeAttributes`; `volumeHandle` is only an identifier | driver source, `pkg/driver/node.go` (aws-fsx-csi-driver 5ede23e, aws-fsx-openzfs-csi-driver 93c640d) |
| M2 | Both drivers reject `ReadOnlyMany`: `InvalidArgument ... Volume capability not supported`. Only `SINGLE_NODE_WRITER` and `MULTI_NODE_MULTI_WRITER` are accepted | real mount + `controller.go` `volumeCaps` |
| M3 | `ReadWriteMany` + `mountOptions: [ro]` mounts read-only; a write fails with `Read-only file system`, while a rw mount of the same file system on the same node keeps writing | two pods on one c5a.xlarge node, Lustre and OpenZFS |
| M4 | A PVC in another namespace naming a claimRef'd PV in `volumeName` stays Pending (`already bound to a different claim`) | real cluster |
| M5 | Delete and recreate a PVC of the same name: the PV goes `Released`, the new PVC stays Pending | real cluster |
| M6 | Removing only `claimRef.uid` and `claimRef.resourceVersion` rebinds the new PVC within 5 s | real cluster |
| M7 | A cluster-scoped object with an ownerReference to a namespaced owner is not garbage collected; the GC emits `OwnerRefInvalidNamespace` | real cluster |

## API

Group `sharedfs.littlemex.github.io`, version `v1alpha1`.

### SharedFilesystem (cluster-scoped, owned by the platform admin)

```yaml
spec:
  source:
    persistentVolumeName: fsx-training   # immutable (CEL self == oldSelf)
  allowedNamespaces:
    selector: {matchLabels: {sharedfs.littlemex.github.io/fsx-training: "rw"}}  # nil = no namespace
  maxAccess: ReadOnly | ReadWrite        # default ReadOnly
status:
  observedGeneration: 1
  resolved:                              # allow-listed fields copied from the source PV
    driver: fsx.csi.aws.com
    volumeHandle: fs-...
    volumeAttributes: {dnsname: ..., mountname: ...}
    mountOptions: [flock]
    capacity: 4800Gi
  claims: 2                              # SharedFilesystemClaims currently Ready
  conditions: [SourceResolved, Ready]
```

### SharedFilesystemClaim (namespaced, owned by the tenant)

```yaml
spec:                                    # whole spec immutable; change = delete + create
  filesystemRef: {name: fsx-training}
  access: ReadOnly | ReadWrite           # default ReadOnly
status:
  observedGeneration: 1
  persistentVolumeName: sfc-<claim uid>
  persistentVolumeClaimName: <claim name>
  conditions: [Accepted, Bound, Ready]
```

The tenant mounts the PVC that carries the claim's own name.

## Reconcile: SharedFilesystemClaim

1. Add finalizer `sharedfs.littlemex.github.io/cleanup`.
2. Accept or reject. Reasons for `Accepted=False`: `FilesystemNotFound`, `FilesystemNotReady`,
   `NamespaceNotAllowed`, `AccessNotAllowed` (asked ReadWrite, maxAccess ReadOnly),
   `PVCConflict` (a PVC with the claim's name exists and is not ours). On rejection, delete the
   PVC and PV this claim owns (pods already running keep running through `pvc-protection`; new pods
   cannot start).
3. Ensure the PV `sfc-<claim uid>`: `Retain`, `storageClassName: ""`, `ReadWriteMany`, capacity and
   CSI fields from `status.resolved`, `volumeHandle = <source handle>-sfc-<claim uid>`, mount options
   from the source plus `ro` for ReadOnly, `claimRef: {namespace, name}` with no uid. Labels carry
   the claim namespace / name / uid and the filesystem name. No ownerReference (M7).
4. Ensure the PVC `<claim name>`: `volumeName` = the PV, `storageClassName: ""`, ownerReference
   (controller=true) to the claim.
5. Heal: if the PV's `claimRef.uid` is set and differs from the PVC's uid, remove `claimRef.uid` and
   `claimRef.resourceVersion` (M6). Never remove the whole claimRef (it would open M4's hole).
6. Drift on mutable PV fields (mountOptions, claimRef namespace/name, reclaim policy, labels) is
   overwritten. A difference in immutable CSI fields means the source changed: the PV is not
   recreated automatically; the claim reports `Ready=False (SourceChanged)`.
7. Status: `Bound` mirrors the PVC phase; `Ready` = Accepted and Bound.

Deletion: delete the PVC, wait for it to disappear (blocked while pods use it), delete the PV, wait,
remove the finalizer.

## Reconcile: SharedFilesystem

1. Add finalizer; while any SharedFilesystemClaim references it, deletion waits
   (`Ready=False (ClaimsRemaining)`).
2. Resolve the source PV. `SourceResolved=False` reasons: `SourceNotFound`, `UnsupportedDriver`
   (only the two FSx drivers in v1alpha1), `SourceNotCSI`. A source with `reclaimPolicy: Delete`
   resolves but sets a warning reason on `Ready` (`SourceReclaimDelete`), since deleting the source
   PVC would delete the file system; generated PVs are always Retain regardless.
3. Copy only allow-listed fields (driver, volumeHandle, the driver's known attribute keys,
   mountOptions, capacity). Annotations such as `pv.kubernetes.io/provisioned-by` are never copied.
4. Count Ready claims.

## Watches

| Watched | Maps to |
|---|---|
| SharedFilesystemClaim | itself |
| PVC (owned) | owner claim |
| PV with the managed-by label | claim via labels |
| SharedFilesystem | every claim referencing it (field index on `spec.filesystemRef.name`) |
| Namespace (label change) | every claim in it |
| Source PV | SharedFilesystem via field index on `spec.source.persistentVolumeName` |

## Boundaries

- Distribution, not isolation: every ReadWrite tenant can modify the whole file system root.
- Tenants need RBAC only on SharedFilesystemClaim; they never get PV permissions.
- No admission webhook: every per-object rule is expressible in the CRD schema (enum, default,
  CEL immutability). Rules that depend on other objects (namespace labels, the filesystem's
  maxAccess) change after admission, so they are evaluated in reconcile and reported in conditions.
- Teardown order: delete claims, then filesystems, then the controller, then the CRDs. Deleting
  the CRD first cascades to every claim; deleting the controller first leaves finalizers stuck.

## Not in v1alpha1

EFS (its driver parses `volumeHandle` as the mount target, so the handle cannot be suffixed), sub
directory mounts, adopting hand-made PVs, automatic re-creation on source change, conversion and
multiple versions.
