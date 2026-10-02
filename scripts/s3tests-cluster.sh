#!/usr/bin/env bash
# Bring up a three-node engine cluster for the s3-tests (up), or tear it down
# (down). Node 1 serves S3 on the port s3tests.conf names; the suite runs
# against it while the other two hold replicas.
#
# Usage: scripts/s3tests-cluster.sh up|down [path/to/fs]
set -euo pipefail

dir=.s3tests-cluster
fs=${2:-./fs}
s3_ports=(8077 8078 8079)
peer_ports=(7081 7082 7083)
admin_ports=(9091 9092 9093)
token=s3tests-admin-token

case "${1:-}" in
up)
  rm -rf "$dir"
  mkdir -p "$dir"

  for i in 0 1 2; do
    n=$((i + 1))
    node="$dir/n$n"
    mkdir -p "$node"
    cp .github/s3tests/master.key "$node/"
    {
      cat .github/s3tests/server.yaml
      echo "admin: {enabled: true, addr: \"127.0.0.1:${admin_ports[$i]}\", token: $token}"
      echo "cluster:"
      echo "  node_id: n$n"
      echo "  addr: \"127.0.0.1:${peer_ports[$i]}\""
      echo "  advertise_addr: \"127.0.0.1:${peer_ports[$i]}\""
      echo "  peers: [\"127.0.0.1:${peer_ports[0]}\"]"
      echo "  secret: s3tests-cluster-secret-0123456789"
    } > "$node/server.yaml"

    (
      cd "$node"
      OTEL_TRACES_EXPORTER=none OTEL_METRICS_EXPORTER=none OTEL_LOGS_EXPORTER=none \
        "$OLDPWD/$fs" s3 --config server.yaml --addr ":${s3_ports[$i]}" --root data > server.log 2>&1 &
      echo $! > pid
    )
  done

  for port in "${s3_ports[@]}"; do
    for _ in $(seq 1 60); do
      curl -fsS -o /dev/null "http://localhost:$port/health" && break
      sleep 0.5
    done
  done

  cat > "$dir/roles.yaml" <<'YAML'
partitions: 64
members:
  - {id: n1, zone: a, capacity: 1TB}
  - {id: n2, zone: b, capacity: 1TB}
  - {id: n3, zone: c, capacity: 1TB}
YAML

  "$fs" layout apply -f "$dir/roles.yaml" --admin-addr "http://127.0.0.1:${admin_ports[0]}" --token "$token"

  # Every node must have adopted the layout before the suite starts.
  for port in "${admin_ports[@]}"; do
    for attempt in $(seq 1 60); do
      if "$fs" layout show --admin-addr "http://127.0.0.1:$port" --token "$token" 2>/dev/null | grep -q '^Version 1,'; then
        break
      fi

      if [ "$attempt" = 60 ]; then
        echo "node on admin port $port did not adopt the layout" >&2
        exit 1
      fi

      sleep 0.5
    done
  done

  "$fs" layout nodes --admin-addr "http://127.0.0.1:${admin_ports[0]}" --token "$token"
  ;;
down)
  for pid in "$dir"/n*/pid; do
    [ -f "$pid" ] && kill "$(cat "$pid")" 2>/dev/null || true
  done
  ;;
*)
  echo "usage: $0 up|down [path/to/fs]" >&2
  exit 2
  ;;
esac
