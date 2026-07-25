#!/usr/bin/env bash
# Bring up a multi-node fs cluster for the ceph/s3-tests conformance suite.
#
#   scripts/s3tests-cluster.sh up     # etcd + N fs nodes, wait until healthy
#   scripts/s3tests-cluster.sh down   # stop everything, remove the run dir
#   scripts/s3tests-cluster.sh logs   # dump every node's log
#
# The point is to run the *same* allow-list the single-node s3tests job runs
# (.github/s3tests/allow.txt), but over the replicated data plane: writes go
# out at quorum across failure domains and reads are served from wherever the
# fragments landed. The single-node suite cannot see any of that.
#
# The suite talks to node 0 only — s3tests.conf takes one endpoint. That still
# exercises the cluster path end to end, because node 0 is a coordinator, not
# an owner: it fans every object out to its peers.
#
# See .github/s3tests/README.md.

set -euo pipefail

cd "$(dirname "$0")/.."
readonly REPO="$PWD"

# Number of fs nodes. Each is its own rack (failure domain), so this is also
# the failure-domain count: rf2.5 places on three, which is why the default is
# three and not two.
NODES="${NODES:-3}"

# Node 0's S3 port. It must match the port in the suite config below; the other
# nodes take the ports after it.
S3_PORT="${S3_PORT:-8177}"

# Peer (replication) ports, one per node starting here.
PEER_PORT="${PEER_PORT:-7177}"

# etcd. Left empty, the script runs one in Docker on ETCD_PORT and removes it
# again on `down`. Point this at an existing etcd (CI service container, the
# dev/ kind cluster, ...) to skip that.
ETCD_ENDPOINT="${ETCD_ENDPOINT:-}"
ETCD_PORT="${ETCD_PORT:-12379}"
ETCD_IMAGE="${ETCD_IMAGE:-gcr.io/etcd-development/etcd:v3.6.1}"
readonly ETCD_CONTAINER="fs-s3tests-etcd"

# Everything the run writes: node configs, disks, logs, pidfiles.
RUN_DIR="${RUN_DIR:-$REPO/.s3tests-cluster}"

# The fs binary under test. Built from the working tree unless supplied.
FS_BIN="${FS_BIN:-$RUN_DIR/fs}"

# Peer mutual auth. Fixed: this cluster is local, throwaway and never exposed.
readonly CLUSTER_SECRET="s3tests-cluster-secret-0123456789"

readonly ETCD_PREFIX="/fs-s3tests"

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

# etcd_endpoint echoes the endpoint nodes should dial.
etcd_endpoint() {
  if [ -n "$ETCD_ENDPOINT" ]; then
    printf '%s' "$ETCD_ENDPOINT"
  else
    printf 'http://127.0.0.1:%s' "$ETCD_PORT"
  fi
}

start_etcd() {
  if [ -n "$ETCD_ENDPOINT" ]; then
    log "using existing etcd at $ETCD_ENDPOINT"
    return
  fi

  command -v docker >/dev/null 2>&1 \
    || die "docker not found; start an etcd yourself and set ETCD_ENDPOINT"

  docker rm -f "$ETCD_CONTAINER" >/dev/null 2>&1 || true

  log "starting etcd ($ETCD_IMAGE) on 127.0.0.1:$ETCD_PORT"
  docker run -d --name "$ETCD_CONTAINER" \
    -p "127.0.0.1:$ETCD_PORT:2379" \
    "$ETCD_IMAGE" \
    /usr/local/bin/etcd \
    --name s3tests \
    --data-dir /etcd-data \
    --listen-client-urls http://0.0.0.0:2379 \
    --advertise-client-urls "http://127.0.0.1:$ETCD_PORT" \
    >/dev/null

  # etcd is up when it answers a version request; /health needs a quorum check
  # that a single member reports only once it has elected itself.
  for _ in $(seq 1 60); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$ETCD_PORT/version" 2>/dev/null; then
      log "etcd up"
      return
    fi
    sleep 0.5
  done

  docker logs "$ETCD_CONTAINER" >&2 || true
  die "etcd did not come up"
}

