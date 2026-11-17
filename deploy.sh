#!/usr/bin/env bash
# Deploy J2 Monitor ke j2-ssh (Samsung J2 Prime, Termux, ARMv7 32-bit).
#
#   ./deploy.sh              deploy (minta konfirmasi)
#   ./deploy.sh -y           deploy tanpa konfirmasi
#   ./deploy.sh --pull-db    hanya ambil salinan DB produksi ke .dev/prod-copy.db
#   ./deploy.sh --rollback [nama]   kembalikan backup terakhir, atau backup tertentu
#   ./deploy.sh --list-backups      tampilkan daftar backup di server
#
# Binary di-build LANGSUNG di HP (GOOS=android GOARCH=arm), sama seperti v1,
# sehingga tidak ada masalah arsitektur/linker. Sebelum mengganti apa pun,
# script membuat backup lengkap (binary, source, frontend, DB) dan otomatis
# rollback bila health check gagal.
set -euo pipefail
cd "$(dirname "$0")"

HOST="${DEPLOY_HOST:-j2-ssh}"
R_APP='$HOME/homelab/apps/go-monitor'
R_WWW='$HOME/homelab/www'
R_DB='$HOME/homelab/database/homelab.db'
R_BAK='$HOME/homelab/backup'
SERVICE=homelab-go-monitor

log() { printf '\033[36m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31mERROR:\033[0m %s\n' "$*" >&2; exit 1; }

pull_db() {
  mkdir -p .dev
  log "Mengambil salinan DB produksi (sqlite .backup, aman saat service berjalan)"
  ssh "$HOST" "sqlite3 $R_DB \".backup \$PREFIX/tmp/j2m-copy.db\" && cat \$PREFIX/tmp/j2m-copy.db && rm -f \$PREFIX/tmp/j2m-copy.db" > .dev/prod-copy.db
  sqlite3 .dev/prod-copy.db "PRAGMA integrity_check;" | grep -q '^ok$' || die "salinan DB rusak"
  log "Tersimpan di .dev/prod-copy.db ($(du -h .dev/prod-copy.db | cut -f1))"
}

rollback() {
  local want="${1:-}"
  [[ -z "$want" || "$want" =~ ^go-monitor-deploy-[0-9-]+$ ]] || die "nama backup tidak valid: $want"
  log "Rollback di $HOST ke ${want:-backup terakhir}"
  ssh "$HOST" "bash -s" <<EOF
set -e
export SVDIR=\$PREFIX/var/service
if [ -n "$want" ]; then B=$R_BAK/$want; else B=\$(ls -1d $R_BAK/go-monitor-deploy-* 2>/dev/null | tail -1); fi
[ -d "\$B" ] || { echo "Backup tidak ditemukan: \$B"; exit 1; }
echo "Backup: \$B"
cp "\$B/monitor" $R_APP/monitor
cp "\$B/main.go" $R_APP/main.go 2>/dev/null || true
rm -rf $R_WWW.rollback-tmp && cp -r "\$B/www" $R_WWW.rollback-tmp && rm -rf $R_WWW && mv $R_WWW.rollback-tmp $R_WWW
sv restart $SERVICE
sleep 3
curl -fsS http://127.0.0.1:8766/health && echo
echo "Rollback selesai. Kolom/tabel DB tambahan dari v2 tidak dihapus (binary v1 tetap kompatibel)."
echo "Bila perlu DB persis seperti sebelum deploy: cp \$B/homelab.db $R_DB (hentikan service dulu)."
EOF
}

case "${1:-}" in
  --pull-db) pull_db; exit 0 ;;
  --rollback) rollback "${2:-}"; exit 0 ;;
  --list-backups) ssh "$HOST" "ls -1d $R_BAK/go-monitor-deploy-* | xargs -n1 basename"; exit 0 ;;
esac

