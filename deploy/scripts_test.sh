#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
export fixture PATH="$fixture:$PATH"
cat > "$fixture/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$fixture/log"
case "$*" in
  *' get secret '*'-o name') if [[ ${fresh_install:-0} == 0 ]]; then echo secret/existing; fi ;;
  *' get secret '*) echo Y2VydA== ;;
  *' rollout status '*) exit "${rollout_status:-0}" ;;
  *' get pvc '*) echo "$claim_uid" ;;
  *' get pod '*runtimeClassName*) echo "${runtime_class:-persistent-rootfs}" ;;
  *' get pod '*) echo "$pod_uid" ;;
  *' exec '* )
    while [[ $1 != -- ]]; do shift; done
    shift
    if [[ $1 == cat ]]; then cat "$fixture/marker"; else sh -ceu "$3" sh "$fixture/marker" "$6" "$7"; fi ;;
esac
MOCK
chmod +x "$fixture/kubectl"
expect_failure() { if "$@"; then echo "unexpected success: $*" >&2; exit 1; fi; }
export rollout_status=1
expect_failure sh install-control-plane.sh example/webhook:test
if grep -Eq '^apply -f .*webhook-admission.yaml' "$fixture/log"; then exit 1; fi
export rollout_status=0
sh install-control-plane.sh example/webhook:test
tail -n 3 "$fixture/log" > "$fixture/activation"
sed -n '1p' "$fixture/activation" | grep -Eq '^apply -f .*webhook-backend.yaml$'
sed -n '2p' "$fixture/activation" | grep -q 'rollout status'
sed -n '3p' "$fixture/activation" | grep -Eq '^apply -f .*webhook-admission.yaml$'
export fresh_install=1
: > "$fixture/log"
sh install-control-plane.sh example/webhook:test
grep -q ' create secret tls ' "$fixture/log"
grep -Eq '^apply -f .*webhook-admission.yaml' "$fixture/log"
printf '#!/bin/sh\nprintf partial\nexit 1\n' > "$fixture/base64"
chmod +x "$fixture/base64"
: > "$fixture/log"
expect_failure sh install-control-plane.sh example/webhook:test
if grep -Eq ' create secret tls |^apply -f .*webhook-(backend|admission).yaml' "$fixture/log"; then exit 1; fi
export pod_uid=first claim_uid=original
check() { bash check-rootfs-recovery.sh test demo "$1" test-token "${2:-}"; }
check seed
runtime_class=other expect_failure check verify
check verify
expect_failure check verify-replacement first
export pod_uid=second
check verify-replacement first
expect_failure check verify-replacement second
export claim_uid=different
expect_failure check verify
