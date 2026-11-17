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
| New mounts end when the grant ends | When a namespace loses its grant, a deadline (`--revocation-grace-period`, default 15 minutes) is written on the PersistentVolume, where tenants cannot edit it. If the grant is still missing at the deadline, sharedfs checks the live objects and then deletes the claim. Lowering a grant to ReadOnly withdraws read-write volumes too. Running pods keep their mount, and the claim stays Terminating, until they stop. A deleted claim is not recreated when the grant returns |
| A mistake does not delete every claim | Deletions are rate-limited cluster-wide (`--revocations-per-minute`, default 30). The annotation `sharedfs.littlemex.github.io/pause-revocation: "true"` on a share holds all of its revocations. A share that was force-deleted, or recreated with a new UID, holds its claims instead of revoking them (`RevocationHeld` event) |
| A recreated namespace does not inherit a directory | For `{namespace}` paths, the namespace UID that first claimed the directory is recorded in a FilesystemShareDirectory. A later namespace with the same name gets `NamespaceRecreated` until an admin deletes the record |
| Nested shares are checked | On one file system, when a share that grants ReadWrite may contain another share's directory, one of the two is not Ready (`OverlappingShare`) and gets no new volumes: the one that brought the conflict later, judged by when the outer share first granted ReadWrite and when the inner share was created. Existing volumes are never withdrawn by this check, so check the warning in a share's `Ready` message before adding ReadWrite to an outer share. Shares of the same directory are allowed (a read-write and a read-only share of one dataset), and paths that may coincide for some namespace only warn. The annotation `sharedfs.littlemex.github.io/allow-overlap: "true"` on the outer share exempts it, for an admin's own root share |
| Deleting a volume never deletes data | Provisioned volumes are marked `provisioned-by` sharedfs, so neither the PV controller nor the driver's provisioner acts on them. When a claim goes, sharedfs deletes only the PersistentVolume object |
| A share is not deleted under its tenants | Its finalizer waits until no provisioned volume remains, and its status names up to five of the claims |

With `provision` set, the directory is created on first use, owned by `uid:gid` (`uid` defaults to 0) with `mode` (default `2770`). A short-lived preparer pod does this in the controller's namespace. It runs as UID 0 with only `CAP_CHOWN`, has no service account token and no network, and mounts the parent of the directory read-write. It creates only the last segment, under a temporary name, sets its mode and owner, and renames it into place without replacing anything. The parent must already exist, be owned by root, and not be writable by group or others. An existing directory is accepted as it is, even if its owner or mode differ; the record's message says so. `provision` needs a path of two segments or more (`teams/{namespace}`, not `{namespace}`), so the preparer never mounts the whole file system. With root squash on, run preparer pods on nodes exempt from it (`--preparer-node-selector`).

### Quotas

On FSx for Lustre 2.15, `provision.quota` limits each created directory with a Lustre project quota:

```yaml
  provision:
    gid: 1000
    mode: "2770"
    quota: {storage: 500Gi, inodes: 2000000}
```

Each directory record gets a project ID of its own (from `--project-id-base`, default 1000000). The preparer gives it to the directory when it creates it, with inheritance, so everything created below it counts and tenants cannot change it. A quota pod then sets the limit, and sets it again whenever the share's limits change. The quota pod runs as UID 0 with only `CAP_SYS_ADMIN`, which `lfs setquota` requires. It mounts only that directory, read-write because Lustre refuses quota changes through a read-only mount, and has no token and no network. A claim is provisioned only once its directory's limit holds. A directory that existed before sharedfs is not tagged; its record reports `DirectoryNotTagged` with the `lfs project` command an admin can run, and the limit applies on the next retry. Quota pods need a namespace of their own at Pod Security `privileged`, and an image with `lfs`:

```bash
kubectl apply -k config/quota
docker build -f Dockerfile.quota -t registry.example.com/sharedfs-quota:dev . && docker push registry.example.com/sharedfs-quota:dev
```

Then start the controller with `--quota-namespace=sharedfs-quota --quota-image=registry.example.com/sharedfs-quota@sha256:...`.

## What it does not do

- **It is not isolation against privileged workloads.** The boundary is the mount. A pod that is privileged, uses `hostPath`, or can create PersistentVolumes can reach the whole file system without sharedfs. Run tenant namespaces at Pod Security `baseline` or stricter.
- **The directory restriction is applied by the Lustre client.** The servers do not enforce it. From inside a directory mount, `.lustre/fid` is not visible and `fid2path` outside the directory is refused (measured on 2.12 and 2.15); `open_by_handle_at`, which needs `CAP_DAC_READ_SEARCH` and so a pod above Pod Security baseline, was not tested.
- **`nosuid` and `nodev` are not added.** They did not take effect on FSx for Lustre mounts when measured, so a writer of a directory can leave setuid binaries for its readers. Readers of a path trust its writers. Run tenants at Pod Security `restricted` (no privilege escalation), with `baseline` as the minimum, and turn on root squash.
- **uid, gid and mode are not a boundary on their own.** Tenants choose `runAsUser` unless a policy stops them, and without root squash a root pod ignores permissions.
- **Grants rely on namespace labels.** Tenants must not be able to set the labels a grant selects on. Selecting on `kubernetes.io/metadata.name` avoids the question.
- **No quotas on Lustre 2.12, or on directories sharedfs did not create.** Capacity and inodes are then shared by every directory of the file system.
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

