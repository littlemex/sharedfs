# sharedfs

**Mount one existing FSx file system from many namespaces, with each namespace granted read-only or read-write access.**

A static PersistentVolume binds to exactly one PersistentVolumeClaim, and the dynamic provisioners of FSx for Lustre and FSx for OpenZFS create a whole new file system per claim. Sharing one existing file system across namespaces therefore means one static PersistentVolume per namespace, each reserved for the right claim, each pointing at the same file system. sharedfs makes that a pair of custom resources:

- **SharedFilesystem** (cluster-scoped, created by the platform admin) names the existing PersistentVolume of the file system and grants namespaces read-only or read-write access.
- **SharedFilesystemClaim** (namespaced, created by the tenant) asks for a mount. The controller creates a PersistentVolumeClaim with the claim's own name, bound to a PersistentVolume generated for this claim only.

```yaml
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystem
metadata:
  name: training-data
spec:
  persistentVolumeRef:
    name: fsx-training
  grants:
  - namespaceSelector:
      matchLabels: {sharedfs.example.com/training-data: rw}
    access: ReadWrite
  - namespaceSelector:
      matchLabels: {sharedfs.example.com/training-data: ro}
    access: ReadOnly
---
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystemClaim
metadata:
  name: data
  namespace: team-a
spec:
  filesystemRef:
    name: training-data
  access: ReadOnly
```

Pods in `team-a` then mount `persistentVolumeClaim: {claimName: data}`.

## What the controller guarantees

| Guarantee | How |
|---|---|
| A generated volume can be newly mounted only while its SharedFilesystem exists and grants its namespace the access it has | Grants are evaluated on every reconcile, before any other check. When a claim stops being granted, `status.withdrawAfter` is set to now plus `--revocation-grace-period` (default 30s, one-second resolution); if the grant is still missing then, on a live re-read of the namespace and the SharedFilesystem, the PersistentVolumeClaim is deleted. Pods already running keep their mount until they stop, and the claim's `Ready` message names them |
| No other namespace can bind a generated volume | Each PersistentVolume is reserved with `claimRef` (namespace and name) for exactly one PersistentVolumeClaim |
| Deleting a generated volume never deletes the file system | Generated PersistentVolumes are always `Retain`, carry their own `volumeHandle`, and copy no provisioner annotation from the source |
| `ReadOnly` is enforced at mount time | The `ro` mount option. Both FSx CSI drivers reject the `ReadOnlyMany` access mode, so generated volumes are `ReadWriteMany` with `ro` added for read-only claims |
| A PVC deleted and recreated rebinds without manual steps | A `Released` volume that still holds the old claim UID has only `claimRef.uid` and `claimRef.resourceVersion` cleared, conditional on the volume not having changed |
| Nothing is left behind | A claim's finalizer deletes its PVC, then its PersistentVolume. A third reconciler removes generated volumes whose claim is gone (finalizer removed by hand, restored from a backup) and sweeps once at startup |
| A SharedFilesystem is not deleted under its tenants | Its finalizer waits until no generated PersistentVolume points at it, and its status lists the claims that remain |

## What it does not do

- **Isolation.** Every namespace granted `ReadWrite` can modify the whole file system. Use separate file systems, or FSx for OpenZFS child volumes, for isolation.
- **Stopping running pods.** Revoking access deletes the PersistentVolumeClaim; pods already running keep their mount until they stop, and new pods cannot start. Evict them if access must end at once.
- **The source PersistentVolume is not protected by sharedfs.** If it has no `claimRef`, any namespace can bind it directly by naming it in a PVC's `volumeName`, bypassing the grants; the SharedFilesystem warns about this. Keep the source bound, or reserve it with `claimRef` for a PVC in an admin-only namespace.
- **Grants are not a data security boundary on their own.** A tenant who can run privileged pods, or (for NFS-based file systems such as OpenZFS) use an inline `nfs` volume under a permissive Pod Security level, can mount the file system without sharedfs. Network access, export settings and POSIX permissions remain the real boundary.
- **Namespace label security.** Grants select namespaces by label. Tenants must not be able to edit those labels, nor write `sharedfilesystemclaims/status`; selecting on `kubernetes.io/metadata.name` avoids the label question. A grant's selector must not be empty; to grant every namespace, match `kubernetes.io/metadata.name` with operator `Exists`.
- **EFS.** `efs.csi.aws.com` mounts from `volumeHandle`, so a generated volume cannot carry its own handle. It is not supported.
- **Following source changes.** If the source PersistentVolume's CSI fields change, existing volumes keep the old ones and claims report `Ready=False (SourceChanged)`. Recreate the claim to remount.

