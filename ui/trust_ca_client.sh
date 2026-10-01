#!/usr/bin/env bash
# For every OTHER machine on the LAN that should open the demo:
# 1) copy the certificate and this script next to each other:
#    scp <deploy-host>:/path/to/ui/caddy-root-ca.crt .
#    scp <deploy-host>:/path/to/ui/trust_ca_client.sh .
# 2) run it with the deploy host's LAN IP (not with sudo -- it asks for your
#    password only when needed): bash trust_ca_client.sh 192.168.1.148
set -e

DOMAIN=qoj.postproc
SERVER_IP="${1:?usage: trust_ca_client.sh <deploy-host-lan-ip>}"
HERE="$(cd "$(dirname "$0")" && pwd)"
CA="$HERE/caddy-root-ca.crt"

if [ ! -f "$CA" ]; then
  echo "   caddy-root-ca.crt not found in: $HERE"
  echo "   Copy it here first:"
  echo "   scp <deploy-host>:/path/to/ui/caddy-root-ca.crt \"$HERE\""
  exit 1
fi

echo "   point $DOMAIN at the server ($SERVER_IP)"
grep -q "$DOMAIN" /etc/hosts || echo "$SERVER_IP $DOMAIN" | sudo tee -a /etc/hosts >/dev/null

echo "   trust the certificate system-wide (curl/CLI)"
sudo cp "$CA" /usr/local/share/ca-certificates/caddy-qoj.crt
sudo update-ca-certificates >/dev/null 2>&1 || true

echo "   trust the certificate in browsers (Chrome/Brave/Firefox)"
command -v certutil >/dev/null 2>&1 || sudo apt-get install -y libnss3-tools >/dev/null 2>&1 || true
if command -v certutil >/dev/null 2>&1; then
  mkdir -p "$HOME/.pki/nssdb"
  [ -f "$HOME/.pki/nssdb/cert9.db" ] || certutil -N -d sql:"$HOME/.pki/nssdb" --empty-password 2>/dev/null || true
  for db in "$HOME/.pki/nssdb" "$HOME"/.mozilla/firefox/*.default*; do
    [ -d "$db" ] && certutil -d sql:"$db" -A -n "qoj_post_processing Caddy" -t "C,," -i "$CA" 2>/dev/null || true
  done
  echo "   ok"
else
  echo "   (couldn't install libnss3-tools --> import $CA manually in the browser instead)"
fi

echo
echo "Done. Restart your browser, then open: https://$DOMAIN:62444"
echo "You should see a secure lock and no warning."
