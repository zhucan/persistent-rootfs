#!/usr/bin/env bash
set -euo pipefail

context=${1:?supply the preproduction kubectl context}
namespace=${2:?supply the namespace containing the disposable shell StatefulSet}
mode=${3:?supply seed, verify or verify-replacement}
token=${4:?supply the same unique run token for both steps}
[[ $token =~ ^[a-zA-Z0-9-]{8,64}$ ]] || exit 2
kube=(kubectl --context "$context" --namespace "$namespace" --request-timeout=30s)
pod=shell-0
claim=rootfs-state-shell-0
claim_uid=$("${kube[@]}" get pvc "$claim" -o jsonpath='{.metadata.uid}')
pod_uid=$("${kube[@]}" get pod "$pod" -o jsonpath='{.metadata.uid}')
runtime=$("${kube[@]}" get pod "$pod" -o jsonpath='{.spec.runtimeClassName}')
[[ -n $claim_uid && -n $pod_uid ]]
[[ $runtime == persistent-rootfs ]] || { echo "unexpected RuntimeClass: $runtime" >&2; exit 1; }
path="/.persistent-rootfs-check-$token"
expected="$claim_uid/$token"
case "$mode" in
  seed)
    "${kube[@]}" exec "$pod" -c app -- sh -ceu 'set -C; printf "%s/%s\n" "$2" "$3" > "$1"; sync' sh "$path" "$expected" "$pod_uid"
    ;;
  verify|verify-replacement)
    actual=$("${kube[@]}" exec "$pod" -c app -- cat "$path")
    seed_uid=${actual##*/}
    [[ -n $seed_uid && $actual == "$expected/$seed_uid" ]] || { echo 'rootfs content or PVC identity changed' >&2; exit 1; }
    if [[ $mode == verify-replacement ]]; then
      previous_uid=${5:?supply the Pod UID immediately before this replacement}
      [[ $pod_uid != "$previous_uid" && $pod_uid != "$seed_uid" ]] || { echo 'Pod was not replaced' >&2; exit 1; }
    fi
    ;;
  *) exit 2 ;;
esac
[[ $("${kube[@]}" get pod "$pod" -o jsonpath='{.metadata.uid}') == "$pod_uid" ]] || { echo 'Pod changed during check; retry with a fresh observation' >&2; exit 1; }
[[ $("${kube[@]}" get pvc "$claim" -o jsonpath='{.metadata.uid}') == "$claim_uid" ]] || { echo 'PVC changed during check' >&2; exit 1; }
printf '%s pod_uid=%s claim_uid=%s runtime=%s\n' "$mode" "$pod_uid" "$claim_uid" "$runtime"