## Supported drivers

| Driver | Mounts from | Verified on |
|---|---|---|
| `fsx.csi.aws.com` | `volumeAttributes.dnsname`, `mountname` | EKS 1.35, FSx for Lustre |
| `fsx.openzfs.csi.aws.com` | `volumeAttributes.DNSName`, `ResourceType`, `VolumePath` | EKS 1.35, FSx for OpenZFS |

The table is part of the code ([internal/driver/driver.go](internal/driver/driver.go)) rather than configuration: each row asserts that the driver's mount does not depend on `volumeHandle`, checked against the driver source.

## Status

```console
$ kubectl get sfs
NAME            VOLUME         DRIVER            VOLUMES   READY   REASON     AGE
training-data   fsx-training   fsx.csi.aws.com   2         True    Resolved   5m
$ kubectl get sfc -A
NAMESPACE   NAME   FILESYSTEM      ACCESS     READY   AGE
team-a      data   training-data   ReadOnly   True    4m
team-c      data   training-data   ReadOnly   False   1m
```

A claim has three conditions. `Accepted` says whether the namespace is granted the requested access and the file system can be used; `Bound` reports the generated PVC and PV as they are; `Ready` is both, and carries the reason of the first that does not hold.

| Condition | Reasons |
|---|---|
| `Accepted` | `Accepted`; `NamespaceNotGranted`, `AccessNotGranted`, `FilesystemNotFound` (these three withdraw access after the grace period); `FilesystemDeleting` and the SharedFilesystem's own reasons such as `SourceNotFound` or `InvalidGrant` (these keep existing volumes) |
| `Bound` | `Bound`, `NoVolume`, `PVCPending`, `PVCConflict`, `PVCTerminating`, `PVCLost`, `PVCCreateFailed`, `VolumeApplyFailed`, `VolumeTerminating` |
| `Ready` | `Ready`, `Revoking`, `SourceChanged`, `Deleting`, or the reason copied from `Accepted` / `Bound` |

A SharedFilesystem has `Ready` with `Resolved`, `SourceNotFound`, `SourceNotCSI`, `UnsupportedDriver`, `SourceIsGenerated`, `InvalidSource`, `InvalidGrant` or `VolumesRemaining`. A source with `reclaimPolicy: Delete` resolves, with a warning in the message and an event, because deleting that source's own PVC deletes the file system.

## Install

The controller needs no cloud credentials: it reads and writes Kubernetes objects only. Its ClusterRole covers PersistentVolumes, PersistentVolumeClaims, the two sharedfs kinds, namespaces (read), events, and pods (list, used only to name the pods that keep a withdrawn PVC).

```bash
export IMG=<registry>/sharedfs-controller:<tag>
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

`make build-installer IMG=$IMG` writes a single `dist/install.yaml` instead.

To remove sharedfs, delete in this order: SharedFilesystemClaims, SharedFilesystems, then the controller (`make undeploy`). Deleting the CRDs first deletes every claim at once; deleting the controller first leaves the finalizers with no one to remove them.

## Development

```bash
make test
```

runs the unit tests and the controller tests against a local API server ([envtest](https://book.kubebuilder.io/reference/envtest)). envtest has no PersistentVolume binder, garbage collector or protection controllers, so binding and mounting are covered by [test/e2e-eks/run.sh](test/e2e-eks/run.sh), which runs against a real cluster and a real file system:

```bash
test/e2e-eks/run.sh <source PersistentVolume> <node name>
```

It mounts a read-write and a read-only claim on one node (claims are Ready about 25 seconds after creation: binding a reserved PersistentVolume waits for the PV controller's periodic sync), writes and reads a nonce, checks that the read-only mount refuses writes, that an ungranted namespace gets nothing, that a recreated PVC rebinds, that revocation keeps the volume through the grace period and then withdraws it, and that deletion goes PVC, then PersistentVolume, then claim. It cleans up after itself and writes one directory, which it removes, on the file system.

See [docs/DESIGN.md](docs/DESIGN.md) for the reasoning behind each choice.

## License

Apache License 2.0.
