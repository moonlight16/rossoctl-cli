#!/usr/bin/env bash

set -u

namespace="team1"
show_k8s=false

usage() {
  cat <<'EOF'
Usage: watch-contexts.sh [options]

Show one snapshot of Rosso agents and Context Service resources.

Options:
  -n, --namespace NAME   Namespace to watch (default: team1)
      --k8s             Also show Sandboxes, StatefulSets, Pods, and PVCs
  -h, --help            Show this help

Examples:
  ./scripts/watch-contexts.sh
  ./scripts/watch-contexts.sh --k8s
  watch -n 2 ./scripts/watch-contexts.sh --k8s
EOF
}

while (($#)); do
  case "$1" in
    -n|--namespace)
      namespace=${2:?"namespace is required"}
      shift 2
      ;;
    --k8s)
      show_k8s=true
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown option: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

printf 'Rosso context snapshot — namespace: %s — %s\n\n' "$namespace" "$(date '+%Y-%m-%d %H:%M:%S')"

echo 'AGENTS'
rossoctl agents --namespace "$namespace" list || true

echo
echo 'CONTEXTS'
rossoctl context --namespace "$namespace" list || true

if $show_k8s; then
  echo
  echo 'KUBERNETES RESOURCES'
  kubectl -n "$namespace" get sandboxes,statefulsets,pods,pvc 2>&1 || true
fi
