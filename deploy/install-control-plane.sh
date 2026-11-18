#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
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
case "$IMAGE" in
  *[!a-zA-Z0-9./:@_-]*) echo "invalid character in webhook image reference" >&2; exit 2 ;;
esac
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
kubectl apply --dry-run=server -f "$SCRIPT_DIR/runtimeclass.yaml"
kubectl create namespace persistent-rootfs-system --dry-run=client -o yaml >"$TMP/namespace.yaml"
kubectl apply -f "$TMP/namespace.yaml"
SECRET=persistent-rootfs-webhook-tls
NAMESPACE=persistent-rootfs-system
secret_name=$(kubectl -n "$NAMESPACE" get secret "$SECRET" --ignore-not-found -o name)
if [ -z "$secret_name" ]; then
cat >"$TMP/openssl.cnf" <<EOF
[req]
prompt = no
distinguished_name = subject
[subject]
CN = $SERVICE_DNS
[webhook]
subjectAltName = DNS:$SERVICE_DNS,DNS:$SERVICE_DNS.cluster.local
EOF
openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$CERT_VALID_DAYS" \
  -config "$TMP/openssl.cnf" -extensions webhook \
  -keyout "$TMP/tls.key" -out "$TMP/tls.crt"

CA_BUNDLE=$(base64 <"$TMP/tls.crt" | tr -d '\r\n')
else
  CA_BUNDLE=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o 'jsonpath={.data.tls\.crt}')
  [ -n "$CA_BUNDLE" ] || { echo "$SECRET has no tls.crt" >&2; exit 1; }
fi
sed \
  -e "s|registry.example/persistent-rootfs-webhook:REPLACE_ME|$IMAGE|g" \
  -e "s|REPLACE_WITH_BASE64_CA|$CA_BUNDLE|g" \
  "$SCRIPT_DIR/webhook.yaml" >"$TMP/webhook.yaml"
kubectl apply --dry-run=server -f "$TMP/webhook.yaml"
if [ -z "$secret_name" ]; then
  kubectl -n "$NAMESPACE" create secret tls "$SECRET" --cert="$TMP/tls.crt" --key="$TMP/tls.key"
fi
kubectl apply -f "$SCRIPT_DIR/runtimeclass.yaml"
kubectl apply -f "$TMP/webhook.yaml"

kubectl -n "$NAMESPACE" rollout status deployment/persistent-rootfs-webhook --timeout=5m
