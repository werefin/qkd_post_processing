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
  ./setup.sh link all                            declare a QKD link between every pair of nodes (A-B, A-C, ..., C-D)
  ./setup.sh sae SAE_ID NODE_ID                  certificate for an application of NODE_ID
  ./setup.sh bundle ID                           nodes/ID.tar.gz: everything node ID needs
  ./setup.sh controller init HOST[,HOST...]      the network controller's certificate and an operator certificate
  ./setup.sh controller up|down|logs|status      run the controller on this machine
  ./setup.sh ctl METHOD PATH [JSON]              call the controller as operator, e.g. ctl GET /v1/network
  ./setup.sh use-controller ID URL               node ID takes its settings from the controller at URL

On each node (inside a clone of this repository):
  ./setup.sh install ID.tar.gz                   unpack a bundle made by "bundle"
  ./setup.sh up ID                               build and start the node, or restart after a change
                                                 (NO_BUILD=1: pull images from a registry instead)
  ./setup.sh down ID | logs ID | status ID

Changing nodes, links or SAEs changes network.json: bundle and install it on every node, then up
Images from a registry instead of a local build: APP_IMAGE, KMS_IMAGE and IMAGE_TAG in the node's .env, then NO_BUILD=1 up
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

CONTROLLER="$HERE/controller"

controller_compose() {
	[[ -f "$CONTROLLER/.env" ]] || { echo "run ./setup.sh controller init HOSTS first" >&2; exit 1; }
	docker compose -f "$HERE/controller.yml" --env-file "$CONTROLLER/.env" -p qkd-controller "$@"
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
		# the node's services show it to each other; nothing outside the node's Docker network sees it
		token="$(head -c 24 /dev/urandom | base64 | tr '+/' 'xy')"
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
SERVICE_TOKEN=$token
CONTROLLER_URL=
EOF
		chmod 600 "$HERE/$dir/.env"
		echo
		echo "Dashboard login for $id: admin / $password  (shown once, only its hash is stored)"
	fi
	echo "Node $id: dashboard https://$first_host:$ui_port, KMS https://$first_host:$kms_port"
	;;
link)
	if [[ "${1:?node A, or all}" == all ]]; then
		net add-link all
		echo "Every pair of nodes linked; bundle and install network.json on every node"
	else
		net add-link "$1" "${2:?node B}"
		echo "Link $1-$2 declared; bundle and install network.json on every node"
	fi
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
controller)
	sub="${1:-}"; shift || true
	case "$sub" in
	init)
		hosts="${1:?hosts}"
		[[ -f "$CA_DIR/ca.key" ]] || { echo "run ./setup.sh ca first" >&2; exit 1; }
		mkdir -p "$CONTROLLER/data"
		qkdpki controller -ca ca -hosts "$hosts" -out controller/pki
		echo "https://${hosts%%,*}:8080" >"$CONTROLLER/url"
		[[ -f "$CONTROLLER/.env" ]] || printf 'APP_UID=%s\nAPP_GID=%s\nCONTROLLER_PORT=8080\n' "$(id -u)" "$(id -g)" >"$CONTROLLER/.env"
		echo "Controller at https://${hosts%%,*}:8080; on each node: ./setup.sh use-controller ID https://${hosts%%,*}:8080"
		;;
	up)     mkdir -p "$CONTROLLER/data"; if [[ -n "${NO_BUILD:-}" ]]; then controller_compose pull --ignore-pull-failures; controller_compose up -d; else controller_compose up -d --build; fi ;;
	down)   controller_compose down ;;
	logs)   controller_compose logs -f --tail=200 ;;
	status) controller_compose ps ;;
	*) usage; exit 1 ;;
	esac
	;;
ctl)
	method="${1:?method}"; path="${2:?path}"; pki="$CONTROLLER/pki"
	url="$(cat "$CONTROLLER/url" 2>/dev/null)" || { echo "run ./setup.sh controller init HOSTS first" >&2; exit 1; }
	curl -sS --fail-with-body --cert "$pki/operator.pem" --key "$pki/operator.key" --cacert "$pki/ca.pem" \
		-X "$method" -H 'Content-Type: application/json' ${3:+--data "$3"} "$url$path"
	echo
	;;
use-controller)
	id="${1:?node ID}"; url="${2:?controller URL}"; need_node "$id"
	sed -i "s|^CONTROLLER_URL=.*|CONTROLLER_URL=$url|" "$NODES/$id/.env"
	grep -q '^CONTROLLER_URL=' "$NODES/$id/.env" || echo "CONTROLLER_URL=$url" >>"$NODES/$id/.env"
	echo "Node $id follows the controller at $url after ./setup.sh up $id"
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
	# a node set up before the services were split has no token yet
	grep -q '^SERVICE_TOKEN=' "$NODES/$id/.env" || echo "SERVICE_TOKEN=$(head -c 24 /dev/urandom | base64 | tr '+/' 'xy')" >>"$NODES/$id/.env"
	for d in kms import recordings continuous evaluations coincidence_logs config sim-keys cache caddy tmp; do mkdir -p "$NODES/$id/data/$d"; done
	# NO_BUILD=1 pulls the images named in .env (APP_IMAGE, KMS_IMAGE, IMAGE_TAG) instead of building them
	if [[ -n "${NO_BUILD:-}" ]]; then compose "$id" pull --ignore-pull-failures kms agent ui; compose "$id" up -d --remove-orphans; else compose "$id" up -d --build --remove-orphans; fi
	compose "$id" ps
	;;
down)   compose "${1:?node ID}" down ;;
logs)   compose "${1:?node ID}" logs -f --tail=200 ;;
status) compose "${1:?node ID}" ps ;;
*) usage; [[ -z "$cmd" ]] || exit 1 ;;
esac
