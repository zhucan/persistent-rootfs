#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ]; then
  echo "usage: $0 <webhook-image>" >&2
  exit 2
fi
for command in kubectl openssl sed base64; do
  command -v "$command" >/dev/null 2>&1 || {
    echo "$command is required" >&2
    exit 1
  }
done

IMAGE=$1
CERT_VALID_DAYS=${CERT_VALID_DAYS:-36500}
case "$CERT_VALID_DAYS" in
  *[!0-9]*|'')
    echo "CERT_VALID_DAYS must be a positive integer" >&2
    exit 2
    ;;
esac
if [ "$CERT_VALID_DAYS" -eq 0 ]; then
  echo "CERT_VALID_DAYS must be a positive integer" >&2
  exit 2
fi
SCRIPT_DIR=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT HUP INT TERM

SERVICE_DNS=persistent-rootfs-webhook.persistent-rootfs-system.svc
openssl req -x509 -newkey rsa:3072 -nodes -days "$CERT_VALID_DAYS" \
  -subj "/CN=$SERVICE_DNS" \
  -addext "subjectAltName=DNS:$SERVICE_DNS,DNS:$SERVICE_DNS.cluster.local" \
  -keyout "$TMP/tls.key" -out "$TMP/tls.crt" >/dev/null 2>&1

kubectl apply -f "$SCRIPT_DIR/runtimeclass.yaml"
kubectl apply -f "$SCRIPT_DIR/pvc-policy.yaml"
kubectl create namespace persistent-rootfs-system --dry-run=client -o yaml | kubectl apply -f -
kubectl -n persistent-rootfs-system create secret tls persistent-rootfs-webhook-tls \
  --cert="$TMP/tls.crt" --key="$TMP/tls.key" --dry-run=client -o yaml | kubectl apply -f -

CA_BUNDLE=$(base64 <"$TMP/tls.crt" | tr -d '\r\n')
sed \
  -e "s|registry.example/persistent-rootfs-webhook:REPLACE_ME|$IMAGE|g" \
  -e "s|REPLACE_WITH_BASE64_CA|$CA_BUNDLE|g" \
  "$SCRIPT_DIR/webhook.yaml" | kubectl apply -f -

kubectl -n persistent-rootfs-system rollout status deployment/persistent-rootfs-webhook
