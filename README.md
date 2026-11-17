# J2 Monitor

Dashboard monitoring homelab yang berjalan di **Samsung Galaxy J2 Prime** (Android 7.1.1, Termux, ARMv7 32-bit).
Produksi: https://j2-monitor.homelabchester.my.id/

Backend Go tanpa dependency eksternal + frontend statis (HTML/CSS/JS tanpa build step) dengan Apache ECharts.

## Arsitektur

```
Browser ──HTTPS──> Cloudflare Tunnel ──> nginx :8080 ──┬── /         -> ~/homelab/www (frontend statis)
                                                       └── /api/*    -> go-monitor 127.0.0.1:8766
go-monitor (runit: homelab-go-monitor)
  ├─ collector tiap 5 dtk  : /proc, /sys  -> status saat ini + ring buffer 1 jam (memori)
  ├─ alert engine          : evaluasi ambang tiap sampel (debounce 3 sampel + hysteresis)
  ├─ history writer 60 dtk : ringkasan per menit -> SQLite (CLI sqlite3), retensi 7 hari
  └─ HTTP API              : JSON, GET-only, query divalidasi & dibatasi
```

| File | Isi |
|---|---|
| `backend/config.go` | Konfigurasi dari environment variable |
| `backend/metrics.go` | Collector & parser `/proc` / `/sys` |
| `backend/store.go` | SQLite (migrasi additive, history, downsampling, cache) |
| `backend/alerts.go` | Alert engine & event timeline |
| `backend/procs.go` | Daftar proses (dilindungi token) |
| `backend/main.go` | HTTP server & handler |
| `frontend/` | `index.html`, `assets/app.css`, `assets/app.js` |
| `deploy/` | Salinan config nginx & script runit produksi (referensi) |

## Fitur

- **Ringkasan**: status Online / Degraded / Offline, hostname, OS, uptime, waktu update, KPI CPU/RAM/storage/jaringan dengan sparkline, suhu & baterai, status service, ringkasan alert aktif.
- **Grafik** (ECharts): CPU (+ puncak per bucket), RAM/swap, jaringan RX/TX, penggunaan disk, disk I/O, suhu CPU.
  Rentang 1m/5m/15m/1j (real-time, sampel 5 dtk) dan 6j/12j/24j/7h (historis, di-downsample di server ≤ 360 titik).
  Tooltip + crosshair, zoom/pan (slider, Ctrl+scroll), legend toggle, garis ambang, min/avg/max sesuai jendela yang terlihat,
  export PNG, putar ulang (playback) data historis, grafik saling tersinkron, celah data ditampilkan sebagai jeda. Deep link: `#metrics/24h`.
- **Sistem**: per-core CPU + frekuensi, load 1/5/15, memori (used/available/cached/buffers/swap), filesystem, disk I/O + IOPS,
  interface jaringan (rate, total, error, drop), kernel/arsitektur/boot time, suhu & baterai.
- **Proses**: PID, nama, state, CPU, RSS, %mem, thread; sorting & pencarian. Dilindungi token. Hanya nama proses (bukan command line) yang dikirim.
- **Alert**: CPU, RAM, disk, suhu CPU, suhu baterai, service down (server) + offline (dideteksi browser). Severity info/warning/critical,
  status active/resolved, durasi, timeline tersimpan di SQLite (30 hari), notifikasi browser opsional (cooldown 10 menit per jenis).
- **UI**: dark navy dengan aksen emerald/cyan/violet, tema terang/gelap/sistem, sidebar (desktop) & bottom nav (mobile),
  skeleton loading, empty/error/offline state, polling berhenti saat tab tersembunyi + backoff saat offline,
  navigasi keyboard, skip link, `prefers-reduced-motion`.

## Menjalankan secara lokal

Butuh Go ≥ 1.27 dan `sqlite3`.

```bash
./dev.sh                     # http://127.0.0.1:8766, DB kosong di .dev/homelab.db
./deploy.sh --pull-db        # (opsional) ambil salinan DB produksi
PROD_COPY=1 ./dev.sh         # jalankan dengan salinan DB produksi (grafik historis terisi)
cd backend && go test ./...  # unit test
```

Di mesin dev, metrik yang tampil adalah milik mesin dev itu sendiri. `SERVICES=none` dan token `dev-token` diset oleh `dev.sh`.

## Konfigurasi (environment variable)

| Variabel | Default | Keterangan |
|---|---|---|
| `LISTEN_ADDR` | `127.0.0.1:8766` | Alamat HTTP |
| `DB_PATH` | `~/homelab/database/homelab.db` (Termux) | File SQLite |
| `STATIC_DIR` | – | Bila diisi, backend juga menyajikan frontend (mode dev) |
| `DISK_PATH` | `/data` (atau `/`) | Storage utama untuk KPI & alert disk |
| `FS_PATHS` | `/data,/system,/` | Mount yang ditampilkan (duplikat otomatis disaring) |
| `SERVICES` | `nginx,sshd,cloudflared` | Service runit yang dipantau (`none` = tidak ada) |
| `SV_DIR` | `$PREFIX/var/service` | Direktori service runit |
| `MONITOR_TOKEN` | – | Token Bearer untuk `/api/processes`. Kosong = endpoint nonaktif |
| `ALLOW_PUBLIC_PROCESSES` | – | `1` = daftar proses publik tanpa token (tidak disarankan di produksi) |
| `RETENTION_DAYS` | `7` | Retensi history (1–90) |
| `THRESH_CPU` | `85,95` | warning,critical (%) |
| `THRESH_RAM` | `85,95` | % |
| `THRESH_DISK` | `80,90` | % |
| `THRESH_TEMP` | `60,70` | °C suhu CPU |
| `THRESH_BATTERY_TEMP` | `42,47` | °C suhu baterai |

