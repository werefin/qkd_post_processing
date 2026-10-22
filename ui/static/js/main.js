// qkd_post_processing dashboard

function setEl(id, val) {
  const el = document.getElementById(id);
  if (el) el.textContent = val;
}

async function apiFetch(url, opts = {}) {
  const res = await fetch(url, opts);
  const ct = res.headers.get('Content-Type') || '';
  const body = ct.includes('json') ? await res.json() : { error: await res.text() };
  if (!res.ok) throw new Error(body.error || `Server error ${res.status}`);
  return body;
}

// Gauge (same arc technique as skQCI's key-pool gauge)
function drawGauge(canvasId, value, max) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  // a canvas in a hidden section has zero size and ctx.arc() would throw; skip it,
  // navigate() redraws from cached state once the section is shown
  if (W <= 0 || H <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);

  const cx = W / 2, cy = H - 10, r = Math.min(W / 2 - 14, H - 24);
  const start = Math.PI, end = 2 * Math.PI;
  const pct = Math.max(0, Math.min(1, value / max));
  const fill = start + pct * Math.PI;

  ctx.beginPath(); ctx.arc(cx, cy, r, start, end);
  ctx.strokeStyle = '#1B1B38'; ctx.lineWidth = 12; ctx.lineCap = 'round'; ctx.stroke();

  if (pct > 0) {
    const grad = ctx.createLinearGradient(cx - r, cy, cx + r, cy);
    grad.addColorStop(0, '#2BBDE8'); grad.addColorStop(0.5, '#9B5CF5'); grad.addColorStop(1, '#E84B8C');
    ctx.beginPath(); ctx.arc(cx, cy, r, start, fill);
    ctx.strokeStyle = grad; ctx.lineWidth = 12; ctx.lineCap = 'round'; ctx.stroke();
  }
}

