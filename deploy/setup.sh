#!/usr/bin/env bash
# Builds and runs QKD network nodes with nothing but Docker: credentials, the shared network
# description, and each node's containers --> run from anywhere; everything lands under deploy/
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
CA_DIR="$HERE/ca"
NODES="$HERE/nodes"
NETWORK="$NODES/network.json"
TAG="${IMAGE_TAG:-local}"
KMS_IMAGE="qkd_post_processing/kms:$TAG"

usage() {
	cat <<'EOF'
On the administration machine (holds the CA, which never goes to a node):
  ./setup.sh ca                                  create the network CA (once)
  ./setup.sh node ID HOST[,HOST...] [UI_PORT] [KMS_PORT]
                                                 credentials, dashboard login and .env for one node
  ./setup.sh link A B                            declare a QKD link between nodes A and B
  ./setup.sh sae SAE_ID NODE_ID                  certificate for an application of NODE_ID
  ./setup.sh bundle ID                           nodes/ID.tar.gz: everything node ID needs

On each node (inside a clone of this repository):
  ./setup.sh install ID.tar.gz                   unpack a bundle made by "bundle"
  ./setup.sh up ID                               build and start the node, or restart after a change
                                                 (NO_BUILD=1: use images already present)
  ./setup.sh down ID | logs ID | status ID

Changing nodes, links or SAEs changes network.json: bundle and install it on every node, then up
EOF
}

build_kms() {
	docker image inspect "$KMS_IMAGE" >/dev/null 2>&1 || docker build -q --target kms -t "$KMS_IMAGE" "$HERE/.." >/dev/null
}

# qkdpki runs from the kms image as the calling user, with deploy/ mounted at /work
qkdpki() {
	build_kms
	docker run --rm --user "$(id -u):$(id -g)" -v "$HERE:/work" -w /work --entrypoint qkdpki "$KMS_IMAGE" "$@"
}

net() { qkdpki net -file nodes/network.json "$@"; }

need_node() {
	[[ -f "$NODES/$1/.env" ]] || { echo "no node $1 here (nodes/$1/.env missing)" >&2; exit 1; }
}

compose() {
	local id="$1"; shift
	need_node "$id"
	docker compose -f "$HERE/compose.yml" --env-file "$NODES/$id/.env" -p "qkd-$(echo "$id" | tr '[:upper:]' '[:lower:]')" "$@"
}

cmd="${1:-}"; shift || true
case "$cmd" in
ca)
	[[ -f "$CA_DIR/ca.key" ]] && { echo "CA already exists in $CA_DIR" >&2; exit 1; }
	mkdir -p "$NODES"
	qkdpki ca -out ca
	echo "Keep deploy/ca/ca.key offline; it can mint credentials for any node"
	;;
node)
	id="${1:?node ID}"; hosts="${2:?hosts}"; ui_port="${3:-443}"; kms_port="${4:-8443}"
	[[ "$id" =~ ^[A-Za-z0-9_-]+$ ]] || { echo "node IDs use letters, digits, - and _ only" >&2; exit 1; }
	[[ -f "$CA_DIR/ca.key" ]] || { echo "run ./setup.sh ca first" >&2; exit 1; }
	dir="nodes/$id"
	qkdpki node -ca ca -id "$id" -hosts "$hosts" -out "$dir/pki"
	qkdpki sae -ca ca -id "dashboard-$id" -out "$dir/pki/app"
	first_host="${hosts%%,*}"
	net add-node "$id" "https://$first_host:$kms_port"
	net add-sae "dashboard-$id" "$id"
	if [[ ! -f "$HERE/$dir/.env" ]]; then
		# 15 random bytes: 120 bits, exactly 20 base64 characters; no stage of this pipe is cut short
		password="$(head -c 15 /dev/urandom | base64 | tr '+/' 'xy')"
		hash="$(printf '%s\n%s\n' "$password" "$password" | docker run --rm -i caddy:2.8 caddy hash-password 2>/dev/null | tail -n1)"
		cat >"$HERE/$dir/.env" <<EOF
NODE_ID=$id
NODE_DIR=./nodes/$id
APP_UID=$(id -u)
APP_GID=$(id -g)
UI_PORT=$ui_port
KMS_PORT=$kms_port
UI_USER=admin
UI_PASSWORD_HASH='$hash'
UPLOAD_MAX_MB=102400
RECORDINGS_MAX_GB=0
EOF
		chmod 600 "$HERE/$dir/.env"
		echo
		echo "Dashboard login for $id: admin / $password  (shown once, only its hash is stored)"
	fi
	echo "Node $id: dashboard https://$first_host:$ui_port, KMS https://$first_host:$kms_port"
	;;
link)
	net add-link "${1:?node A}" "${2:?node B}"
	echo "Link $1-$2 declared; bundle and install network.json on every node"
	;;
sae)
	sae="${1:?SAE ID}"; node="${2:?node ID}"
	qkdpki sae -ca ca -id "$sae" -out "saes/$sae"
	net add-sae "$sae" "$node"
	echo "Give deploy/saes/$sae/ (sae.pem, sae.key, ca.pem) to the application; it calls https://<$node>:KMS_PORT/api/v1/keys/..."
	;;
bundle)
	id="${1:?node ID}"; need_node "$id"
	cp "$NETWORK" "$NODES/$id/network.json"
	tar -C "$NODES" -czf "$NODES/$id.tar.gz" --exclude="$id/data" "$id"
	chmod 600 "$NODES/$id.tar.gz"
	echo "nodes/$id.tar.gz holds node $id's private keys: copy it over a secure channel, then delete it"
	;;
install)
	archive="${1:?bundle archive}"
	mkdir -p "$NODES"
	tar -C "$NODES" -xzf "$archive"
	id="$(tar -tzf "$archive" | head -n1 | cut -d/ -f1)"
	# the containers run as whoever installs the node, so its files stay theirs
	sed -i "s/^APP_UID=.*/APP_UID=$(id -u)/; s/^APP_GID=.*/APP_GID=$(id -g)/" "$NODES/$id/.env"
	git -C "$HERE/.." config core.hooksPath .githooks 2>/dev/null || true
	echo "Installed $id; now ./setup.sh up $id"
	;;
up)
	id="${1:?node ID}"; need_node "$id"
	[[ -f "$NODES/$id/network.json" ]] || cp "$NETWORK" "$NODES/$id/network.json"
	for d in kms import recordings continuous evaluations config sim-keys cache caddy tmp; do mkdir -p "$NODES/$id/data/$d"; done
	# NO_BUILD=1 starts from images already present (built elsewhere or pulled from a registry)
	if [[ -n "${NO_BUILD:-}" ]]; then compose "$id" up -d --remove-orphans; else compose "$id" up -d --build --remove-orphans; fi
	compose "$id" ps
	;;
down)   compose "${1:?node ID}" down ;;
logs)   compose "${1:?node ID}" logs -f --tail=200 ;;
status) compose "${1:?node ID}" ps ;;
*) usage; [[ -z "$cmd" ]] || exit 1 ;;
esac
