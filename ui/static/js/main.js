// qoj_post_processing dashboard

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

// Sparkline (same technique as skQCI's monitoring plots)
function sparkline(canvasId, data, color) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const pad = 8;
  const pts = (data || []).filter(v => Number.isFinite(v));
  if (pts.length === 0) {
    ctx.fillStyle = '#5A6480';
    ctx.font = '11px "JetBrains Mono", monospace';
    ctx.fillText('waiting for data...', 8, H / 2);
    return;
  }
  if (pts.length === 1) {
    // one run so far: a line needs two points, so just mark the value
    ctx.beginPath(); ctx.arc(W / 2, H / 2, 3.5, 0, Math.PI * 2);
    ctx.fillStyle = color; ctx.fill();
    ctx.font = '600 11px "JetBrains Mono", monospace';
    ctx.fillStyle = '#aab3d6'; ctx.textAlign = 'left';
    ctx.fillText(`${pts[0].toFixed(2)} (1 run so far)`, 6, H - 6);
    return;
  }
  let mn = Math.min(...pts), mx = Math.max(...pts);
  if (mn === mx) { mn -= 1; mx += 1; }
  const x = i => pad + (i / (pts.length - 1)) * (W - 2 * pad);
  const y = v => H - pad - ((v - mn) / (mx - mn)) * (H - 2 * pad);

  ctx.beginPath(); ctx.moveTo(x(0), y(pts[0]));
  pts.forEach((v, i) => ctx.lineTo(x(i), y(v)));
  ctx.lineTo(x(pts.length - 1), H - pad); ctx.lineTo(x(0), H - pad); ctx.closePath();
  const g = ctx.createLinearGradient(0, 0, 0, H);
  g.addColorStop(0, color + '55'); g.addColorStop(1, color + '00');
  ctx.fillStyle = g; ctx.fill();

  ctx.beginPath(); ctx.moveTo(x(0), y(pts[0]));
  pts.forEach((v, i) => ctx.lineTo(x(i), y(v)));
  ctx.strokeStyle = color; ctx.lineWidth = 2; ctx.lineJoin = 'round'; ctx.stroke();

  ctx.beginPath(); ctx.arc(x(pts.length - 1), y(pts[pts.length - 1]), 3, 0, Math.PI * 2);
  ctx.fillStyle = color; ctx.fill();

  ctx.font = '600 11px "JetBrains Mono", monospace';
  ctx.fillStyle = '#aab3d6'; ctx.textAlign = 'left';
  ctx.fillText(`${mn.toFixed(2)} – ${mx.toFixed(2)}`, 6, H - 6);
}

// Gauge (same arc technique as skQCI's key-pool gauge)
function drawGauge(canvasId, value, max) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
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

// Run and metrics
const METRICS_MAX_HISTORY = 30;
const history = { qber: [], key_rate: [], compression: [], leaked: [] };

const PLOTS = [
  { title: 'QBER (%)', short: 'QBER', color: '#E84B8C', key: 'qber', val: m => (m.qber * 100).toFixed(2) },
  { title: 'Final key (bits)', short: 'Key bits', color: '#2BBDE8', key: 'key_rate', val: m => m.final_key_bits },
  { title: 'Compression ratio', short: 'Compression', color: '#9B5CF5', key: 'compression', val: m => m.compression_rate.toFixed(4) },
  { title: 'Leaked bits (total)', short: 'Leaked', color: '#F59E0B', key: 'leaked', val: m => m.leaked_bits_total },
];

function renderMetrics(m) {
  const chips = document.getElementById('run-chips');
  const items = [['Sifted bits', m.sifted_bits]];
  if (m.method === 'winnow') {
    items.push(['Corrections', m.winnow_corrections], ['Leaked (syndromes)', m.winnow_leaked_bits]);
  } else if (m.method === 'ldpc') {
    items.push(['Blocks converged', `${m.ldpc_converged} / ${m.ldpc_blocks}`], ['Leaked (syndromes)', m.ldpc_leaked_bits]);
  } else {
    items.push(['Blocks kept', `${m.blocks_kept} / ${m.blocks_total}`]);
  }
  items.push(
    ['QBER', (m.qber * 100).toFixed(2) + '%'],
    ['Final key', m.final_key_bits + ' bits'],
    ['Compression', m.compression_rate.toFixed(4)],
    ['Keys saved', m.keys_saved],
    ['Elapsed', m.elapsed_ms + ' ms'],
  );
  chips.innerHTML = items.map(([k, v]) => `<div class="chip"><span class="chip-k">${k}</span><span class="chip-v">${v}</span></div>`).join('');

  PLOTS.forEach((p, i) => {
    history[p.key].push(p.val(m) * 1);
    if (history[p.key].length > METRICS_MAX_HISTORY) history[p.key].shift();
    setEl('plot-v' + i, p.val(m));
    sparkline('plot-c' + i, history[p.key], p.color);
  });

  if (m.keys_in_store != null) drawGauge('gauge-canvas', m.keys_in_store, 200);
  setEl('gauge-value', m.keys_in_store ?? '-');
}

