#!/usr/bin/env bash
# Trusts the network CA on a machine that opens the node dashboards, so every node's HTTPS
# certificate is accepted; run as the user who uses the browser, it asks for sudo when needed
set -euo pipefail

CA="${1:?usage: trust-ca.sh path/to/ca.pem}"
[[ -f "$CA" ]] || { echo "no such file: $CA" >&2; exit 1; }

echo "system trust store (curl, command-line tools)"
sudo cp "$CA" /usr/local/share/ca-certificates/qkd-network-ca.crt
sudo update-ca-certificates >/dev/null

echo "browser trust stores (Chrome, Chromium, Firefox)"
if ! command -v certutil >/dev/null 2>&1; then
	echo "  certutil missing: install libnss3-tools, or import $CA in the browser as an authority"
	exit 0
fi
mkdir -p "$HOME/.pki/nssdb"
[[ -f "$HOME/.pki/nssdb/cert9.db" ]] || certutil -N -d "sql:$HOME/.pki/nssdb" --empty-password
for db in "$HOME/.pki/nssdb" "$HOME"/.mozilla/firefox/*.default* "$HOME"/snap/firefox/common/.mozilla/firefox/*.default*; do
	[[ -d "$db" ]] && certutil -d "sql:$db" -A -n "QKD network CA" -t "C,," -i "$CA" && echo "  trusted in $db"
done
echo "Restart the browser; every node's dashboard is now trusted"
