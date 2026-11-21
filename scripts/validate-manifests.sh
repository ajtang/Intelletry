#!/usr/bin/env bash
# Builds every Kustomize entry point and validates the output against Kubernetes schemas,
# plus CRD schemas for Flux, the Secrets Store CSI driver and Azure Monitor.
set -euo pipefail
cd "$(dirname "$0")/.."

CATALOG='https://raw.githubusercontent.com/datreeio/CRDs-catalog/main/{{.Group}}/{{.ResourceKind}}_{{.ResourceAPIVersion}}.json'
status=0
for dir in apps/overlays/dev apps/overlays/prod platform clusters/dev clusters/prod; do
  echo "== $dir"
  if ! kustomize build "$dir" | kubeconform -strict -summary \
      -kubernetes-version 1.34.0 \
      -schema-location default -schema-location "$CATALOG" \
      -skip PodMonitor; then
    status=1
  fi
done
exit $status
