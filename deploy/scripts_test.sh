#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"
fixture=$(mktemp -d)
trap 'rm -rf "$fixture"' EXIT
export fixture PATH="$fixture:$PATH"
MSYS2_ARG_CONV_EXCL='/CN=' openssl req -x509 -newkey rsa:2048 -nodes -days 1 -subj '/CN=test' \
  -addext 'subjectAltName=DNS:persistent-rootfs-webhook.persistent-rootfs-system.svc' \
  -keyout "$fixture/key" -out "$fixture/cert"
base64 <"$fixture/cert" | tr -d '\r\n' >"$fixture/cert.b64"
cat > "$fixture/kubectl" <<'MOCK'
#!/usr/bin/env bash
set -eu
printf '%s\n' "$*" >> "$fixture/log"
case "$*" in
  *' get secret '*'-o name') if [[ ${fresh_install:-0} == 0 ]]; then echo secret/existing; fi ;;
  *' get secret '*) cat "$fixture/cert.b64" ;;
  *' create --dry-run=server '*'/probe.yaml') cp "${!#}" "$fixture/probe"; exit "${probe_status:-0}" ;;
  *' create --dry-run=server '*'/probe-deny.yaml') echo 'admission webhook "pods.persistent-rootfs.io" denied the request' >&2; exit "${deny_probe_status:-1}" ;;
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
tail -n 5 "$fixture/log" > "$fixture/activation"
sed -n '1p' "$fixture/activation" | grep -Eq '^apply -f .*webhook-backend.yaml$'
sed -n '2p' "$fixture/activation" | grep -q 'rollout status'
sed -n '3p' "$fixture/activation" | grep -Eq '^apply -f .*webhook-admission.yaml$'
sed -n '4p' "$fixture/activation" | grep -q 'create --dry-run=server'
grep -q 'persistent-rootfs.io/state-volume' "$fixture/probe"
probe_status=1 expect_failure sh install-control-plane.sh example/webhook:test
deny_probe_status=0 expect_failure sh install-control-plane.sh example/webhook:test
MSYS2_ARG_CONV_EXCL='/CN=' openssl req -new -nodes -newkey rsa:2048 -subj '/CN=webhook' -keyout "$fixture/leaf.key" -out "$fixture/leaf.csr"
printf 'subjectAltName=DNS:persistent-rootfs-webhook.persistent-rootfs-system.svc\n' >"$fixture/extensions"
openssl x509 -req -in "$fixture/leaf.csr" -CA "$fixture/cert" -CAkey "$fixture/key" -set_serial 2 -days 1 -extfile "$fixture/extensions" -out "$fixture/leaf.crt"
base64 <"$fixture/leaf.crt" | tr -d '\r\n' >"$fixture/cert.b64"
expect_failure sh install-control-plane.sh example/webhook:test
WEBHOOK_CA_FILE="$fixture/cert" sh install-control-plane.sh example/webhook:test
printf 'invalid' > "$fixture/cert.b64"
: > "$fixture/log"
expect_failure sh install-control-plane.sh example/webhook:test
if grep -q '^apply -f .*webhook-' "$fixture/log"; then exit 1; fi
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