It runs in the namespace `sharedfs-system` as two replicas on different nodes, with a PodDisruptionBudget; only the leader reconciles, and a standby takes over within the lease duration. The namespace is labelled Pod Security `baseline`, because preparer pods need `CAP_CHOWN`, and nothing else should run there or be granted pod access there. If Kueue manages plain pods, exclude this namespace and `sharedfs-quota`. Every PersistentVolume and PersistentVolumeClaim of the cluster is cached: the leader used about 45 MiB with 800 of each, so the 512 MiB limit leaves room for clusters about ten times larger. The revocation rate limit is shared by every share: a mass revocation in one share delays revocations in others.

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

A ValidatingAdmissionPolicy (`config/policy`) bounds what the controller's service account can do with those permissions, so a compromised controller cannot turn them into node or file-system access. It may create only CSI volumes of the supported drivers named `sfs-*` with a sharedfs handle, delete only its own volumes and claims bound to them, and run only its preparer and quota pods (image by digest, only `CHOWN` or only `SYS_ADMIN`, no token, only their own internal claim).

The `sharedfs-share-viewer` ClusterRole lets its holders list shares. A share's status shows the file system's DNS name, mount name and ID, so bind it only to users who may see those.

| Flag | Default | Meaning |
|---|---|---|
| `--revocation-grace-period` | `15m` | How long a claim stays after its namespace loses the grant |
| `--revocations-per-minute` | `30` | Most claims deleted by revocation per minute, cluster-wide |
| `--provisioner-concurrency` | `4` | Claims handled at once |
| `--kube-api-qps`, `--kube-api-burst` | `50`, `100` | Client rate limits towards the API server |
| `--quota-namespace`, `--quota-image` | none | Turn on quotas (see Quotas) |
| `--quota-service-account` | `sharedfs-quota` | Service account of quota pods |
| `--project-id-base` | `1000000` | First Lustre project ID sharedfs hands out |
| `--preparer-node-selector` | none | `key=value,...` for preparer pods, for example nodes exempt from root squash |
| `--preparer-tolerations` | none | JSON list of tolerations for preparer pods |
| `--preparer-service-account` | `sharedfs-preparer` | Service account of preparer pods; it is bound to nothing and its token is not mounted |
| `--preparer-image` | the controller's own image, pinned to the digest it runs | Image of preparer pods; the controller refuses to start if it cannot learn its digest |
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
| FilesystemShare | `Ready` | `Resolved`; `SourceNotFound`, `SourceNotCSI`, `UnsupportedDriver`, `SourceIsGenerated`, `InvalidSource`, `SourceUnreserved`, `SourceChanged`, `InvalidPath`, `SubPathUnsupported`, `InvalidGrant`, `DriverNotInstalled`, `UnsafeDriverPolicy`, `OverlappingShare`, `StorageClassTaken`, `VolumesRemaining` |
| FilesystemShareDirectory | `Prepared` | `DirectoryCreated`, `DirectoryExists`; `Preparing`, `PreparerNotCreated`, `PreparerTimedOut`, `InsecureParent`, `NotADirectory`, `PermissionDenied`, `PreparationFailed` |
| FilesystemShareDirectory | `QuotaApplied` | `QuotaApplied`; `QuotaPending`, `QuotaNotConfigured`, `DirectoryNotTagged`, `QuotaFailed`, `PreparerNotCreated`, `PreparerTimedOut` |
| PersistentVolumeClaim | events | `Provisioned`; `NotGranted`, `UnsupportedClaim`, `ShareNotReady`, `NamespaceRecreated`, `DirectoryNotReady`, `Revoking`, `RevocationHeld`, `Revoked` |

The metrics endpoint serves, besides controller-runtime's own:

| Metric | Use |
|---|---|
| `sharedfs_share_ready{share,status,reason}` | Alert on any share not Ready |
| `sharedfs_claims_pending_total{reason}` | Rate of claims left Pending, by reason |
| `sharedfs_provision_latency_seconds` | Claim creation to volume creation |
| `sharedfs_revocations_total{outcome}` | `scheduled`, `cancelled`, `executed`, `held`, `ratelimited`: alert on a burst of `scheduled` |
| `sharedfs_task_outcomes_total{task,reason}` | Preparer and quota pod results: alert on anything but `DirectoryCreated`, `DirectoryExists`, `QuotaApplied` |
| `sharedfs_volumes_provisioned_total`, `sharedfs_volumes_reclaimed_total` | Volume churn |

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

[test/e2e-eks/quota.sh](test/e2e-eks/quota.sh) checks quotas on a Lustre 2.15 file system: the project ID and limits of a new directory, a tenant stopped at the limit, a raised limit, and a directory that existed before. [test/e2e-eks/scale.sh](test/e2e-eks/scale.sh) creates many claims at once, kills the leader while they bind and again during revocation, and stops the controller while claims come and go.

The preparer's file system logic is also tested as root with only `CAP_CHOWN`, in a container:

```bash
CGO_ENABLED=0 GOOS=linux go test -c -o /tmp/prepare.test ./internal/prepare
docker run --rm --cap-drop ALL --cap-add CHOWN -v /tmp:/t:ro busybox:1.36 /t/prepare.test -test.v
```

See [docs/DESIGN.md](docs/DESIGN.md) for the reasoning behind each choice.

## License

Apache License 2.0.
