#!/usr/bin/env bash
# Starts a 3-node musql cloud cluster as three processes on this host.
# usage: cluster-local.sh <root> <bindir> <admin-password> [start|stop]
# Nodes n1..n3 at locations fra,nyc,sin: HTTP 127.0.0.1:18081-3, libp2p tcp 19081-3.
# public_url is n1's for every node: a cluster issues tokens for one address.
set -euo pipefail
root=$(readlink -m "${1:?root dir}"); bin=$(readlink -m "${2:?bin dir}"); pw=${3:?admin password}
cmd=${4:-start}
locs=(fra nyc sin)

if [ "$cmd" = stop ]; then
  for i in 1 2 3; do
    [ -f "$root/n$i/pid" ] && kill "$(cat "$root/n$i/pid")" 2>/dev/null || true
  done
  exit 0
fi

dist=""
[ -d "$root/console/dist" ] && dist="$root/console/dist"
gen() { # gen <i> <with-peers>
  local i=$1 k
  mkdir -p "$root/n$i"
  {
    echo "listen: \"127.0.0.1:1808$i\""
    echo "public_url: \"http://127.0.0.1:18081\""
    echo "data_dir: \"$root/n$i/data\""
    [ -n "$dist" ] && printf 'console:\n  dir: "%s"\n' "$dist"
    printf 'node:\n  name: "n%s"\n  location: "%s"\n  p2p_listen: ["/ip4/127.0.0.1/tcp/1908%s"]\n' "$i" "${locs[$((i-1))]}" "$i"
    if [ "$2" = yes ]; then
      echo "peers:"
      for k in 1 2 3; do
        [ "$k" = "$i" ] && continue
        printf '  - name: "n%s"\n    location: "%s"\n    addrs: ["/ip4/127.0.0.1/tcp/1908%s"]\n    peer_id: "%s"\n' "$k" "${locs[$((k-1))]}" "$k" "$(cat "$root/n$k/peer_id")"
      done
    fi
  } > "$root/n$i/config.yaml"
}
for i in 1 2 3; do gen "$i" no; "$bin/musql-cloud" -peer-id -config "$root/n$i/config.yaml" > "$root/n$i/peer_id"; done
for i in 1 2 3; do gen "$i" yes; done
"$bin/musql-cloud" init -config "$root/n1/config.yaml" --admin-email admin@test --admin-password "$pw"
start() {
  nohup "$bin/musql-cloud" -config "$root/n$1/config.yaml" > "$root/n$1/log" 2>&1 &
  echo $! > "$root/n$1/pid"
}
for i in 1 2 3; do start "$i"; done
