# Proposal: per-path shares for LLM training and inference (v1alpha1, unpublished)

## Goal

Make one existing FSx for Lustre file system usable by many teams and jobs on EKS, the way LLM training and inference actually use it:

| Use | What is needed |
|---|---|
| Training datasets | One large tree, read-only, to many namespaces |
| Model weights for inference | One tree, read-only, to many serving namespaces |
| Checkpoints and outputs | A writable directory per team (and per job under it), not visible to other teams' mounts |
| Shared scratch | A writable tree for a group of namespaces |

Today the controller can only hand out the whole file system. The driver issue kubernetes-sigs/aws-fsx-csi-driver#247 (open since 2022, frozen, "no updates") asks for exactly the missing piece: a directory per claim on an existing file system.

## Measured (EKS 1.35, FSx for Lustre 2.15 client on AL2023, aws-fsx-csi-driver 5ede23e)

| # | Observation |
|---|---|
| M9 | A static PV with `volumeAttributes.mountname: <mountname>/<dir>` mounts only that directory: `<dns>@tcp:/<mountname>/s3data` showed the directory's content at the mount root, not the file system root. No driver change; the driver builds the source as `<dnsname>@tcp:/<mountname>` |
| M10 | A missing directory fails the mount (`MountVolume.SetUp failed ... Could not mount`), the pod stays ContainerCreating |
| M11 | A symlink as the last component (relative `link -> secret`, absolute `abslink -> /.fssprobe/secret`) fails the mount; the path is not followed |
| M12 | A symlink as an intermediate component (`.fssprobe/link/inner`, where `link -> secret` and `secret/inner` exists, and `ls link/inner` works through a root mount) also fails the mount |
| M13 | fsx.openzfs.csi.aws.com refuses any VolumePath other than the root for ResourceType filesystem (`volumePath must match the root volume path`), so sub-paths are a Lustre feature (and later csi-driver-nfs, whose source is `server:share[/subDir]`) |
| M14 | PVC expansion of a generated PVC is refused by the API server (`only dynamically provisioned pvc can be resized`), so the suffixed volumeHandle never reaches ControllerExpandVolume |
| M15 | The Lustre driver's CreateVolume always creates a new file system; there is no "existing file system" mode to extend upstream without the controller plugin mounting Lustre itself |

M11 and M12 matter most: Lustre resolves the fileset path itself and does not follow symlinks, so a tenant who can write inside its directory cannot redirect a deeper claim path to another team's data. A swap only makes the mount fail.

## API (renamed)

Kinds become `FilesystemShare` (cluster-scoped, admin) and `FilesystemShareClaim` (namespaced, tenant). Group and project name are open (see the end).

```yaml
kind: FilesystemShare
metadata: {name: training-fsx}
spec:
  persistentVolumeRef: {name: fsx-training}
  grants:
  - namespaceSelector: {matchLabels: {ml.example.com/team: ""}}   # every team namespace
    access: ReadOnly
    path: datasets
  - namespaceSelector: {matchLabels: {ml.example.com/team: ""}}
    access: ReadWrite
    path: checkpoints/{namespace}          # expanded per claiming namespace
    provision: {uid: 1000, gid: 1000, mode: "0770"}   # create the directory if missing
  - namespaceSelector: {matchLabels: {ml.example.com/serving: ""}}
    access: ReadOnly
    path: models
---
kind: FilesystemShareClaim
metadata: {name: ckpt, namespace: team-a}
spec:
  shareRef: {name: training-fsx}
  access: ReadWrite
  path: checkpoints/team-a                 # default "" = root; must lie within a matching grant's path
status:
  path: checkpoints/team-a
  persistentVolumeName: fsc-<uid>
  conditions: [Accepted, DirectoryReady, Bound, Ready]
```

Rules:

- `path` is relative, slash-separated, segments `[A-Za-z0-9._-]+`, no `.` or `..`, at most 32 segments and 1024 bytes (CEL). `{namespace}` is the only placeholder, grant-side only.
- A claim is accepted when some grant selects its namespace, allows its access, and its expanded `path` equals or lies below the claim path (segment prefix, not string prefix). Strongest access wins among matching grants.
- The generated PV sets the driver's sub-path attribute (Lustre: `mountname` joined with the path). Drivers without sub-path support accept only `path: ""`.
- Grants without `provision` never create anything; the directory must exist.

## Directory preparation

A claim with a non-empty path gets `DirectoryReady` before any PV is created.

- The controller runs a one-shot Job, the *preparer*, in its own namespace. It mounts the share's root through an internal static PV (`Retain`, own volumeHandle, claimRef to the controller namespace), and runs the controller image with a `prepare-directory` subcommand (no shell, distroless).
- The preparer walks the path from the root one component at a time with `openat(O_DIRECTORY|O_NOFOLLOW)`. If `provision` applies, it creates the missing components of the grant path only: intermediate ones `root:root 0755`, the last one with the grant's uid, gid and mode, and never changes an existing directory. It then verifies that the claim path exists and is a real directory. The result goes to the claim's `DirectoryReady` condition (reasons `DirectoryCreated`, `DirectoryExists`, `DirectoryNotFound`, `NotADirectory`, `PreparationFailed`).
- Nothing is ever deleted: releasing a claim deletes its PVC and PV, never the directory.
- The preparer's node placement (nodeSelector, tolerations) comes from controller flags. It needs a node with the Lustre client and network access to the file system, like any consumer pod.

## Unchanged

Grants evaluated first, revocation after a grace period with a live re-read, `Retain` PVs with suffixed handles, claimRef pre-binding, PV created before the PVC, Released healing, orphan sweep, the source PV only read.

## Upstream path

The pieces are separated so the sub-path part could be offered to the FSx driver later:

| Piece | Where it lives | Upstream candidate |
|---|---|---|
| Sub-path PV construction (mountname join, validation) | `internal/subpath` (pure) | Yes: as static-provisioning documentation now, and as the PV template of a future "existing file system" CreateVolume mode |
| Directory preparation (no-follow walk, create, verify) | `internal/prepare` | Yes, if the driver's controller plugin gains a Lustre mount; that is the blocker named in M15 |
| Grants, revocation, healing, orphan sweep | controllers | No: policy above the CSI layer |

## Open questions for review

1. Should claims be allowed to go deeper than the grant path (per-job directories), given M11/M12, or only equal it?
2. Is a Job-based preparer the right mechanism, versus a long-running privileged helper DaemonSet or requiring admins to pre-create directories?
3. Ownership: fixed uid/gid per grant, or taken from the claiming namespace (annotation)?
4. Names: project, API group, Kind.
