#!/bin/bash
set -e

echo "Starting etcd..."
/usr/local/bin/etcd \
  --data-dir=/tmp/envtest/etcd \
  --listen-client-urls=http://0.0.0.0:2379 \
  --advertise-client-urls=http://localhost:2379 \
  > /tmp/etcd.log 2>&1 &

ETCD_PID=$!
echo "Etcd started with PID: $ETCD_PID"
sleep 3

echo "Starting kube-apiserver..."
NO_PROXY="*" /usr/local/bin/kube-apiserver \
  --etcd-servers=http://localhost:2379 \
  --cert-dir=/tmp/envtest/certs \
  --secure-port=6443 \
  --bind-address=0.0.0.0 \
  --service-cluster-ip-range=10.96.0.0/12 \
  --service-account-key-file=/tmp/envtest/certs/sa.pub \
  --service-account-signing-key-file=/tmp/envtest/certs/sa.key \
  --service-account-issuer=https://kubernetes.default.svc.cluster.local \
  --token-auth-file=/tmp/envtest/certs/token-auth-file \
  --disable-admission-plugins=ServiceAccount \
  --authorization-mode=AlwaysAllow \
  --anonymous-auth=true \
  --allow-privileged=true \
  --max-requests-inflight=200 \
  --max-mutating-requests-inflight=100 \
  --enable-garbage-collector=false \
  > /tmp/apiserver.log 2>&1 &

APISERVER_PID=$!
echo "Kube-apiserver started with PID: $APISERVER_PID"

echo "Envtest is running. Logs:"
echo "  - Etcd: /tmp/etcd.log"
echo "  - API Server: /tmp/apiserver.log"

while kill -0 $ETCD_PID 2>/dev/null && kill -0 $APISERVER_PID 2>/dev/null; do
  sleep 5
done

echo "One or both processes died. Check logs."
exit 1
