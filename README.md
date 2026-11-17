# sharedfs

**Give many namespaces their own directories of one existing FSx for Lustre file system, claimed with ordinary PersistentVolumeClaims.**

LLM training and inference on Kubernetes usually share one large file system: datasets every team reads, model weights the serving fleet reads, and a checkpoint directory per team that only that team writes. The FSx for Lustre CSI driver does not offer this. Its dynamic provisioner creates a new file system per claim, and a static PersistentVolume exposes the whole file system to whichever namespace binds it.

sharedfs adds one cluster-scoped resource, **FilesystemShare**. An admin names an existing PersistentVolume, a path on it, and the namespaces allowed to mount it. sharedfs then provisions a PersistentVolume that mounts only that directory for every granted claim.

```yaml
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: FilesystemShare
metadata:
  name: checkpoints
spec:
  persistentVolumeRef:
    name: fsx-lustre
  path: "checkpoints/{namespace}"
  provision:
    gid: 1000
    mode: "2770"
  grants:
  - namespaceSelector:
      matchExpressions:
      - {key: example.com/team, operator: Exists}
    access: ReadWrite
---
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: FilesystemShare
metadata:
  name: datasets
spec:
  persistentVolumeRef:
    name: fsx-lustre
  path: "datasets"
  grants:
  - namespaceSelector:
      matchExpressions:
      - {key: example.com/team, operator: Exists}
    access: ReadOnly
```

Each share owns two StorageClasses, `<share>-rw` and `<share>-ro`, and is their provisioner. A team claims it like any other storage:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ckpt
  namespace: team-a
spec:
  storageClassName: checkpoints-rw
  accessModes: [ReadWriteMany]
  resources: {requests: {storage: 1Gi}}