// Everything below talks to the backend; server text is always escaped before it reaches the page
function esc(v) {
  return String(v ?? '').replace(/[&<>"']/g, c => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));
}

function chipsHTML(items) {
  return items.map(([k, v]) => `<div class="chip"><span class="chip-k">${esc(k)}</span><span class="chip-v">${esc(v)}</span></div>`).join('');
}

function postJSON(url, body) {
  return apiFetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
}

function setBadge(id, text, kind) {
  const b = document.getElementById(id);
  if (!b) return;
  b.textContent = text;
  b.className = 'badge badge-' + (kind || 'muted');
}

function emptyRow(cols, text) {
  return `<tr><td colspan="${cols}" class="empty">${esc(text)}</td></tr>`;
}

function fmtTime(t) {
  return t ? new Date(t * 1000).toLocaleString() : '-';
}

// Navigation: each section polls only while it is open
const pollers = {};
function stopPolls() {
  for (const k of Object.keys(pollers)) { clearInterval(pollers[k]); delete pollers[k]; }
}
function poll(name, fn, ms) {
  fn();
  pollers[name] = setInterval(fn, ms);
}

function navigate(section) {
  window.history.replaceState(null, '', '#' + section);
  document.querySelectorAll('.nav-item').forEach(el => el.classList.toggle('active', el.dataset.section === section));
  document.querySelectorAll('.section').forEach(el => el.classList.toggle('active', el.id === 'section-' + section));
  stopPolls();
  if (section === 'home') { redrawHomeGauge(); refreshNode(); refreshConfig(); }
  else if (section === 'network') { poll('node', refreshNode, 10000); refreshAudit(); }
  else if (section === 'live') { tpLoadChoices(); poll('twoparty', refreshTwoParty, 3000); }
  else if (section === 'monitor') { poll('monitor', refreshMonitor, 3000); }
  else if (section === 'clog') { clogLoadOptions(); refreshRecordings().then(fillClogRecordings); refreshClogFiles(); poll('clog', refreshClog, 1000); }
  else if (section === 'chsh') refreshRecordings();
}

document.getElementById('main-nav').addEventListener('click', e => {
  const item = e.target.closest('.nav-item');
  if (item) { e.preventDefault(); navigate(item.dataset.section); }
});
document.querySelectorAll('.nav-card').forEach(card => card.addEventListener('click', e => { e.preventDefault(); navigate(card.dataset.section); }));
document.getElementById('alerts-row').addEventListener('click', e => { e.preventDefault(); navigate('live'); });

// Status: backend, KMS and alerts, always polled
let poolKeys = 0;

function redrawHomeGauge() {
  drawGauge('home-gauge-canvas', poolKeys, Math.max(1000, poolKeys * 1.25));
  setEl('home-gauge-value', poolKeys);
}

async function refreshStatus() {
  try {
    const d = await apiFetch('/api/status');
    document.getElementById('status-dot').className = 'status-dot online';
    setEl('status-label', 'Dashboard reachable');
    setEl('sidebar-node-id', d.node_id || '');
    if (!d.kms) {
      document.getElementById('kms-dot').className = 'status-dot offline';
      setEl('kms-label', 'No KMS configured');
    } else {
      document.getElementById('kms-dot').className = 'status-dot ' + (d.kms_ok === false ? 'offline' : 'online');
      setEl('kms-label', d.kms_ok === false ? 'KMS unreachable' : 'KMS connected');
    }
  } catch (e) {
    document.getElementById('status-dot').className = 'status-dot offline';
    setEl('status-label', 'Dashboard unreachable');
  }
  try {
    const a = (await apiFetch('/api/alerts')).alerts || [];
    const critical = a.some(x => x.level === 'critical');
    document.getElementById('alerts-dot').className = 'status-dot ' + (a.length ? (critical ? 'offline' : 'checking') : 'online');
    setEl('alerts-label', a.length ? `${a.length} alert${a.length > 1 ? 's' : ''}: ${a[0].text}` : 'No alerts');
  } catch (e) { /* shown by the line above */ }
}
refreshStatus();
setInterval(refreshStatus, 10000);

// Network and KMS
async function refreshNode() {
  try {
    const n = await apiFetch('/api/node');
    document.getElementById('kms-dot').className = 'status-dot online';
    poolKeys = (n.links || []).reduce((s, l) => s + l.mine + l.theirs, 0);
    setEl('kms-label', `KMS: ${poolKeys} pool keys`);
    renderNode(n);
  } catch (e) {
    document.getElementById('kms-dot').className = 'status-dot offline';
    setEl('kms-label', 'KMS unreachable');
    setBadge('net-badge', e.message, 'danger');
    // a node without a KMS, such as a lab PC, is not an error: say what it means instead of the raw message
    document.getElementById('home-node-chips').innerHTML = chipsHTML([['KMS', /not connected/.test(e.message)
      ? 'not configured: coincidence log and CHSH only' : e.message]]);
  }
  redrawHomeGauge();
}

function renderNode(n) {
  const links = n.links || [];
  const up = links.filter(l => l.reachable).length;
  setBadge('net-badge', `${up} of ${links.length} links reachable`, up === links.length ? 'success' : 'warn');
  const nodeChips = [
    ['Node', n.node_id], ['Applications here', (n.saes || []).join(', ') || 'none'],
    ['Reserved keys waiting', n.local.reserved_key_count], ['Delivered', n.local.consumed_key_count],
    ['Links', links.length], ['Pool keys', poolKeys],
  ];
  document.getElementById('net-node-chips').innerHTML = chipsHTML(nodeChips);
  document.getElementById('home-node-chips').innerHTML = chipsHTML([
    ['Node', n.node_id], ['Links up', `${up} / ${links.length}`], ['Pool keys', poolKeys],
    ['Reachable nodes', Object.keys(n.routes || {}).length],
  ]);
  document.getElementById('net-links').innerHTML = links.length ? links.map(l => `<tr>
      <td>${esc(l.neighbor)}</td>
      <td class="${l.reachable ? 'positive' : ''}">${l.reachable ? 'yes, ' + l.rtt_ms.toFixed(1) + ' ms' : 'no' + (l.error ? ': ' + esc(l.error) : '')}</td>
      <td>${l.mine}</td><td>${l.theirs}</td><td>${l.pool.consumed_key_count + l.pool.expired_key_count}</td>
    </tr>`).join('') : emptyRow(5, 'no QKD links declared for this node');

  const saes = (n.network && n.network.saes) || {};
  const byNode = {};
  for (const [sae, home] of Object.entries(saes)) (byNode[home] = byNode[home] || []).push(sae);
  const nodes = Object.keys((n.network && n.network.nodes) || {}).sort();
  document.getElementById('net-routes').innerHTML = nodes.map(id => `<tr>
      <td>${esc(id)}${id === n.node_id ? ' (this node)' : ''}</td>
      <td>${id === n.node_id ? '-' : esc((n.routes || {})[id] || 'unreachable')}</td>
      <td class="mono">${esc((byNode[id] || []).sort().join(', '))}</td>
    </tr>`).join('');

  // the dashboard's own SAE can exchange keys with applications on every other reachable node
  const remote = Object.entries(saes).filter(([, home]) => home !== n.node_id && (n.routes || {})[home]).map(([s, home]) => [s, home]).sort();
  for (const id of ['kx-slave', 'kx-master']) {
    const sel = document.getElementById(id);
    const keep = sel.value;
    sel.innerHTML = remote.map(([s, home]) => `<option value="${esc(s)}">${esc(s)} (node ${esc(home)})</option>`).join('');
    if (keep) sel.value = keep;
  }
}

document.getElementById('kx-enc-btn').addEventListener('click', async () => {
  const err = document.getElementById('kx-enc-error');
  err.textContent = '';
  try {
    const d = await postJSON('/api/kms/enc', {
      slave: document.getElementById('kx-slave').value,
      number: Number(document.getElementById('kx-number').value), size: Number(document.getElementById('kx-size').value),
    });
    document.getElementById('kx-enc-table').innerHTML = d.keys.map(k => `<tr><td class="mono">${esc(k.key_ID)}</td><td class="mono">${esc(k.fingerprint)}</td></tr>`).join('');
    refreshNode(); refreshAudit();
  } catch (e) { err.textContent = e.message; }
});

document.getElementById('kx-dec-btn').addEventListener('click', async () => {
  const err = document.getElementById('kx-dec-error');
  err.textContent = '';
  try {
    const d = await postJSON('/api/kms/dec', {
      master: document.getElementById('kx-master').value,
      key_ids: document.getElementById('kx-ids').value.split(/\s+/).filter(Boolean),
    });
    document.getElementById('kx-dec-table').innerHTML = d.keys.map(k => `<tr><td class="mono">${esc(k.key_ID)}</td><td class="mono">${esc(k.fingerprint)}</td></tr>`).join('');
    refreshNode(); refreshAudit();
  } catch (e) { err.textContent = e.message; }
});

async function refreshAudit() {
  try {
    const d = await apiFetch('/api/kms/audit?n=200');
    document.getElementById('audit-table').innerHTML = d.events.length ? d.events.map(e => `<tr>
        <td>${esc((e.time || '').replace('T', ' ').slice(0, 19))}</td><td>${esc(e.event)}</td>
        <td class="mono" title="${esc(e.key_ID)}">${esc((e.key_ID || '').slice(0, 8))}</td>
        <td>${esc(e.master_SAE_ID)}</td><td>${esc(e.slave_SAE_ID)}</td><td>${esc(e.peer)}</td>
        <td>${esc(e.bits || e.count || '')}</td><td>${esc(e.detail)}</td>
      </tr>`).join('') : emptyRow(8, 'no key events yet');
  } catch (e) {
    document.getElementById('audit-table').innerHTML = emptyRow(8, e.message);
  }
}
document.getElementById('audit-refresh').addEventListener('click', refreshAudit);

// Recordings, and the pair pickers every analysis page shares
let recordings = { files: [], pairs: [], items: [] };

function fillPairPickers() {
  const opts = recordings.files.map(f => `<option value="${esc(f)}">${esc(f.replace('_timestamp_sequence.npy', ''))}</option>`).join('');
  document.querySelectorAll('select.pair-a, select.pair-b').forEach(sel => {
    const keep = sel.value;
    sel.innerHTML = opts;
    if (keep && recordings.files.includes(keep)) sel.value = keep;
  });
  // default to the newest auto-detected pair, else the two newest files
  const last = recordings.pairs[recordings.pairs.length - 1];
  const [da, db] = last ? [last.a, last.b] : recordings.files.slice(-2);
  document.querySelectorAll('select.pair-a').forEach(s => { if (!s.dataset.touched && da) s.value = da; });
  document.querySelectorAll('select.pair-b').forEach(s => { if (!s.dataset.touched && db) s.value = db; });
}
document.querySelectorAll('select.pair-a, select.pair-b').forEach(s => s.addEventListener('change', () => { s.dataset.touched = '1'; }));

async function refreshRecordings() {
  try {
    recordings = await apiFetch('/api/recordings');
  } catch (e) { return; }
  fillPairPickers();
}

function formatBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let u = 0;
  while (n >= 1024 && u < units.length - 1) { n /= 1024; u++; }
  return (u === 0 ? n : n.toFixed(1)) + ' ' + units[u];
}

function setupDropzone(dropId, inputId, onFiles) {
  const drop = document.getElementById(dropId);
  const input = document.getElementById(inputId);
  drop.addEventListener('click', () => input.click());
  drop.addEventListener('keydown', e => { if (e.key === 'Enter' || e.key === ' ') input.click(); });
  drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('drag-over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('drag-over'));
  drop.addEventListener('drop', e => {
    e.preventDefault();
    drop.classList.remove('drag-over');
    if (e.dataTransfer.files.length) onFiles(e.dataTransfer.files);
  });
  input.addEventListener('change', () => { if (input.files.length) onFiles(input.files); });
}

// CHSH diagnostic for the chosen pair
document.getElementById('chsh-run-btn').addEventListener('click', async () => {
  const btn = document.getElementById('chsh-run-btn');
  const errEl = document.getElementById('chsh-error');
  errEl.textContent = '';
  btn.disabled = true;
  setBadge('chsh-badge', 'Running...');
  const num = id => { const v = document.getElementById(id).value; return v === '' ? null : Number(v); };
  try {
    const d = await postJSON('/api/chsh/run', {
      a: document.getElementById('chsh-a').value, b: document.getElementById('chsh-b').value,
      block_s: num('chsh-block-s'), roi1_width_ns: num('chsh-roi1-width'), roi1_center_ns: num('chsh-roi1-center'),
      roi2_offset_ns: num('chsh-roi2-offset'), roi2_width_ns: num('chsh-roi2-width'),
    });
    setBadge('chsh-badge', `${d.metadata.n_blocks} blocks`, 'success');
    document.getElementById('chsh-chips').innerHTML = chipsHTML([
      ['A', d.a], ['B', d.b], ['Blocks', `${d.metadata.n_blocks} x ${d.metadata.block_s}s`],
      ['ROI1', `${(d.metadata.roi1.center_ps / 1000).toFixed(2)} ns ± ${(d.metadata.roi1.width_ps / 2000).toFixed(2)} ns`],
      ['ROI2', `${(d.metadata.roi2.center_ps / 1000).toFixed(2)} ns ± ${(d.metadata.roi2.width_ps / 2000).toFixed(2)} ns` + (d.roi2_offset_auto ? ' (auto)' : '')],
      ['Clock skew', d.metadata.clock_skew_ppb.toFixed(2) + ' ppb'],
    ]);
    document.getElementById('chsh-table-body').innerHTML = d.pairs.map(p => `<tr>
        <td>${esc(p.pair)}</td><td>${p.roi1_total.toLocaleString()}</td><td>${p.roi2_total.toLocaleString()}</td>
        <td class="${p.subtracted > 0 ? 'positive' : ''}">${p.subtracted.toLocaleString()}</td></tr>`).join('');
    document.getElementById('chsh-download-btn').style.display = '';
  } catch (e) {
    errEl.textContent = e.message;
    setBadge('chsh-badge', 'Error', 'danger');
  } finally {
    btn.disabled = false;
  }
});

// TimeTagger config, loaded on the home page
async function refreshConfig() {
  try {
    const d = await apiFetch('/api/config');
    if (!d.loaded) { setBadge('config-status', 'No config loaded yet'); return; }
    setBadge('config-status', `${d.sites.length} sites, ${d.links.length} links`, 'success');
  } catch (e) { /* backend not reachable yet */ }
}
refreshConfig();

setupDropzone('config-drop', 'config-file-input', files => {
  const status = document.getElementById('config-error');
  status.textContent = '';
  const fd = new FormData();
  fd.append('file', files[0]);
  apiFetch('/api/config', { method: 'POST', body: fd }).then(refreshConfig).catch(err => { status.textContent = err.message; });
});

const fmtNum = (v, digits) => (v == null || !isFinite(v)) ? '-' : Number(v).toFixed(digits);
const fmtDur = s => s == null ? '-' : s >= 3600 ? `${(s / 3600).toFixed(1)} h` : s >= 60 ? `${(s / 60).toFixed(1)} min` : `${s.toFixed(0)} s`;

// Coincidence log: the settings form, the QBER, the coincidence matrix, the delay histograms and the rows;
// series colors are fixed slots validated on this surface
const CLOG_COLORS = ['#3987e5', '#d95926', '#199e70', '#c98500', '#d55181', '#008300', '#9085e9', '#e66767'];
const CLOG_RAMP = ['#2b4d94', '#3766bd', '#4d84dd', '#7cabf1', '#b5d5fc'];
const CLOG_PRESETS = ['30', '300', '1200', '3600', '18000'];
const CLOG_INK = { text: '#EDF0FF', text2: '#8892B0', text3: '#5A6480', grid: '#161632', dark: '#06060F', neutral: '#111128' };
const clog = { opts: null, status: null, started: null, live: [], liveSeen: 0, hist: null, rows: [], columns: [], written: -1, fetching: false };
const clogTip = document.createElement('div');
clogTip.className = 'clog-tip';
document.body.appendChild(clogTip);
const $c = id => document.getElementById(id);

function clogFmt(v) {
  if (v == null || !isFinite(v)) return '-';
  const a = Math.abs(v);
  if (a >= 1e6) return (v / 1e6).toFixed(2) + 'M';
  if (a >= 1e4) return (v / 1e3).toFixed(1) + 'k';
  if (Number.isInteger(v)) return String(v);
  return a >= 100 ? v.toFixed(0) : String(Number(a >= 1 ? v.toFixed(2) : v.toPrecision(3)));
}

// One plot: thin lines (or bars), recessive grid and axes, a legend from two series, a crosshair and tooltip on hover
function clogPlot(canvas, series, o = {}) {
  const c = typeof canvas === 'string' ? $c(canvas) : canvas;
  if (!c) return;
  c._args = [series, o];
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  if (W <= 0 || H <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const pts = series.flatMap(s => s.xs.map((x, i) => [x, s.ys[i]])).filter(([x, y]) => x != null && y != null && isFinite(y) && (!o.logY || y > 0));
  ctx.font = '10px "JetBrains Mono", monospace';
  if (!pts.length) {
    ctx.fillStyle = CLOG_INK.text3;
    ctx.fillText(o.empty || 'waiting for data...', 8, H / 2);
    c._map = null;
    return;
  }
  const mini = !!o.mini;
  const L = mini ? 4 : 52, R = mini ? 4 : (o.badges ? 84 : 10), T = mini ? 4 : (series.length >= 2 ? 22 : 8), B = mini ? 4 : 20;
  let xmn = Math.min(...pts.map(p => p[0])), xmx = Math.max(...pts.map(p => p[0]));
  if (o.xRange) [xmn, xmx] = o.xRange;
  if (xmn === xmx) { xmn -= 1; xmx += 1; }
  const tf = o.logY ? v => Math.log10(v) : v => v;
  let ymn = Math.min(...pts.map(p => tf(p[1]))), ymx = Math.max(...pts.map(p => tf(p[1])));
  if (!o.logY && o.zero !== false) ymn = Math.min(ymn, 0);
  if (ymn === ymx) { ymn -= 1; ymx += 1; }
  ymx += (ymx - ymn) * 0.05;
  const X = v => L + ((v - xmn) / (xmx - xmn)) * (W - L - R);
  const Y = v => H - B - ((tf(v) - ymn) / (ymx - ymn)) * (H - T - B);
  c._map = { X, xmn, xmx, L, R, W, series };

  ctx.strokeStyle = CLOG_INK.grid; ctx.lineWidth = 1;
  ctx.fillStyle = CLOG_INK.text3; ctx.textAlign = 'right';
  for (const v of clogTicks(ymn, ymx, o.logY)) {
    const gy = H - B - ((v - ymn) / (ymx - ymn)) * (H - T - B);
    ctx.beginPath(); ctx.moveTo(L, gy); ctx.lineTo(W - R, gy); ctx.stroke();
    if (!mini) ctx.fillText((o.yFmt || clogFmt)(o.logY ? 10 ** v : v), L - 6, gy + 3);
  }
  if (!mini) {
    ctx.textAlign = 'center';
    for (const v of clogTicks(xmn, xmx, false)) {
      ctx.fillText((o.xFmt || clogFmt)(v), Math.min(Math.max(X(v), L + 20), W - R - 20), H - 5);
    }
  }
  for (const [lo, hi, dashed] of o.bands || []) {
    const a = Math.max(X(Math.max(lo, xmn)), L), b = Math.min(X(Math.min(hi, xmx)), W - R);
    if (b <= a) continue;
    if (dashed) {
      ctx.setLineDash([3, 3]); ctx.strokeStyle = CLOG_INK.text3;
      ctx.strokeRect(a, T, Math.max(1, b - a), H - T - B); ctx.setLineDash([]);
    } else {
      ctx.fillStyle = 'rgba(237,240,255,0.08)';
      ctx.fillRect(a, T, Math.max(1, b - a), H - T - B);
    }
  }
  const base = o.logY ? null : Y(Math.max(ymn, 0));
  for (const s of series) {
    ctx.strokeStyle = s.color; ctx.fillStyle = s.color;
    if (o.bars) {
      const w = Math.max(1, (W - L - R) / s.xs.length - (s.xs.length < 120 ? 2 : 0));
      s.xs.forEach((x, i) => {
        const y = s.ys[i];
        if (y == null || (o.logY && y <= 0)) return;
        const top = Y(y), bot = o.logY ? H - B : base;
        ctx.fillRect(X(x) - w / 2, Math.min(top, bot), w, Math.max(1, Math.abs(bot - top)));
      });
      continue;
    }
    ctx.lineWidth = mini ? 1.2 : 2; ctx.lineJoin = 'round';
    ctx.setLineDash(s.dashed ? [6, 5] : []);
    ctx.beginPath();
    let pen = false;
    s.xs.forEach((x, i) => {
      const y = s.ys[i];
      if (y == null || !isFinite(y) || (o.logY && y <= 0)) { pen = false; return; }
      if (pen) ctx.lineTo(X(x), Y(y)); else { ctx.moveTo(X(x), Y(y)); pen = true; }
    });
    ctx.stroke();
    ctx.setLineDash([]);
  }
  if (o.badges && !mini) {
    // each series' latest value, in text ink beside its colour, in a column right of the plot
    ctx.font = '600 10px "JetBrains Mono", monospace'; ctx.textAlign = 'left';
    series.filter(sr => sr.badge !== false).forEach((sr, i) => {
      const v = [...sr.ys].reverse().find(y => y != null && isFinite(y));
      const y0 = T + i * 16;
      ctx.fillStyle = sr.color; ctx.fillRect(W - R + 10, y0, 9, 9);
      ctx.fillStyle = CLOG_INK.text; ctx.fillText(v == null ? '-' : (o.yFmt || clogFmt)(v), W - R + 24, y0 + 8);
    });
  }
  if (!mini && series.length >= 2) {
    // legend in text ink, the colored swatch beside each name carries the identity
    let lx = L;
    ctx.textAlign = 'left'; ctx.font = '600 10px "JetBrains Mono", monospace';
    for (const s of series) {
      const w = ctx.measureText(s.name).width;
      if (lx + w + 18 > W - R) break;
      ctx.fillStyle = s.color; ctx.fillRect(lx, 6, 9, 9);
      ctx.fillStyle = CLOG_INK.text2; ctx.fillText(s.name, lx + 13, 14);
      lx += w + 26;
    }
  }
  if (o.hoverX != null && !mini) {
    const hx = X(o.hoverX);
    ctx.strokeStyle = CLOG_INK.text3; ctx.lineWidth = 1;
    ctx.beginPath(); ctx.moveTo(hx, T); ctx.lineTo(hx, H - B); ctx.stroke();
    for (const s of series) {
      const i = clogNearest(s.xs, o.hoverX);
      const y = s.ys[i];
      if (i < 0 || y == null || (o.logY && y <= 0)) continue;
      ctx.beginPath(); ctx.arc(X(s.xs[i]), Y(y), 4, 0, Math.PI * 2);
      ctx.fillStyle = s.color; ctx.fill();
      ctx.lineWidth = 2; ctx.strokeStyle = '#0C0C1E'; ctx.stroke();
    }
  }
  if (!c._hooked && !mini) {
    c._hooked = true;
    c.addEventListener('mousemove', e => {
      const m = c._map;
      if (!m) return;
      const r = c.getBoundingClientRect();
      const x = m.xmn + ((e.clientX - r.left - m.L) / (m.W - m.L - m.R)) * (m.xmx - m.xmn);
      const [ser, opt] = c._args;
      clogPlot(c, ser, { ...opt, hoverX: x });
      const lines = ser.map(s => {
        const i = clogNearest(s.xs, x);
        return i < 0 ? '' : `<div><span class="sw" style="background:${s.color}"></span>${esc(s.name)}: ${clogFmt(s.ys[i])}</div>`;
      }).join('');
      const i0 = ser.length ? clogNearest(ser[0].xs, x) : -1;
      clogTip.innerHTML = (i0 >= 0 ? `<div style="color:${CLOG_INK.text2}">${esc((opt.xFmt || clogFmt)(ser[0].xs[i0]))}${opt.xUnit || ''}</div>` : '') + lines;
      clogShowTip(e);
    });
    c.addEventListener('mouseleave', () => { clogTip.style.display = 'none'; const [ser, opt] = c._args; clogPlot(c, ser, { ...opt, hoverX: null }); });
  }
}

function clogTicks(lo, hi, log) {
  // round steps of 1, 2 or 5 times a power of ten; on a log axis, whole decades
  if (log) {
    const out = [];
    for (let e = Math.ceil(lo); e <= hi; e++) out.push(e);
    return out.length >= 2 ? out : [lo, hi];
  }
  const raw = (hi - lo) / 4, mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 5, 10].map(m => m * mag).find(st => st >= raw);
  const out = [];
  for (let v = Math.ceil(lo / step) * step; v <= hi + 1e-9 * step; v += step) out.push(Math.abs(v) < step * 1e-9 ? 0 : v);
  return out;
}

function clogNearest(xs, x) {
  let best = -1, d = Infinity;
  xs.forEach((v, i) => { const dd = Math.abs(v - x); if (dd < d) { d = dd; best = i; } });
  return best;
}

function clogShowTip(e) {
  clogTip.style.display = 'block';
  const w = clogTip.offsetWidth;
  clogTip.style.left = `${Math.min(e.clientX + 14, window.innerWidth - w - 8)}px`;
  clogTip.style.top = `${e.clientY + 14}px`;
}

// Form

async function clogLoadOptions() {
  let d;
  try { d = await apiFetch('/api/clog/options'); } catch (e) { return; }
  clog.opts = d;
  const a = $c('clog-site-a'), b = $c('clog-site-b');
  const ka = a.value, kb = b.value;
  const opts = Object.keys(d.site_channels || {}).map(s => `<option value="${esc(s)}">${esc(s)}</option>`).join('');
  a.innerHTML = d.loaded ? opts : '<option value="">load a config on the Home page</option>';
  b.innerHTML = '<option value="">none, one TimeTagger</option>' + opts;
  if (ka) a.value = ka;
  b.value = kb || '';
  if (!a.dataset.filled) {
    // the defaults under the config's own section fill the form once
    a.dataset.filled = '1';
    const o = { ...d.defaults, ...d.config };
    if (o.sites && o.sites.length) { a.value = o.sites[0]; b.value = o.sites[1] || ''; }
    const v = String(o.integration_s);
    $c('clog-int').value = CLOG_PRESETS.includes(v) ? v : 'custom';
    $c('clog-int-custom').value = v;
    $c('clog-int-custom-f').style.display = CLOG_PRESETS.includes(v) ? 'none' : '';
    $c('clog-window').value = o.window_ps; $c('clog-center').value = o.roi_center_ps; $c('clog-noise').value = o.noise_offset_ps;
    $c('clog-delays').value = Object.entries(o.delays_ps || {}).map(([k, x]) => `${k}=${x}`).join(', ');
    $c('clog-higher').value = (o.higher_order || []).map(g => (g.channels || g).join('+')).join(', ');
    $c('clog-derived').value = Object.entries(o.derived || {}).map(([k, x]) => `${k} = ${x}`).join('\n');
    $c('clog-exp').value = (o.metadata || {}).experiment || '';
    $c('clog-notes').value = (o.metadata || {}).notes || '';
  }
  clogLabels();
}

function fillClogRecordings() {
  const opts = recordings.files.map(f => `<option value="${esc(f)}">${esc(f.replace('_timestamp_sequence.npy', ''))}</option>`).join('');
  const ra = $c('clog-ra'), rb = $c('clog-rb');
  const ka = ra.value, kb = rb.value;
  ra.innerHTML = opts;
  rb.innerHTML = '<option value="">none, one recording</option>' + opts;
  if (ka) ra.value = ka;
  rb.value = kb || '';
}

function clogTwoSites() {
  return $c('clog-source').value === 'sites' ? !!$c('clog-site-b').value : !!$c('clog-rb').value;
}

// two sites' channels are labelled by site name, as the logger does it (coincidences/plan.py site_prefixes)
function clogPrefixes(names) {
  const p = names.map(n => String(n).replace(/[^A-Za-z0-9]/g, ''));
  return new Set(p).size !== p.length || p.some(x => !x || /[0-9]$/.test(x)) ? ['X', 'Y'] : p;
}

function clogLabels() {
  const recs = $c('clog-source').value !== 'sites';
  const names = recs ? (clogTwoSites() ? ['A', 'B'] : ['A']) : [$c('clog-site-a').value, $c('clog-site-b').value].filter(Boolean);
  if (names.length === 2) {
    const [p, q] = clogPrefixes(names);
    $c('clog-labels').textContent = `Channels ${p}1-${p}4 and ${q}1-${q}4; columns S_${p}1 (singles), C_${p}1_${q}1 (coincidences), C_${p}1_${q}1_noise, `
      + 'and QBER_HV, QBER_DA, QBER with channels 1-2 the H/V bits and 3-4 the D/A bits.';
  } else {
    $c('clog-labels').textContent = 'Channels 1-4; columns S_1 (singles), C_1_2 (coincidences) and C_1_2_noise.';
  }
}

function clogOptions() {
  const val = id => $c(id).value.trim();
  const o = { integration_s: Number(val('clog-int') === 'custom' ? val('clog-int-custom') : val('clog-int')) };
  for (const [id, key] of [['clog-window', 'window_ps'], ['clog-center', 'roi_center_ps'], ['clog-noise', 'noise_offset_ps']]) {
    if (val(id) !== '') o[key] = Number(val(id));
  }
  o.delays_ps = {};
  for (const part of val('clog-delays').split(',').map(x => x.trim()).filter(Boolean)) {
    const [k, v] = part.split('=').map(x => (x || '').trim());
    if (!k || v === '' || isNaN(Number(v))) throw new Error(`delay "${part}" is not channel=ps`);
    o.delays_ps[k] = Number(v);
  }
  o.higher_order = val('clog-higher').split(',').map(g => g.replace(/\s+/g, '')).filter(Boolean).map(g => g.split('+'));
  o.derived = {};
  for (const line of $c('clog-derived').value.split('\n').map(x => x.trim()).filter(Boolean)) {
    const i = line.indexOf('=');
    if (i < 1) throw new Error(`"${line}" is not name = formula`);
    o.derived[line.slice(0, i).trim()] = line.slice(i + 1).trim();
  }
  o.metadata = {};
  if (val('clog-exp')) o.metadata.experiment = val('clog-exp');
  if (val('clog-notes')) o.metadata.notes = val('clog-notes');
  return o;
}

$c('clog-source').addEventListener('change', e => {
  const sites = e.target.value === 'sites';
  document.querySelectorAll('.clog-sites').forEach(el => { el.style.display = sites ? '' : 'none'; });
  document.querySelectorAll('.clog-recs').forEach(el => { el.style.display = sites ? 'none' : ''; });
  clogLabels();
});
['clog-site-a', 'clog-site-b', 'clog-rb'].forEach(id => $c(id).addEventListener('change', clogLabels));
$c('clog-int').addEventListener('change', e => { $c('clog-int-custom-f').style.display = e.target.value === 'custom' ? '' : 'none'; });

$c('clog-start').addEventListener('click', async () => {
  const err = $c('clog-error');
  err.textContent = '';
  let body;
  try { body = { source: $c('clog-source').value, options: clogOptions(), windows: $c('clog-windows').value || null }; } catch (e) { err.textContent = e.message; return; }
  if (body.source === 'sites') body.sites = [$c('clog-site-a').value, $c('clog-site-b').value].filter(Boolean);
  else { body.a = $c('clog-ra').value; body.b = $c('clog-rb').value; }
  try {
    await postJSON('/api/clog/start', body);
    clog.rows = []; clog.written = -1;
    refreshClog();
  } catch (e) { err.textContent = e.message; }
});
$c('clog-stop').addEventListener('click', async () => {
  try { await apiFetch('/api/clog/stop', { method: 'POST' }); refreshClog(); } catch (e) { $c('clog-error').textContent = e.message; }
});

// Status and rows

let clogWasRunning = false;
async function refreshClog() {
  let d;
  try { d = await apiFetch(`/api/clog/status?since=${clog.liveSeen}`); } catch (e) { return; }
  if (d.started_at !== clog.started) {
    // a new run: its live history from the start
    clog.started = d.started_at;
    clog.live = [];
    try { d = await apiFetch('/api/clog/status?since=0'); } catch (e) { return; }
  }
  clog.live.push(...d.live);
  if (clog.live.length > 2400) clog.live.splice(0, clog.live.length - 2400);
  clog.liveSeen = d.live_total;
  clog.status = d.status;
  if (d.histograms) clog.hist = d.histograms;
  const st = d.status;
  setBadge('clog-badge', d.running ? (st ? st.state : 'starting') : st ? 'Finished' : 'Idle',
    d.running ? (st && st.state === 'running' ? 'success' : 'warn') : 'muted');
  $c('clog-start').disabled = d.running;
  $c('clog-stop').disabled = !d.running;
  const chips = [];
  if (d.params) chips.push(['Source', d.params.source], ['Integration', fmtDur(d.params.integration_s)]);
  if (st) {
    chips.push(['File', st.file], ['Rows', st.written + (st.max_windows ? ` of ${st.max_windows}` : '')]);
    if (st.window) {
      chips.push([`Window ${st.window.index}`, `${fmtDur(st.window.progress_s)} of ${fmtDur(st.integration_s)}`]);
      $c('clog-progress').style.width = `${Math.min(100, 100 * st.window.progress_s / st.integration_s).toFixed(1)}%`;
    }
    if (st.alignment) chips.push(['Clocks', st.alignment.state === 'locked' ? `locked, skew ${fmtNum(st.alignment.skew_ppb, 2)} ppb` : st.alignment.state]);
    const last = clog.live[clog.live.length - 1];
    if (last) chips.push(['Singles per second', st.singles.map(s => `${s.slice(2)} ${clogFmt(last.rates[s])}`).join(', ')]);
    if (st.written !== clog.written && st.file && !clog.fetching) clogFetchRows(st.file, st.written);
  }
  if (clogWasRunning && !d.running) refreshClogFiles();
  clogWasRunning = d.running;
  $c('clog-chips').innerHTML = chipsHTML(chips);
  const logEl = $c('clog-log');
  logEl.textContent = (d.log || []).join('\n');
  logEl.scrollTop = logEl.scrollHeight;
  clogDrawAll();
}

async function clogFetchRows(name, written) {
  clog.fetching = true;
  try {
    const d = await apiFetch(`/api/clog/files/${encodeURIComponent(name)}/data`);
    clog.rows = d.rows; clog.columns = d.columns; clog.written = written;
  } catch (e) { /* the file appears with the first row */ } finally { clog.fetching = false; }
  clogDrawRows();
  clogDrawQber();
}

async function refreshClogFiles() {
  let d;
  try { d = await apiFetch('/api/clog/files'); } catch (e) { return; }
  $c('clog-files').innerHTML = d.files.length ? d.files.map(f => `<tr>
      <td class="mono">${esc(f.name)}</td><td>${f.rows}</td><td>${formatBytes(f.bytes)}</td><td>${fmtTime(f.modified)}</td>
      <td style="white-space:nowrap"><a class="btn btn-outline clog-btn" href="/api/clog/files/${encodeURIComponent(f.name)}">Download</a>
        <button class="btn btn-outline clog-btn" data-del="${esc(f.name)}">Delete</button></td></tr>`).join('') : emptyRow(5, 'none yet');
}
$c('clog-files').addEventListener('click', async e => {
  const del = e.target.closest('[data-del]');
  if (del && confirm(`Delete ${del.dataset.del} for good?`)) {
    try { await apiFetch(`/api/clog/files/${encodeURIComponent(del.dataset.del)}`, { method: 'DELETE' }); } catch (err) { $c('clog-error').textContent = err.message; }
    refreshClogFiles();
  }
});

// Drawing

function clogDrawQber() {
  const st = clog.status;
  const names = ['QBER_HV', 'QBER_DA', 'QBER'].filter(n => (st ? st.columns : clog.columns).includes(n));
  $c('clog-qber-card').style.display = names.length ? '' : 'none';
  if (!names.length) return;
  const last = clog.live[clog.live.length - 1];
  const pct = v => v == null ? '-' : `${(100 * v).toFixed(2)}%`;
  const row = clog.rows[clog.rows.length - 1];
  $c('clog-qber').innerHTML = chipsHTML([
    ...names.map(n => [`${n.replace('_', ' ')}, last ${last ? Math.round(last.derived_span_s || 0) : 10} s`, pct(last && last.derived ? last.derived[n] : null)]),
    ...(row ? names.map(n => [`${n.replace('_', ' ')}, last row`, pct(row[n])]) : []),
  ]);
  const rows = clog.rows;
  const t0 = rows.length ? Date.parse(rows[0].start) : 0;
  const xs = rows.map(r => (Date.parse(r.start) - t0) / 60000);
  clogPlot('clog-qber-plot', names.map((n, i) => ({ name: n, color: CLOG_COLORS[i], xs, ys: rows.map(r => r[n] == null ? null : 100 * r[n]) })),
    { xFmt: v => `${v.toFixed(1)} min`, yFmt: v => `${clogFmt(v)}%`, empty: 'one point per row: the first comes at the end of the first window' });
}

function clogDrawMatrix() {
  const c = $c('clog-mx');
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  if (W <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  ctx.font = '10px "JetBrains Mono", monospace';
  c._cells = [];
  const groups = ((clog.status && clog.status.groups) || []).filter(g => g.channels.length === 2);
  if (!groups.length || !clog.live.length) { ctx.fillStyle = CLOG_INK.text3; ctx.fillText('no coincidences counted yet', 8, H / 2); return; }
  const end = clog.live[clog.live.length - 1].t_s;
  const pts = clog.live.filter(p => p.t_s > end - 10);
  const get = n => pts.reduce((a, p) => a + (p.rates[n] || 0) * p.dt_s, 0);
  const rowsL = [...new Set(groups.map(g => g.channels[0]))], colsL = [...new Set(groups.map(g => g.channels[1]))];
  const cells = {};
  for (const g of groups) { const sig = get(g.name), noise = get(g.name + '_noise'); cells[g.channels[0] + '|' + g.channels[1]] = { v: sig - noise, sig, noise, name: g.name }; }
  const vmax = Math.max(1, ...Object.values(cells).map(x => x.v));
  const L = 40, T = 20, cw = (W - L - 4) / colsL.length, ch = (H - T - 4) / rowsL.length;
  ctx.fillStyle = CLOG_INK.text2; ctx.textAlign = 'center';
  colsL.forEach((n, j) => ctx.fillText(n, L + (j + 0.5) * cw, 13));
  ctx.textAlign = 'right';
  rowsL.forEach((n, i) => ctx.fillText(n, L - 8, T + (i + 0.5) * ch + 3));
  rowsL.forEach((r, i) => colsL.forEach((col, j) => {
    const cell = cells[r + '|' + col];
    if (!cell) return;
    const x = L + j * cw + 1, y = T + i * ch + 1, w = cw - 2, h = ch - 2;
    // one hue, brighter is more; zero or below is the neutral surface
    const k = cell.v > 0 ? Math.min(CLOG_RAMP.length - 1, Math.floor((cell.v / vmax) * CLOG_RAMP.length * 0.999)) : -1;
    ctx.fillStyle = k < 0 ? CLOG_INK.neutral : CLOG_RAMP[k];
    ctx.beginPath(); ctx.roundRect(x, y, w, h, 4); ctx.fill();
    if (w > 34 && h > 16) { ctx.textAlign = 'center'; ctx.fillStyle = k >= 3 ? CLOG_INK.dark : CLOG_INK.text; ctx.fillText(clogFmt(cell.v), x + w / 2, y + h / 2 + 3); }
    c._cells.push({ x, y, w, h, r, col, ...cell });
  }));
  if (!c._hooked) {
    c._hooked = true;
    c.addEventListener('mousemove', e => {
      const b = c.getBoundingClientRect(), px = e.clientX - b.left, py = e.clientY - b.top;
      const hit = (c._cells || []).find(q => px >= q.x && px <= q.x + q.w && py >= q.y && py <= q.y + q.h);
      if (!hit) { clogTip.style.display = 'none'; return; }
      clogTip.innerHTML = `<div>${esc(hit.name)}</div><div>signal ${clogFmt(hit.sig)}, noise ${clogFmt(hit.noise)}, net ${clogFmt(hit.v)}</div>`;
      clogShowTip(e);
    });
    c.addEventListener('mouseleave', () => { clogTip.style.display = 'none'; });
  }
}

function clogDrawHist() {
  const h = clog.hist, st = clog.status, sel = $c('clog-hist-group');
  if (!h || !st) { clogPlot('clog-hist', [], { empty: 'no histogram yet' }); $c('clog-hist-v').textContent = ''; return; }
  const names = Object.keys(h.groups);
  if (sel.options.length !== names.length) {
    const keep = sel.value;
    sel.innerHTML = names.map(n => `<option value="${esc(n)}">${esc(n)}</option>`).join('');
    sel.value = names.includes(keep) ? keep : names[0];
  }
  const n = sel.value, ys = h.groups[n] || [];
  const [lo, hi] = st.roi_ps.map(v => v / 1e3), [nlo, nhi] = st.noise_roi_ps.map(v => v / 1e3);
  clogPlot('clog-hist', [{ name: n, color: CLOG_COLORS[0], xs: h.x_ns, ys }],
    { bars: true, bands: [[lo, hi, false], [nlo, nhi, true]], xFmt: v => `${v.toFixed(1)}`, xUnit: ' ns' });
  const inRoi = ys.reduce((a, y, i) => a + (h.x_ns[i] >= lo && h.x_ns[i] < hi ? y : 0), 0);
  $c('clog-hist-v').textContent = `${inRoi} of ${ys.reduce((a, b) => a + b, 0)} in the ROI (shaded); delay from the first channel in ns. Move the peak into the ROI with the ROI center or the delays.`;
}
$c('clog-hist-group').addEventListener('change', clogDrawHist);

function clogDrawRows() {
  const cols = clog.columns.length ? clog.columns : (clog.status ? clog.status.columns : []);
  const rows = clog.rows;
  setBadge('clog-rows-badge', rows.length ? `${rows.length} row(s), newest first` : 'none yet', rows.length ? 'success' : 'muted');
  $c('clog-head').innerHTML = cols.length ? '<tr>' + cols.map(c => `<th>${esc(c)}</th>`).join('') + '</tr>' : '';
  $c('clog-rows').innerHTML = rows.length ? rows.slice(-200).reverse().map(r => '<tr>' + cols.map(c => {
    let v = r[c];
    if (c === 'start' || c === 'end') v = String(v).replace('T', ' ').slice(0, 19);
    else if (c.startsWith('QBER') && typeof v === 'number') v = `${(100 * v).toFixed(2)}%`;
    else if (typeof v === 'number') v = clogFmt(v);
    return `<td>${v == null ? '' : esc(v)}</td>`;
  }).join('') + '</tr>').join('') : emptyRow(Math.max(cols.length, 1), 'no row yet');
}

function clogDrawAll() {
  if (!$c('section-clog').classList.contains('active')) return;
  clogDrawQber();
  clogDrawMatrix();
  clogDrawHist();
  clogDrawRows();
}
window.addEventListener('resize', clogDrawAll);

// Live QKD: this node's TimeTagger recorded without end, a block at a time with each neighbor
let tpLast = null;

async function tpLoadChoices() {
  try {
    const [cfg, st] = await Promise.all([apiFetch('/api/config'), apiFetch('/api/twoparty/status')]);
    const sel = document.getElementById('tp-site');
    const keep = sel.value || st.state.site || '';
    sel.innerHTML = cfg.loaded ? (cfg.sites || []).map(x => `<option value="${esc(x)}">${esc(x)}</option>`).join('') : '<option value="">load a config on the Home page</option>';
    if (keep) sel.value = keep;
    document.getElementById('tp-block').value = st.state.block_s;
    document.getElementById('tp-auto').checked = st.state.block_auto !== false;
    document.getElementById('tp-segment').value = st.state.segment_s;
    document.getElementById('tp-qber').value = st.state.expected_qber;
    let peers = [];
    try { peers = ((await apiFetch('/api/node')).links || []).map(l => l.neighbor); } catch (e) { /* no KMS */ }
    document.getElementById('tp-peers').innerHTML = peers.length ? peers.map(p => `<label class="chip chip--check"><input type="checkbox" value="${esc(p)}" ${st.state.announce_to.includes(p) ? 'checked' : ''}>${esc(p)}</label>`).join('')
      : '<span class="hint">this node has no QKD link in the key management system</span>';
  } catch (e) { /* backend not reachable yet */ }
}

document.getElementById('tp-start').addEventListener('click', async () => {
  const err = document.getElementById('tp-error');
  err.textContent = '';
  const body = {
    site: document.getElementById('tp-site').value, block_s: Number(document.getElementById('tp-block').value),
    block_auto: document.getElementById('tp-auto').checked,
    segment_s: Number(document.getElementById('tp-segment').value), expected_qber: Number(document.getElementById('tp-qber').value),
    announce_to: [...document.querySelectorAll('#tp-peers input:checked')].map(x => x.value),
  };
  try { await postJSON('/api/twoparty/start', body); refreshTwoParty(); } catch (e) { err.textContent = e.message; }
});
document.getElementById('tp-stop').addEventListener('click', async () => {
  const err = document.getElementById('tp-error');
  err.textContent = '';
  try { await postJSON('/api/twoparty/stop', {}); refreshTwoParty(); } catch (e) { err.textContent = e.message; }
});

async function refreshTwoParty() {
  let d;
  try { d = await apiFetch('/api/twoparty/status'); } catch (e) { return; }
  tpLast = d;
  const active = d.state.active;
  setBadge('tp-badge', active ? (d.recording ? 'Recording and distilling' : 'Recorder restarting') : 'Idle', active ? (d.recording ? 'success' : 'warn') : 'muted');
  // a node the network controller runs is started, stopped and set there: here its settings only show
  const managed = !!d.state.managed;
  document.getElementById('tp-managed').style.display = managed ? '' : 'none';
  document.getElementById('tp-start').disabled = active || managed;
  document.getElementById('tp-stop').disabled = !active || managed;
  ['tp-site', 'tp-block', 'tp-segment', 'tp-qber', 'tp-auto'].forEach(id => { document.getElementById(id).disabled = managed; });
  document.querySelectorAll('#tp-peers input').forEach(x => { x.disabled = managed; });
  const blocks = (d.blocks || []).slice().reverse();
  const ok = blocks.filter(b => b.ok);
  const chips = [];
  if (active) {
    chips.push(['Site', d.state.site], ['Blocks', `${d.state.block_auto ? 'automatic, up to ' : ''}${fmtDur(d.state.block_s)}, ${fmtDur(d.state.segment_s)} segments`],
      ['Segments on disk', `${d.segments}` + (d.last_segment ? `, newest ${d.last_segment.slice(9, 11)}:${d.last_segment.slice(11, 13)}:${d.last_segment.slice(13, 15)} UTC` : '')],
      ['Announcing to', d.state.announce_to.join(', ') || 'none (this node answers)'],
      ['Working on', d.running ? `${d.running.role === 'alice' ? 'offering' : 'answering'} a block with ${d.running.peer}` : (d.queued ? `${d.queued} queued` : 'waiting for the next block')]);
  }
  for (const [peer, p] of Object.entries(active ? d.state.plans || {} : {})) {
    chips.push([`Block with ${peer}`, fmtDur(p.block_s) + (p.rate != null ? `, ${fmtNum(p.rate, 1)} bit/s expected at QBER ${fmtNum(100 * p.qber, 2)}%` : ', first block')]);
  }
  for (const [peer, ppb] of Object.entries(d.skew_ppb || {})) chips.push([`Clock skew to ${peer}`, `${fmtNum(ppb, 2)} ppb`]);
  if (ok.length) chips.push(['Key in the last blocks', `${ok.slice(0, 12).reduce((a, b) => a + (b.key_bits || 0), 0)} bits in ${Math.min(12, ok.length)} blocks`]);
  document.getElementById('tp-chips').innerHTML = chipsHTML(chips);
  const al = document.getElementById('tp-alerts');
  al.style.display = d.alerts.length ? '' : 'none';
  al.innerHTML = chipsHTML(d.alerts.map(a => [a.level, a.text]));
  const utc = t => t ? new Date(t * 1000).toISOString().slice(11, 19) : '-';
  document.getElementById('tp-blocks').innerHTML = blocks.length ? blocks.slice(0, 50).map(b => `<tr>
      <td>${utc(b.block)}</td><td>${esc(b.peer)}</td><td>${b.role === 'alice' ? 'offered' : 'answered'}</td><td>${b.sifted ?? '-'}</td>
      <td>${b.qber != null ? fmtNum(100 * b.qber, 2) + '%' : '-'}</td><td>${b.phase_error_bound != null ? fmtNum(100 * b.phase_error_bound, 1) + '%' : '-'}</td>
      <td>${b.ec_leak ?? '-'}</td><td>${b.rounds ?? '-'}</td>
      <td class="${b.ok ? (b.key_bits > 0 ? 'positive' : '') : 'wrap error-cell'}">${b.ok ? b.key_bits + ' bits' : 'failed: ' + esc(b.error || '')}</td><td>${b.keys_saved ?? 0}</td>
      <td>${esc(b.aligned_by || '-')}</td><td>${fmtNum((b.finished || 0) - (b.started || 0), 1)} s</td></tr>`).join('') : emptyRow(12, 'no block yet');
  const logEl = document.getElementById('tp-log');
  logEl.textContent = (d.log || []).join('\n');
  logEl.scrollTop = logEl.scrollHeight;
}

// Link monitor: one QKD link as it runs, the same views as the TimeTagger vendor's QKD monitor, from this
// node's Live QKD blocks; channels 1-4 are H, V, D, A as importtt wires them
let monLast = null, monTab = 'overview';
const MON_CH = ['H', 'V', 'D', 'A'];
const monSum = a => (a || []).reduce((x, y) => x + (y || 0), 0);
const monClock = t => new Date(t * 1000).toISOString().slice(11, 16);

// a bar per category, its label under it, values in text ink on top
function monBars(canvasId, labels, values, color) {
  const c = document.getElementById(canvasId);
  const dpr = window.devicePixelRatio || 1, W = c.clientWidth, H = c.clientHeight;
  if (W <= 0 || H <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const L = 8, R = 8, T = 22, B = 22, n = labels.length, slot = (W - L - R) / n, w = Math.min(64, slot * 0.6);
  const vmax = Math.max(...values, 1);
  ctx.strokeStyle = CLOG_INK.grid; ctx.beginPath(); ctx.moveTo(L, H - B); ctx.lineTo(W - R, H - B); ctx.stroke();
  ctx.font = '10px "JetBrains Mono", monospace'; ctx.textAlign = 'center';
  labels.forEach((lab, i) => {
    const x = L + slot * (i + 0.5), h = (H - T - B) * (values[i] || 0) / vmax;
    ctx.fillStyle = Array.isArray(color) ? color[i] : color;
    ctx.beginPath(); ctx.roundRect(x - w / 2, H - B - h, w, Math.max(h, 1), [4, 4, 0, 0]); ctx.fill();
    ctx.fillStyle = CLOG_INK.text2; ctx.fillText(lab, x, H - 6);
    ctx.fillStyle = CLOG_INK.text; ctx.fillText(clogFmt(values[i] || 0), x, H - B - h - 6);
  });
}

// breaks a series where blocks are missing, so a stopped link shows as a gap and not a straight line
function monGaps(xs, ys, gap) {
  const ox = [], oy = [];
  xs.forEach((x, i) => {
    if (i && x - xs[i - 1] > gap) { ox.push((x + xs[i - 1]) / 2); oy.push(null); }
    ox.push(x); oy.push(ys[i]);
  });
  return { xs: ox, ys: oy };
}

function monDerive(b) {
  const secs = b.seconds || (monLast && monLast.local.block_s) || 1;
  const rate = (b.basis_checked || []).map((k, i) => (k ? (b.basis_errors || [])[i] / k : null));
  const fid = rate.length === 2 && rate[0] != null && rate[1] != null ? Math.max(0, 1 - rate[0] - rate[1]) : null;
  const local = monSum(b.singles_local), remote = monSum(b.singles_remote), sift = (b.sifted || 0) / secs;
  return {
    t: b.block, ok: !!b.ok, secs, local: b.singles_local ? local : null, remote: b.singles_remote ? remote : null,
    sift: b.ok ? sift : null, qber: b.ok && b.qber != null ? 100 * b.qber : null, fid: fid == null || !b.ok ? null : 100 * fid,
    herLocal: b.ok && local ? 100 * sift / local : null, herRemote: b.ok && remote ? 100 * sift / remote : null,
    afterEC: b.ok ? Math.max(0, (b.sifted || 0) - (b.ec_leak || 0)) / secs : null,
    afterVer: b.ok && b.surviving != null ? Math.max(0, b.surviving - (b.charged || 0)) / secs : null,
    key: b.ok ? (b.key_bits || 0) / secs : null,
  };
}

// a value per block, and its running average, minimum and maximum over the last n blocks
function monRolling(xs, ys, n = 10) {
  const avg = [], lo = [], hi = [];
  ys.forEach((_, i) => {
    const w = ys.slice(Math.max(0, i - n + 1), i + 1).filter(v => v != null);
    avg.push(w.length ? w.reduce((a, b) => a + b, 0) / w.length : null);
    lo.push(w.length ? Math.min(...w) : null);
    hi.push(w.length ? Math.max(...w) : null);
  });
  return { avg, lo, hi };
}

async function refreshMonitor(force) {
  if (document.getElementById('mon-pause').checked && !force) return;
  const sel = document.getElementById('mon-peer');
  let d;
  try { d = await apiFetch('/api/monitor' + (sel.value ? `?peer=${encodeURIComponent(sel.value)}` : '')); } catch (e) { return; }
  monLast = d;
  if (sel.options.length !== d.peers.length) {
    sel.innerHTML = d.peers.map(p => `<option value="${esc(p)}">${esc(p)}</option>`).join('') || '<option value="">no QKD link</option>';
    if (d.peer) sel.value = d.peer;
  }
  document.getElementById('mon-csv').href = `/api/monitor/blocks.csv?peer=${encodeURIComponent(d.peer || '')}`;
  document.getElementById('mon-json').href = `/api/monitor/blocks.json?peer=${encodeURIComponent(d.peer || '')}`;
  drawMonitor();
}

function monBox(id, cls, name, info) {
  const el = document.getElementById(id);
  el.className = (id === 'mon-classical' ? 'mon-link ' : 'mon-sys ') + cls;
  if (name != null) el.querySelector('.mon-sys-name') && (el.querySelector('.mon-sys-name').textContent = name);
  el.querySelector('.mon-sys-info').textContent = info;
}

function drawMonitor() {
  const d = monLast;
  if (!d) return;
  const der = d.blocks.map(monDerive);
  const ok = der.filter(x => x.ok);
  const last = d.blocks[d.blocks.length - 1], lastOk = ok[ok.length - 1];
  const okRaw = d.blocks.filter(b => b.ok), lastOkRaw = okRaw[okRaw.length - 1];
  // systems and the channel between them
  const site = d.local.site || 'no site';
  monBox('mon-local', d.local.recording ? 'ok' : 'idle', !d.node || d.node === site ? site : `${d.node}, site ${site}`,
    d.local.recording ? `TimeTagger recording\nfor ${fmtDur((Date.now() / 1000) - (d.local.since || Date.now() / 1000))}` : 'not recording');
  monBox('mon-remote', d.remote.reachable ? 'ok' : 'idle', d.remote.node || '-', d.remote.reachable ? 'key management system reachable' : (d.remote.error || 'not reachable'));
  const ann = okRaw.filter(b => b.announced_bytes).slice(-5);
  const mbit = ann.length ? ann.reduce((a, b) => a + 8 * b.announced_bytes / 1e6 / (b.role === 'alice' ? Math.max(b.announce_s || 1, 1e-3) : (b.seconds || d.local.block_s)), 0) / ann.length : null;
  monBox('mon-classical', d.remote.reachable ? 'ok' : '', null,
    d.remote.reachable ? `mutual TLS between the key servers\nround trip ${fmtNum(d.remote.rtt_ms, 1)} ms` + (mbit != null ? `\nannouncements ${fmtNum(mbit, 1)} Mbit/s, ${d.announces ? 'sent' : 'received'}` : '') : 'broken');
  // the quantum channel's stage
  document.getElementById('mon-stages').innerHTML = d.stages.map((s, i) => `<div class="mon-stage ${i < d.stage ? 'done' : ''} ${i === d.stage ? 'now' + (i === 4 ? ' good' : '') : ''}">${esc(s)}</div>`).join('');
  setBadge('mon-stage-badge', d.stages[d.stage], d.stage === 4 ? 'success' : d.stage === 3 ? 'warn' : 'muted');
  // gauges
  // sync: the share of the last segment's quarter-second slices whose peak centre the clock tracker accepted,
  // measured on the answering node; the announcing node, which has no tracker, falls back on how blocks aligned
  const syncPts = d.sync || [], sp = syncPts[syncPts.length - 1];
  const how = last && (last.aligned_by || last.aligned_by_peer);
  const sync = sp && sp.slices ? Math.round(100 * sp.locked_slices / sp.slices)
    : !last ? 0 : !last.ok ? 20 : how && how.startsWith('previous') ? 100 : 60;
  const recent = ok.slice(-10).map(x => x.key);
  const keyRate = recent.length ? recent.reduce((a, b) => a + b, 0) / recent.length : 0;
  drawGauge('mon-g-sync', sync, 100); setEl('mon-gv-sync', `${sync}%`);
  drawGauge('mon-g-fid', lastOk && lastOk.fid != null ? lastOk.fid : 0, 100); setEl('mon-gv-fid', lastOk && lastOk.fid != null ? `${fmtNum(lastOk.fid, 1)}%` : '-');
  drawGauge('mon-g-qber', lastOk && lastOk.qber != null ? lastOk.qber : 0, 100 * d.qber_threshold * 1.5); setEl('mon-gv-qber', lastOk && lastOk.qber != null ? `${fmtNum(lastOk.qber, 2)}%` : '-');
  const kmax = Math.max(...ok.map(x => x.key || 0), 1);
  drawGauge('mon-g-rate', keyRate, kmax * 1.2); setEl('mon-gv-rate', `${fmtNum(keyRate, 1)} bit/s`);
  // gaps where blocks or reads are missing break the lines: seconds for live charts, minutes for block charts
  const gap = 2.5 * (d.local.block_s || 300);
  const plot = (id, series, opts) => {
    const g = opts.xFmt && opts.xFmt(1) === '1s' ? 5 : opts.xFmt && opts.xFmt(1) === '1 min' ? gap / 60 : gap;
    clogPlot(id, series.map(sr => (sr.dashed ? sr : { ...sr, ...monGaps(sr.xs, sr.ys, g) })), opts);
  };
  // settings, pool and service, as label and value rows
  const kv = rows => rows.map(([k, v]) => `<span class="k">${esc(k)}</span><span class="v">${esc(v)}</span>`).join('');
  const win = okRaw.map(b => b.window_ps ?? b.window_ps_peer).filter(v => v != null).pop();
  const pm = (v, e, unit) => v == null ? '-' : `${fmtNum(v, 2)}${e != null ? ' ± ' + fmtNum(e, 2) : ''} ${unit}`;
  document.getElementById('mon-sync').innerHTML = kv(sp ? [
    ['Peak centres accepted', `${sp.locked_slices} of ${sp.slices} slices`],
    ['Residual of the centres', sp.residual_rms_ps != null ? `${fmtNum(sp.residual_rms_ps, 1)} ps rms` : '-'],
    ['Delay uncertainty', `± ${fmtNum(sp.delay_sd_ps, 1)} ps`],
    ['Clock skew', pm(sp.skew_ppb, sp.skew_sd_ppb, 'ppb')],
    ['Skew drift', `${fmtNum(sp.skew_drift_ppb_per_min, 3)} ppb/min`],
    ['Peak width (sigma)', sp.peak_sigma_ps != null ? `${fmtNum(sp.peak_sigma_ps, 0)} ps` : '-'],
    ['Last alignment', sp.lost ? 'lock lost, searching again' : sp.how],
  ] : [['Tracked by', 'the answering node; this node announces']]);
  document.getElementById('mon-settings').innerHTML = kv([
    ['Coincidence time window', win != null ? `±${fmtNum(win / 1e3, 2)} ns` : '-- ns'],
    ['Privacy amplification ratio', lastOkRaw && lastOkRaw.compression != null ? fmtNum(lastOkRaw.compression, 2) : '--'],
    ['Error correction', 'optimized Cascade'], ['Block', `${fmtDur(d.local.block_s)} (${fmtDur(d.local.segment_s)} segments)`],
    ['This node', d.announces ? 'announces its timing' : 'answers'], ['Blocks with key', `${d.ok_blocks} of ${d.blocks.length}`]]);
  document.getElementById('mon-pool').innerHTML = kv([['Originated here (ETSI 014)', d.pool.mine ?? '-'], ['Originated by the neighbor (ETSI 014)', d.pool.theirs ?? '-'],
    ['In all', d.pool.mine != null ? d.pool.mine + d.pool.theirs : '-']]);
  document.getElementById('mon-pool-brief').innerHTML = chipsHTML([['Originated here', d.pool.mine ?? '-'], ['Originated by the neighbor', d.pool.theirs ?? '-']]);
  setBadge('mon-settings-brief', win != null ? `window ±${fmtNum(win / 1e3, 2)} ns` : '-', 'muted');
  setBadge('mon-svc', d.local.active ? (d.local.recording ? 'running' : 'restarting') : 'stopped', d.local.active && d.local.recording ? 'success' : 'warn');
  // live charts span the last 120 s, block charts the last two hours, both counted back from now
  const now = Date.now() / 1000;
  const LOCAL = CLOG_COLORS[3], REMOTE = CLOG_COLORS[0], TOTAL = CLOG_INK.text2;
  const secAgo = v => `${Math.round(v)}s`, minAgo = v => `${Math.round(v)} min`;
  const fast = (d.fast || []).filter(p => p.t > now - 120), rfast = (d.remote_fast || []).filter(p => p.t > now - 120);
  const recentOk = ok.filter(x => x.t > now - 7200);
  const okT = recentOk.map(x => (x.t + (x.secs || 0) - now) / 60);
  const note = (id, text) => setEl('mon-n-' + id, text);
  if (monTab === 'singles') {
    const lastFast = fast[fast.length - 1], lastRemote = rfast[rfast.length - 1];
    const loc = lastFast ? lastFast.singles : [0, 0, 0, 0], rem = lastRemote ? lastRemote.singles : (lastOkRaw && lastOkRaw.singles_remote) || [0, 0, 0, 0];
    monBars('mon-singles-bars', MON_CH.concat(MON_CH), loc.concat(rem), loc.map(() => LOCAL).concat(rem.map(() => REMOTE)));
    note('singles-bars', `left four: this node (${d.local.site || '-'}); right four: ${d.peer || 'the neighbor'}` + (lastRemote ? '' : ', from the last block'));
    const pc = lastOkRaw && lastOkRaw.pair_counts;
    const PAIRS = ['HH', 'VV', 'HV', 'VH', 'DD', 'AA', 'DA', 'AD'];
    const secsP = (lastOkRaw && lastOkRaw.seconds) || d.local.block_s;
    monBars('mon-pair-bars', PAIRS, PAIRS.map(k => (pc ? (pc[k] || 0) / secsP : 0)), CLOG_COLORS[2]);
    note('pair-bars', pc ? 'verified pairs of the last block; first letter the announcing node, second the answering node'
      : 'shown on the answering node, which alone holds both bits of a pair after error correction; sending the counts would reveal how many zeros the key holds');
    const tot = fast.map(p => {
      const r = rfast.length ? rfast.reduce((best, q) => (Math.abs(q.t - p.t) < Math.abs(best.t - p.t) ? q : best)) : null;
      return r ? monSum(p.singles) + monSum(r.singles) : null;
    });
    plot('mon-singles-time', [
      { name: 'total', color: TOTAL, xs: fast.map(p => p.t - now), ys: tot },
      { name: 'singles local', color: LOCAL, xs: fast.map(p => p.t - now), ys: fast.map(p => monSum(p.singles)) },
      { name: 'singles remote', color: REMOTE, xs: rfast.map(p => p.t - now), ys: rfast.map(p => monSum(p.singles)) }],
      { xFmt: secAgo, xRange: [-120, 0], badges: true });
    plot('mon-coinc-time', [{ name: 'sifted coincidences', color: CLOG_COLORS[7], xs: okT, ys: recentOk.map(x => x.sift) }],
      { xFmt: minAgo, xRange: [-120, 0], badges: true });
    note('coinc-time', 'per block: coincidences in the window and in the same basis, as both nodes count them');
    plot('mon-herald-time', [{ name: 'heralding local', color: LOCAL, xs: okT, ys: recentOk.map(x => x.herLocal) },
      { name: 'heralding remote', color: REMOTE, xs: okT, ys: recentOk.map(x => x.herRemote) }],
      { xFmt: minAgo, xRange: [-120, 0], badges: true, yFmt: v => `${clogFmt(v)}%` });
    note('herald-time', 'sifted coincidences per single, per block');
  }
  if (monTab === 'protocol') {
    plot('mon-rates-time', [{ name: 'sifting', color: CLOG_COLORS[7], xs: okT, ys: recentOk.map(x => x.sift) },
      { name: 'error detection', color: CLOG_COLORS[1], xs: okT, ys: recentOk.map(x => x.afterEC) },
      { name: 'verification', color: CLOG_COLORS[3], xs: okT, ys: recentOk.map(x => x.afterVer) },
      { name: 'secure key', color: CLOG_COLORS[2], xs: okT, ys: recentOk.map(x => x.key) }], { xFmt: minAgo, xRange: [-120, 0], badges: true });
    note('rates-time', 'bits per second of data after each stage: sifted, left after error correction\'s disclosure, left after verification, the secure key');
    const thr = 100 * d.qber_threshold;
    plot('mon-qber-time', [{ name: 'QBER', color: CLOG_COLORS[0], xs: okT, ys: recentOk.map(x => x.qber) },
      { name: 'threshold for QKD protocol', color: CLOG_COLORS[7], dashed: true, xs: [-120, 0], ys: [thr, thr] }],
      { xFmt: minAgo, xRange: [-120, 0], badges: true, yFmt: v => `${clogFmt(v)}%` });
    plot('mon-fid-time', [{ name: 'fidelity', color: CLOG_COLORS[6], xs: okT, ys: recentOk.map(x => x.fid) }],
      { xFmt: minAgo, xRange: [-120, 0], badges: true, yFmt: v => `${clogFmt(v)}%` });
    note('fid-time', 'Bell-state fidelity lower bound 1 - QBER_HV - QBER_DA, per block');
    const sRecent = syncPts.filter(p => p.at > now - 7200), sT = sRecent.map(p => (p.at - now) / 60);
    plot('mon-sync-time', [{ name: 'residual of the peak centres', color: CLOG_COLORS[0], xs: sT, ys: sRecent.map(p => p.residual_rms_ps) },
      { name: 'delay uncertainty', color: CLOG_COLORS[2], xs: sT, ys: sRecent.map(p => p.delay_sd_ps) }],
      { xFmt: minAgo, xRange: [-120, 0], badges: true, yFmt: v => `${clogFmt(v)} ps` });
    note('sync-time', syncPts.length ? 'per segment: how far the measured peak centres sit from the tracked clock model, and how well the model knows the delay' : 'measured on the answering node of this link');
  }
  if (monTab === 'history') {
    const okAll = ok.map(x => x.t);
    const xFmtDay = v => new Date(v * 1000).toISOString().slice(5, 16).replace('T', ' ');
    plot('mon-h-singles', [{ name: 'local', color: LOCAL, xs: okAll, ys: ok.map(x => x.local) }, { name: 'remote', color: REMOTE, xs: okAll, ys: ok.map(x => x.remote) }], { xFmt: xFmtDay, badges: true });
    plot('mon-h-qber', [{ name: 'QBER', color: CLOG_COLORS[0], xs: okAll, ys: ok.map(x => x.qber) }], { xFmt: xFmtDay, badges: true, yFmt: v => `${clogFmt(v)}%` });
    plot('mon-h-rate', [{ name: 'secure key', color: CLOG_COLORS[2], xs: okAll, ys: ok.map(x => x.key) }], { xFmt: xFmtDay, badges: true });
    setEl('mon-v-h-singles', `${ok.length} blocks`); setEl('mon-v-h-qber', ''); setEl('mon-v-h-rate', `${clogFmt(monSum(ok.map(x => x.key * x.secs)))} key bits in all`);
  }
}

document.getElementById('mon-tabs').addEventListener('click', e => {
  const b = e.target.closest('.mon-tab');
  if (!b) return;
  monTab = b.dataset.tab;
  document.querySelectorAll('.mon-tab').forEach(x => x.classList.toggle('active', x === b));
  document.querySelectorAll('.mon-page').forEach(x => x.classList.toggle('active', x.dataset.tab === monTab));
  drawMonitor();
});
document.getElementById('mon-peer').addEventListener('change', () => refreshMonitor(true));
document.getElementById('mon-restart').addEventListener('click', async () => {
  const err = document.getElementById('mon-error');
  err.textContent = '';
  try {
    await postJSON('/api/twoparty/stop', {});
    await postJSON('/api/twoparty/start', {});
    refreshMonitor(true);
  } catch (e) { err.textContent = e.message; }
});
window.addEventListener('resize', drawMonitor);

// #section in the address opens that page, so a page can be bookmarked or linked; last, once everything is defined
const [startSection, startTab] = location.hash.slice(1).split('/');
if (startSection === 'monitor' && startTab) document.querySelector(`.mon-tab[data-tab="${startTab}"]`)?.click();
navigate(startSection && document.getElementById('section-' + startSection) ? startSection : 'home');