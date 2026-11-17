#!/usr/bin/env bash
# Build and push the manager image with the in-cluster BuildKit builder of
# github.com/littlemex/distributed-ai (infra/eks/charts/experiments, image-build-custom.yaml).
#
# Usage: hack/build-in-cluster.sh <distributed-ai clone> <ECR repository URI> <tag> [Dockerfile.context]
#   The last argument picks the image: hack/Dockerfile.context (controller, default) or
#   hack/Dockerfile.quota.context (quota pods).
set -euo pipefail
DAI=${1:?distributed-ai clone}; REPO=${2:?ECR repository URI}; TAG=${3:?tag}; DF=${4:-hack/Dockerfile.context}
ROOT=$(cd "$(dirname "$0")/.." && pwd)
WORK=$(mktemp -d); trap 'rm -rf "$WORK"' EXIT
CM=sharedfs-ctx-$TAG

COPYFILE_DISABLE=1 tar -C "$ROOT" -czf "$WORK/src.tar.gz" go.mod go.sum cmd api internal
cp "$ROOT/$DF" "$WORK/Dockerfile"
kubectl -n image-builder create configmap "$CM" --from-file="$WORK/Dockerfile" --from-file="$WORK/src.tar.gz"
kubectl -n image-builder patch configmap "$CM" -p '{"immutable":true}'
helm template exp "$DAI/infra/eks/charts/experiments" -s templates/image-build-custom.yaml \
  --set imageBuild.enabled=true --set imageBuild.jobName=build-sharedfs \
  --set imageBuild.repository="$REPO" --set imageBuild.tag="$TAG" \
  --set imageBuild.contextSource=configMap --set imageBuild.contextConfigMap="$CM" \
  | kubectl apply -f -
kubectl -n image-builder wait --for=condition=complete "job/build-sharedfs-$TAG" --timeout=30m
echo "$REPO:$TAG"