const methodSelect = document.getElementById('f-method');
function syncMethodFields() {
  document.getElementById('method-discard').style.display = methodSelect.value === 'discard' ? '' : 'none';
  document.getElementById('method-winnow').style.display = methodSelect.value === 'winnow' ? '' : 'none';
  document.getElementById('method-ldpc').style.display = methodSelect.value === 'ldpc' ? '' : 'none';
}
methodSelect.addEventListener('change', syncMethodFields);
syncMethodFields();

document.getElementById('run-form').addEventListener('submit', async e => {
  e.preventDefault();
  const btn = document.getElementById('run-btn');
  const errEl = document.getElementById('run-error');
  errEl.textContent = '';
  btn.disabled = true;
  btn.textContent = 'Running...';
  try {
    const method = methodSelect.value;
    const body = {
      n: Number(document.getElementById('f-n').value),
      err: Number(document.getElementById('f-err').value),
      method,
      save_keystore: true,
    };
    if (method === 'winnow') {
      body.winnow_blocks = document.getElementById('f-winnow-blocks').value;
      body.winnow_sample = Number(document.getElementById('f-winnow-sample').value);
    } else if (method === 'ldpc') {
      body.ldpc_wc = Number(document.getElementById('f-ldpc-wc').value);
      body.ldpc_wr = Number(document.getElementById('f-ldpc-wr').value);
      body.ldpc_sample = Number(document.getElementById('f-ldpc-sample').value);
    } else {
      // no matrix field to send: the backend defaults -matrix to bch,
      // the only fixed matrix left, unless a searched one is requested
      body.search_matrix = document.getElementById('f-search').checked;
    }
    const m = await apiFetch('/api/run', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body) });
    renderMetrics(m);
  } catch (err) {
    errEl.textContent = err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Run pipeline';
  }
});

for (let i = 0; i < PLOTS.length; i++) sparkline('plot-c' + i, [], PLOTS[i].color);
drawGauge('gauge-canvas', 0, 200);

// Status (sidebar dot + keystore gauge on load)
async function refreshStatus() {
  const dot = document.getElementById('status-dot');
  const label = document.getElementById('status-label');
  try {
    const d = await apiFetch('/api/status');
    dot.className = 'status-dot online';
    label.textContent = 'Backend reachable';
    drawGauge('gauge-canvas', d.keys_saved, 200);
    setEl('gauge-value', d.keys_saved);
  } catch (e) {
    dot.className = 'status-dot offline';
    label.textContent = 'Backend unreachable';
  }
}
refreshStatus();
setInterval(refreshStatus, 15000);

// Dropzone: upload recordings (e.g., Trnava's, copied off the box by hand)
function setupDropzone(dropId, inputId, onFiles) {
  const drop = document.getElementById(dropId);
  const input = document.getElementById(inputId);
  if (!drop || !input) return;
  drop.addEventListener('click', () => input.click());
  drop.addEventListener('dragover', e => { e.preventDefault(); drop.classList.add('drag-over'); });
  drop.addEventListener('dragleave', () => drop.classList.remove('drag-over'));
  drop.addEventListener('drop', e => {
    e.preventDefault();
    drop.classList.remove('drag-over');
    if (e.dataTransfer.files.length) onFiles(e.dataTransfer.files);
  });
  input.addEventListener('change', () => { if (input.files.length) onFiles(input.files); });
}

function uploadRecordings(files) {
  const track = document.getElementById('upload-progress');
  const fill = document.getElementById('upload-progress-fill');
  const list = document.getElementById('upload-file-list');
  const fd = new FormData();
  for (const f of files) fd.append('files', f);

  track.classList.add('active');
  fill.style.width = '0%';

  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/upload');
  xhr.upload.addEventListener('progress', e => {
    if (e.lengthComputable) fill.style.width = ((e.loaded / e.total) * 100).toFixed(0) + '%';
  });
  xhr.onload = () => {
    track.classList.remove('active');
    try {
      const d = JSON.parse(xhr.responseText);
      const rows = d.saved.map(n => `<div class="ok">saved: ${n}</div>`)
        .concat(d.skipped.map(n => `<div class="bad">skipped (unexpected name): ${n}</div>`));
      list.innerHTML = rows.join('');
      renderRecordings(d.recordings);
    } catch (e) {
      list.innerHTML = `<div class="bad">upload failed</div>`;
    }
  };
  xhr.onerror = () => { track.classList.remove('active'); list.innerHTML = `<div class="bad">upload failed</div>`; };
  xhr.send(fd);
}
setupDropzone('rec-drop', 'rec-file-input', uploadRecordings);

