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

Each directory record gets a project ID of its own, from `--project-id-base` (default 1000000) up, never handed out twice: the next free ID is kept in the ConfigMap `sharedfs-project-ids`, so deleting a record does not free its ID. The preparer gives the ID to the directory when it creates it, with inheritance, so everything created below it counts. Before a limit is first set, the quota pod checks that the project counts nothing yet (`ProjectInUse` otherwise). Clusters that share one file system need ranges of their own: give each its own `--project-id-base`. A quota pod then sets the limit, and sets it again whenever the share's limits change. The quota pod runs as UID 0 with the share's group and only `CAP_SYS_ADMIN`, which `lfs setquota` requires; the directory must be owned by root or give that group read and search, as the default `2770` does. It mounts only that directory, read-write because Lustre refuses quota changes through a read-only mount, and has no token and no network. Lustre enforces quotas in units it grants to each storage target, so writes overshoot the limit somewhat: with a 50 MiB limit, writing stopped at 53 to 76 MiB across runs. A claim is provisioned only once its directory has a limit; while a changed limit is applied, the earlier one holds and claims keep being provisioned. At most `--quota-max-pods` (10) quota pods run at once. A directory that existed before the share had a quota is not tagged: its record reports `DirectoryNotTagged` with the `lfs project` command an admin can run, the limit applies on a later retry, and until then new claims for that directory wait. Adding a quota to a share in use therefore pauses new claims of its existing directories. Removing the quota clears the limits; deleting the share leaves them on the file system.

A quota holds only against non-root writers. On Lustre a root process can change a file's project ID, even without `CAP_FOWNER`, and so take files out of the quota or into another team's project; a non-root owner cannot (both measured). A root writer can do this through any share of the file system, with a quota or not. So on a file system where any share has a quota, read-write claims of every share are provisioned only in namespaces that enforce Pod Security `restricted` (`UnsupportedClaim` otherwise), and each such share's `Ready` message names granted namespaces that do not. A non-root owner cannot clear the inheritance either (measured), so files below a directory always count. Where root squash keeps pods from writing as root, `--quota-requires-restricted=false` lifts this; preparer and quota pods then need nodes exempt from root squash, which should be nodes tenants cannot use. An existing directory an admin tags is limited on the next retry, and the check that its project counts nothing is only made for directories sharedfs created. The parent of the directories must not carry a project ID with inheritance itself: the kernel refuses to move a directory of another project into it, and the record reports why. Quota pods need a namespace of their own at Pod Security `privileged`, and an image with `lfs`:

```bash
kubectl apply -k config/quota
docker build -f Dockerfile.quota -t registry.example.com/sharedfs-quota:dev . && docker push registry.example.com/sharedfs-quota:dev
```

Then start the controller with `--quota-namespace=sharedfs-quota --quota-image=registry.example.com/sharedfs-quota@sha256:...`.

## What it does not do

- **It is not isolation against privileged workloads.** The boundary is the mount. A pod that is privileged, uses `hostPath`, or can create PersistentVolumes can reach the whole file system without sharedfs. Run tenant namespaces at Pod Security `baseline` or stricter.
- **"No network" for task pods relies on NetworkPolicy being enforced** (for VPC CNI, its network policy agent turned on).
- **The directory restriction is applied by the Lustre client.** The servers do not enforce it. From inside a directory mount, `.lustre/fid` is not visible and `fid2path` outside the directory is refused (measured on 2.12 and 2.15); A pod with `CAP_DAC_READ_SEARCH` can open files outside its directory by handle (measured); Pod Security baseline does not grant it, and no sharedfs pod has it.
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

It runs in the namespace `sharedfs-system` as two replicas, spread across nodes when the scheduler can, with a PodDisruptionBudget; only the leader reconciles, and a standby takes over within the lease duration. The namespace is labelled Pod Security `baseline`, because preparer pods need `CAP_CHOWN`, and nothing else should run there or be granted pod access there. If Kueue manages plain pods, exclude this namespace and `sharedfs-quota`. Every PersistentVolume and PersistentVolumeClaim of the cluster is cached: the leader used about 45 MiB with 800 of each, so the 512 MiB limit leaves room for clusters about ten times larger. The revocation rate limit is shared by every share: a mass revocation in one share delays revocations in others.

The controller needs no cloud credentials. Its permissions are:

| Resource | Verbs |
|---|---|
| PersistentVolumes | read, create, patch, delete |
| PersistentVolumeClaims | read and delete in every namespace; create in its own and in `sharedfs-quota` |
| Pods | read, create, delete in its own namespace and in `sharedfs-quota` |
| ConfigMaps | read, create, update in its own namespace (project IDs) |
| StorageClasses | read, create |
| Namespaces, CSIDrivers | read |
| FilesystemShares | read, patch, status, finalizers |
| FilesystemShareDirectories | read, create, patch, status, finalizers |
| Events | create, patch |

ValidatingAdmissionPolicies (`config/policy`, one per kind) bound what the controller's service account can do with those permissions, so a compromised controller cannot turn them into node access. It may create only CSI volumes of the supported drivers named `sfs-*`, with a sharedfs handle, reserved for a claim by UID; create only its internal claims; delete only its own volumes and claims bound to them; and run only its preparer and quota pods. Those must come from the repositories in the `sharedfs-guard` ConfigMap, by digest, with only `CHOWN` (preparer) or only `SYS_ADMIN` (quota), running the controller's binary with no command, hooks, probes or environment, never restarted and with a deadline, without privilege, host namespaces or a token, with RuntimeDefault seccomp, and mounting only their own internal claim. Set the repositories in `config/policy/controller_guard.yaml` before deploying. The controller cannot change them: its only ConfigMap permission is on `sharedfs-project-ids`, which the install creates. A volume the controller creates can still point at any directory of a supported file system, so the controller remains part of the trusted base for file-system access. `config/quota` adds a policy that lets nobody but the controller run pods in `sharedfs-quota` or exec into them.

