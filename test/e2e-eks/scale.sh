#!/usr/bin/env bash
# Scale and failover check of sharedfs on a real cluster. No pods mount anything: it measures
# provisioning, binding, revocation and reclaim of many claims, with the controller killed
# in the middle.
#
# Usage: test/e2e-eks/scale.sh <source PV name> [namespaces] [claims per namespace]
#   CTRL_NS  the controller's namespace (default sharedfs-system)
#   GRACE    the controller's --revocation-grace-period in seconds (default 30)
set -uo pipefail

SRC=${1:?source PV}; NNS=${2:-20}; NPVC=${3:-10}
ID=$(LC_ALL=C tr -dc a-z0-9 </dev/urandom | head -c 5)
P=sfsscale-$ID; KEY=sharedfs-scale/$P
CTRL_NS=${CTRL_NS:-sharedfs-system}; DEPLOY=sharedfs-controller-manager
GRACE=${GRACE:-30}
G=sharedfs.littlemex.github.io
TOTAL=$((NNS*NPVC))
PASS=0; FAIL=0
ok() { PASS=$((PASS+1)); echo "[PASS] $*"; }
ng() { FAIL=$((FAIL+1)); echo "[FAIL] $*"; }
check() { local d=$1; shift; if "$@"; then ok "$d"; else ng "$d"; fi; }
wait_for() { local t=$1; shift; for _ in $(seq "$t"); do "$@" >/dev/null 2>&1 && return 0; sleep 1; done; return 1; }
# Predicates are plain functions called directly (never inside sh -c, where they do not exist).
bound() { kubectl get pvc -A -l "$KEY" --no-headers 2>/dev/null | awk '$3=="Bound"' | wc -l | tr -d ' '; }
count() { kubectl get pvc -A -l "$KEY" --no-headers 2>/dev/null | wc -l | tr -d ' '; }
pvs() { kubectl get pv -o 'custom-columns=S:.metadata.annotations.sharedfs\.littlemex\.github\.io/share-name' --no-headers | grep -cx "$P"; }
deadlines() { kubectl get pv -o 'custom-columns=S:.metadata.annotations.sharedfs\.littlemex\.github\.io/share-name,W:.metadata.annotations.sharedfs\.littlemex\.github\.io/withdraw-after' --no-headers | awk -v p="$P" '$1==p && $2!="<none>"' | wc -l | tr -d ' '; }
at_least() { local v; v=$("$1"); [ "$v" -ge "$2" ] || { echo "  ($1=$v, want >= $2)" >&2; return 1; }; }
exactly() { local v; v=$("$1"); [ "$v" -eq "$2" ] || { echo "  ($1=$v, want $2)" >&2; return 1; }; }
leader() { kubectl -n "$CTRL_NS" get lease f5b32313.littlemex.github.io -o jsonpath='{.spec.holderIdentity}' | cut -d_ -f1; }
restarts() { kubectl -n "$CTRL_NS" get pods -l control-plane=controller-manager -o jsonpath='{range .items[*]}{.status.containerStatuses[0].restartCount} {end}'; }

cleanup() {
  echo; echo "== cleanup"
  for i in $(seq "$NNS"); do echo "$P-$i"; done | xargs kubectl delete ns --wait=false >/dev/null 2>&1
  kubectl delete fss "$P" --wait=false >/dev/null 2>&1
  wait_for 600 sh -c "! kubectl get fss $P >/dev/null 2>&1"
  echo "left over: share $(kubectl get fss "$P" -o name 2>/dev/null | wc -l | tr -d ' '), PVs $(pvs)"
  kubectl -n "$CTRL_NS" scale deploy "$DEPLOY" --replicas=1 >/dev/null
  echo "RESULT pass=$PASS fail=$FAIL"
}
trap cleanup EXIT

echo "== setup: $NNS namespaces x $NPVC claims = $TOTAL claims on $SRC"
kubectl -n "$CTRL_NS" scale deploy "$DEPLOY" --replicas=2 >/dev/null
kubectl -n "$CTRL_NS" rollout status deploy "$DEPLOY" --timeout=180s >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: $G/v1alpha1
kind: FilesystemShare
metadata: {name: $P}
spec:
  persistentVolumeRef: {name: $SRC}
  path: "$P"
  grants: [{namespaceSelector: {matchLabels: {$KEY: "yes"}}, access: ReadOnly}]
