#!/usr/bin/env bash
# Build untuk Android/arm64 dan deploy ke j2-ssh
set -e
cd "$(dirname "$0")"
(cd backend && CGO_ENABLED=0 GOOS=android GOARCH=arm64 go build -o ../.dev/monitor .)
ssh j2-ssh 'cd ~/homelab/apps/go-monitor && cp monitor monitor.bak-$(date +%Y%m%d%H%M%S)'
scp .dev/monitor j2-ssh:homelab/apps/go-monitor/monitor.new
scp frontend/index.html j2-ssh:homelab/www/index.html
ssh j2-ssh 'cd ~/homelab/apps/go-monitor && mv monitor.new monitor && chmod +x monitor && sv restart homelab-go-monitor'
echo "Deployed."