Grants select namespaces by label, so tenants must not be able to set those labels. [config/samples/namespace-label-guard.yaml](config/samples/namespace-label-guard.yaml) is a policy to adapt: it lets only an admin group set labels under a prefix, and the Pod Security labels.

The `sharedfs-share-viewer` ClusterRole lets its holders list shares. A share's status shows the file system's DNS name, mount name and ID, so bind it only to users who may see those.

| Flag | Default | Meaning |
|---|---|---|
| `--revocation-grace-period` | `15m` | How long a claim stays after its namespace loses the grant |
| `--revocations-per-minute` | `30` | Most claims deleted by revocation per minute, cluster-wide |
| `--provisioner-concurrency` | `4` | Claims handled at once |
| `--kube-api-qps`, `--kube-api-burst` | `50`, `100` | Client rate limits towards the API server |
| `--quota-namespace`, `--quota-image` | none | Turn on quotas (see Quotas) |
| `--quota-service-account` | `sharedfs-quota` | Service account of quota pods |
| `--quota-max-pods` | `10` | Quota pods running at once |
| `--preparer-max-pods` | `20` | Preparer pods running at once |
| `--quota-requires-restricted` | `true` | Provision read-write claims of shares with a quota only in namespaces at Pod Security `restricted` |
| `--project-id-base` | `1000000` | First Lustre project ID sharedfs hands out |
| `--preparer-node-selector` | none | `key=value,...` for preparer pods, for example nodes exempt from root squash |
| `--preparer-tolerations` | none | JSON list of tolerations for preparer pods |
| `--preparer-service-account` | `sharedfs-preparer` | Service account of preparer pods; it is bound to nothing and its token is not mounted |
| `--preparer-image` | the controller's own image, pinned to the digest it runs | Image of preparer pods, by digest; the controller refuses to start without one |
| `--leader-elect` | `true` | Keep on: two active controllers would race on the same volumes |

To remove sharedfs, delete the claims, then the shares, then the controller (`make undeploy`). Deleting the CRDs first deletes every share at once; deleting the controller first leaves the finalizers with no one to remove them.

## Operations

- **Versions.** Kubernetes 1.30 or later (ValidatingAdmissionPolicy is GA there); verified on EKS 1.35 with the FSx for Lustre CSI driver add-on. Run `test/e2e-eks/run.sh` (and `quota.sh` with quotas) before upgrading the driver add-on: sharedfs depends on how it joins `mountname`.
- **Upgrades.** v1alpha1 has no conversion. Apply the CRDs first, then the controller and the quota image together (the controller passes flags the quota image must know). The controller can be replaced at any time: a standby takes over, and deadlines, records and project IDs are all stored in the API.
- **Backup and restore.** Back up the FilesystemShares, FilesystemShareDirectories, the provisioned PersistentVolumes and the ConfigMap `sharedfs-project-ids` together, and restore with UIDs preserved where the tool allows. Records are named after the share's UID, so a share restored under a new UID starts with no records: its claims are held (`RevocationHeld`) instead of revoked, the namespace pinning of its directories is lost, and directories with a quota report `DirectoryNotTagged` until an admin tags them with their new project IDs. Namespaces restored under new UIDs report `NamespaceRecreated`. Keep `sharedfs-project-ids` at least as high as before, so no ID is handed out twice. Exclude these volumes from file-system backups of the PVC kind: every one of them is a directory of the same shared file system.
- **A directory removed by hand.** A record stays Prepared, so mounts fail; delete the record, then create a new claim (records are made for Pending claims), and the directory is prepared again.
- **Quota pods pull with the node's credentials** (or a pull secret you add to the `sharedfs-quota` service account), not the controller's pull secrets.
- **Not yet verified**: Bottlerocket and arm64 nodes, Karpenter cold starts for task pods, controller upgrade and rollback, Velero restores, more than 1,000 claims, Lustre 2.15 on PERSISTENT_2.
- **Alerts.** A share not Ready (`sharedfs_share_ready{status="false"}`), a burst of `sharedfs_revocations_total{outcome="scheduled"}`, any `held`, and task outcomes other than success.

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
| FilesystemShareDirectory | `QuotaApplied` | `QuotaApplied`; `QuotaPending`, `QuotaNotConfigured`, `DirectoryNotTagged`, `ProjectInUse`, `QuotaFailed`, `PreparerNotCreated`, `PreparerTimedOut` |
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

[test/e2e-eks/density.sh](test/e2e-eks/density.sh) runs many pods with a volume each on one node. [test/e2e-eks/quota.sh](test/e2e-eks/quota.sh) checks quotas on a Lustre 2.15 file system: the project ID and limits of a new directory, a tenant stopped at the limit, a raised limit, and a directory that existed before. [test/e2e-eks/scale.sh](test/e2e-eks/scale.sh) creates many claims at once, kills the leader while they bind and again during revocation, and stops the controller while claims come and go.

The preparer's file system logic is also tested as root with only `CAP_CHOWN`, in a container:

```bash
CGO_ENABLED=0 GOOS=linux go test -c -o /tmp/prepare.test ./internal/prepare
docker run --rm --cap-drop ALL --cap-add CHOWN -v /tmp:/t:ro busybox:1.36 /t/prepare.test -test.v
```

See [docs/DESIGN.md](docs/DESIGN.md) for the reasoning behind each choice.

## License

Apache License 2.0.