```

The claim in `team-a` mounts `checkpoints/team-a` and nothing above it. Because a claim is a plain PVC, Helm charts that take a `storageClassName`, StatefulSet `volumeClaimTemplates` and generic ephemeral volumes all work unchanged.

Pods write as a member of the directory's group: set `runAsGroup: 1000` or `supplementalGroups: [1000]`. `fsGroup` has no effect on these volumes. A claim's size is not enforced, and its access modes do not make a volume read-only; only the `-ro` class does.

## What a share guarantees

| Guarantee | How |
|---|---|
| A volume mounts only its share's directory | The directory is appended to the Lustre `mountname` (`<mountname>/<path>`), so the client mounts that directory as its root. Lustre resolves the path itself and does not follow symlinks in it |
| Only granted namespaces get a volume | A claim is provisioned only when a single grant of the share selects its namespace and allows the class's access. Otherwise it stays Pending with a `NotGranted` event |
| Read-only is enforced at mount time | Read-only volumes carry the `ro` mount option. If the source mounts with `flock`, read-only volumes use `localflock` instead, so readers take no cluster-wide locks |
| New mounts end when the grant ends | When a namespace loses its grant, a deadline (`--revocation-grace-period`, default 30s) is written on the PersistentVolume, where tenants cannot edit it. If the grant is still missing at the deadline, sharedfs checks the live objects and then deletes the claim. Lowering a grant to ReadOnly withdraws read-write volumes too. Running pods keep their mount, and the claim stays Terminating, until they stop. A deleted claim is not recreated when the grant returns, so set the grace period long enough to ride out a mistaken label change |
| A recreated namespace does not inherit a directory | For `{namespace}` paths, the namespace UID that first claimed the directory is recorded in a FilesystemShareDirectory. A later namespace with the same name gets `NamespaceRecreated` until an admin deletes the record |
| Nested shares are checked | On one file system, when a share that grants ReadWrite may contain another share's directory, one of the two is not Ready (`OverlappingShare`) and gets no new volumes: the one that brought the conflict later, judged by when the outer share first granted ReadWrite and when the inner share was created. Existing volumes are never withdrawn by this check, so check the warning in a share's `Ready` message before adding ReadWrite to an outer share. Shares of the same directory are allowed (a read-write and a read-only share of one dataset), and paths that may coincide for some namespace only warn. The annotation `sharedfs.littlemex.github.io/allow-overlap: "true"` on the outer share exempts it, for an admin's own root share |
| Deleting a volume never deletes data | Provisioned volumes are marked `provisioned-by` sharedfs, so neither the PV controller nor the driver's provisioner acts on them. When a claim goes, sharedfs deletes only the PersistentVolume object |
| A share is not deleted under its tenants | Its finalizer waits until no provisioned volume remains, and its status names up to five of the claims |

With `provision` set, the directory is created on first use, owned by `uid:gid` (`uid` defaults to 0) with `mode` (default `2770`). A short-lived preparer pod does this in the controller's namespace. It runs as UID 0 with only `CAP_CHOWN` and has no service account token. It mounts the parent of the directory read-write; for a one-segment path such as `{namespace}` that is everything the source mounts. It creates only the last segment, under a temporary name, sets its mode and owner, and renames it into place without replacing anything. The parent must already exist, be owned by root, and not be writable by group or others. An existing directory is accepted as it is, even if its owner or mode differ; the record's message says so. `provision` cannot be used with `path: "/"`. With root squash on, run preparer pods on nodes exempt from it (`--preparer-node-selector`).

## What it does not do

- **It is not isolation against privileged workloads.** The boundary is the mount. A pod that is privileged, uses `hostPath`, or can create PersistentVolumes can reach the whole file system without sharedfs. Run tenant namespaces at Pod Security `baseline` or stricter.
- **The directory restriction is applied by the Lustre client.** The servers do not enforce it. Access by Lustre file identifier from inside a directory mount has not been measured.
- **`nosuid` and `nodev` are not added.** They did not take effect on FSx for Lustre mounts when measured, so a writer of a directory can leave setuid binaries for its readers. Readers of a path trust its writers. Run tenants at Pod Security `restricted` (no privilege escalation), with `baseline` as the minimum, and turn on root squash.
- **uid, gid and mode are not a boundary on their own.** Tenants choose `runAsUser` unless a policy stops them, and without root squash a root pod ignores permissions.
- **Grants rely on namespace labels.** Tenants must not be able to set the labels a grant selects on. Selecting on `kubernetes.io/metadata.name` avoids the question.
- **No quotas.** Capacity and inodes are shared by every directory of the file system.
- **Replicas share a directory.** Every replica of a StatefulSet gets its own claim, but all of them mount the same directory. Use `subPathExpr` with the pod name for a directory per replica.
- **Kueue does not see Pending claims.** A Job whose claim is NotGranted is still admitted and holds its quota; use `waitForPodsReady`. If Kueue manages plain pods, exclude the controller namespace, or preparer pods wait on a scheduling gate.
- **Only FSx for Lustre can mount a directory.** FSx for OpenZFS volumes with `ResourceType: filesystem` are supported for `path: "/"` only, because the driver rejects other paths, and so without `provision`. EFS is not supported: its driver mounts from `volumeHandle`.
- **Same file system, different names.** Shares are compared by the source's driver and mount attributes, with DNS names compared case-insensitively. Two source PersistentVolumes that reach one file system by an IP address and a DNS name are taken as different file systems.

## Install

Each share names an existing static PersistentVolume of the file system. It must be bound, or reserved with `claimRef` for a claim in a namespace only admins use. Otherwise any namespace could bind it directly and mount the whole file system, and the share reports `SourceUnreserved`. sharedfs checks only that `claimRef` is set.

```yaml
apiVersion: v1
kind: PersistentVolume
metadata:
  name: fsx-lustre
spec:
  capacity: {storage: 1200Gi}
  accessModes: [ReadWriteMany]
  persistentVolumeReclaimPolicy: Retain
  mountOptions: [flock]
  claimRef: {namespace: fs-admin, name: fsx-lustre}
  csi:
    driver: fsx.csi.aws.com
    volumeHandle: fs-0123456789abcdef0
    volumeAttributes:
      dnsname: fs-0123456789abcdef0.fsx.us-east-1.amazonaws.com
      mountname: abcdefgh
