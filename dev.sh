#!/usr/bin/env bash
# Jalankan j2-monitor secara lokal: http://127.0.0.1:8766
set -e
cd "$(dirname "$0")"
mkdir -p .dev
DB=.dev/homelab.db
if ! command -v sqlite3 >/dev/null; then
  echo "WARN: sqlite3 tidak ada (sudo apt install sqlite3) — /api/history akan gagal"
elif [ ! -f "$DB" ]; then
  sqlite3 "$DB" < deploy/schema.sql
fi
export DB_PATH="$PWD/$DB" STATIC_DIR="$PWD/frontend" LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8766}"
cd backend && exec go run .