# write_config renders one node's config: $1 is the node index.
write_config() {
  local i="$1"
  local dir="$RUN_DIR/n$i"
  local s3_port=$((S3_PORT + i))
  local peer_port=$((PEER_PORT + i))

  mkdir -p "$dir/disk0"

  # auth stays ON with the ceph/s3-tests credentials, exactly as the
  # single-node job runs it, so the suite exercises SigV4 through boto3.
  #
  # auth.source is left at its default (file): every node carries the same
  # static keys in its own config, so any node authenticates any request
  # without a shared credential store. The etcd source is a different feature
  # (runtime key management) and is not what this suite is measuring.
  #
  # Each node is its own rack, so N nodes are N failure domains.
  sed \
    -e "s|@S3_ADDR@|:$s3_port|g" \
    -e "s|@ROOT@|$dir|g" \
    -e "s|@NODE_ID@|n$i|g" \
    -e "s|@RACK@|rack-$i|g" \
    -e "s|@PEER_ADDR@|127.0.0.1:$peer_port|g" \
    -e "s|@SECRET@|$CLUSTER_SECRET|g" \
    -e "s|@DISK@|$dir/disk0|g" \
    -e "s|@ETCD@|$(etcd_endpoint)|g" \
    -e "s|@ETCD_PREFIX@|$ETCD_PREFIX|g" \
    "$REPO/.github/s3tests/cluster/node.yaml.tmpl" > "$dir/config.yaml"
}

wait_healthy() {
  local i="$1"
  local port=$((S3_PORT + i))

  for _ in $(seq 1 120); do
    if curl -fsS -o /dev/null "http://127.0.0.1:$port/health" 2>/dev/null; then
      log "node n$i healthy on :$port"
      return
    fi
    sleep 0.5
  done

  log "--- n$i log ---"
  tail -n 50 "$RUN_DIR/n$i/server.log" >&2 || true
  die "node n$i did not become healthy"
}

up() {
  mkdir -p "$RUN_DIR"

  if [ ! -x "$FS_BIN" ]; then
    log "building fs -> $FS_BIN"
    go build -o "$FS_BIN" ./cmd/fs
  fi

  start_etcd

  for i in $(seq 0 $((NODES - 1))); do
    write_config "$i"

    local dir="$RUN_DIR/n$i"
    "$FS_BIN" s3 --config "$dir/config.yaml" > "$dir/server.log" 2>&1 &
    echo "$!" > "$dir/fs.pid"
    log "started n$i (pid $(cat "$dir/fs.pid"))"
  done

  for i in $(seq 0 $((NODES - 1))); do
    wait_healthy "$i"
  done

  log
  log "cluster of $NODES nodes ready; S3 on http://127.0.0.1:$S3_PORT"
  log "run the suite with S3TEST_CONF=$REPO/.github/s3tests/cluster/s3tests.conf"
}

down() {
  # SIGTERM first: fs drains in-flight requests and deregisters from etcd,
  # which takes a few seconds. Wait for that before removing the run dir —
  # otherwise the configs and logs vanish from under a still-running node —
  # and escalate to SIGKILL for anything that has not gone by the deadline.
  local pids=()

  if [ -d "$RUN_DIR" ]; then
    for pidfile in "$RUN_DIR"/n*/fs.pid; do
      [ -f "$pidfile" ] || continue
      local pid
      pid="$(cat "$pidfile")"
      if kill "$pid" 2>/dev/null; then
        pids+=("$pid")
      fi
    done
  fi

  for _ in $(seq 1 60); do
    local alive=0
    for pid in ${pids[@]+"${pids[@]}"}; do
      kill -0 "$pid" 2>/dev/null && alive=1
    done
    [ "$alive" -eq 0 ] && break
    sleep 0.5
  done

  for pid in ${pids[@]+"${pids[@]}"}; do
    if kill -0 "$pid" 2>/dev/null; then
      log "node pid $pid did not exit on SIGTERM; killing"
      kill -9 "$pid" 2>/dev/null || true
    fi
  done

  if [ -z "$ETCD_ENDPOINT" ]; then
    docker rm -f "$ETCD_CONTAINER" >/dev/null 2>&1 || true
  fi

  rm -rf "$RUN_DIR"
  log "cluster down"
}

dump_logs() {
  for dir in "$RUN_DIR"/n*/; do
    [ -f "$dir/server.log" ] || continue
    log "===== $dir/server.log ====="
    cat "$dir/server.log" >&2
  done
}

case "${1:-up}" in
  up) up ;;
  down) down ;;
  logs) dump_logs ;;
  *) die "usage: $0 [up|down|logs]" ;;
esac