```

Then build and deploy the controller:

```bash
export IMG=registry.example.com/sharedfs-controller:dev
make docker-build docker-push IMG=$IMG
make deploy IMG=$IMG
```

It runs in the namespace `sharedfs-system`. The namespace is labelled Pod Security `baseline`, because preparer pods need `CAP_CHOWN`, and nothing else should run there or be granted pod access there. If Kueue manages plain pods, exclude this namespace.

The controller needs no cloud credentials. Its permissions are:

| Resource | Verbs |
|---|---|
| PersistentVolumes | read, create, patch, delete |
| PersistentVolumeClaims | read and delete in every namespace; create in its own |
| Pods | read, create, delete in its own namespace |
| StorageClasses | read, create |
| Namespaces, CSIDrivers | read |
| FilesystemShares | read, patch, status, finalizers |
| FilesystemShareDirectories | read, create, status, finalizers |
| Events | create |

The `sharedfs-share-viewer` ClusterRole lets its holders list shares. A share's status shows the file system's DNS name, mount name and ID, so bind it only to users who may see those.

| Flag | Default | Meaning |
|---|---|---|
| `--revocation-grace-period` | `30s` | How long a claim stays after its namespace loses the grant |
| `--preparer-node-selector` | none | `key=value,...` for preparer pods, for example nodes exempt from root squash |
| `--preparer-tolerations` | none | JSON list of tolerations for preparer pods |
| `--preparer-service-account` | `sharedfs-preparer` | Service account of preparer pods; it is bound to nothing and its token is not mounted |
| `--preparer-image` | the controller's own image, pinned to its digest when its pod reports one | Image of preparer pods |
| `--leader-elect` | `true` | Keep on: two active controllers would race on the same volumes |

To remove sharedfs, delete the claims, then the shares, then the controller (`make undeploy`). Deleting the CRDs first deletes every share at once; deleting the controller first leaves the finalizers with no one to remove them.

## Status

```console
$ kubectl get fss
NAME          VOLUME       PATH                      VOLUMES   READY   REASON     AGE
checkpoints   fsx-lustre   checkpoints/{namespace}   3         True    Resolved   2d
datasets      fsx-lustre   datasets                  12        True    Resolved   2d
$ kubectl get fsd
NAME                          SHARE         NAMESPACE   PATH                 PREPARED   REASON             AGE
sfd-7e214b5fd4eeff6523a4d8b5   checkpoints   team-a      checkpoints/team-a   True       DirectoryCreated   2d
```

| Object | Condition | Reasons |
|---|---|---|
| FilesystemShare | `Ready` | `Resolved`; `SourceNotFound`, `SourceNotCSI`, `UnsupportedDriver`, `SourceIsGenerated`, `InvalidSource`, `SourceUnreserved`, `InvalidPath`, `SubPathUnsupported`, `InvalidGrant`, `DriverNotInstalled`, `UnsafeDriverPolicy`, `OverlappingShare`, `StorageClassTaken`, `VolumesRemaining` |
| FilesystemShareDirectory | `Prepared` | `DirectoryCreated`, `DirectoryExists`; `Preparing`, `PreparerNotCreated`, `PreparerTimedOut`, `InsecureParent`, `NotADirectory`, `PermissionDenied`, `PreparationFailed` |
| PersistentVolumeClaim | events | `Provisioned`; `NotGranted`, `UnsupportedClaim`, `ShareNotReady`, `NamespaceRecreated`, `DirectoryNotReady`, `Revoking`, `Revoked` |

A share that is not Ready stops new claims only. Volumes already provisioned stay until their grant is removed.

## Development

```bash
make test
```

This runs the unit tests and the controller tests against a local API server ([envtest](https://book.kubebuilder.io/reference/envtest)). envtest has no PV controller, garbage collector or kubelet, so binding, mounting and the preparer are covered by [test/e2e-eks/run.sh](test/e2e-eks/run.sh). It runs against a real cluster and a real FSx for Lustre file system:

```bash
test/e2e-eks/run.sh <source PersistentVolume> <node name>
```

It writes only below one directory of the file system, which it removes at the end. It checks:

- preparation of a per-namespace directory (owner, mode, no leftovers);
- read-write and read-only mounts that see only their directory;
- refusal of ungranted, read-write-on-read-only and `ReadWriteOncePod` claims, and of expansion;
- a generic ephemeral volume;
- a symlinked share path that does not mount;
- an overlapping share that is stopped without stopping the shares in use;
- revocation after the grace period;
- a recreated namespace;
- deletion of a share that still has volumes.

The preparer's file system logic is also tested as root with only `CAP_CHOWN`, in a container:

```bash
CGO_ENABLED=0 GOOS=linux go test -c -o /tmp/prepare.test ./internal/prepare
docker run --rm --cap-drop ALL --cap-add CHOWN -v /tmp:/t:ro busybox:1.36 /t/prepare.test -test.v
```

See [docs/DESIGN.md](docs/DESIGN.md) for the reasoning behind each choice.

## License

Apache License 2.0.
