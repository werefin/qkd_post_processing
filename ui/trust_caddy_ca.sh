#!/usr/bin/env bash
# Trust Caddy's local CA so the browser shows no warning
# Run after docker compose up -d, on each machine that opens the demo
set -e

DOMAIN=qoj.postproc
LAN_IP=$(ip route get 1 2>/dev/null | awk '{print $7; exit}')
[ -z "$LAN_IP" ] && LAN_IP=$(hostname -I | awk '{print $1}')

# Make qoj.postproc resolve to this workstation
grep -q "$DOMAIN" /etc/hosts || echo "127.0.0.1 $DOMAIN" >> /etc/hosts

# Pull Caddy's root CA out of the container
echo "Extracting Caddy's local root CA..."
docker compose cp \
  caddy:/data/caddy/pki/authorities/local/root.crt ./caddy-root-ca.crt
# Public cert --> make it readable so client machines can scp it without sudo
chmod 644 ./caddy-root-ca.crt

# Install it into the system trust store (curl/CLI)
cp ./caddy-root-ca.crt /usr/local/share/ca-certificates/caddy-qoj.crt
update-ca-certificates

# Install it into the browser stores (Chrome + Firefox use NSS)
if command -v certutil >/dev/null; then
    for db in "$HOME"/.pki/nssdb "$HOME"/.mozilla/firefox/*.default*; do
        [ -d "$db" ] && certutil -d sql:"$db" -A -n "Caddy qoj_post_processing" -t "C,," -i ./caddy-root-ca.crt 2>/dev/null || true
    done
else
    echo "Install 'libnss3-tools' for automatic browser trust, or import"
    echo "caddy-root-ca.crt manually in the browser (authorities)"
fi

echo ""
echo "Done. Open: https://$DOMAIN:62444"
echo "For other machines: copy caddy-root-ca.crt and trust_ca_client.sh to them,"
echo "then run: bash trust_ca_client.sh $LAN_IP"
