#!/usr/bin/env sh
set -eu

if [ "$#" -ne 1 ] || [ -z "${1:-}" ]; then
  echo "usage: $0 <webhook-image>" >&2
  exit 2
fi
for command in kubectl openssl sed base64 tr grep; do
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

base64 <"$TMP/tls.crt" >"$TMP/ca.b64"
CA_BUNDLE=$(tr -d '\r\n' <"$TMP/ca.b64")
else
  CA_BUNDLE=$(kubectl -n "$NAMESPACE" get secret "$SECRET" -o 'jsonpath={.data.tls\.crt}')
  printf '%s' "$CA_BUNDLE" | base64 -d >"$TMP/tls.crt"
fi
[ -n "$CA_BUNDLE" ] || { echo "empty CA bundle for $SECRET" >&2; exit 1; }
trust_file="$TMP/tls.crt"
if [ -n "${WEBHOOK_CA_FILE:-}" ]; then
  trust_file=$WEBHOOK_CA_FILE
  base64 <"$trust_file" >"$TMP/ca.b64"
  CA_BUNDLE=$(tr -d '\r\n' <"$TMP/ca.b64")
fi
openssl verify -purpose sslserver -verify_hostname "$SERVICE_DNS" -CAfile "$trust_file" -untrusted "$TMP/tls.crt" "$TMP/tls.crt"
openssl x509 -in "$TMP/tls.crt" -noout -ext subjectAltName | tr ',' '\n' >"$TMP/san"
grep -Eq '^[[:space:]]*DNS:persistent-rootfs-webhook\.persistent-rootfs-system\.svc[[:space:]]*$' "$TMP/san" || { echo "missing webhook service DNS SAN" >&2; exit 1; }
sed "s|registry.example/persistent-rootfs-webhook:REPLACE_ME|$IMAGE|g" \
  "$SCRIPT_DIR/webhook-backend.yaml" >"$TMP/webhook-backend.yaml"
sed "s|REPLACE_WITH_BASE64_CA|$CA_BUNDLE|g" \
  "$SCRIPT_DIR/webhook-admission.yaml" >"$TMP/webhook-admission.yaml"
kubectl apply --dry-run=server -f "$TMP/webhook-backend.yaml" -f "$TMP/webhook-admission.yaml"
if [ -z "$secret_name" ]; then
  kubectl -n "$NAMESPACE" create secret tls "$SECRET" --cert="$TMP/tls.crt" --key="$TMP/tls.key"
fi
kubectl apply -f "$SCRIPT_DIR/runtimeclass.yaml"
kubectl apply -f "$TMP/webhook-backend.yaml"
kubectl -n "$NAMESPACE" rollout status deployment/persistent-rootfs-webhook --timeout=5m
kubectl apply -f "$TMP/webhook-admission.yaml"
cat >"$TMP/probe.yaml" <<'EOF'
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  generateName: persistent-rootfs-probe-
  labels:
    persistent-rootfs.io/state-volume: "true"
spec:
  accessModes: [ReadWriteOncePod]
  resources:
    requests:
      storage: 1Mi
EOF
kubectl -n "$NAMESPACE" create --dry-run=server --request-timeout=10s -f "$TMP/probe.yaml"
cat >"$TMP/probe-deny.yaml" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  generateName: persistent-rootfs-probe-
  annotations:
    persistent-rootfs.io/enabled: "true"
spec:
  containers:
    - name: probe
      image: registry.k8s.io/pause:3.10
EOF
if kubectl -n "$NAMESPACE" create --dry-run=server --request-timeout=10s -f "$TMP/probe-deny.yaml" >"$TMP/probe-result" 2>&1; then
  echo 'invalid managed Pod passed the admission probe; verify registration and rerun' >&2
  exit 1
fi
grep -F 'admission webhook "pods.persistent-rootfs.io" denied the request' "$TMP/probe-result" >/dev/null || {
  cat "$TMP/probe-result" >&2
  echo 'could not confirm the Pod webhook handled the probe' >&2
  exit 1
}
