#!/bin/sh
# Reproducible check of the Docker Compose example.
#
# Builds xvia, starts the agent on this machine, starts the containers, and
# fetches both dev servers through the forwarded ports. Requires Docker with
# the compose plugin, Go and curl.
#
# Environment:
#   XVIA_SOCKET_DIR         socket directory used by the agent (default: a temp dir)
#   XVIA_DOCKER_SOCKET_DIR  the same directory as seen by the Docker daemon, if it
#                           differs (e.g. when this script runs inside a dev container)
set -eu

here=$(cd "$(dirname "$0")" && pwd)
root=$(cd "$here/../.." && pwd)
work=$(mktemp -d)
sock_dir=${XVIA_SOCKET_DIR:-$work/xvia}
export XVIA_SOCKET_DIR="${XVIA_DOCKER_SOCKET_DIR:-$sock_dir}"
project=xvia-example-$$
agent_pid=

cleanup() {
	docker compose -p "$project" -f "$here/compose.yaml" down --remove-orphans >/dev/null 2>&1 || true
	[ -n "$agent_pid" ] && kill "$agent_pid" 2>/dev/null && wait "$agent_pid" 2>/dev/null || true
	rm -rf "$work"
}
trap cleanup EXIT INT TERM

fail() { echo "FAIL: $*" >&2; exit 1; }

echo "==> building xvia"
(cd "$root" && CGO_ENABLED=0 go build -o "$work/xvia" ./cmd/xvia)

echo "==> checking compose file (no ports, no docker.sock, not privileged)"
config=$(docker compose -p "$project" -f "$here/compose.yaml" config)
echo "$config" | grep -qE '^\s+ports:' && fail "compose file publishes ports"
echo "$config" | grep -q 'docker.sock' && fail "compose file mounts docker.sock"
echo "$config" | grep -qE 'privileged: true|network_mode: host' && fail "compose file is privileged"

echo "==> starting agent ($sock_dir/agent.sock)"
"$work/xvia" agent --socket "$sock_dir/agent.sock" > "$work/agent.log" 2>&1 &
agent_pid=$!
i=0; until [ -S "$sock_dir/agent.sock" ]; do i=$((i+1)); [ $i -gt 50 ] && fail "agent did not start"; sleep 0.1; done

echo "==> starting containers"
docker compose -p "$project" -f "$here/compose.yaml" up -d --build --quiet-pull

fetch() {
	i=0
	until out=$(curl -fs --max-time 2 "http://127.0.0.1:$1/"); do
		i=$((i+1)); [ $i -gt 60 ] && { docker compose -p "$project" -f "$here/compose.yaml" logs; cat "$work/agent.log"; fail "port $1 not reachable"; }
		sleep 0.5
	done
	echo "    127.0.0.1:$1 -> $(echo "$out" | head -n1)"
}
echo "==> fetching through xvia"
fetch 8000
fetch 8001

echo "==> parallel requests"
pids=
for n in 1 2 3 4 5 6 7 8 9 10; do curl -fsS --max-time 5 -o /dev/null "http://127.0.0.1:8000/" & pids="$pids $!"; done
for p in $pids; do wait "$p" || fail "parallel request failed"; done

echo "==> stopping the api container; its forwarding must disappear"
docker compose -p "$project" -f "$here/compose.yaml" stop api >/dev/null
i=0; while curl -fsS --max-time 1 -o /dev/null "http://127.0.0.1:8001/" 2>/dev/null; do
	i=$((i+1)); [ $i -gt 20 ] && fail "port 8001 still forwarded"; sleep 0.5
done
curl -fsS --max-time 2 -o /dev/null "http://127.0.0.1:8000/" || fail "port 8000 broke"

echo "==> agent log"
sed 's/^/    /' "$work/agent.log"
echo "PASS"