# ---------- 1. cek lokal ----------
log "Menjalankan test & vet lokal"
(cd backend && gofmt -l . | (! grep .) && go vet ./... && go test ./...) || die "test/vet/gofmt gagal"
for f in frontend/index.html frontend/assets/app.css frontend/assets/app.js; do [[ -s $f ]] || die "$f tidak ada"; done
if command -v node >/dev/null; then node --check frontend/assets/app.js || die "app.js syntax error"; fi

if [[ "${1:-}" != "-y" ]]; then
  read -r -p "Deploy ke $HOST (produksi) sekarang? ketik 'yes': " ans
  [[ "$ans" == "yes" ]] || die "dibatalkan"
fi

TS=$(date +%Y%m%d-%H%M%S)
PKG=$(mktemp -d)
trap 'rm -rf "$PKG"' EXIT
tar -C backend -czf "$PKG/src.tgz" go.mod $(cd backend && ls *.go | grep -v _test.go)
# cache-busting: ganti ?v=dev dengan versi deploy agar browser memuat aset baru
mkdir -p "$PKG/www" && cp -r frontend/. "$PKG/www/"
VER="$TS-$(git rev-parse --short HEAD 2>/dev/null || echo local)"
sed -i "s/?v=dev/?v=$VER/g" "$PKG/www/index.html"
tar -C "$PKG/www" -czf "$PKG/www.tgz" .

# ---------- 2. backup di server ----------
log "Backup di server -> $R_BAK/go-monitor-deploy-$TS"
ssh "$HOST" "bash -s" <<EOF
set -e
export SVDIR=\$PREFIX/var/service
B=$R_BAK/go-monitor-deploy-$TS
mkdir -p "\$B"
cp $R_APP/monitor "\$B/monitor"
cp $R_APP/main.go "\$B/main.go" 2>/dev/null || true
cp -r $R_WWW "\$B/www"
sqlite3 $R_DB ".backup \$B/homelab.db"
ls -la "\$B"
EOF

# ---------- 3. upload & build di HP ----------
log "Upload source & build native di HP (bisa 1-3 menit)"
scp -q "$PKG/src.tgz" "$PKG/www.tgz" "$HOST:/data/data/com.termux/files/usr/tmp/"
ssh "$HOST" "bash -s" <<EOF
set -e
export SVDIR=\$PREFIX/var/service
SRC=$R_APP/src
rm -rf "\$SRC" && mkdir -p "\$SRC"
tar -xzf \$PREFIX/tmp/src.tgz -C "\$SRC"
cd "\$SRC"
go build -trimpath -ldflags='-s -w' -o $R_APP/monitor.new .
test -x $R_APP/monitor.new
rm -rf $R_WWW.new && mkdir -p $R_WWW.new && tar -xzf \$PREFIX/tmp/www.tgz -C $R_WWW.new
rm -f \$PREFIX/tmp/src.tgz \$PREFIX/tmp/www.tgz
EOF

# ---------- 4. swap & restart ----------
log "Mengganti binary & frontend, restart service"
ssh "$HOST" "bash -s" <<EOF
set -e
export SVDIR=\$PREFIX/var/service
cd $R_APP
mv monitor.new monitor && chmod +x monitor
rm -rf $R_WWW.prev && mv $R_WWW $R_WWW.prev && mv $R_WWW.new $R_WWW
sv restart $SERVICE
EOF

# ---------- 5. health check ----------
log "Health check"
ok=0
for i in $(seq 1 10); do
  sleep 3
  if ssh "$HOST" 'curl -fsS http://127.0.0.1:8766/api/version && curl -fsS -o /dev/null http://127.0.0.1:8766/api/status && curl -fsS -o /dev/null -H "Host: j2-monitor.homelabchester.my.id" http://127.0.0.1:8080/'; then ok=1; break; fi
done
if [[ $ok != 1 ]]; then
  printf '\033[31mHealth check gagal — rollback otomatis\033[0m\n'
  rollback
  exit 1
fi
echo
log "Deploy selesai. Backup: $R_BAK/go-monitor-deploy-$TS"