Di produksi, variabel diset di script runit `$PREFIX/var/service/homelab-go-monitor/run`, contoh:

```sh
#!/data/data/com.termux/files/usr/bin/sh
cd "$HOME/homelab/apps/go-monitor"
export MONITOR_TOKEN="ganti-dengan-token-acak-panjang"
exec ./monitor
```

## API

Semua endpoint GET, respon JSON, `Cache-Control: no-store`. Metode lain → 405.

| Endpoint | Keterangan |
|---|---|
| `GET /health` | `{ok, service, time}` |
| `GET /api/status` | Snapshot terkini. Field v1 tetap: `hostname, architecture, cpu_cores, cpu_percent, load[], uptime, memory{total,used,free,percent,swap_*}, disk{total,used,free,percent}, services{}, time`. Tambahan v2: `os, kernel, model, boot_time, interval, cpu{per_core[],freq_mhz[]}, memory{available,cached,buffers}, network{rx_rate,tx_rate,rx_total,tx_total,interfaces[]}, disk_io{devices,read_rate,write_rate,read_iops,write_iops}, filesystems[], temperature{cpu,cpu_source,battery}, battery{level,status}, containers (null), health{state,reasons[]}`. Nilai `null` = tidak didukung/belum tersedia. |
| `GET /api/live?minutes=1..60&since=<unix>` | Sampel 5 detik dari memori: `{interval, points:[{time,cpu,ram,swap,disk,net_rx,net_tx,disk_read,disk_write,temp}]}` |
| `GET /api/history?hours=1..168[&points=30..2000]` | Tanpa `points`: data mentah per menit (kompatibel v1). Dengan `points`: rata-rata per bucket, `step` = detik per titik, plus `cpu_max`. Cache 30 dtk. |
| `GET /api/alerts?limit=1..300` | `{active:[], events:[]}` — event `{id,key,severity,message,value,started_at,resolved_at}` |
| `GET /api/config` | Versi, interval, retensi, ambang, service, fitur |
| `GET /api/processes` | Butuh `Authorization: Bearer <MONITOR_TOKEN>`. 401 token salah, 403 nonaktif. |
| `GET /api/version` | Versi backend & Go |

## Batasan di Android/Termux

- **Proses**: Android membatasi `/proc` — hanya proses milik user Termux yang terlihat.
- **Docker/container**: tidak tersedia; ditampilkan "Tidak didukung".
- **Suhu CPU**: dari sensor `tsens_tz_sensor*` (nilai tertinggi). Suhu baterai dari `power_supply/battery`. Bila tidak ada sensor → N/A.
- **Core CPU**: core yang di-hotplug offline oleh kernel ditampilkan "offline", bukan 0%.
- **Frekuensi CPU**: dari `cpufreq/scaling_cur_freq`; N/A bila tidak terbaca.
- Data jaringan, disk I/O, dan suhu di history baru terekam sejak v2.

## Deployment

```bash
./deploy.sh        # minta konfirmasi 'yes'
```

Langkah yang dilakukan:
1. `gofmt`, `go vet`, `go test`, cek sintaks JS di lokal.
2. Backup di server ke `~/homelab/backup/go-monitor-deploy-<waktu>/` (binary, main.go, www, salinan DB via `.backup`).
3. Upload source → build **native di HP** (`android/arm`) → `monitor.new`.
4. Ganti binary & frontend (`www` lama disimpan sebagai `www.prev`), `sv restart homelab-go-monitor`.
5. Health check (`/api/version`, `/api/status`, nginx). Bila gagal → **rollback otomatis**.

Aset diberi cache-busting `?v=<waktu>-<commit>` agar browser tidak memakai JS/CSS lama.
Konfigurasi nginx tidak diubah oleh deploy.

Saat pertama start, v2 menjalankan migrasi DB yang **hanya menambah** kolom (`net_rx, net_tx, disk_read, disk_write, temp, cpu_max`),
index `timestamp`, dan tabel `monitor_events`. Tidak ada data yang dihapus; binary v1 tetap bisa memakai DB tersebut.

## Rollback

```bash
./deploy.sh --list-backups                                 # daftar backup di server
./deploy.sh --rollback                                     # pulihkan backup terakhir & restart
./deploy.sh --rollback go-monitor-deploy-20261118-063250   # pulihkan backup tertentu
```

Setiap deploy membuat backup baru berisi versi yang sedang berjalan **sebelum** deploy tersebut.
Backup versi v1 (sebelum redesign) adalah `go-monitor-deploy-20261118-063250`.

Manual di server:

```sh
B=$(ls -1d ~/homelab/backup/go-monitor-deploy-* | tail -1)
cp "$B/monitor" ~/homelab/apps/go-monitor/monitor
rm -rf ~/homelab/www && cp -r "$B/www" ~/homelab/www
sv restart homelab-go-monitor
# opsional, DB persis seperti sebelum deploy:
# sv stop homelab-go-monitor && cp "$B/homelab.db" ~/homelab/database/homelab.db && sv start homelab-go-monitor
```