EOF
check "share Ready" wait_for 60 sh -c "[ \"\$(kubectl get fss $P -o jsonpath='{.status.conditions[0].status}')\" = True ]"
for i in $(seq "$NNS"); do
  kubectl create ns "$P-$i" >/dev/null && kubectl label ns "$P-$i" "$KEY=yes" >/dev/null
done

echo; echo "== 1. $TOTAL claims at once, the leader killed after the first ones bind"
L0=$(leader); echo "leader before: $L0"
T0=$(date +%s)
{
  for i in $(seq "$NNS"); do for j in $(seq "$NPVC"); do cat <<EOF
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: c$j, namespace: $P-$i, labels: {$KEY: "yes"}}
spec: {accessModes: [ReadOnlyMany], storageClassName: $P-ro, resources: {requests: {storage: 1Gi}}}
EOF
  done; done
} | kubectl apply -f - >/dev/null
wait_for 120 at_least bound $((TOTAL/10))
echo "bound when the leader is killed: $(bound)/$TOTAL after $(( $(date +%s) - T0 ))s"
kubectl -n "$CTRL_NS" delete pod "$L0" --wait=false >/dev/null
check "all $TOTAL claims Bound despite the failover" wait_for 600 exactly bound $TOTAL
echo "seconds from apply to all Bound: $(( $(date +%s) - T0 ))"
echo "new leader: $(leader)"
check "exactly one PV per claim" exactly pvs $TOTAL
check "no claim was given an event of failure" sh -c "! kubectl get events -A --field-selector type=Warning -o jsonpath='{range .items[*]}{.involvedObject.namespace} {.reason}{\"\\n\"}{end}' | grep \"^$P-\" | grep -v Revoking"
kubectl -n "$CTRL_NS" top pods 2>/dev/null | sed 's/^/  /'

echo; echo "== 2. revoke half the namespaces, kill the leader during the grace period"
HALF=$((NNS/2))
for i in $(seq "$HALF"); do kubectl label ns "$P-$i" "$KEY-" >/dev/null; done
T1=$(date +%s)
wait_for 60 at_least deadlines $((HALF*NPVC))
echo "deadlines recorded after $(( $(date +%s) - T1 ))s"
kubectl -n "$CTRL_NS" delete pod "$(leader)" --wait=false >/dev/null
check "the revoked claims are deleted after the grace period" wait_for $((GRACE+300)) exactly count $((TOTAL-HALF*NPVC))
echo "seconds from unlabel to all revoked claims gone: $(( $(date +%s) - T1 )) (grace ${GRACE}s)"
check "their PVs are reclaimed" wait_for 300 exactly pvs $((TOTAL-HALF*NPVC))
check "the granted half is untouched" exactly bound $((TOTAL-HALF*NPVC))

echo; echo "== 3. controller down: claims created and deleted while no controller runs"
kubectl -n "$CTRL_NS" scale deploy "$DEPLOY" --replicas=0 >/dev/null
kubectl -n "$CTRL_NS" wait --for=delete pod -l control-plane=controller-manager --timeout=120s >/dev/null 2>&1
N=$((HALF+1))
kubectl -n "$P-$N" delete pvc c1 --wait=false >/dev/null
kubectl apply -f - >/dev/null <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata: {name: late, namespace: $P-$N, labels: {$KEY: "yes"}}
spec: {accessModes: [ReadOnlyMany], storageClassName: $P-ro, resources: {requests: {storage: 1Gi}}}
EOF
sleep 20
kubectl -n "$CTRL_NS" scale deploy "$DEPLOY" --replicas=2 >/dev/null
check "after restart, the new claim is Bound" wait_for 180 sh -c "[ \"\$(kubectl -n $P-$N get pvc late -o jsonpath='{.status.phase}')\" = Bound ]"
check "and the deleted claim's PV is reclaimed" wait_for 180 exactly pvs $((TOTAL-HALF*NPVC))
check "no controller container restarted" sh -c "! echo '$(restarts)' | grep -qv '^\(0 \)*$'"
check "no reconciler errors in the last hour" sh -c "! kubectl -n $CTRL_NS logs -l control-plane=controller-manager --since=1h --tail=-1 | grep -q 'Reconciler error'"
