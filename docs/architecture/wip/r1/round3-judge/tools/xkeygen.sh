#!/bin/bash
# xkeygen.sh DIR: the judge's own test keys for the xray lab (R1 round 3,
# priority 5), in the format of the xray tools' keys.env (mkxray.py): a
# fresh x25519 pair, uuid, shortId, path, ML-KEM-768 VLESS encryption pair,
# Shadowsocks-2022 key, and a self-signed TLS cert with its SHA-256 pin.
# Nothing secret is printed; DIR is created 0700 outside the repo.
set -eu
DIR=${1:?}
X=${XR:-/root/rxray/bin/xray}
mkdir -p "$DIR"; chmod 700 "$DIR"
k=$("$X" x25519)
priv=$(echo "$k" | awk -F': ' '/^PrivateKey/{print $2}')
pub=$(echo "$k" | awk -F': ' '/^Password/{print $2}')
uuid=$("$X" uuid)
sid=$(openssl rand -hex 8)
xpath=/$(openssl rand -hex 6)
v=$("$X" vlessenc)
dec=$(echo "$v" | awk -F'"' '/ML-KEM-768/{f=1} f && $2 == "decryption" {print $4; exit}')
enc=$(echo "$v" | awk -F'"' '/ML-KEM-768/{f=1} f && $2 == "encryption" {print $4; exit}')
ssk=$(openssl rand -base64 16)
openssl req -x509 -newkey ec -pkeyopt ec_paramgen_curve:prime256v1 -nodes \
  -keyout "$DIR/tls.key" -out "$DIR/tls.crt" -days 30 -subj /CN=www.example.test \
  -addext subjectAltName=DNS:www.example.test 2>/dev/null
pcs=$(openssl x509 -in "$DIR/tls.crt" -outform DER | sha256sum | cut -c1-64)
umask 077
printf 'PRIV=%s\nPUB=%s\nUUID=%s\nSID=%s\nXPATH=%s\nDEC=%s\nENC=%s\nSSK=%s\nPCS=%s\n' \
  "$priv" "$pub" "$uuid" "$sid" "$xpath" "$dec" "$enc" "$ssk" "$pcs" > "$DIR/keys.env"
awk -F= '{print $1, length($2)}' "$DIR/keys.env"