function renderRecordings(files) {
  const el = document.getElementById('recordings-list');
  if (!el) return;
  el.textContent = files.length ? files.join(', ') : 'none uploaded yet';
}

// Live alignment
let alignInFlight = false;

async function refreshAlignment() {
  if (alignInFlight) return;  // full analysis can take a while; don't stack polls
  alignInFlight = true;
  const badge = document.getElementById('align-badge');
  const chips = document.getElementById('align-chips');
  badge.textContent = 'Checking...';
  badge.className = 'badge badge-muted';
  try {
    const d = await apiFetch('/api/align/live');
    if (!d.ready) {
      badge.textContent = 'Waiting for data';
      badge.className = 'badge badge-muted';
      chips.innerHTML = `<div class="chip"><span class="chip-k">Status</span><span class="chip-v">${d.reason}</span></div>`;
      return;
    }
    badge.textContent = d.detected ? 'PEAK' : 'No peak yet';
    badge.className = 'badge ' + (d.detected ? 'badge-success' : 'badge-warn');
    chips.innerHTML = [
      ['Lag', d.lag_ns.toFixed(1) + ' ns'],
      ['Window', d.window_ns.toFixed(1) + ' ns'],
      ['Score', d.score.toFixed(2)],
      ['Skew', d.skew_ppb.toFixed(2) + ' ppb'],
      ['Skew method', d.skew_method],
      ['Corrected window', d.corrected_window_ns.toFixed(2) + ' ns'],
      ['A', `${d.a} (${d.n_a.toLocaleString()})`],
      ['B', `${d.b} (${d.n_b.toLocaleString()})`],
    ].map(([k, v]) => `<div class="chip"><span class="chip-k">${k}</span><span class="chip-v">${v}</span></div>`).join('');
  } catch (e) {
    badge.textContent = 'Error';
    badge.className = 'badge badge-danger';
  } finally {
    alignInFlight = false;
  }
}
refreshAlignment();
setInterval(refreshAlignment, 5000);

// Config load + acquisition start/stop
let loadedLinks = [];

async function refreshConfig() {
  try {
    const d = await apiFetch('/api/config');
    const sel = document.getElementById('acq-link');
    const status = document.getElementById('config-status');
    if (!d.loaded) {
      status.textContent = 'No config loaded yet';
      status.className = 'badge badge-muted';
      sel.innerHTML = '';
      return;
    }
    loadedLinks = d.links;
    status.textContent = `${d.sites.length} sites, ${d.links.length} links`;
    status.className = 'badge badge-success';
    sel.innerHTML = d.links.map(l => `<option value="${l.name}">${l.name} (${l.site_a} + ${l.site_b})</option>`).join('');
    if (d.default_link) sel.value = d.default_link;
  } catch (e) { /* backend not reachable yet */ }
}
refreshConfig();

function uploadConfigFile(files) {
  const f = files[0];
  const status = document.getElementById('config-error');
  status.textContent = '';
  const fd = new FormData();
  fd.append('file', f);
  fetch('/api/config', { method: 'POST', body: fd })
    .then(r => r.json().then(d => ({ ok: r.ok, d })))
    .then(({ ok, d }) => { if (!ok) throw new Error(d.error); refreshConfig(); })
    .catch(err => { status.textContent = err.message; });
}
setupDropzone('config-drop', 'config-file-input', uploadConfigFile);

document.getElementById('acq-start').addEventListener('click', async () => {
  const errEl = document.getElementById('acq-error');
  errEl.textContent = '';
  try {
    await apiFetch('/api/acquire/start', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({
        link: document.getElementById('acq-link').value,
        duration_s: Number(document.getElementById('acq-duration').value) || 60,
      }),
    });
  } catch (e) { errEl.textContent = e.message; }
});

document.getElementById('acq-stop').addEventListener('click', async () => {
  const errEl = document.getElementById('acq-error');
  errEl.textContent = '';
  try { await apiFetch('/api/acquire/stop', { method: 'POST' }); }
  catch (e) { errEl.textContent = e.message; }
});

async function refreshAcquireStatus() {
  try {
    const d = await apiFetch('/api/acquire/status');
    const badge = document.getElementById('acq-badge');
    badge.textContent = d.running ? 'Recording' : 'Idle';
    badge.className = 'badge ' + (d.running ? 'badge-success' : 'badge-muted');
    document.getElementById('acq-start').disabled = d.running;
    document.getElementById('acq-stop').disabled = !d.running;
    document.getElementById('acq-log').textContent = d.log.join('\n');
    const logEl = document.getElementById('acq-log');
    logEl.scrollTop = logEl.scrollHeight;
    if (!d.running) refreshAlignment();
  } catch (e) { /* backend not reachable yet */ }
}
refreshAcquireStatus();
setInterval(refreshAcquireStatus, 2000);
