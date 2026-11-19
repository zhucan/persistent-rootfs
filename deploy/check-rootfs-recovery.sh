#!/usr/bin/env bash
set -euo pipefail

context=${1:?supply the preproduction kubectl context}
namespace=${2:?supply the namespace containing the disposable shell StatefulSet}
mode=${3:?supply seed or verify}
token=${4:?supply the same unique run token for both steps}
[[ $token =~ ^[a-zA-Z0-9-]{8,64}$ ]] || exit 2
kube=(kubectl --context "$context" --namespace "$namespace" --request-timeout=30s)
pod=shell-0
claim=rootfs-state-shell-0
claim_uid=$("${kube[@]}" get pvc "$claim" -o jsonpath='{.metadata.uid}')
pod_uid=$("${kube[@]}" get pod "$pod" -o jsonpath='{.metadata.uid}')
runtime=$("${kube[@]}" get pod "$pod" -o jsonpath='{.spec.runtimeClassName}')
[[ -n $claim_uid && -n $pod_uid && -n $runtime ]]
path="/.persistent-rootfs-check-$token"
expected="$claim_uid/$token"
case "$mode" in
  seed)
    "${kube[@]}" exec "$pod" -c app -- sh -ceu 'set -C; printf "%s\n" "$2" > "$1"; sync' sh "$path" "$expected"
    ;;
  verify)
    actual=$("${kube[@]}" exec "$pod" -c app -- cat "$path")
    [[ $actual == "$expected" ]] || { echo 'rootfs content or PVC identity changed' >&2; exit 1; }
    ;;
  *) exit 2 ;;
esac
printf '%s pod_uid=%s claim_uid=%s runtime=%s\n' "$mode" "$pod_uid" "$claim_uid" "$runtime"
