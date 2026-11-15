#!/usr/bin/env bash
# End-to-end check of sharedfs on a real cluster with a real FSx file system.
#
# Usage: test/e2e-eks/run.sh <source PV name> <node name> [<label prefix>]
#   source PV  an existing static PV of fsx.csi.aws.com or fsx.openzfs.csi.aws.com
#   node       a node that can mount it; both probe pods run there (same-node rw and ro mounts)
#
# Creates namespaces sfse2e-<id>-{rw,ro,none} and a SharedFilesystem sfse2e-<id>, and removes
# them at the end. The source PV and the file system are only read; one directory
# .sharedfs-e2e-<id> is written on the file system and removed.
set -uo pipefail

SRC=${1:?source PV}; NODE=${2:?node}
ID=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c 5)
P=sfse2e-$ID; NS_RW=$P-rw; NS_RO=$P-ro; NS_NONE=$P-none
KEY=sharedfs-e2e/$P
DIR=/shared/.sharedfs-e2e-$ID
GRACE=${GRACE:-30}
PASS=0; FAIL=0

ok()   { PASS=$((PASS+1)); echo "[PASS] $*"; }
ng()   { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
step() { echo; echo "== $*"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else ng "$d"; fi; }
# wait_for <seconds> <command...>: poll until the command succeeds.
wait_for() { local t=$1; shift; for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
cond() { kubectl -n "$1" get sfc "$2" -o jsonpath="{.status.conditions[?(@.type==\"$3\")].status}/{.status.conditions[?(@.type==\"$3\")].reason}"; }
cond_is() { [ "$(cond "$1" "$2" "$3")" = "$4" ]; }
pvc_phase_is() { [ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$3" ]; }
gone() { ! kubectl get "$@" >/dev/null 2>&1; }
pod() { # pod <ns> <name> <claim>
  kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: $2, namespace: $1}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  terminationGracePeriodSeconds: 1
  restartPolicy: Never
  containers:
  - name: c
    image: public.ecr.aws/docker/library/busybox:1.36
    command: [sleep, "3600"]
    volumeMounts: [{name: v, mountPath: /shared}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: $3}}]
EOF
}
running() { [ "$(kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; }
sh_in() { kubectl -n "$1" exec "$2" -- sh -c "$3"; }

cleanup() {
  step cleanup
  kubectl -n "$NS_RW" exec w -- rm -rf "$DIR" >/dev/null 2>&1
  kubectl delete ns "$NS_RW" "$NS_RO" "$NS_NONE" --wait=false >/dev/null 2>&1
  kubectl delete sfs "$P" --wait=false >/dev/null 2>&1
  wait_for 180 gone sfs "$P"
  echo "left over PVs: $(kubectl get pv -l sharedfs.littlemex.github.io/claim-uid -o name | wc -l | tr -d ' ') (all sharedfs PVs in the cluster)"
  echo "RESULT pass=$PASS fail=$FAIL"
}
trap cleanup EXIT

step "setup: SharedFilesystem $P on $SRC (phase $(kubectl get pv "$SRC" -o jsonpath='{.status.phase}'))"
for ns in "$NS_RW" "$NS_RO" "$NS_NONE"; do kubectl create ns "$ns" >/dev/null; done
kubectl label ns "$NS_RW" "$KEY=rw" >/dev/null; kubectl label ns "$NS_RO" "$KEY=ro" >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystem
metadata: {name: $P}
spec:
  persistentVolumeRef: {name: $SRC}
  grants:
  - {namespaceSelector: {matchLabels: {$KEY: rw}}, access: ReadWrite}
  - {namespaceSelector: {matchLabels: {$KEY: ro}}, access: ReadOnly}
EOF
check "SharedFilesystem Ready on a source whose phase does not matter" wait_for 30 sh -c "[ \"\$(kubectl get sfs $P -o jsonpath='{.status.conditions[0].status}')\" = True ]"
kubectl get sfs "$P"

step "1. read-write and read-only claims mount on one node"
T0=$(date +%s)
for pair in "$NS_RW:ReadWrite" "$NS_RO:ReadOnly"; do
  kubectl apply -f - >/dev/null <<EOF
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystemClaim
metadata: {name: data, namespace: ${pair%%:*}}
spec: {filesystemRef: {name: $P}, access: ${pair##*:}}
EOF
done
check "rw claim Ready" wait_for 60 cond_is "$NS_RW" data Ready True/Ready
check "ro claim Ready" wait_for 60 cond_is "$NS_RO" data Ready True/Ready
echo "seconds from creating the claims to both Ready: $(( $(date +%s) - T0 ))"
pod "$NS_RW" w data; pod "$NS_RO" r data
check "rw pod running" wait_for 120 running "$NS_RW" w
check "ro pod running" wait_for 120 running "$NS_RO" r
NONCE=$P-$(date +%s)
check "rw pod writes a nonce" sh_in "$NS_RW" w "mkdir -p $DIR && echo $NONCE > $DIR/nonce"
check "ro pod reads the same nonce" sh -c "[ \"\$(kubectl -n $NS_RO exec r -- cat $DIR/nonce)\" = $NONCE ]"
check "ro pod cannot write (EROFS)" sh -c "kubectl -n $NS_RO exec r -- sh -c 'echo x > $DIR/from-ro' 2>&1 | grep -q 'Read-only file system'"
sh_in "$NS_RO" r "grep ' /shared ' /proc/mounts"
kubectl get pv -l sharedfs.littlemex.github.io/claim-uid -o custom-columns=NAME:.metadata.name,CLAIM:.spec.claimRef.namespace,OPTS:.spec.mountOptions,RECLAIM:.spec.persistentVolumeReclaimPolicy,STATUS:.status.phase | grep -E "NAME|$P"

step "2. an ungranted namespace gets nothing, and cannot take a reserved PV by name"
kubectl apply -f - >/dev/null <<EOF
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystemClaim
metadata: {name: data, namespace: $NS_NONE}
spec: {filesystemRef: {name: $P}}
EOF
check "claim rejected NamespaceNotGranted" wait_for 30 cond_is "$NS_NONE" data Accepted False/NamespaceNotGranted
RW_PV=$(kubectl -n "$NS_RW" get sfc data -o jsonpath='{.status.persistentVolumeName}')
check "no PV generated for it" sh -c "[ -z \"\$(kubectl -n $NS_NONE get sfc data -o jsonpath='{.status.persistentVolumeName}')\" ]"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: steal, namespace: $NS_NONE}
spec: {accessModes: [ReadWriteMany], storageClassName: "", volumeName: $RW_PV, resources: {requests: {storage: 1Gi}}}
EOF
sleep 20
check "a PVC naming the rw PV stays Pending" pvc_phase_is "$NS_NONE" steal Pending

step "3. deleting the PVC heals with no human step (Released trap)"
kubectl -n "$NS_RW" delete pod w --wait=true >/dev/null
OLD_UID=$(kubectl -n "$NS_RW" get pvc data -o jsonpath='{.metadata.uid}')
# --wait=false: the controller recreates a PVC of the same name at once, which kubectl --wait
# can keep waiting on.
kubectl -n "$NS_RW" delete pvc data --wait=false >/dev/null
check "a new PVC is created and Bound" wait_for 90 sh -c "pvc=\$(kubectl -n $NS_RW get pvc data -o jsonpath='{.metadata.uid}/{.status.phase}' 2>/dev/null); [ \"\${pvc%/*}\" != $OLD_UID ] && [ \"\${pvc#*/}\" = Bound ]"
kubectl get events -n "$NS_RW" --field-selector reason=Rebinding -o custom-columns=REASON:.reason,NOTE:.note 2>/dev/null | tail -2
pod "$NS_RW" w data
check "the nonce is still there" wait_for 120 sh -c "[ \"\$(kubectl -n $NS_RW exec w -- cat $DIR/nonce 2>/dev/null)\" = $NONCE ]"

step "4. server-side apply does not churn the PV"
RV1=$(kubectl get pv "$RW_PV" -o jsonpath='{.metadata.resourceVersion}')
kubectl -n "$NS_RW" annotate sfc data sharedfs-e2e/poke="$(date +%s)" --overwrite >/dev/null; sleep 10
check "PV resourceVersion unchanged after a reconcile" sh -c "[ \"\$(kubectl get pv $RW_PV -o jsonpath='{.metadata.resourceVersion}')\" = $RV1 ]"

step "5. revoking a namespace: grace period, running pod keeps its mount, new pods do not start"
kubectl label ns "$NS_RO" "$KEY-" >/dev/null
check "claim reports NamespaceNotGranted" wait_for 30 cond_is "$NS_RO" data Accepted False/NamespaceNotGranted
sleep $((GRACE/2))
check "PVC not deleted during the grace period" sh -c "[ -z \"\$(kubectl -n $NS_RO get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
check "PVC Terminating after the grace period" wait_for $((GRACE+30)) sh -c "[ -n \"\$(kubectl -n $NS_RO get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
check "claim Ready reason is Revoking" cond_is "$NS_RO" data Ready False/Revoking
check "the running pod still reads" sh -c "[ \"\$(kubectl -n $NS_RO exec r -- cat $DIR/nonce)\" = $NONCE ]"
pod "$NS_RO" r2 data; sleep 20
check "a new pod does not start" sh -c "! kubectl -n $NS_RO get pod r2 -o jsonpath='{.status.phase}' | grep -q Running"
kubectl -n "$NS_RO" get events --field-selector involvedObject.name=r2 -o custom-columns=REASON:.reason,MSG:.message 2>/dev/null | tail -2
kubectl -n "$NS_RO" delete pod r r2 --wait=true >/dev/null
check "PVC and PV are gone once the pods stop" wait_for 90 sh -c "! kubectl -n $NS_RO get pvc data >/dev/null 2>&1 && [ -z \"\$(kubectl -n $NS_RO get sfc data -o jsonpath='{.status.persistentVolumeName}')\" ]"

step "5b. a claim removed without its finalizer leaves nothing behind"
kubectl apply -f - >/dev/null <<EOF
apiVersion: sharedfs.littlemex.github.io/v1alpha1
kind: SharedFilesystemClaim
metadata: {name: stray, namespace: $NS_RW}
spec: {filesystemRef: {name: $P}}
EOF
check "stray claim Ready" wait_for 60 cond_is "$NS_RW" stray Ready True/Ready
STRAY_PV=$(kubectl -n "$NS_RW" get sfc stray -o jsonpath='{.status.persistentVolumeName}')
kubectl -n "$NS_RW" delete sfc stray --wait=false >/dev/null
kubectl -n "$NS_RW" patch sfc stray --type=merge -p '{"metadata":{"finalizers":null}}' >/dev/null 2>&1
check "its PVC and PV are swept" wait_for 60 sh -c "! kubectl -n $NS_RW get pvc stray >/dev/null 2>&1 && ! kubectl get pv $STRAY_PV >/dev/null 2>&1"

step "6. the SharedFilesystem waits for its volumes"
kubectl delete sfs "$P" --wait=false >/dev/null
check "SharedFilesystem reports VolumesRemaining" wait_for 30 sh -c "kubectl get sfs $P -o jsonpath='{.status.conditions[0].reason}' | grep -q VolumesRemaining"
check "the rw claim reports FilesystemDeleting" wait_for 30 cond_is "$NS_RW" data Accepted False/FilesystemDeleting
sleep $((GRACE+5))
check "and keeps its PVC past the grace period" sh -c "[ -z \"\$(kubectl -n $NS_RW get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"

step "6b. removing the grants of a SharedFilesystem being deleted still withdraws access"
kubectl patch sfs "$P" --type=merge -p '{"spec":{"grants":null}}' >/dev/null
check "claim reports NamespaceNotGranted" wait_for 30 cond_is "$NS_RW" data Accepted False/NamespaceNotGranted
check "PVC Terminating after the grace period" wait_for $((GRACE+30)) sh -c "[ -n \"\$(kubectl -n $NS_RW get pvc data -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
check "claim names the pod that still mounts it" sh -c "kubectl -n $NS_RW get sfc data -o jsonpath='{.status.conditions[?(@.type==\"Ready\")].message}' | grep -q 'still mounted by w'"

step "7. deleting a claim in use waits for its pod, then PVC, PV, claim go in order"
kubectl -n "$NS_RW" delete sfc data --wait=false >/dev/null
sleep 10
check "claim waits (Deleting) while the pod runs" cond_is "$NS_RW" data Ready False/Deleting
check "the PV is not deleted while the PVC is in use" sh -c "[ -z \"\$(kubectl get pv $RW_PV -o jsonpath='{.metadata.deletionTimestamp}')\" ]"
kubectl -n "$NS_RW" exec w -- rm -rf "$DIR" >/dev/null 2>&1
kubectl -n "$NS_RW" delete pod w --wait=true >/dev/null
check "claim, PVC and PV are gone" wait_for 90 sh -c "! kubectl -n $NS_RW get sfc data >/dev/null 2>&1 && ! kubectl get pv $RW_PV >/dev/null 2>&1"
check "the SharedFilesystem lets go by itself" wait_for 60 gone sfs "$P"
check "the source PV is untouched" sh -c "kubectl get pv $SRC >/dev/null"
