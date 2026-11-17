/* J2 Monitor v2 — frontend.
 * Tanpa build step. Satu dependency: Apache ECharts (CDN) untuk grafik.
 * Semua angka berasal dari API backend; tidak ada data simulasi.
 */
'use strict';

(() => {
  // ------------------------------------------------------------------ utils
  const $ = (id) => document.getElementById(id);
  const $$ = (sel, root = document) => Array.from(root.querySelectorAll(sel));
  const RM = window.matchMedia('(prefers-reduced-motion: reduce)');
  const NF1 = new Intl.NumberFormat('id-ID', { maximumFractionDigits: 1 });
  const NF0 = new Intl.NumberFormat('id-ID', { maximumFractionDigits: 0 });
  const NA = '<em class="na">N/A</em>';

  const esc = (s) => String(s ?? '').replace(/[&<>"']/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
  const isNum = (v) => typeof v === 'number' && isFinite(v);
  const n1 = (v) => (isNum(v) ? NF1.format(v) : '—');

  function fmtBytes(b, digits = 1) {
    if (!isNum(b)) return '—';
    const u = ['B', 'KB', 'MB', 'GB', 'TB'];
    let i = 0;
    while (Math.abs(b) >= 1024 && i < u.length - 1) { b /= 1024; i++; }
    return new Intl.NumberFormat('id-ID', { maximumFractionDigits: i === 0 ? 0 : digits }).format(b) + ' ' + u[i];
  }
  const fmtRate = (b) => (isNum(b) ? fmtBytes(b) + '/s' : '—');

  function fmtDur(sec) {
    if (!isNum(sec)) return '—';
    const d = Math.floor(sec / 86400), h = Math.floor((sec % 86400) / 3600), m = Math.floor((sec % 3600) / 60);
    if (d) return `${d} hari ${h} jam`;
    if (h) return `${h} jam ${m} mnt`;
    if (m) return `${m} mnt`;
    return `${Math.floor(sec)} dtk`;
  }
  function fmtAgo(ts) {
    if (!ts) return '—';
    const s = Math.max(0, Math.round(Date.now() / 1000 - ts));
    if (s < 5) return 'baru saja';
    if (s < 60) return `${s} dtk lalu`;
    if (s < 3600) return `${Math.floor(s / 60)} mnt lalu`;
    return `${Math.floor(s / 3600)} jam lalu`;
  }
  const fmtTime = (ts) => new Date(ts * 1000).toLocaleString('id-ID', { day: '2-digit', month: 'short', hour: '2-digit', minute: '2-digit', second: '2-digit' });

  const store = {
    get(k, def) { try { const v = localStorage.getItem('j2m.' + k); return v == null ? def : JSON.parse(v); } catch { return def; } },
    set(k, v) { try { localStorage.setItem('j2m.' + k, JSON.stringify(v)); } catch { /* storage tidak tersedia */ } },
    del(k) { try { localStorage.removeItem('j2m.' + k); } catch { /* */ } },
  };

  function level(v, th) {
    if (!isNum(v) || !th) return 'ok';
    if (v >= th.critical) return 'crit';
    if (v >= th.warning) return 'warn';
    return 'ok';
  }

  function cssVar(name) { return getComputedStyle(document.documentElement).getPropertyValue(name).trim(); }

  // ------------------------------------------------------------------ state
  const settings = Object.assign({ theme: 'system', interval: 5, notify: false }, store.get('settings', {}));
  const S = {
    status: null,
    config: null,
    live: [],            // titik 5 detik dari /api/live (maks 1 jam)
    lastLive: 0,
    online: null,
    fails: 0,
    lastOk: 0,
    view: 'overview',
    range: store.get('range', '15m'),
    hist: {},            // cache /api/history per jam
    alerts: { active: [], events: [] },
    clientEvents: [],    // event dari sisi browser (koneksi putus/pulih)
    alertFilter: 'all',
    seenAlerts: null,
    proc: { list: [], sort: 'cpu', dir: -1, q: '', timer: 0, state: 'idle' },
  };
  const timers = { poll: 0, alerts: 0, clock: 0, play: 0 };

  // ------------------------------------------------------------------ API
  async function api(path, opts = {}) {
    const ctrl = new AbortController();
    const t = setTimeout(() => ctrl.abort(), opts.timeout || 8000);
    try {
      const r = await fetch(path, { cache: 'no-store', signal: ctrl.signal, headers: opts.headers || {} });
      if (!r.ok) { const e = new Error('HTTP ' + r.status); e.status = r.status; throw e; }
      return await r.json();
    } finally {
      clearTimeout(t);
    }
  }

  // ------------------------------------------------------------------ routing
  const VIEWS = {
    overview: ['Ringkasan', 'Status server dan metrik utama'],
    metrics: ['Grafik', 'Metrik historis & real-time'],
    system: ['Sistem', 'CPU, memori, storage, jaringan'],
    processes: ['Proses', 'Proses yang berjalan'],
    alerts: ['Alert', 'Alert aktif & timeline event'],
    settings: ['Pengaturan', 'Preferensi dashboard'],
  };

  function route() {
    const v = (location.hash || '#overview').slice(1);
    const view = VIEWS[v] ? v : 'overview';
    S.view = view;
    $$('[data-view]').forEach((el) => { el.hidden = el.dataset.view !== view; });
    $$('[data-nav]').forEach((a) => {
      if (a.dataset.nav === view) a.setAttribute('aria-current', 'page');
      else a.removeAttribute('aria-current');
    });
    $('page-title').textContent = VIEWS[view][0];
    document.title = `${VIEWS[view][0]} · J2 Monitor`;
    stopPlayback();
    if (view === 'overview') { ensureOverviewChart(); renderOverviewChart(); }
    if (view === 'metrics') { ensureMetricCharts(); updateMetrics(true); }
    if (view === 'processes') startProcesses(); else stopProcesses();
    if (view === 'alerts') renderAlerts();
    if (view === 'settings') renderSettings();
    requestAnimationFrame(resizeCharts);
  }

  // ------------------------------------------------------------------ polling
  async function poll() {
    clearTimeout(timers.poll);
    if (document.hidden) return; // dilanjutkan saat tab terlihat lagi
    try {
      const first = !S.lastLive;
      const [st, lv] = await Promise.all([
        api('/api/status'),
        api('/api/live?minutes=60' + (first ? '' : '&since=' + S.lastLive)),
      ]);
      onLive(lv.points || [], first);
      onStatus(st);
      setOnline(true);
    } catch (e) {
      setOnline(false, e);
    }
    const base = Math.max(5, Number(settings.interval) || 5);
    const delay = S.online ? base : Math.min(60, base * Math.pow(2, Math.min(S.fails - 1, 3)));
    timers.poll = setTimeout(poll, delay * 1000);
    if (!S.online) $('banner-text').textContent = `Menampilkan data terakhir (${fmtAgo(S.lastOk)}). Mencoba lagi dalam ${delay} dtk.`;
  }

  function onLive(points, replace) {
    if (replace) S.live = [];
    for (const p of points) if (p.time > S.lastLive) { S.live.push(p); S.lastLive = p.time; }
    const cutoff = Date.now() / 1000 - 3600;
    while (S.live.length && S.live[0].time < cutoff) S.live.shift();
    if (S.view === 'overview') renderOverviewChart();
    if (S.view === 'metrics') updateMetrics(); // rentang historis memakai cache 60 dtk
    renderSparks();
  }

  function setOnline(ok, err) {
    const was = S.online;
    S.online = ok;
    if (ok) { S.fails = 0; S.lastOk = Date.now() / 1000; } else S.fails++;
    document.body.classList.toggle('is-offline', !ok);
    $('banner').hidden = ok;
    if (was === ok) return;
    if (!ok && was !== null) {
      S.clientEvents.unshift({ id: 'c' + Date.now(), key: 'client:offline', severity: 'critical', message: 'Server tidak dapat dihubungi dari browser' + (err && err.status ? ` (HTTP ${err.status})` : ''), started_at: Math.floor(Date.now() / 1000), resolved_at: null, client: true });
      notify('client:offline', 'J2 Monitor offline', 'Server tidak dapat dihubungi.');
    } else if (ok) {
      const open = S.clientEvents.find((e) => e.key === 'client:offline' && !e.resolved_at);
      if (open) open.resolved_at = Math.floor(Date.now() / 1000);
    }
    S.clientEvents = S.clientEvents.slice(0, 50);
    renderHealth();
    if (S.view === 'alerts') renderAlerts();
  }

  async function pollAlerts() {
    clearTimeout(timers.alerts);
    if (document.hidden) return;
    try {
      const a = await api('/api/alerts?limit=200');
      S.alerts = a;
      checkNewAlerts(a.active);
      renderAlertSummary();
      if (S.view === 'alerts') renderAlerts();
    } catch { /* status offline sudah ditangani poll() */ }
    timers.alerts = setTimeout(pollAlerts, 15000);
  }

  async function loadConfig() {
    try {
      S.config = await api('/api/config');
      $$('[data-version]').forEach((el) => { el.textContent = S.config.version; });
      $$('[data-interval]').forEach((el) => { el.textContent = S.config.interval; });
      if (S.view === 'settings') renderSettings();
      if (charts.metrics.size) rebuildCharts();
    } catch { /* coba lagi nanti */ setTimeout(loadConfig, 15000); }
  }

  // ------------------------------------------------------------------ overview
  function th(key) { return S.config && S.config.thresholds ? S.config.thresholds[key] : null; }

  function setMeter(id, v, lvl) {
    const el = $(id);
    el.style.width = Math.max(0, Math.min(100, v || 0)) + '%';
    el.className = lvl === 'ok' ? '' : lvl;
  }
  function setBadge(id, lvl) {
    const el = $(id);
    const map = { ok: ['ok', 'Normal'], warn: ['warn', 'Warning'], crit: ['crit', 'Critical'] };
    el.className = 'badge ' + map[lvl][0];
    el.textContent = map[lvl][1];
  }

  function onStatus(d) {
    S.status = d;
    document.body.classList.add('loaded');
    $('page-sub').textContent = [d.model ? 'Samsung ' + d.model : '', d.os].filter(Boolean).join(' · ') || 'Termux Homelab';

    // KPI CPU
    const cpuL = level(d.cpu_percent, th('cpu'));
    $('kpi-cpu').innerHTML = `${n1(d.cpu_percent)}<small>%</small>`;
    setMeter('kpi-cpu-m', d.cpu_percent, cpuL); setBadge('kpi-cpu-b', cpuL);
    const freqs = (d.cpu && d.cpu.freq_mhz || []).filter(isNum);
    $('kpi-cpu-s').textContent = `${d.cpu_cores} core` + (freqs.length ? ` · ${NF0.format(Math.max(...freqs))} MHz` : '') + ` · load ${d.load[0]}`;
    // KPI RAM
    const m = d.memory;
    const ramL = level(m.percent, th('ram'));
    $('kpi-ram').innerHTML = `${n1(m.percent)}<small>%</small>`;
    setMeter('kpi-ram-m', m.percent, ramL); setBadge('kpi-ram-b', ramL);
    $('kpi-ram-s').textContent = `${fmtBytes(m.used)} dari ${fmtBytes(m.total)} · tersedia ${fmtBytes(m.available ?? m.free)}`;
    // KPI Disk
    const dk = d.disk;
    const diskL = level(dk.percent, th('disk'));
    if (dk.total) {
      $('kpi-disk').innerHTML = `${n1(dk.percent)}<small>%</small>`;
      setMeter('kpi-disk-m', dk.percent, diskL); setBadge('kpi-disk-b', diskL);
      $('kpi-disk-s').textContent = `${fmtBytes(dk.free)} bebas dari ${fmtBytes(dk.total)}`;
    } else {
      $('kpi-disk').innerHTML = NA; $('kpi-disk-s').textContent = 'Storage tidak terbaca';
    }
    // KPI Net
    const net = d.network;
    $('kpi-rx').textContent = net ? fmtRate(net.rx_rate) : 'N/A';
    $('kpi-tx').textContent = net ? fmtRate(net.tx_rate) : 'N/A';
    $('kpi-net-s').textContent = net ? (isNum(net.rx_rate) ? `Total ↓ ${fmtBytes(net.rx_total)} · ↑ ${fmtBytes(net.tx_total)}` : 'Menghitung kecepatan…') : 'Tidak didukung';

    // Hero
    $('ov-host').textContent = d.hostname || '—';
    $('ov-os').textContent = [d.os, d.architecture].filter(Boolean).join(' · ');
    $('ov-uptime').textContent = fmtDur(d.uptime);

    // Suhu & daya
    const t = d.temperature || {};
    $('ov-temp').innerHTML = isNum(t.cpu) ? `<span class="badge ${level(t.cpu, th('temp'))}">${n1(t.cpu)} °C</span>` : NA;
    $('ov-btemp').innerHTML = isNum(t.battery) ? `<span class="badge ${level(t.battery, th('battery_temp'))}">${n1(t.battery)} °C</span>` : NA;
    $('ov-batt').innerHTML = d.battery && isNum(d.battery.level) ? `${NF0.format(d.battery.level)}% · ${esc(battStatus(d.battery.status))}` : NA;
    $('ov-load').textContent = d.load.join(' / ');

    // Service
    const svcs = Object.entries(d.services || {});
    const up = svcs.filter(([, v]) => v).length;
    $('ov-svc-sum').textContent = svcs.length ? `${up}/${svcs.length} berjalan` : '';
    $('ov-services').innerHTML = svcs.length
      ? svcs.map(([k, v]) => `<li><span class="dot ${v ? 'ok' : 'crit'}"></span><span class="mono">${esc(k)}</span><span class="end badge ${v ? 'ok' : 'crit'}">${v ? 'Running' : 'Down'}</span></li>`).join('')
      : '<li class="muted">Tidak ada service yang dipantau</li>';

    renderHealth();
    tickClock();
    if (S.view === 'system') renderSystem();
  }

  function battStatus(s) {
    return ({ Charging: 'mengisi', Discharging: 'tidak mengisi', Full: 'penuh', 'Not charging': 'tidak mengisi' })[s] || s || '';
  }

  function renderHealth() {
    let state = 'loading', label = 'Memuat…', reasons = '';
    if (S.online === false) {
      state = 'offline'; label = 'Offline';
      reasons = S.lastOk ? `Tidak ada respon sejak ${fmtAgo(S.lastOk)}` : 'API tidak dapat dihubungi';
    } else if (S.status) {
      const h = S.status.health || { state: 'online', reasons: [] };
      state = h.state === 'degraded' ? 'degraded' : 'online';
      label = state === 'degraded' ? 'Degraded' : 'Online';
      reasons = state === 'degraded' ? h.reasons.slice(0, 2).join(' · ') + (h.reasons.length > 2 ? ` · +${h.reasons.length - 2} lainnya` : '') : 'Semua metrik dalam batas normal';
    }
    $('hero').dataset.state = state;
    $('ov-state').textContent = label;
    $('ov-reasons').textContent = reasons;
    const connLabel = { loading: 'Menghubungkan…', online: 'Online', degraded: 'Degraded', offline: 'Offline' }[state];
    $$('.conn').forEach((el) => { el.dataset.state = state; el.querySelector('.conn-label').textContent = connLabel; });
  }

  function tickClock() {
    const d = S.status;
    if (!d) return;
    const ago = fmtAgo(d.time);
    $$('[data-updated]').forEach((el) => { el.textContent = ago; });
    $('ov-updated').textContent = `${ago} · tiap ${settings.interval} dtk`;
    if (S.online === false) renderHealth();
  }

  function sparkPath(vals, max) {
    const pts = vals.filter(isNum);
    if (pts.length < 2) return '';
    const mx = max || Math.max(...pts, 1);
    const step = 100 / (vals.length - 1);
    let d = '', started = false;
    vals.forEach((v, i) => {
      if (!isNum(v)) return;
      const x = (i * step).toFixed(2), y = (28 - (Math.min(v, mx) / mx) * 26).toFixed(2);
      d += (started ? 'L' : 'M') + x + ' ' + y;
      started = true;
    });
    return d;
  }
  function renderSparks() {
    const last = S.live.slice(-60);
    const set = (id, vals, max) => {
      const line = sparkPath(vals, max);
      $(id).innerHTML = line ? `<path class="area" d="${line}L100 30L0 30Z"/><path class="line" d="${line}"/>` : '';
    };
    set('spark-cpu', last.map((p) => p.cpu), 100);
    set('spark-ram', last.map((p) => p.ram), 100);
    set('spark-disk', last.map((p) => p.disk), 100);
    set('spark-net', last.map((p) => (isNum(p.net_rx) || isNum(p.net_tx) ? (p.net_rx || 0) + (p.net_tx || 0) : null)));
  }

  // ------------------------------------------------------------------ charts
  const charts = { overview: null, metrics: new Map(), ro: null };
  const hasEcharts = () => typeof window.echarts !== 'undefined';

  const CHARTS = [
    { id: 'cpu', title: 'CPU', unit: '%', max: 100, th: 'cpu', series: [
      { key: 'cpu', name: 'CPU', color: '--cyan', area: true },
      { key: 'cpu_max', name: 'CPU puncak', color: '--violet', dashed: true, historyOnly: true },
    ] },
    { id: 'ram', title: 'Memori', unit: '%', max: 100, th: 'ram', series: [
      { key: 'ram', name: 'RAM', color: '--emerald', area: true },
      { key: 'swap', name: 'Swap', color: '--amber' },
    ] },
    { id: 'net', title: 'Jaringan', unit: 'rate', series: [
      { key: 'net_rx', name: 'Download (RX)', color: '--cyan', area: true },
      { key: 'net_tx', name: 'Upload (TX)', color: '--violet', area: true },
    ] },
    { id: 'disk', title: 'Penggunaan disk', unit: '%', th: 'disk', series: [
      { key: 'disk', name: 'Disk terpakai', color: '--amber', area: true },
    ] },
    { id: 'io', title: 'Disk I/O', unit: 'rate', series: [
      { key: 'disk_read', name: 'Baca', color: '--emerald', area: true },
      { key: 'disk_write', name: 'Tulis', color: '--rose', area: true },
    ] },
    { id: 'temp', title: 'Suhu CPU', unit: '°C', th: 'temp', series: [
      { key: 'temp', name: 'Suhu CPU', color: '--rose', area: true },
    ] },
  ];

  const RANGES = {
    '1m': { live: 60 }, '5m': { live: 300 }, '15m': { live: 900 }, '1h': { live: 3600 },
    '6h': { hours: 6 }, '12h': { hours: 12 }, '24h': { hours: 24 }, '7d': { hours: 168 },
  };
  const isLiveRange = () => !!RANGES[S.range].live;

  function fmtVal(v, unit) {
    if (!isNum(v)) return '—';
    if (unit === 'rate') return fmtRate(v);
    if (unit === '%') return n1(v) + '%';
    return n1(v) + ' ' + unit;
  }

  function hexA(color, a) {
    // warna CSS (#rrggbb) -> rgba
    const m = /^#([0-9a-f]{6})$/i.exec(color);
    if (!m) return color;
    const n = parseInt(m[1], 16);
    return `rgba(${n >> 16},${(n >> 8) & 255},${n & 255},${a})`;
  }

  function baseOption(def, opts = {}) {
    const text = cssVar('--text'), muted = cssVar('--muted'), line = cssVar('--line'), panel = cssVar('--panel');
    const anim = !RM.matches;
    const unit = def.unit;
    const th0 = def.th ? th(def.th) : null;
    const markLine = th0 ? {
      silent: true, symbol: 'none', animation: false,
      label: { color: muted, fontSize: 10, formatter: (p) => p.name },
      data: [
        { yAxis: th0.warning, name: 'warning', lineStyle: { color: cssVar('--amber'), type: 'dashed', width: 1, opacity: .6 } },
        { yAxis: th0.critical, name: 'critical', lineStyle: { color: cssVar('--rose'), type: 'dashed', width: 1, opacity: .6 } },
      ],
    } : undefined;
    return {
      animation: anim, animationDuration: 300, animationDurationUpdate: 250,
      backgroundColor: 'transparent',
      textStyle: { fontFamily: getComputedStyle(document.body).fontFamily, color: text },
      grid: { left: 8, right: 14, top: opts.compact ? 30 : 36, bottom: opts.compact ? 8 : 44, containLabel: true },
      legend: { top: 0, left: 0, icon: 'roundRect', itemWidth: 10, itemHeight: 10, textStyle: { color: muted, fontSize: 11.5 }, inactiveColor: hexA(muted, .35) },
      toolbox: opts.compact ? undefined : {
        right: 0, top: -4, itemSize: 14, iconStyle: { borderColor: muted },
        emphasis: { iconStyle: { borderColor: cssVar('--cyan') } },
        feature: { saveAsImage: { title: 'Simpan PNG', name: 'j2-monitor-' + def.id, backgroundColor: panel, pixelRatio: 2 } },
      },
      tooltip: {
        trigger: 'axis', confine: true,
        backgroundColor: cssVar('--panel-2'), borderColor: cssVar('--line-2'), textStyle: { color: text, fontSize: 12 },
        axisPointer: { type: 'cross', label: { backgroundColor: cssVar('--panel-3'), color: text }, crossStyle: { color: muted }, lineStyle: { color: muted, type: 'dashed' } },
        valueFormatter: (v) => fmtVal(v, unit),
      },
      xAxis: {
        type: 'time', boundaryGap: false,
        axisLine: { lineStyle: { color: line } }, axisTick: { show: false },
        axisLabel: { color: muted, fontSize: 10.5, hideOverlap: true },
        splitLine: { show: false },
      },
      yAxis: {
        type: 'value', min: 0, max: def.max, scale: !def.max && unit === '°C',
        splitNumber: 4,
        axisLabel: { color: muted, fontSize: 10.5, formatter: (v) => (unit === 'rate' ? fmtBytes(v, 0) : unit === '%' ? v + '%' : v + '°') },
        splitLine: { lineStyle: { color: line } },
      },
      dataZoom: opts.compact ? undefined : [
        { type: 'inside', zoomOnMouseWheel: 'ctrl', moveOnMouseWheel: false, moveOnMouseMove: false, filterMode: 'none' },
        { type: 'slider', height: 16, bottom: 6, filterMode: 'none', brushSelect: false, showDetail: false, borderColor: line, backgroundColor: 'transparent',
          fillerColor: hexA(cssVar('--cyan'), .12), dataBackground: { lineStyle: { color: muted, opacity: .4 }, areaStyle: { opacity: 0 } },
          selectedDataBackground: { lineStyle: { color: cssVar('--cyan') }, areaStyle: { opacity: 0 } },
          handleStyle: { color: cssVar('--panel-3'), borderColor: muted }, moveHandleStyle: { color: muted }, textStyle: { color: muted } },
      ],
      series: def.series.map((s, i) => {
        const c = cssVar(s.color);
        return {
          id: s.key, name: s.name, type: 'line', smooth: 0.25, showSymbol: false, symbolSize: 5,
          connectNulls: false, sampling: 'lttb',
          lineStyle: { width: s.dashed ? 1.2 : 1.8, color: c, type: s.dashed ? 'dashed' : 'solid' },
          itemStyle: { color: c },
          areaStyle: s.area ? { color: { type: 'linear', x: 0, y: 0, x2: 0, y2: 1, colorStops: [{ offset: 0, color: hexA(c, .28) }, { offset: 1, color: hexA(c, 0) }] } } : undefined,
          emphasis: { focus: 'series' },
          markLine: i === 0 ? markLine : undefined,
          data: [],
        };
      }),
    };
  }

  function observe(el, chart) {
    if (!charts.ro) {
      let raf = 0;
      charts.ro = new ResizeObserver(() => { cancelAnimationFrame(raf); raf = requestAnimationFrame(resizeCharts); });
    }
    charts.ro.observe(el);
    return chart;
  }
  function resizeCharts() {
    if (charts.overview) charts.overview.resize();
    charts.metrics.forEach((c) => c.chart.resize());
  }

  function chartUnavailable(boxId) {
    const el = $(boxId);
    if (el) { el.hidden = false; el.firstElementChild.innerHTML = '<svg class="i"><use href="#i-empty"/></svg>Library grafik gagal dimuat (cek koneksi ke CDN).'; }
  }

  function ensureOverviewChart() {
    if (charts.overview) return;
    if (!hasEcharts()) { chartUnavailable('ov-chart-empty'); return; }
    const el = $('ov-chart');
    const c = echarts.init(el, null, { renderer: 'canvas' });
    c.setOption(overviewOption());
    charts.overview = observe(el, c);
  }
  function overviewOption() {
    const o = baseOption({ id: 'overview', unit: '%', max: 100, series: [
      { key: 'cpu', name: 'CPU', color: '--cyan', area: true },
      { key: 'ram', name: 'RAM', color: '--emerald', area: true },
    ] }, { compact: true });
    return o;
  }
  function renderOverviewChart() {
    if (!charts.overview) return;
    const cutoff = Date.now() / 1000 - 900;
    const pts = S.live.filter((p) => p.time >= cutoff);
    $('ov-chart-empty').hidden = pts.length >= 2;
    charts.overview.setOption({ series: [
      { id: 'cpu', data: pts.map((p) => [p.time * 1000, p.cpu]) },
      { id: 'ram', data: pts.map((p) => [p.time * 1000, p.ram]) },
    ] });
  }

  function ensureMetricCharts() {
    if (charts.metrics.size) return;
    const grid = $('charts-grid');
    grid.innerHTML = CHARTS.map((def) => `
      <article class="card chart-card" aria-labelledby="ch-${def.id}-t">
        <div class="card-head">
          <h2 id="ch-${def.id}-t">${esc(def.title)}</h2>
          <div class="stats num" id="ch-${def.id}-stats" aria-label="Statistik ${esc(def.title)}"></div>
        </div>
        <div class="chart-box">
          <div class="chart" id="ch-${def.id}" role="img" aria-label="Grafik ${esc(def.title)}"></div>
          <div class="chart-empty" id="ch-${def.id}-empty" hidden><div><svg class="i"><use href="#i-empty"/></svg><span>Belum ada data</span></div></div>
        </div>
      </article>`).join('');
    if (!hasEcharts()) { CHARTS.forEach((d) => chartUnavailable(`ch-${d.id}-empty`)); return; }
    CHARTS.forEach((def) => {
      const el = $('ch-' + def.id);
      const c = echarts.init(el, null, { renderer: 'canvas' });
      c.group = 'metrics';
      c.setOption(baseOption(def));
      c.on('datazoom', () => updateStats(def.id));
      c.on('legendselectchanged', () => updateStats(def.id));
      charts.metrics.set(def.id, { chart: observe(el, c), def, data: {} });
    });
    echarts.connect('metrics');
  }

  function rebuildCharts() {
    if (charts.overview) { charts.overview.setOption(overviewOption(), true); renderOverviewChart(); }
    charts.metrics.forEach((m) => m.chart.setOption(baseOption(m.def), true));
    if (S.view === 'metrics') updateMetrics(true);
  }

  async function getRangeData() {
    const r = RANGES[S.range];
    if (r.live) {
      const cutoff = Date.now() / 1000 - r.live;
      return { points: S.live.filter((p) => p.time >= cutoff), step: S.config ? S.config.interval : 5, live: true };
    }
    const c = S.hist[r.hours];
    if (c && Date.now() - c.at < 60000) return c.data;
    const h = await api(`/api/history?hours=${r.hours}&points=360`, { timeout: 15000 });
    const data = { points: h.points || [], step: h.step, live: false };
    S.hist[r.hours] = { at: Date.now(), data };
    return data;
  }

  let metricsBusy = false;
  async function updateMetrics(force) {
    if (!charts.metrics.size || metricsBusy) return;
    metricsBusy = true;
    try {
      const data = await getRangeData();
      const pts = data.points;
      const r = RANGES[S.range];
      const dur = r.live || r.hours * 3600;
      const xMax = Date.now(), xMin = xMax - dur * 1000;
      $('range-note').textContent = data.live
        ? `Real-time · sampel tiap ${data.step} dtk · ${pts.length} titik`
        : `Historis · rata-rata per ${fmtDur(data.step)} · ${pts.length} titik`;
      $('play-btn').disabled = data.live || pts.length < 10;
      const st = S.status || {};
      charts.metrics.forEach((m) => {
        const series = m.def.series.map((s) => {
          const d = (s.historyOnly && data.live) ? [] : pts.map((p) => [p.time * 1000, isNum(p[s.key]) ? p[s.key] : null]);
          m.data[s.key] = d;
          return { id: s.key, data: d };
        });
        m.chart.setOption({ xAxis: { min: xMin, max: xMax }, series });
        // empty / unsupported state
        const any = series.some((s) => s.data.some((p) => p[1] !== null));
        const empty = $(`ch-${m.def.id}-empty`);
        empty.hidden = any;
        if (!any) {
          let msg = data.live ? 'Mengumpulkan data…' : 'Belum ada data historis untuk rentang ini';
          if (m.def.id === 'temp' && st.temperature && !isNum(st.temperature.cpu)) msg = 'Sensor suhu CPU tidak tersedia di perangkat ini';
          if (m.def.id === 'io' && st.time && !st.disk_io) msg = 'Disk I/O tidak didukung di perangkat ini';
          if (m.def.id === 'net' && st.time && !st.network) msg = 'Statistik jaringan tidak didukung';
          if (!data.live && ['net', 'io', 'temp'].includes(m.def.id) && pts.length) msg = 'Data ini baru direkam sejak v2 — tunggu beberapa menit';
          empty.querySelector('span').textContent = msg;
        }
        updateStats(m.def.id);
      });
      if (force) charts.metrics.forEach((m) => m.chart.resize());
    } catch (e) {
      $('range-note').textContent = 'Gagal memuat data historis' + (e.status ? ` (HTTP ${e.status})` : '');
    } finally {
      metricsBusy = false;
    }
  }

  function updateStats(id) {
    const m = charts.metrics.get(id);
    if (!m) return;
    const opt = m.chart.getOption();
    let lo = -Infinity, hi = Infinity;
    try { [lo, hi] = m.chart.getModel().getComponent('xAxis', 0).axis.scale.getExtent(); } catch { /* pakai seluruh data */ }
    const sel = (opt.legend && opt.legend[0] && opt.legend[0].selected) || {};
    const primary = m.def.series.find((s) => sel[s.name] !== false) || m.def.series[0];
    const vals = (m.data[primary.key] || []).filter((p) => p[0] >= lo && p[0] <= hi && p[1] !== null).map((p) => p[1]);
    const el = $(`ch-${id}-stats`);
    if (!vals.length) { el.innerHTML = ''; return; }
    const min = Math.min(...vals), max = Math.max(...vals), avg = vals.reduce((a, b) => a + b, 0) / vals.length;
    const label = m.def.series.length > 1 ? `<span>${esc(primary.name)}</span>` : '';
    el.innerHTML = `${label}<span>min<b>${fmtVal(min, m.def.unit)}</b></span><span>avg<b>${fmtVal(avg, m.def.unit)}</b></span><span>max<b>${fmtVal(max, m.def.unit)}</b></span>`;
  }

  function setRange(r) {
    if (!RANGES[r]) r = '15m';
    S.range = r;
    store.set('range', r);
    $$('#range-seg button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.range === r)));
    stopPlayback();
    resetZoom();
    updateMetrics(true);
  }

  function resetZoom() {
    const first = charts.metrics.values().next().value;
    if (first) first.chart.dispatchAction({ type: 'dataZoom', start: 0, end: 100 });
  }

  // Playback: menggeser jendela zoom sepanjang data historis.
  function togglePlayback() {
    if (timers.play) { stopPlayback(); return; }
    const first = charts.metrics.values().next().value;
    if (!first) return;
    const win = 15;
    let pos = 0;
    const btn = $('play-btn');
    btn.setAttribute('aria-pressed', 'true');
    btn.querySelector('use').setAttribute('href', '#i-pause');
    btn.querySelector('span').textContent = 'Jeda';
    const step = () => {
      first.chart.dispatchAction({ type: 'dataZoom', start: pos, end: pos + win });
      pos += 1;
      if (pos + win > 100) { stopPlayback(); return; }
      timers.play = setTimeout(step, RM.matches ? 600 : 250);
    };
    step();
  }
  function stopPlayback() {
    if (!timers.play && $('play-btn').getAttribute('aria-pressed') !== 'true') return;
    clearTimeout(timers.play);
    timers.play = 0;
    const btn = $('play-btn');
    btn.setAttribute('aria-pressed', 'false');
    btn.querySelector('use').setAttribute('href', '#i-play');
    btn.querySelector('span').textContent = 'Putar ulang';
  }

  // ------------------------------------------------------------------ system view
  function kv(rows) {
    return rows.map(([k, v]) => `<div><dt>${esc(k)}</dt><dd class="num">${v}</dd></div>`).join('');
  }

  function renderSystem() {
    const d = S.status;
    if (!d) return;
    // CPU
    const per = (d.cpu && d.cpu.per_core) || [];
    const fq = (d.cpu && d.cpu.freq_mhz) || [];
    $('sys-cpu-sub').textContent = `${d.cpu_cores} core · ${d.architecture}`;
    const cl = level(d.cpu_percent, th('cpu'));
    $('sys-cpu-total').className = 'badge ' + cl;
    $('sys-cpu-total').textContent = `Total ${n1(d.cpu_percent)}%`;
    $('sys-cores').innerHTML = per.length ? per.map((v, i) => {
      const l = level(v, th('cpu'));
      return `<div class="core"><span class="mono">cpu${i}</span>
        <div class="meter" role="meter" aria-label="Core ${i}" aria-valuemin="0" aria-valuemax="100" aria-valuenow="${isNum(v) ? v : 0}"><i class="${l === 'ok' ? '' : l}" style="width:${isNum(v) ? v : 0}%;background:${l === 'ok' ? 'var(--cyan)' : ''}"></i></div>
        <span class="num">${isNum(v) ? n1(v) + '%' : '<em class="na">offline</em>'}</span>
        <span class="num muted freq">${isNum(fq[i]) ? NF0.format(fq[i]) + ' MHz' : 'N/A'}</span></div>`;
    }).join('') : '<p class="muted">Data per-core belum tersedia</p>';
    $('sys-l1').textContent = d.load[0]; $('sys-l5').textContent = d.load[1]; $('sys-l15').textContent = d.load[2];

    // Memori
    const m = d.memory;
    const cache = (m.cached || 0) + (m.buffers || 0);
    $('mem-used').style.width = (m.total ? (m.used / m.total) * 100 : 0) + '%';
    $('mem-cache').style.width = (m.total ? Math.min(cache, m.total - m.used) / m.total * 100 : 0) + '%';
    $('sys-mem-sub').textContent = `${n1(m.percent)}% terpakai`;
    $('sys-mem').innerHTML = kv([
      ['Total', fmtBytes(m.total)],
      ['Dipakai', fmtBytes(m.used)],
      ['Tersedia', fmtBytes(m.available ?? m.free)],
      ['Cached', isNum(m.cached) ? fmtBytes(m.cached) : NA],
      ['Buffers', isNum(m.buffers) ? fmtBytes(m.buffers) : NA],
      ['Swap', m.swap_total ? `${fmtBytes(m.swap_used)} / ${fmtBytes(m.swap_total)} (${n1(m.swap_percent)}%)` : '<em class="na">Tidak ada swap</em>'],
    ]);

    // Storage
    const fs = d.filesystems && d.filesystems.length ? d.filesystems : (d.disk.total ? [Object.assign({ mount: 'utama' }, d.disk)] : []);
    $('sys-fs').innerHTML = fs.length ? fs.map((f) => {
      const l = level(f.percent, th('disk'));
      return `<div class="fs-row"><div class="top"><span class="mono">${esc(f.mount)}</span><span class="num">${fmtBytes(f.used)} / ${fmtBytes(f.total)} · <b>${n1(f.percent)}%</b></span></div>
        <div class="meter"><i class="${l === 'ok' ? '' : l}" style="width:${f.percent}%"></i></div>
        <span class="small muted">${fmtBytes(f.free)} tersedia</span></div>`;
    }).join('') : '<p class="muted">Filesystem tidak terbaca</p>';
    const io = d.disk_io;
    $('sys-io').innerHTML = io ? `
      <div><span>Baca</span><b class="num">${fmtRate(io.read_rate)}</b></div>
      <div><span>Tulis</span><b class="num">${fmtRate(io.write_rate)}</b></div>
      <div><span>IOPS baca</span><b class="num">${n1(io.read_iops)}</b></div>
      <div><span>IOPS tulis</span><b class="num">${n1(io.write_iops)}</b></div>
      <p class="small muted" style="grid-column:1/-1">Perangkat: <span class="mono">${esc(io.devices.join(', '))}</span></p>`
      : '<div style="grid-column:1/-1"><span>Disk I/O</span><b>N/A</b></div>';

    // Jaringan
    const net = d.network;
    $('sys-net-sub').textContent = net ? `Total ↓ ${fmtBytes(net.rx_total)} · ↑ ${fmtBytes(net.tx_total)}` : 'Tidak didukung';
    $('sys-net').innerHTML = net && net.interfaces.length ? net.interfaces.map((i) => `<tr>
        <td class="mono">${esc(i.name)}</td><td class="r num">${fmtRate(i.rx_rate)}</td><td class="r num">${fmtRate(i.tx_rate)}</td>
        <td class="r num">${fmtBytes(i.rx_bytes)}</td><td class="r num">${fmtBytes(i.tx_bytes)}</td>
        <td class="r num">${NF0.format(i.rx_errors + i.tx_errors)}</td><td class="r num">${NF0.format(i.rx_drops + i.tx_drops)}</td></tr>`).join('')
      : '<tr><td colspan="7" class="muted">Tidak ada interface aktif</td></tr>';

    // Info sistem
    $('sys-info').innerHTML = kv([
      ['Hostname', esc(d.hostname)],
      ['OS', esc(d.os || 'N/A')],
      ['Perangkat', d.model ? esc(d.model) : NA],
      ['Kernel', d.kernel ? `<span class="mono">${esc(d.kernel)}</span>` : NA],
      ['Arsitektur', esc(d.architecture)],
      ['Uptime', fmtDur(d.uptime)],
      ['Boot', d.boot_time ? esc(fmtTime(d.boot_time)) : NA],
      ['Load (1/5/15)', esc(d.load.join(' / '))],
    ]);
    const t = d.temperature || {};
    $('sys-power').innerHTML = kv([
      ['Suhu CPU', isNum(t.cpu) ? `<span class="badge ${level(t.cpu, th('temp'))}">${n1(t.cpu)} °C</span>` : NA],
      ['Sensor', t.cpu_source ? `<span class="mono">${esc(t.cpu_source)}</span>` : NA],
      ['Suhu baterai', isNum(t.battery) ? `<span class="badge ${level(t.battery, th('battery_temp'))}">${n1(t.battery)} °C</span>` : NA],
      ['Level baterai', d.battery && isNum(d.battery.level) ? NF0.format(d.battery.level) + '%' : NA],
      ['Status baterai', d.battery && d.battery.status ? esc(battStatus(d.battery.status)) : NA],
    ]);
  }

  // ------------------------------------------------------------------ processes
  function token() { return store.get('token', ''); }

  function startProcesses() {
    clearTimeout(S.proc.timer);
    loadProcesses();
  }
  function stopProcesses() { clearTimeout(S.proc.timer); S.proc.timer = 0; }

  async function loadProcesses() {
    clearTimeout(S.proc.timer);
    if (S.view !== 'processes') return;
    if (!document.hidden) {
      const tok = token();
      try {
        const r = await api('/api/processes', { headers: tok ? { Authorization: 'Bearer ' + tok } : {}, timeout: 10000 });
        S.proc.list = r.processes || [];
        S.proc.state = 'ok';
        S.proc.limited = r.limited;
      } catch (e) {
        S.proc.state = e.status === 401 ? 'locked' : e.status === 403 ? 'disabled' : 'error';
        if (e.status === 401 && tok) S.proc.badToken = true;
      }
      renderProcesses();
    }
    if (S.proc.state === 'ok') S.proc.timer = setTimeout(loadProcesses, 10000);
  }

  const PROC_COLS = [
    ['pid', 'PID', true], ['name', 'Nama', false], ['state', 'State', false],
    ['cpu', 'CPU', true], ['rss', 'RAM', true], ['mem', 'Mem %', true], ['threads', 'Thread', true],
  ];

  function renderProcesses() {
    const body = $('proc-body');
    const p = S.proc;
    $('proc-tools').hidden = p.state !== 'ok';
    if (p.state === 'locked' || p.state === 'disabled') {
      $('proc-sub').textContent = '';
      body.innerHTML = p.state === 'disabled'
        ? `<div class="empty"><svg class="i"><use href="#i-lock"/></svg><p><b>Monitoring proses dinonaktifkan di server.</b></p>
           <p class="small">Set <code class="mono">MONITOR_TOKEN</code> pada service backend untuk mengaktifkannya dengan autentikasi.</p></div>`
        : `<div class="lock"><p><svg class="i" style="vertical-align:-4px"><use href="#i-lock"/></svg> Daftar proses dilindungi token. Masukkan <code class="mono">MONITOR_TOKEN</code>.</p>
           ${p.badToken ? '<p class="small" style="color:var(--crit)">Token salah.</p>' : ''}
           <form id="token-form"><label class="sr-only" for="token-input">Token</label>
           <input type="password" id="token-input" placeholder="Token monitoring" autocomplete="current-password" required>
           <button class="btn primary" type="submit">Buka</button></form>
           <p class="small muted">Token disimpan di browser ini saja (localStorage).</p></div>`;
      const f = $('token-form');
      if (f) f.addEventListener('submit', (e) => {
        e.preventDefault();
        store.set('token', $('token-input').value.trim());
        p.badToken = false;
        loadProcesses();
      });
      return;
    }
    if (p.state === 'error') {
      body.innerHTML = '<div class="empty"><svg class="i"><use href="#i-off"/></svg><p>Gagal memuat daftar proses.</p><button class="btn" type="button" data-proc-retry>Coba lagi</button></div>';
      return;
    }
    if (p.state !== 'ok') return;
    const q = p.q.toLowerCase();
    let rows = p.list.filter((r) => !q || r.name.toLowerCase().includes(q) || String(r.pid).includes(q));
    const k = p.sort, dir = p.dir;
    rows = rows.slice().sort((a, b) => {
      const va = a[k] ?? -1, vb = b[k] ?? -1;
      return (typeof va === 'string' ? va.localeCompare(vb) : va - vb) * dir;
    });
    $('proc-sub').textContent = `${rows.length} dari ${p.list.length} proses terlihat`;
    const head = PROC_COLS.map(([key, label, right]) => {
      const sort = p.sort === key ? (p.dir > 0 ? 'ascending' : 'descending') : 'none';
      return `<th scope="col" class="${right ? 'r' : ''}" aria-sort="${sort}"><button type="button" data-sort="${key}">${label}</button></th>`;
    }).join('');
    body.innerHTML = `${p.limited ? '<p class="note-box" style="margin-bottom:12px">Android membatasi akses <span class="mono">/proc</span>: hanya proses milik user Termux yang terlihat. Proses sistem dan aplikasi lain tidak ditampilkan.</p>' : ''}
      <div class="table-wrap"><table><caption class="sr-only">Daftar proses</caption><thead><tr>${head}</tr></thead><tbody>
      ${rows.length ? rows.map((r) => `<tr><td class="r num">${r.pid}</td><td class="mono">${esc(r.name)}</td><td>${esc(r.state)}</td>
        <td class="r num">${isNum(r.cpu) ? n1(r.cpu) + '%' : '—'}</td><td class="r num">${fmtBytes(r.rss)}</td><td class="r num">${n1(r.mem)}%</td><td class="r num">${r.threads}</td></tr>`).join('')
        : '<tr><td colspan="7" class="muted">Tidak ada proses yang cocok</td></tr>'}
      </tbody></table></div>
      <p class="small muted" style="margin-top:10px">CPU dihitung relatif terhadap satu core (bisa &gt; 100% untuk proses multi-thread). Command line tidak ditampilkan demi keamanan.</p>`;
  }

  // ------------------------------------------------------------------ alerts
  function allEvents() {
    return S.alerts.events.concat(S.clientEvents).sort((a, b) => b.started_at - a.started_at);
  }

  function renderAlertSummary() {
    const active = S.alerts.active.concat(S.clientEvents.filter((e) => !e.resolved_at));
    $$('[data-alert-count]').forEach((el) => { el.hidden = !active.length; el.textContent = active.length; });
    $('ov-alerts').innerHTML = active.length
      ? active.slice(0, 4).map((e) => `<li><span class="badge ${e.severity}">${e.severity}</span><span>${esc(e.message)}<br><span class="small muted">${fmtAgo(e.started_at)}</span></span></li>`).join('')
      : '<li class="muted"><svg class="i" style="color:var(--ok);vertical-align:-4px"><use href="#i-check"/></svg> Tidak ada alert aktif</li>';
  }

  function renderAlerts() {
    renderAlertSummary();
    const active = S.alerts.active.concat(S.clientEvents.filter((e) => !e.resolved_at));
    $('al-active').innerHTML = active.length ? active.map((e) => `
      <li class="${e.severity}"><span class="badge ${e.severity}">${e.severity}</span>
        <div><b>${esc(e.message)}</b><div class="small muted">Sejak ${esc(fmtTime(e.started_at))} · ${fmtDur(Date.now() / 1000 - e.started_at)}${e.client ? ' · dideteksi browser' : ''}</div></div></li>`).join('')
      : '<li class="empty" style="border:0;background:none"><svg class="i" style="color:var(--ok)"><use href="#i-check"/></svg><p>Tidak ada alert aktif. Semua metrik dalam batas normal.</p></li>';

    const f = S.alertFilter;
    const evs = allEvents().filter((e) => f === 'all' || e.severity === f).slice(0, 150);
    $('al-timeline').innerHTML = evs.length ? evs.map((e) => {
      const resolved = e.resolved_at && e.severity !== 'info';
      const status = e.severity === 'info' ? '' : resolved ? '<span class="badge ok">resolved</span>' : '<span class="badge crit">active</span>';
      const dur = resolved ? ` · durasi ${fmtDur(e.resolved_at - e.started_at)}` : '';
      return `<li class="${e.severity}"><span class="tdot" aria-hidden="true"></span>
        <div class="row1"><span class="badge ${e.severity}">${e.severity}</span>${status}<span class="msg">${esc(e.message)}</span></div>
        <div class="when num">${esc(fmtTime(e.started_at))}${dur}${e.client ? ' · browser' : ''}</div></li>`;
    }).join('') : '<li class="empty" style="padding:20px 0"><p>Belum ada event.</p></li>';
    renderNotifBtn();
  }

  // ------------------------------------------------------------------ notifications
  const notifSupported = () => 'Notification' in window;

  function renderNotifBtn() {
    const btn = $('notif-btn');
    if (!notifSupported()) { btn.disabled = true; btn.querySelector('span').textContent = 'Notifikasi tidak didukung'; return; }
    const on = settings.notify && Notification.permission === 'granted';
    btn.querySelector('span').textContent = on ? 'Notifikasi aktif' : Notification.permission === 'denied' ? 'Notifikasi diblokir browser' : 'Aktifkan notifikasi';
    btn.disabled = Notification.permission === 'denied';
    btn.setAttribute('aria-pressed', String(on));
    $('set-notify').checked = on;
  }

  async function enableNotifications(on) {
    if (!notifSupported()) return;
    if (on && Notification.permission !== 'granted') {
      const p = await Notification.requestPermission();
      on = p === 'granted';
    }
    settings.notify = on;
    store.set('settings', settings);
    renderNotifBtn();
  }

  function notify(key, title, body) {
    if (!settings.notify || !notifSupported() || Notification.permission !== 'granted') return;
    const cd = store.get('notifCooldown', {});
    const now = Date.now();
    if (cd[key] && now - cd[key] < 10 * 60 * 1000) return; // cooldown per jenis alert
    cd[key] = now;
    store.set('notifCooldown', cd);
    try { new Notification(title, { body, tag: key, icon: document.querySelector('link[rel=icon]').href }); } catch { /* mobile butuh service worker */ }
  }

  function checkNewAlerts(active) {
    const ids = new Set(active.map((e) => e.id));
    if (S.seenAlerts) {
      active.filter((e) => !S.seenAlerts.has(e.id) && e.severity !== 'info')
        .forEach((e) => notify(e.key, `J2 Monitor · ${e.severity.toUpperCase()}`, e.message));
    }
    S.seenAlerts = ids; // muatan pertama tidak memicu notifikasi
  }

  // ------------------------------------------------------------------ settings & theme
  function applyTheme() {
    const t = settings.theme;
    if (t === 'light' || t === 'dark') document.documentElement.dataset.theme = t;
    else delete document.documentElement.dataset.theme;
    const dark = t === 'dark' || (t === 'system' && !window.matchMedia('(prefers-color-scheme: light)').matches);
    $('theme-btn').querySelector('use').setAttribute('href', dark ? '#i-sun' : '#i-moon');
    $('theme-btn').setAttribute('aria-label', dark ? 'Ganti ke tema terang' : 'Ganti ke tema gelap');
    document.querySelector('meta[name=theme-color]').content = cssVar('--bg') || '#060b17';
    $$('#set-theme button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.themeOpt === t)));
    rebuildCharts();
  }

  const TH_LABELS = { cpu: ['CPU', '%', 'THRESH_CPU'], ram: ['RAM', '%', 'THRESH_RAM'], disk: ['Disk', '%', 'THRESH_DISK'], temp: ['Suhu CPU', '°C', 'THRESH_TEMP'], battery_temp: ['Suhu baterai', '°C', 'THRESH_BATTERY_TEMP'] };

  function renderSettings() {
    $('set-interval').value = String(settings.interval);
    $$('#set-theme button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.themeOpt === settings.theme)));
    renderNotifBtn();
    const c = S.config;
    $('set-thresholds').innerHTML = c ? Object.entries(TH_LABELS).map(([k, [label, unit, env]]) => {
      const t = c.thresholds[k];
      return t ? `<tr><td>${label}</td><td class="r num">${n1(t.warning)}${unit}</td><td class="r num">${n1(t.critical)}${unit}</td><td class="mono">${env}</td></tr>` : '';
    }).join('') + '<tr><td>Service down</td><td class="r">—</td><td class="r">selalu</td><td class="mono">SERVICES</td></tr>'
      : '<tr><td colspan="4" class="muted">Memuat…</td></tr>';
    $('set-token-state').textContent = token() ? 'Token tersimpan di browser ini' : 'Tidak ada token tersimpan di browser ini';
    $('set-about').innerHTML = c ? kv([
      ['Versi backend', esc(c.version)],
      ['Interval sampel', c.interval + ' dtk'],
      ['Interval penyimpanan', c.history_interval + ' dtk'],
      ['Data live di memori', fmtDur(c.live_window)],
      ['Retensi history', c.retention_days + ' hari'],
      ['Service dipantau', `<span class="mono">${esc(c.services.join(', ') || '—')}</span>`],
      ['Monitoring proses', esc({ token: 'aktif (token)', public: 'publik', disabled: 'nonaktif' }[c.features.processes])],
      ['Container', 'tidak didukung'],
    ]) : '';
  }

  // ------------------------------------------------------------------ wiring
  function bind() {
    window.addEventListener('hashchange', route);
    document.addEventListener('visibilitychange', () => {
      if (!document.hidden) { poll(); pollAlerts(); if (S.view === 'processes') loadProcesses(); }
    });
    window.addEventListener('online', () => poll());
    $('retry-btn').addEventListener('click', () => poll());
    $('theme-btn').addEventListener('click', () => {
      const dark = document.documentElement.dataset.theme === 'dark' || (!document.documentElement.dataset.theme && !window.matchMedia('(prefers-color-scheme: light)').matches);
      settings.theme = dark ? 'light' : 'dark';
      store.set('settings', settings);
      applyTheme();
    });
    window.matchMedia('(prefers-color-scheme: light)').addEventListener('change', () => { if (settings.theme === 'system') applyTheme(); });
    $('set-theme').addEventListener('click', (e) => {
      const b = e.target.closest('button'); if (!b) return;
      settings.theme = b.dataset.themeOpt; store.set('settings', settings); applyTheme();
    });
    $('set-interval').addEventListener('change', (e) => {
      settings.interval = Number(e.target.value) || 5; store.set('settings', settings); poll();
    });
    $('set-notify').addEventListener('change', (e) => enableNotifications(e.target.checked));
    $('notif-btn').addEventListener('click', () => enableNotifications(!(settings.notify && Notification.permission === 'granted')));
    $('set-token-clear').addEventListener('click', () => { store.del('token'); renderSettings(); });
    $('range-seg').addEventListener('click', (e) => { const b = e.target.closest('button'); if (b) setRange(b.dataset.range); });
    $('reset-zoom').addEventListener('click', () => { stopPlayback(); resetZoom(); });
    $('play-btn').addEventListener('click', togglePlayback);
    $('al-filter').addEventListener('click', (e) => {
      const b = e.target.closest('button'); if (!b) return;
      S.alertFilter = b.dataset.sev;
      $$('#al-filter button').forEach((x) => x.setAttribute('aria-pressed', String(x === b)));
      renderAlerts();
    });
    $('proc-search').addEventListener('input', (e) => { S.proc.q = e.target.value; renderProcesses(); });
    $('proc-refresh').addEventListener('click', loadProcesses);
    $('proc-body').addEventListener('click', (e) => {
      const s = e.target.closest('[data-sort]');
      if (s) {
        const k = s.dataset.sort;
        if (S.proc.sort === k) S.proc.dir *= -1; else { S.proc.sort = k; S.proc.dir = k === 'name' || k === 'state' ? 1 : -1; }
        renderProcesses();
        const again = $('proc-body').querySelector(`[data-sort="${k}"]`); if (again) again.focus();
      }
      if (e.target.closest('[data-proc-retry]')) loadProcesses();
    });
    RM.addEventListener('change', rebuildCharts);
  }

  function init() {
    bind();
    applyTheme();
    $$('#range-seg button').forEach((b) => b.setAttribute('aria-pressed', String(b.dataset.range === S.range)));
    route();
    loadConfig();
    poll();
    pollAlerts();
    timers.clock = setInterval(tickClock, 1000);
  }

  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', init);
  else init();
})();
