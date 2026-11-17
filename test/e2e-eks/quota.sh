#!/usr/bin/env bash
# End-to-end check of per-directory project quotas on a real cluster with FSx for Lustre 2.15.
#
# Usage: test/e2e-eks/quota.sh <source PV name> <node name>
#   The controller must run with --quota-namespace and --quota-image (config/quota).
#
# Creates namespaces sfsq-<id>-{admin,team,old}, writes only below the directory sfsq-<id> of the
# file system, and removes all of it at the end.
set -uo pipefail

SRC=${1:?source PV}; NODE=${2:?node}
ID=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c 5)
P=sfsq-$ID; KEY=sharedfs-quota-e2e/$P
NS_ADMIN=$P-admin; NS_TEAM=$P-team; NS_OLD=$P-old
G=sharedfs.littlemex.github.io
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); echo "[PASS] $*"; }
ng() { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else ng "$d"; fi; }
wait_for() { local t=$1; shift; for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
phase_is() { [ "$(kubectl -n "$1" get pvc "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = "$3" ]; }
running() { [ "$(kubectl -n "$1" get pod "$2" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ]; }
fsd_of() { kubectl get fsd -o jsonpath="{.items[?(@.spec.namespace==\"$1\")].metadata.name}"; }
cond_is() { [ "$(kubectl get fsd "$(fsd_of "$1")" -o jsonpath="{.status.conditions[?(@.type==\"$2\")].status}/{.status.conditions[?(@.type==\"$2\")].reason}")" = "$3" ]; }
sh_in() { kubectl -n "$1" exec "$2" -- sh -c "$3"; }
pvc() { kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: $2, namespace: $1}
spec: {accessModes: [ReadWriteMany], storageClassName: $3, resources: {requests: {storage: 1Gi}}}
EOF
}

cleanup() {
  echo; echo "== cleanup"
  kubectl -n "$NS_ADMIN" exec lfs -- rm -rf "/fs/$P" >/dev/null 2>&1
  kubectl delete ns "$NS_ADMIN" "$NS_TEAM" "$NS_OLD" --wait=false >/dev/null 2>&1
  for s in $(kubectl get fss -o name | grep "/$P"); do kubectl delete "$s" --wait=false >/dev/null 2>&1; done
  wait_for 240 sh -c "! kubectl get fss -o name | grep -q /$P"
  echo "left over: shares $(kubectl get fss -o name | grep -c "/$P"), quota pods $(kubectl -n sharedfs-quota get pods -o name | wc -l | tr -d ' ')"
  echo "RESULT pass=$PASS fail=$FAIL"
}
trap cleanup EXIT

echo "== setup on $SRC"
for ns in "$NS_ADMIN" "$NS_TEAM" "$NS_OLD"; do kubectl create ns "$ns" >/dev/null; kubectl label ns "$ns" "$KEY=${ns##*-}" >/dev/null; done
kubectl apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: FilesystemShare
metadata: {name: $P-root, annotations: {$G/allow-overlap: "true"}}
spec:
  persistentVolumeRef: {name: $SRC}
  path: "/"
  grants: [{namespaceSelector: {matchLabels: {$KEY: admin}}, access: ReadWrite}]
EOF
pvc "$NS_ADMIN" root "$P-root-rw"
check "admin root claim Bound" wait_for 120 phase_is "$NS_ADMIN" root Bound
# An admin pod with the Lustre client utilities, to read project IDs and quotas.
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: lfs, namespace: $NS_ADMIN}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  containers:
  - name: c
    image: public.ecr.aws/amazonlinux/amazonlinux:2023
    command: [sh, -c, "dnf install -y -q lustre-client >/dev/null 2>&1 && touch /tmp/ready; sleep 7200"]
    volumeMounts: [{name: v, mountPath: /fs}]
  volumes: [{name: v, persistentVolumeClaim: {claimName: root}}]
EOF
check "admin lfs pod ready" wait_for 300 sh_in "$NS_ADMIN" lfs "test -f /tmp/ready"
sh_in "$NS_ADMIN" lfs "mkdir -m 0755 /fs/$P && mkdir -m 0700 /fs/$P/$NS_OLD"

kubectl apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: FilesystemShare
metadata: {name: $P-team}
spec:
  persistentVolumeRef: {name: $SRC}
  path: "$P/{namespace}"
  provision: {gid: 2000, mode: "2770", quota: {storage: 50Mi, inodes: 1000}}
  grants: [{namespaceSelector: {matchExpressions: [{key: $KEY, operator: In, values: [team, old]}]}, access: ReadWrite}]
EOF
check "quota share Ready" wait_for 60 sh -c "[ \"\$(kubectl get fss $P-team -o jsonpath='{.status.conditions[0].status}')\" = True ]"

echo; echo "== 1. a new directory gets its own project and limit before the claim is Bound"
T0=$(date +%s)
pvc "$NS_TEAM" work "$P-team-rw"
check "team claim Bound" wait_for 300 phase_is "$NS_TEAM" work Bound
echo "seconds from claim to Bound, preparer and quota pod included: $(( $(date +%s) - T0 ))"
PID=$(kubectl get fsd "$(fsd_of "$NS_TEAM")" -o jsonpath='{.spec.projectID}')
echo "project ID: $PID"
check "Prepared/DirectoryCreated" cond_is "$NS_TEAM" Prepared True/DirectoryCreated
check "QuotaApplied/QuotaApplied" cond_is "$NS_TEAM" QuotaApplied True/QuotaApplied
check "the directory carries the project ID with inheritance" sh -c "kubectl -n $NS_ADMIN exec lfs -- lfs project -d /fs/$P/$NS_TEAM | grep -q \"^ *$PID P \""
check "the project's limits are 51200 KiB and 1000 inodes" sh -c "kubectl -n $NS_ADMIN exec lfs -- lfs quota -p $PID /fs | awk 'NR==3{print \$4, \$8}' | grep -qx '51200 1000'"
check "quota pod, claim and volume are gone" wait_for 60 sh -c "! kubectl -n sharedfs-quota get pod,pvc -o name | grep -q . && ! kubectl get pv -o name | grep -q sfs-quota-"

echo; echo "== 2. the limit holds against the tenant"
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: Pod
metadata: {name: w, namespace: $NS_TEAM}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  restartPolicy: Never
  terminationGracePeriodSeconds: 1
  securityContext: {runAsUser: 1000, runAsGroup: 2000, supplementalGroups: [2000]}
  containers:
  - {name: c, image: public.ecr.aws/docker/library/busybox:1.36, command: [sleep, "3600"], volumeMounts: [{name: v, mountPath: /shared}]}
  volumes: [{name: v, persistentVolumeClaim: {claimName: work}}]
EOF
check "team pod running" wait_for 180 running "$NS_TEAM" w
OUT=$(kubectl -n "$NS_TEAM" exec w -- sh -c 'dd if=/dev/zero of=/shared/big bs=1M count=200 2>&1; ls -l /shared/big' 2>&1)
echo "$OUT" | tail -2
check "writing 200 MiB is stopped by the quota" sh -c "echo '$OUT' | grep -q 'Disk quota exceeded'"
SIZE=$(kubectl -n "$NS_TEAM" exec w -- stat -c %s /shared/big)
check "no more than the limit and a little slack was written ($SIZE bytes)" sh -c "[ $SIZE -le $((60*1024*1024)) ]"
check "a subdirectory made by the tenant inherits the project" sh -c "kubectl -n $NS_TEAM exec w -- mkdir /shared/sub && kubectl -n $NS_ADMIN exec lfs -- lfs project -d /fs/$P/$NS_TEAM/sub | grep -q \"^ *$PID P \""

echo; echo "== 3. raising the limit applies to the same project"
kubectl patch fss "$P-team" --type=merge -p '{"spec":{"provision":{"quota":{"storage":"150Mi"}}}}' >/dev/null
check "QuotaApplied again" wait_for 240 sh -c "[ \"\$(kubectl get fsd $(fsd_of "$NS_TEAM") -o jsonpath='{.status.quota.storage}')\" = 150Mi ] && kubectl get fsd $(fsd_of "$NS_TEAM") -o jsonpath='{.status.conditions[?(@.type==\"QuotaApplied\")].status}' | grep -qx True"
check "the limit is now 153600 KiB" sh -c "kubectl -n $NS_ADMIN exec lfs -- lfs quota -p $PID /fs | awk 'NR==3{print \$4}' | grep -qx 153600"
check "and the tenant can write more" sh -c "kubectl -n $NS_TEAM exec w -- sh -c 'rm /shared/big; dd if=/dev/zero of=/shared/big bs=1M count=100' >/dev/null 2>&1"

echo; echo "== 4. a directory that existed before is not tagged and not handed out"
pvc "$NS_OLD" work "$P-team-rw"
check "its record: Prepared/DirectoryExists" wait_for 240 cond_is "$NS_OLD" Prepared True/DirectoryExists
check "its record: QuotaApplied False/DirectoryNotTagged" wait_for 240 cond_is "$NS_OLD" QuotaApplied False/DirectoryNotTagged
check "its claim stays Pending" phase_is "$NS_OLD" work Pending
check "the existing directory was not changed (root 0700, no project)" sh -c "[ \"\$(kubectl -n $NS_ADMIN exec lfs -- stat -c '%u %a' /fs/$P/$NS_OLD)\" = '0 700' ] && kubectl -n $NS_ADMIN exec lfs -- lfs project -d /fs/$P/$NS_OLD | grep -q '^ *0 - '"

echo; echo "== 5. the controller logged no reconciler errors"
check "no reconciler errors" sh -c "! kubectl -n sharedfs-system logs -l control-plane=controller-manager --since=1h --tail=-1 | grep -q 'Reconciler error'"
