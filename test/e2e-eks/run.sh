#!/usr/bin/env bash
# End-to-end check of sharedfs on a real cluster with a real FSx for Lustre file system.
#
# Usage: test/e2e-eks/run.sh <source PV name> <node name>
#   source PV  an existing static PV of fsx.csi.aws.com, reserved (claimRef set) or bound
#   node       a node that can mount it; tenant pods run there, whatever its taints
#   CTRL_NS    the controller's namespace (default sharedfs-system)
#   GRACE      the controller's --revocation-grace-period in seconds (default 30)
#
# Creates namespaces sfse2e-<id>-{admin,team,reader,none} and FilesystemShares sfse2e-<id>-*,
# writes only below the directory sfse2e-<id> of the file system, and removes all of it at the
# end. The source PV is only read.
set -uo pipefail

SRC=${1:?source PV}; NODE=${2:?node}
ID=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c 5)
P=sfse2e-$ID
NS_ADMIN=$P-admin; NS_TEAM=$P-team; NS_READER=$P-reader; NS_NONE=$P-none
KEY=sharedfs-e2e/$P
GRACE=${GRACE:-30}
CTRL_NS=${CTRL_NS:-sharedfs-system}
G=sharedfs.littlemex.github.io
PASS=0; FAIL=0

ok()   { PASS=$((PASS+1)); echo "[PASS] $*"; }
ng()   { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
step() { echo; echo "== $*"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else ng "$d"; fi; }
# wait_for <seconds> <command...>: poll until the command succeeds.
wait_for() { local t=$1; shift; for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
ready() { kubectl get fss "$1" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}/{.status.conditions[?(@.type=="Ready")].reason}'; }
ready_is() { [ "$(ready "$1")" = "$2" ]; }
phase_is() { [ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$3" ]; }
event_seen() { kubectl -n "$1" get events --field-selector "involvedObject.name=$2,reason=$3" -o name 2>/dev/null | grep -q .; }
running() { [ "$(kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; }
sh_in() { kubectl -n "$1" exec "$2" -- sh -c "$3"; }
not() { ! "$@"; }
gone() { ! kubectl get "$@" >/dev/null 2>&1; }

share() { # share <name> <path> <grants yaml> [provision yaml] [annotations yaml]
  kubectl apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: FilesystemShare
metadata: {name: $1, annotations: {${5:-}}}
spec:
  persistentVolumeRef: {name: $SRC}
  path: "$2"
  grants: $3
  ${4:-}
EOF
}
pvc() { # pvc <ns> <name> <class> [accessMode]
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $2, namespace: $1}
spec: {accessModes: [${4:-ReadWriteMany}], storageClassName: $3, resources: {requests: {storage: 1Gi}}}
EOF
}
pod() { # pod <ns> <name> <claim> [uid] [gid]
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  tolerations: [{operator: Exists}]
  terminationGracePeriodSeconds: 1
  restartPolicy: Never
  securityContext: {runAsUser: ${4:-0}, runAsGroup: ${5:-0}, supplementalGroups: [${5:-0}]}
  containers:
  - name: c
    image: public.ecr.aws/docker/library/busybox:1.36
    command: [sleep, "3600"]
    volumeMounts: [{name: v, mountPath: /shared}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: $3}}]
EOF
}

SRC_RV=$(kubectl get pv "$SRC" -o jsonpath='{.metadata.resourceVersion}')
cleanup() {
  step cleanup
  kubectl -n "$NS_ADMIN" exec root -- rm -rf "/shared/$P" >/dev/null 2>&1
  kubectl delete ns "$NS_ADMIN" "$NS_TEAM" "$NS_READER" "$NS_NONE" --wait=false >/dev/null 2>&1
  for s in $(kubectl get fss -o name 2>/dev/null | grep "/$P"); do kubectl delete "$s" --wait=false >/dev/null 2>&1; done
  wait_for 240 sh -c "! kubectl get fss -o name | grep -q /$P"
  echo "left over: shares $(kubectl get fss -o name | grep -c "/$P"), tenant PVs $(kubectl get pv -o name | grep -c '/sfs-[0-9a-f]\{8\}-'), preparer PVs $(kubectl get pv -o name | grep -c '/sfs-prep-')"
  echo "RESULT pass=$PASS fail=$FAIL"
}
trap cleanup EXIT

step "setup on $SRC (phase $(kubectl get pv "$SRC" -o jsonpath='{.status.phase}'))"
for ns in "$NS_ADMIN" "$NS_TEAM" "$NS_READER" "$NS_NONE"; do kubectl create ns "$ns" >/dev/null; done
kubectl label ns "$NS_ADMIN" "$KEY=admin" >/dev/null
kubectl label ns "$NS_TEAM" "$KEY=team" >/dev/null
kubectl label ns "$NS_READER" "$KEY=reader" >/dev/null
sel() { echo "{namespaceSelector: {matchLabels: {$KEY: $1}}, access: $2}"; }

share "$P-root" "/" "[$(sel admin ReadWrite)]" "" "$G/allow-overlap: \"true\""
check "admin root share Ready with allow-overlap" wait_for 60 ready_is "$P-root" True/Resolved
pvc "$NS_ADMIN" root "$P-root-rw"
check "admin claim Bound" wait_for 60 phase_is "$NS_ADMIN" root Bound
pod "$NS_ADMIN" root root
check "admin pod running" wait_for 180 running "$NS_ADMIN" root
NONCE=$P-$(date +%s)
sh_in "$NS_ADMIN" root "mkdir -m 0755 /shared/$P && mkdir -m 0755 /shared/$P/data && echo $NONCE > /shared/$P/data/nonce && ln -s / /shared/$P/link"
check "admin prepared the test tree" sh_in "$NS_ADMIN" root "[ -f /shared/$P/data/nonce ] && [ -L /shared/$P/link ]"

step "1. a per-namespace directory is created by the preparer, then provisioned"
share "$P-team" "$P/{namespace}" "[$(sel team ReadWrite)]" "provision: {gid: 2000, mode: \"2770\"}"
check "team share Ready" wait_for 60 ready_is "$P-team" True/Resolved
T0=$(date +%s)
pvc "$NS_TEAM" work "$P-team-rw" ReadWriteOnce
check "team claim Bound (RWO requested, as Helm charts do)" wait_for 300 phase_is "$NS_TEAM" work Bound
echo "seconds from claim to Bound, including the preparer: $(( $(date +%s) - T0 ))"
kubectl get fsd -o wide | grep -E "NAME|$NS_TEAM"
check "the directory record is Prepared/DirectoryCreated" sh -c "kubectl get fsd -o jsonpath='{range .items[?(@.spec.namespace==\"$NS_TEAM\")]}{.status.conditions[0].reason}{end}' | grep -qx DirectoryCreated"
check "the directory is root:2000 2770 on the file system" sh -c "[ \"\$(kubectl -n $NS_ADMIN exec root -- stat -c '%u:%g %a' /shared/$P/$NS_TEAM)\" = '0:2000 2770' ]"
check "no temporary directory is left" sh -c "! kubectl -n $NS_ADMIN exec root -- ls -a /shared/$P | grep -q sharedfs-tmp"
check "the preparer pod, claim and volume are gone" wait_for 60 sh -c "! kubectl -n $CTRL_NS get pod,pvc -o name | grep -q sfs-prep- && ! kubectl get pv -o name | grep -q sfs-prep-"
pod "$NS_TEAM" w work 1000 2000
check "team pod (uid 1000, group 2000) running" wait_for 180 running "$NS_TEAM" w
check "team pod writes its directory" sh_in "$NS_TEAM" w "echo $NONCE > /shared/mine && [ \"\$(stat -c %g /shared/mine)\" = 2000 ]"
check "team pod sees only its own directory" sh_in "$NS_TEAM" w "[ \"\$(ls -A /shared)\" = mine ]"
WORK_PV=$(kubectl -n "$NS_TEAM" get pvc work -o jsonpath='{.spec.volumeName}')
kubectl get pv "$WORK_PV" -o custom-columns=NAME:.metadata.name,MODES:.spec.accessModes,OPTS:.spec.mountOptions,SUB:.spec.csi.volumeAttributes.mountname,FIN:.metadata.finalizers

step "2. a read-only share for another namespace"
share "$P-data" "$P/data" "[$(sel reader ReadOnly), $(sel team ReadOnly)]"
check "data share Ready" wait_for 60 ready_is "$P-data" True/Resolved
pvc "$NS_READER" data "$P-data-ro" ReadOnlyMany
check "reader claim Bound (ROX requested)" wait_for 60 phase_is "$NS_READER" data Bound
pod "$NS_READER" r data 1000 1000
check "reader pod running" wait_for 180 running "$NS_READER" r
check "reader reads the admin's file" sh -c "[ \"\$(kubectl -n $NS_READER exec r -- cat /shared/nonce)\" = $NONCE ]"
check "reader cannot write (read-only mount)" sh -c "kubectl -n $NS_READER exec r -- sh -c 'echo x > /shared/x' 2>&1 | grep -q 'Read-only file system'"
check "reader sees only the data directory" sh_in "$NS_READER" r "[ \"\$(ls -A /shared)\" = nonce ]"
sh_in "$NS_READER" r "grep ' /shared ' /proc/mounts"
check "the read-only mount uses localflock" sh_in "$NS_READER" r "grep ' /shared ' /proc/mounts | grep -q localflock"

step "3. claims that must not be provisioned"
pvc "$NS_NONE" data "$P-data-ro"
check "ungranted namespace: Pending with NotGranted" wait_for 60 event_seen "$NS_NONE" data NotGranted
check "  and still Pending" phase_is "$NS_NONE" data Pending
pvc "$NS_READER" rw "$P-data-rw"
check "read-write class of a read-only grant: NotGranted" wait_for 60 event_seen "$NS_READER" rw NotGranted
pvc "$NS_TEAM" rwop "$P-data-ro" ReadWriteOncePod
check "ReadWriteOncePod: UnsupportedClaim" wait_for 60 event_seen "$NS_TEAM" rwop UnsupportedClaim
check "expansion of a provisioned claim is refused" sh -c "! kubectl -n $NS_TEAM patch pvc work -p '{\"spec\":{\"resources\":{\"requests\":{\"storage\":\"2Gi\"}}}}' 2>/dev/null"

step "4. a generic ephemeral volume (one PV per pod, created and deleted with it)"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: eph, namespace: $NS_TEAM}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  tolerations: [{operator: Exists}]
  terminationGracePeriodSeconds: 1
  restartPolicy: Never
  containers:
  - {name: c, image: public.ecr.aws/docker/library/busybox:1.36, command: [sleep, "3600"], volumeMounts: [{name: d, mountPath: /shared}]}
  volumes:
  - name: d
    ephemeral:
      volumeClaimTemplate:
        spec: {accessModes: [ReadOnlyMany], storageClassName: $P-data-ro, resources: {requests: {storage: 1Gi}}}
EOF
check "ephemeral pod running" wait_for 180 running "$NS_TEAM" eph
check "it reads the data" sh -c "[ \"\$(kubectl -n $NS_TEAM exec eph -- cat /shared/nonce)\" = $NONCE ]"
EPH_PV=$(kubectl -n "$NS_TEAM" get pvc eph-d -o jsonpath='{.spec.volumeName}')
kubectl -n "$NS_TEAM" delete pod eph --wait=true >/dev/null
check "its claim and volume go with the pod" wait_for 120 sh -c "! kubectl -n $NS_TEAM get pvc eph-d >/dev/null 2>&1 && ! kubectl get pv $EPH_PV >/dev/null 2>&1"

step "5. a symlink as the share's directory is not followed"
share "$P-link" "$P/link" "[$(sel admin ReadOnly)]"
check "link share Ready (the controller cannot see the file system)" wait_for 60 ready_is "$P-link" True/Resolved
pvc "$NS_ADMIN" link "$P-link-ro"
check "its claim is Bound" wait_for 60 phase_is "$NS_ADMIN" link Bound
pod "$NS_ADMIN" l link
sleep 60
check "but the pod never starts: Lustre refuses the symlink" not running "$NS_ADMIN" l
kubectl -n "$NS_ADMIN" get events --field-selector involvedObject.name=l,reason=FailedMount -o custom-columns=MSG:.message 2>/dev/null | tail -1 | cut -c1-200
kubectl -n "$NS_ADMIN" delete pod l --wait=false >/dev/null

step "6. a new outer read-write share does not stop shares already in use"
share "$P-outer" "$P" "[$(sel team ReadWrite)]"
check "outer share not Ready: OverlappingShare" wait_for 60 ready_is "$P-outer" False/OverlappingShare
check "the inner team share stays Ready" ready_is "$P-team" True/Resolved
check "the inner data share stays Ready" ready_is "$P-data" True/Resolved
kubectl get fss "$P-outer" -o jsonpath='{.status.conditions[0].message}'; echo
pvc "$NS_TEAM" outer "$P-outer-rw"
check "claims of the outer share are not provisioned" wait_for 60 event_seen "$NS_TEAM" outer ShareNotReady

step "7. revoking a namespace: grace period, running pods keep their mount"
kubectl label ns "$NS_READER" "$KEY-" >/dev/null
DATA_PV=$(kubectl -n "$NS_READER" get pvc data -o jsonpath='{.spec.volumeName}')
check "the deadline is recorded on the PV" wait_for 30 sh -c "kubectl get pv $DATA_PV -o jsonpath='{.metadata.annotations}' | grep -q withdraw-after"
sleep $((GRACE/2))
check "PVC not deleted during the grace period" sh -c "[ -z \"\$(kubectl -n $NS_READER get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
check "PVC Terminating after the grace period" wait_for $((GRACE+30)) sh -c "[ -n \"\$(kubectl -n $NS_READER get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
check "the running pod still reads" sh -c "[ \"\$(kubectl -n $NS_READER exec r -- cat /shared/nonce)\" = $NONCE ]"
kubectl -n "$NS_READER" delete pod r --wait=true >/dev/null
check "PVC and PV are gone once the pod stops (Released, then deleted by sharedfs)" wait_for 120 sh -c "! kubectl -n $NS_READER get pvc data >/dev/null 2>&1 && ! kubectl get pv $DATA_PV >/dev/null 2>&1"

step "8. a namespace deleted and created again does not inherit the directory"
kubectl -n "$NS_TEAM" delete pod w --wait=true >/dev/null
kubectl delete ns "$NS_TEAM" --wait=true --timeout=180s >/dev/null
kubectl create ns "$NS_TEAM" >/dev/null; kubectl label ns "$NS_TEAM" "$KEY=team" >/dev/null
pvc "$NS_TEAM" work "$P-team-rw"
check "the new namespace's claim: NamespaceRecreated" wait_for 60 event_seen "$NS_TEAM" work NamespaceRecreated
check "  and Pending" phase_is "$NS_TEAM" work Pending
check "the earlier team's file is still there, untouched" sh -c "[ \"\$(kubectl -n $NS_ADMIN exec root -- cat /shared/$P/$NS_TEAM/mine)\" = $NONCE ]"
kubectl delete fsd "$(kubectl get fsd -o jsonpath="{.items[?(@.spec.namespace==\"$NS_TEAM\")].metadata.name}")" >/dev/null
check "after an admin deletes the record, the claim is Bound (the directory already exists)" wait_for 120 phase_is "$NS_TEAM" work Bound
check "  and the record says DirectoryExists" wait_for 30 sh -c "kubectl get fsd -o jsonpath='{range .items[?(@.spec.namespace==\"$NS_TEAM\")]}{.status.conditions[0].reason}{end}' | grep -qx DirectoryExists"

step "9. a share waits for its volumes, and the source is never touched"
kubectl delete fss "$P-team" --wait=false >/dev/null
check "team share reports VolumesRemaining" wait_for 30 sh -c "kubectl get fss $P-team -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].reason}' | grep -qx VolumesRemaining"
kubectl get fss "$P-team" -o jsonpath='{.metadata.deletionTimestamp} volumes={.status.volumes} {.status.conditions[0].reason}: {.status.conditions[0].message}{"\n"}' 2>&1
kubectl -n "$NS_TEAM" delete pvc work --wait=false >/dev/null
check "then lets go once its claims are gone" wait_for 120 gone fss "$P-team"
check "the source PV is unchanged" sh -c "[ \"\$(kubectl get pv $SRC -o jsonpath='{.metadata.resourceVersion}')\" = $SRC_RV ]"
check "the controller logged no reconciler errors" sh -c "! kubectl -n $CTRL_NS logs deploy/sharedfs-controller-manager --since=1h | grep -q 'Reconciler error'"
