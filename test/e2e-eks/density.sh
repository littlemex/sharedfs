#!/usr/bin/env bash
# Mount density on one node: N pods, each with its own sharedfs claim, all on one node. Measures
# the time to all Running, the Lustre mounts on the node and the node's memory, and checks every
# pod reads the directory. The share is read-only, so nothing is written to the file system.
#
# Usage: test/e2e-eks/density.sh <source PV name> <node name> [pods]
set -uo pipefail

SRC=${1:?source PV}; NODE=${2:?node}; N=${3:-50}
ID=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c 5)
P=sfsden-$ID; NS=$P
G=sharedfs.littlemex.github.io
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); echo "[PASS] $*"; }
ng() { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else ng "$d"; fi; }
wait_for() { local t=$1; shift; for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
running() { [ "$(kubectl -n "$NS" get pods --no-headers 2>/dev/null | awk '$3=="Running"' | wc -l | tr -d ' ')" -eq "$N" ]; }
node_mem() { kubectl get --raw "/api/v1/nodes/$NODE/proxy/stats/summary" | python3 -c 'import json,sys; print(round(json.load(sys.stdin)["node"]["memory"]["workingSetBytes"]/2**20))'; }

cleanup() {
  echo; echo "== cleanup"
  kubectl delete ns "$NS" --wait=false >/dev/null 2>&1
  kubectl delete fss "$P" --wait=false >/dev/null 2>&1
  wait_for 600 sh -c "! kubectl get fss $P >/dev/null 2>&1"
  echo "RESULT pass=$PASS fail=$FAIL"
}
trap cleanup EXIT

kubectl create ns "$NS" >/dev/null && kubectl label ns "$NS" "sharedfs-density=$P" >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: FilesystemShare
metadata: {name: $P}
spec:
  persistentVolumeRef: {name: $SRC}
  path: "/"
  grants: [{namespaceSelector: {matchLabels: {sharedfs-density: $P}}, access: ReadOnly}]
EOF
check "share Ready" wait_for 60 sh -c "[ \"\$(kubectl get fss $P -o jsonpath='{.status.conditions[0].status}')\" = True ]"
M0=$(node_mem); echo "node memory before: ${M0} MiB"
T0=$(date +%s)
{
  for i in $(seq "$N"); do cat <<EOF
---
apiVersion: v1
kind: Pod
metadata: {name: p$i, namespace: $NS}
spec:
  nodeSelector: {kubernetes.io/hostname: $NODE}
  tolerations: [{operator: Exists}]
  terminationGracePeriodSeconds: 1
  containers:
  - {name: c, image: public.ecr.aws/docker/library/busybox:1.36, command: [sleep, "3600"], resources: {requests: {cpu: 1m, memory: 4Mi}}, volumeMounts: [{name: v, mountPath: /shared}]}
  volumes:
  - name: v
    ephemeral:
      volumeClaimTemplate:
        spec: {accessModes: [ReadOnlyMany], storageClassName: $P-ro, resources: {requests: {storage: 1Gi}}}
EOF
  done
} | kubectl apply -f - >/dev/null
check "all $N pods Running" wait_for 900 running
echo "seconds to all $N Running: $(( $(date +%s) - T0 ))"
M1=$(node_mem); echo "node memory after: ${M1} MiB (+$((M1-M0)) MiB for $N mounts)"
FAILED=$(kubectl -n "$NS" get events --field-selector reason=FailedMount -o name | wc -l | tr -d ' ')
echo "FailedMount events: $FAILED"
READ=0
for i in $(seq "$N"); do kubectl -n "$NS" exec "p$i" -- ls /shared >/dev/null 2>&1 && READ=$((READ+1)); done
check "every pod reads its mount ($READ/$N)" test "$READ" -eq "$N"
T1=$(date +%s)
kubectl -n "$NS" delete pods --all --wait=true >/dev/null
share_pvs() { kubectl get pv -o 'custom-columns=S:.metadata.annotations.sharedfs\.littlemex\.github\.io/share-name' --no-headers | grep -cx "$P"; }
reclaimed() { [ "$(kubectl -n "$NS" get pvc --no-headers 2>/dev/null | wc -l | tr -d ' ')" -eq 0 ] && [ "$(share_pvs)" -eq 0 ]; }
check "all claims and volumes go with the pods" wait_for 600 reclaimed
echo "seconds to delete $N pods and reclaim their volumes: $(( $(date +%s) - T1 ))"
