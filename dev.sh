#!/usr/bin/env bash
# Jalankan j2-monitor secara lokal: http://127.0.0.1:8766
#   ./dev.sh                 -> DB lokal kosong (.dev/homelab.db)
#   PROD_COPY=1 ./dev.sh     -> pakai salinan DB produksi (.dev/prod-copy.db) agar grafik historis terisi
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p .dev
command -v sqlite3 >/dev/null || { echo "ERROR: sqlite3 belum terpasang (sudo apt install sqlite3)"; exit 1; }
command -v go >/dev/null || { echo "ERROR: Go belum terpasang"; exit 1; }

DB=.dev/homelab.db
if [[ "${PROD_COPY:-0}" == "1" ]]; then
  [[ -f .dev/prod-copy.db ]] || { echo "Ambil salinan dulu: ./deploy.sh --pull-db"; exit 1; }
  cp .dev/prod-copy.db .dev/homelab-prod.db
  DB=.dev/homelab-prod.db
fi

export DB_PATH="$PWD/$DB"
export STATIC_DIR="$PWD/frontend"
export LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8766}"
export SERVICES="${SERVICES:-none}"           # runit tidak ada di mesin dev
export MONITOR_TOKEN="${MONITOR_TOKEN:-dev-token}"

echo "DB      : $DB_PATH"
echo "URL     : http://$LISTEN_ADDR"
echo "Token   : $MONITOR_TOKEN  (untuk halaman Proses)"
cd backend && exec go run .
