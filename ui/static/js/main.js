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

// Snapshot plots of coincidence_peak.py's save_plot() panels, redrawn on every check; scroll
// zooms around the cursor, drag zooms to a range, double-click resets (attachZoom below)
function drawSeriesPlot(canvasId, xs, ys, color, opts = {}) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  if (W <= 0 || H <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const pad = 8;

  if (!xs || !xs.length) {
    ctx.fillStyle = '#5A6480';
    ctx.font = '11px "JetBrains Mono", monospace';
    ctx.fillText('waiting for data...', 8, H / 2);
    return;
  }

  const fullXmn = Math.min(...xs), fullXmx = Math.max(...xs);
  let xmn = opts.xDomain ? opts.xDomain[0] : fullXmn;
  let xmx = opts.xDomain ? opts.xDomain[1] : fullXmx;
  if (xmn === xmx) { xmn -= 1; xmx += 1; }

  let vxs = xs, vys = ys;
  if (opts.xDomain) {
    const idx = [];
    xs.forEach((v, i) => { if (v >= xmn && v <= xmx) idx.push(i); });
    if (idx.length >= 2) { vxs = idx.map(i => xs[i]); vys = idx.map(i => ys[i]); }
  }

  let ymn = opts.yMin != null ? opts.yMin : Math.min(...vys);
  let ymx = Math.max(...vys);
  if (ymn === ymx) { ymn -= 1; ymx += 1; }

  const x = v => pad + ((v - xmn) / (xmx - xmn)) * (W - 2 * pad);
  const y = v => H - pad - ((v - ymn) / (ymx - ymn)) * (H - 2 * pad);

  ctx.strokeStyle = '#161632'; ctx.lineWidth = 1;
  for (let i = 1; i < 4; i++) {
    const gy = pad + (i / 4) * (H - 2 * pad);
    ctx.beginPath(); ctx.moveTo(pad, gy); ctx.lineTo(W - pad, gy); ctx.stroke();
  }

  if (opts.band) {
    const lo = Math.max(opts.band.center - opts.band.width / 2, xmn);
    const hi = Math.min(opts.band.center + opts.band.width / 2, xmx);
    if (hi > lo) {
      ctx.fillStyle = color + '26';
      ctx.fillRect(x(lo), pad, Math.max(1, x(hi) - x(lo)), H - 2 * pad);
    }
  }

  ctx.beginPath(); ctx.moveTo(x(vxs[0]), y(vys[0]));
  vxs.forEach((v, i) => ctx.lineTo(x(v), y(vys[i])));
  ctx.lineTo(x(vxs[vxs.length - 1]), H - pad); ctx.lineTo(x(vxs[0]), H - pad); ctx.closePath();
  const g = ctx.createLinearGradient(0, 0, 0, H);
  g.addColorStop(0, color + '55'); g.addColorStop(1, color + '00');
  ctx.fillStyle = g; ctx.fill();

  ctx.beginPath(); ctx.moveTo(x(vxs[0]), y(vys[0]));
  vxs.forEach((v, i) => ctx.lineTo(x(v), y(vys[i])));
  ctx.strokeStyle = color; ctx.lineWidth = 1.6; ctx.lineJoin = 'round'; ctx.stroke();

  if (opts.markerX != null && opts.markerX >= xmn && opts.markerX <= xmx) {
    ctx.beginPath(); ctx.moveTo(x(opts.markerX), pad); ctx.lineTo(x(opts.markerX), H - pad);
    ctx.strokeStyle = '#EDF0FF'; ctx.setLineDash([3, 3]); ctx.lineWidth = 1; ctx.stroke();
    ctx.setLineDash([]);
  }

  if (opts.hoverX != null && opts.hoverX >= xmn && opts.hoverX <= xmx) {
    let nearest = 0, best = Infinity;
    vxs.forEach((v, i) => { const dd = Math.abs(v - opts.hoverX); if (dd < best) { best = dd; nearest = i; } });
    const hx = x(vxs[nearest]), hy = y(vys[nearest]);
    ctx.beginPath(); ctx.moveTo(hx, pad); ctx.lineTo(hx, H - pad);
    ctx.strokeStyle = '#5A6480'; ctx.lineWidth = 1; ctx.stroke();
    ctx.beginPath(); ctx.arc(hx, hy, 3, 0, Math.PI * 2);
    ctx.fillStyle = '#fff'; ctx.fill();
    const label = `${vxs[nearest].toFixed(2)}${opts.xUnit || ''}, ${vys[nearest].toFixed(2)}`;
    ctx.font = '600 10px "JetBrains Mono", monospace';
    const tw = ctx.measureText(label).width;
    const lx = Math.min(Math.max(hx - tw / 2, pad), W - pad - tw);
    ctx.fillStyle = 'rgba(6,6,15,0.85)'; ctx.fillRect(lx - 4, pad, tw + 8, 14);
    ctx.fillStyle = '#EDF0FF'; ctx.textAlign = 'left';
    ctx.fillText(label, lx, pad + 10);
  }

  if (opts.selectionPx) {
    const [a, b] = opts.selectionPx;
    ctx.fillStyle = 'rgba(155,92,245,0.18)';
    ctx.fillRect(Math.min(a, b), pad, Math.abs(b - a), H - 2 * pad);
    ctx.strokeStyle = 'rgba(155,92,245,0.6)'; ctx.lineWidth = 1;
    ctx.strokeRect(Math.min(a, b), pad, Math.abs(b - a), H - 2 * pad);
  }

  ctx.font = '600 11px "JetBrains Mono", monospace';
  ctx.fillStyle = '#aab3d6'; ctx.textAlign = 'left';
  ctx.fillText(`${xmn.toFixed(2)}${opts.xUnit || ''} – ${xmx.toFixed(2)}${opts.xUnit || ''}`, 6, H - 6);
  if (opts.xDomain) {
    ctx.textAlign = 'right'; ctx.fillStyle = '#9B5CF5';
    ctx.fillText('zoomed · dbl-click to reset', W - 6, H - 6);
    ctx.textAlign = 'left';
  }
}

function drawDriftPlot(canvasId, segX, segY, fitY, color, opts = {}) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const dpr = window.devicePixelRatio || 1;
  const W = c.clientWidth, H = c.clientHeight;
  if (W <= 0 || H <= 0) return;
  c.width = W * dpr; c.height = H * dpr;
  const ctx = c.getContext('2d');
  ctx.setTransform(dpr, 0, 0, dpr, 0, 0);
  ctx.clearRect(0, 0, W, H);
  const pad = 8;
  if (!segX || !segX.length) {
    ctx.fillStyle = '#5A6480';
    ctx.font = '11px "JetBrains Mono", monospace';
    ctx.fillText('no segment data (skew method: grid search / none)', 8, H / 2);
    return;
  }

  const fullXmn = Math.min(...segX), fullXmx = Math.max(...segX);
  let xmn = opts.xDomain ? opts.xDomain[0] : fullXmn;
  let xmx = opts.xDomain ? opts.xDomain[1] : fullXmx;
  if (xmn === xmx) { xmn -= 1; xmx += 1; }

  let vx = segX, vy = segY, vf = fitY;
  if (opts.xDomain) {
    const idx = [];
    segX.forEach((v, i) => { if (v >= xmn && v <= xmx) idx.push(i); });
    if (idx.length >= 1) { vx = idx.map(i => segX[i]); vy = idx.map(i => segY[i]); vf = idx.map(i => fitY[i]); }
  }

  let ymn = Math.min(...vy, ...vf), ymx = Math.max(...vy, ...vf);
  if (ymn === ymx) { ymn -= 1; ymx += 1; }
  const x = v => pad + ((v - xmn) / (xmx - xmn)) * (W - 2 * pad);
  const y = v => H - pad - ((v - ymn) / (ymx - ymn)) * (H - 2 * pad);

  ctx.strokeStyle = '#161632'; ctx.lineWidth = 1;
  for (let i = 1; i < 4; i++) {
    const gy = pad + (i / 4) * (H - 2 * pad);
    ctx.beginPath(); ctx.moveTo(pad, gy); ctx.lineTo(W - pad, gy); ctx.stroke();
  }

  ctx.beginPath(); ctx.moveTo(x(vx[0]), y(vf[0]));
  vx.forEach((v, i) => ctx.lineTo(x(v), y(vf[i])));
  ctx.strokeStyle = color; ctx.lineWidth = 1.6; ctx.stroke();

  vx.forEach((v, i) => {
    ctx.beginPath(); ctx.arc(x(v), y(vy[i]), 3, 0, Math.PI * 2);
    ctx.fillStyle = '#EDF0FF'; ctx.fill();
    ctx.lineWidth = 1; ctx.strokeStyle = color; ctx.stroke();
  });

  if (opts.hoverX != null && opts.hoverX >= xmn && opts.hoverX <= xmx) {
    let nearest = 0, best = Infinity;
    vx.forEach((v, i) => { const dd = Math.abs(v - opts.hoverX); if (dd < best) { best = dd; nearest = i; } });
    const hx = x(vx[nearest]);
    ctx.beginPath(); ctx.moveTo(hx, pad); ctx.lineTo(hx, H - pad);
    ctx.strokeStyle = '#5A6480'; ctx.lineWidth = 1; ctx.stroke();
    const label = `${vx[nearest].toFixed(2)}s, ${vy[nearest].toFixed(2)}ns`;
    ctx.font = '600 10px "JetBrains Mono", monospace';
    const tw = ctx.measureText(label).width;
    const lx = Math.min(Math.max(hx - tw / 2, pad), W - pad - tw);
    ctx.fillStyle = 'rgba(6,6,15,0.85)'; ctx.fillRect(lx - 4, pad, tw + 8, 14);
    ctx.fillStyle = '#EDF0FF'; ctx.textAlign = 'left';
    ctx.fillText(label, lx, pad + 10);
  }

  if (opts.selectionPx) {
    const [a, b] = opts.selectionPx;
    ctx.fillStyle = 'rgba(155,92,245,0.18)';
    ctx.fillRect(Math.min(a, b), pad, Math.abs(b - a), H - 2 * pad);
    ctx.strokeStyle = 'rgba(155,92,245,0.6)'; ctx.lineWidth = 1;
    ctx.strokeRect(Math.min(a, b), pad, Math.abs(b - a), H - 2 * pad);
  }

  ctx.font = '600 11px "JetBrains Mono", monospace';
  ctx.fillStyle = '#aab3d6'; ctx.textAlign = 'left';
  ctx.fillText(`${xmn.toFixed(1)}s – ${xmx.toFixed(1)}s`, 6, H - 6);
  if (opts.xDomain) {
    ctx.textAlign = 'right'; ctx.fillStyle = '#9B5CF5';
    ctx.fillText('zoomed · dbl-click to reset', W - 6, H - 6);
    ctx.textAlign = 'left';
  }
}

// Zoom state per canvas id: null (full range) or [lo, hi] in that plot's own x units,
// attached once per canvas by attachZoom()
const _zoomState = {};
function _clamp(v, lo, hi) { return Math.max(lo, Math.min(hi, v)); }

function attachZoom(canvasId, getFullDomain, drawFn) {
  const c = document.getElementById(canvasId);
  if (!c) return;
  const pad = 8;
  let dragStart = null;

  c.addEventListener('wheel', e => {
    const full = getFullDomain();
    if (!full) return;
    e.preventDefault();
    const rect = c.getBoundingClientRect();
    const px = e.clientX - rect.left;
    const cur = _zoomState[canvasId] || full;
    const frac = _clamp((px - pad) / (rect.width - 2 * pad), 0, 1);
    const atX = cur[0] + frac * (cur[1] - cur[0]);
    const factor = e.deltaY > 0 ? 1.3 : 0.7;
    let lo = atX - (atX - cur[0]) * factor;
    let hi = atX + (cur[1] - atX) * factor;
    lo = Math.max(lo, full[0]); hi = Math.min(hi, full[1]);
    if (hi <= lo) return;
    _zoomState[canvasId] = (lo <= full[0] + 1e-12 && hi >= full[1] - 1e-12) ? null : [lo, hi];
    drawFn();
  }, { passive: false });

  c.addEventListener('mousedown', e => {
    const rect = c.getBoundingClientRect();
    dragStart = e.clientX - rect.left;
  });
  c.addEventListener('mousemove', e => {
    const rect = c.getBoundingClientRect();
    const px = e.clientX - rect.left;
    if (dragStart != null) {
      drawFn(null, [dragStart, px]);
      return;
    }
    const full = getFullDomain();
    if (!full) return;
    const cur = _zoomState[canvasId] || full;
    const frac = _clamp((px - pad) / (rect.width - 2 * pad), 0, 1);
    drawFn(cur[0] + frac * (cur[1] - cur[0]));
  });
  c.addEventListener('mouseleave', () => { if (dragStart == null) drawFn(); });
  window.addEventListener('mouseup', e => {
    if (dragStart == null) return;
    const rect = c.getBoundingClientRect();
    const px = _clamp(e.clientX - rect.left, 0, rect.width);
    const full = getFullDomain();
    if (full && Math.abs(px - dragStart) > 6) {
      const cur = _zoomState[canvasId] || full;
      const f1 = _clamp((Math.min(dragStart, px) - pad) / (rect.width - 2 * pad), 0, 1);
      const f2 = _clamp((Math.max(dragStart, px) - pad) / (rect.width - 2 * pad), 0, 1);
      const lo = cur[0] + f1 * (cur[1] - cur[0]);
      const hi = cur[0] + f2 * (cur[1] - cur[0]);
      if (hi > lo) _zoomState[canvasId] = [lo, hi];
    }
    dragStart = null;
    drawFn();
  });
  c.addEventListener('dblclick', () => { _zoomState[canvasId] = null; drawFn(); });
}

// Per-canvas draw wrappers over the last alignment data and the canvas's zoom state, so the
// poll and every zoom or hover redraw from the same source
function _alignXs(canvasId) {
  const d = lastAlignData;
  if (!d || !d.plots) return null;
  switch (canvasId) {
    case 'align-plot-c0': return d.plots.coarse.x_ms;
    case 'align-plot-c1': return d.plots.fine.x_ns;
    case 'align-plot-c2': return d.plots.drift.seg_x_s;
    case 'align-plot-c3': return d.plots.corrected.x_ns;
    default: return null;
  }
}
function _alignFullDomain(canvasId) {
  const xs = _alignXs(canvasId);
  if (!xs || !xs.length) return null;
  return [Math.min(...xs), Math.max(...xs)];
}

function drawAlignCoarse(hoverX, selectionPx) {
  const d = lastAlignData;
  if (!d || !d.plots) return drawSeriesPlot('align-plot-c0', [], []);
  drawSeriesPlot('align-plot-c0', d.plots.coarse.x_ms, d.plots.coarse.y, '#2BBDE8', {
    markerX: d.lag_ns / 1e6, xDomain: _zoomState['align-plot-c0'], hoverX, selectionPx, xUnit: 'ms',
  });
}
function drawAlignFine(hoverX, selectionPx) {
  const d = lastAlignData;
  if (!d || !d.plots) return drawSeriesPlot('align-plot-c1', [], []);
  const f = d.plots.fine;
  drawSeriesPlot('align-plot-c1', f.x_ns, f.y, '#9B5CF5', {
    band: { center: f.window_center_ns, width: f.window_width_ns }, yMin: 0,
    xDomain: _zoomState['align-plot-c1'], hoverX, selectionPx, xUnit: 'ns',
  });
}
function drawAlignDrift(hoverX, selectionPx) {
  const d = lastAlignData;
  if (!d || !d.plots) return drawDriftPlot('align-plot-c2', [], [], [], '#F59E0B');
  const dr = d.plots.drift;
  drawDriftPlot('align-plot-c2', dr.seg_x_s, dr.seg_y_ns, dr.fit_y_ns, '#F59E0B', {
    xDomain: _zoomState['align-plot-c2'], hoverX, selectionPx,
  });
}
function drawAlignCorrected(hoverX, selectionPx) {
  const d = lastAlignData;
  if (!d || !d.plots) return drawSeriesPlot('align-plot-c3', [], []);
  const cw = d.plots.corrected;
  drawSeriesPlot('align-plot-c3', cw.x_ns, cw.y, '#22C55E', {
    band: { center: cw.window_center_ns, width: cw.window_width_ns }, yMin: 0,
    xDomain: _zoomState['align-plot-c3'], hoverX, selectionPx, xUnit: 'ns',
  });
}

attachZoom('align-plot-c0', () => _alignFullDomain('align-plot-c0'), drawAlignCoarse);
attachZoom('align-plot-c1', () => _alignFullDomain('align-plot-c1'), drawAlignFine);
attachZoom('align-plot-c2', () => _alignFullDomain('align-plot-c2'), drawAlignDrift);
attachZoom('align-plot-c3', () => _alignFullDomain('align-plot-c3'), drawAlignCorrected);

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
  document.querySelectorAll('.nav-item').forEach(el => el.classList.toggle('active', el.dataset.section === section));
  document.querySelectorAll('.section').forEach(el => el.classList.toggle('active', el.id === 'section-' + section));
  stopPolls();
  if (section === 'home') { redrawHomeGauge(); refreshNode(); }
  else if (section === 'run') redrawRunPlots();
  else if (section === 'align') { refreshRecordings().then(() => { redrawAlignPlots(); poll('align', refreshAlignment, 5000); }); }
  else if (section === 'extract') { refreshRecordings(); refreshNode(); poll('jobs', refreshJobs, 2000); }
  else if (section === 'network') { poll('node', refreshNode, 10000); refreshAudit(); }
  else if (section === 'continuous') { refreshConfigLinks(); refreshRecordings(); refreshNode(); poll('continuous', refreshContinuous, 1500); }
  else if (section === 'evaluate') { refreshRecordings().then(renderEvalPairs); refreshEvaluations(); poll('evaljob', refreshEvalJob, 2000); }
  else if (section === 'chsh') refreshRecordings();
  else if (section === 'acquire') { refreshRecordings(); poll('acquire', refreshAcquireStatus, 2000); poll('live', refreshLive, 1000); }
  else if (section === 'recordings') refreshRecordings();
}

document.getElementById('main-nav').addEventListener('click', e => {
  const item = e.target.closest('.nav-item');
  if (item) { e.preventDefault(); navigate(item.dataset.section); }
});
document.querySelectorAll('.nav-card').forEach(card => card.addEventListener('click', e => { e.preventDefault(); navigate(card.dataset.section); }));

// One result renderer for any -json report, simulated or real
function metricItems(m) {
  const items = [['Sifted bits', m.sifted_bits]];
  if (m.filter_window_ps) {
    items.push(['Window (fit HWHM)', `±${m.filter_window_ps} ps`],
      ['Predicted accidentals', ((m.filter_predicted_accidental_fraction || 0) * 100).toFixed(1) + '%']);
  }
  if (m.method === 'ldpc5g') items.push(['5G NR frames', `${m.ldpc5g_frames || 0} (${m.ldpc5g_failed || 0} failed, ${m.ldpc5g_attempts || 0} attempts)`], ['Bits corrected', m.ldpc5g_bits_corrected || 0], ['Leaked (syndromes, tags)', m.ldpc5g_leaked_bits]);
  else if (m.method === 'polar') items.push(['Polar blocks', m.polar_blocks], ['Bits corrected', m.polar_bits_corrected], ['Leaked (frozen values)', m.polar_leaked_bits]);
  else if (m.method === 'winnow') items.push(['Corrections', m.winnow_corrections], ['Leaked (syndromes)', m.winnow_leaked_bits]);
  else if (m.method === 'ldpc') items.push(['Blocks converged', `${m.ldpc_converged || 0} / ${m.ldpc_blocks}`], ['Leaked (syndromes)', m.ldpc_leaked_bits]);
  else items.push(['Blocks kept', `${m.blocks_kept} / ${m.blocks_total}`]);
  items.push(['QBER', (m.qber * 100).toFixed(2) + '%']);
  const fk = m.finite_key;
  if (fk) {
    items.push(['Phase-error bound', (fk.phase_error_bound * 100).toFixed(2) + '%'],
      ['Correctness check', fk.surviving_bits === 0 ? 'nothing survived' : fk.verified ? 'hash match' : 'MISMATCH, no key']);
  } else {
    items.push(['Key length', 'asymptotic (no finite-size guarantee)']);
  }
  items.push(['Final key', m.final_key_bits + ' bits'], ['Compression', (m.compression_rate || 0).toFixed(4)], ['Keys saved', m.keys_saved]);
  return items;
}

// Simulator
const METRICS_MAX_HISTORY = 30;
const history = { qber: [], key_rate: [], compression: [], leaked: [] };
let lastMetrics = null;
const PLOTS = [
  { color: '#E84B8C', key: 'qber', val: m => (m.qber * 100).toFixed(2) },
  { color: '#2BBDE8', key: 'key_rate', val: m => m.final_key_bits },
  { color: '#9B5CF5', key: 'compression', val: m => m.compression_rate.toFixed(4) },
  { color: '#F59E0B', key: 'leaked', val: m => m.leaked_bits_total },
];

function redrawRunPlots() {
  PLOTS.forEach((p, i) => {
    sparkline('plot-c' + i, history[p.key], p.color);
    if (lastMetrics) setEl('plot-v' + i, p.val(lastMetrics));
  });
}

const methodSelect = document.getElementById('f-method');
function syncMethodFields() {
  for (const name of ['cascade', 'ldpc5g', 'polar', 'ldpc', 'winnow', 'discard']) {
    document.getElementById('method-' + name).style.display = methodSelect.value === name ? '' : 'none';
  }
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
      n: Number(document.getElementById('f-n').value), err: Number(document.getElementById('f-err').value),
      method, asymptotic: document.getElementById('f-asymptotic').checked,
    };
    if (method === 'polar') {
      body.polar_margin = Number(document.getElementById('f-polar-margin').value);
      body.polar_list = Number(document.getElementById('f-polar-list').value);
    } else if (method === 'winnow') {
      body.winnow_blocks = document.getElementById('f-winnow-blocks').value;
    } else if (method === 'ldpc') {
      body.ldpc_wc = Number(document.getElementById('f-ldpc-wc').value);
      body.ldpc_wr = Number(document.getElementById('f-ldpc-wr').value);
    } else {
      body.search_matrix = document.getElementById('f-search').checked;
    }
    const m = await postJSON('/api/run', body);
    document.getElementById('run-chips').innerHTML = chipsHTML([...metricItems(m), ['Elapsed', m.elapsed_ms + ' ms']]);
    PLOTS.forEach(p => history[p.key].push(p.val(m) * 1) > METRICS_MAX_HISTORY && history[p.key].shift());
    lastMetrics = m;
    redrawRunPlots();
  } catch (err) {
    errEl.textContent = err.message;
  } finally {
    btn.disabled = false;
    btn.textContent = 'Run simulation';
  }
});

// Status: backend, KMS and the running job, always polled
let poolKeys = 0;
let lastNode = null;

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
    setEl('job-label', d.job ? `Job ${d.job.id} (${d.job.kind}) ${d.job.status}` : 'No job running');
    if (!d.kms) {
      document.getElementById('kms-dot').className = 'status-dot offline';
      setEl('kms-label', 'No KMS configured');
    }
  } catch (e) {
    document.getElementById('status-dot').className = 'status-dot offline';
    setEl('status-label', 'Dashboard unreachable');
  }
}
refreshStatus();
setInterval(refreshStatus, 10000);

// Network and KMS
async function refreshNode() {
  try {
    const n = await apiFetch('/api/node');
    lastNode = n;
    document.getElementById('kms-dot').className = 'status-dot online';
    poolKeys = (n.links || []).reduce((s, l) => s + l.mine + l.theirs, 0);
    setEl('kms-label', `KMS: ${poolKeys} pool keys`);
    renderNode(n);
  } catch (e) {
    lastNode = null;
    document.getElementById('kms-dot').className = 'status-dot offline';
    setEl('kms-label', 'KMS unreachable');
    setBadge('net-badge', e.message, 'danger');
    document.getElementById('home-node-chips').innerHTML = chipsHTML([['KMS', e.message]]);
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
  const tpSel = document.getElementById('tp-peer');
  const keepPeer = tpSel.value;
  tpSel.innerHTML = links.map(l => `<option value="${esc(l.neighbor)}">${esc(l.neighbor)}${l.reachable === false ? ' (unreachable)' : ''}</option>`).join('');
  if (keepPeer) tpSel.value = keepPeer;
  const linkSel = document.getElementById('extract-link');
  const keepLink = linkSel.value;
  linkSel.innerHTML = links.map(l => `<option value="${esc(l.neighbor)}">${esc(n.node_id)}-${esc(l.neighbor)}</option>`).join('') +
    '<option value="">do not save the key</option>';
  if (keepLink !== undefined && [...linkSel.options].some(o => o.value === keepLink)) linkSel.value = keepLink;
  const contSel = document.getElementById('cont-neighbor');
  const keepCont = contSel.value;
  contSel.innerHTML = links.map(l => `<option value="${esc(l.neighbor)}">${esc(n.node_id)}-${esc(l.neighbor)}</option>`).join('') +
    '<option value="">do not save the key</option>';
  if ([...contSel.options].some(o => o.value === keepCont)) contSel.value = keepCont;
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

const extractPairs = [];  // chosen pairs, [{a, b, checked}]

function renderExtractPairs() {
  for (const p of recordings.pairs) {
    if (!extractPairs.some(q => q.a === p.a && q.b === p.b)) extractPairs.push({ a: p.a, b: p.b, checked: false });
  }
  const tb = document.getElementById('extract-pairs');
  tb.innerHTML = extractPairs.length ? extractPairs.map((p, i) => `<tr>
      <td><input type="checkbox" data-pair="${i}" ${p.checked ? 'checked' : ''}></td>
      <td class="mono">${esc(p.a.replace('_timestamp_sequence.npy', ''))}</td>
      <td class="mono">${esc(p.b.replace('_timestamp_sequence.npy', ''))}</td>
    </tr>`).join('') : emptyRow(3, 'no recording pairs yet: upload or record, or pair two by hand below');
}
document.getElementById('extract-pairs').addEventListener('change', e => {
  const i = e.target.dataset.pair;
  if (i !== undefined) extractPairs[i].checked = e.target.checked;
});
document.getElementById('extract-add-btn').addEventListener('click', () => {
  const a = document.getElementById('extract-add-a').value, b = document.getElementById('extract-add-b').value;
  if (!a || !b || a === b) return;
  const found = extractPairs.find(q => q.a === a && q.b === b);
  if (found) found.checked = true; else extractPairs.push({ a, b, checked: true });
  renderExtractPairs();
});

async function refreshRecordings() {
  try {
    recordings = await apiFetch('/api/recordings');
  } catch (e) { return; }
  fillPairPickers();
  renderExtractPairs();
  renderTwoPartyRecordings();
  const badge = document.getElementById('recordings-usage-badge');
  const cap = recordings.max_gb > 0 ? recordings.max_gb * 1024 ** 3 : null;
  badge.textContent = formatBytes(recordings.total_bytes) + (cap ? ` / ${recordings.max_gb} GB cap` : ' (no cap set)');
  badge.className = 'badge ' + (cap && recordings.total_bytes > cap * 0.9 ? 'badge-warn' : 'badge-muted');
  document.getElementById('recordings-table-body').innerHTML = recordings.items.length ? recordings.items.map(it => `<tr>
      <td class="mono">${esc(it.name)}</td><td>${formatBytes(it.bytes)}</td>
      <td><button class="btn btn-outline" style="padding:.3rem .6rem;font-size:.72rem" data-delete-recording="${esc(it.base)}">Delete</button></td>
    </tr>`).join('') : emptyRow(3, 'none uploaded yet');
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

setupDropzone('rec-drop', 'rec-file-input', files => {
  const track = document.getElementById('upload-progress');
  const fill = document.getElementById('upload-progress-fill');
  const list = document.getElementById('upload-file-list');
  const fd = new FormData();
  for (const f of files) fd.append('files', f);
  track.classList.add('active');
  fill.style.width = '0%';
  const xhr = new XMLHttpRequest();
  xhr.open('POST', '/api/upload');
  xhr.upload.addEventListener('progress', e => { if (e.lengthComputable) fill.style.width = ((e.loaded / e.total) * 100).toFixed(0) + '%'; });
  xhr.onload = () => {
    track.classList.remove('active');
    try {
      const d = JSON.parse(xhr.responseText);
      list.innerHTML = d.saved.map(n => `<div class="ok">saved: ${esc(n)}</div>`)
        .concat(d.skipped.map(n => `<div class="bad">skipped (unexpected name): ${esc(n)}</div>`)).join('');
      refreshRecordings();
    } catch (e) { list.innerHTML = '<div class="bad">upload failed</div>'; }
  };
  xhr.onerror = () => { track.classList.remove('active'); list.innerHTML = '<div class="bad">upload failed</div>'; };
  xhr.send(fd);
});

document.getElementById('recordings-table-body').addEventListener('click', async e => {
  const btn = e.target.closest('[data-delete-recording]');
  if (!btn) return;
  const base = btn.dataset.deleteRecording;
  if (!confirm(`Delete recording "${base}"? This removes the raw TimeTagger files from disk and cannot be undone.`)) return;
  btn.disabled = true;
  try {
    await apiFetch(`/api/recordings/${encodeURIComponent(base)}`, { method: 'DELETE' });
    refreshRecordings();
  } catch (err) { alert('Delete failed: ' + err.message); btn.disabled = false; }
});

document.getElementById('recordings-delete-all-btn').addEventListener('click', async () => {
  if (!confirm('Delete ALL stored recordings? This removes every raw TimeTagger file from disk and cannot be undone.')) return;
  try {
    await apiFetch('/api/recordings', { method: 'DELETE' });
    refreshRecordings();
  } catch (err) { alert('Delete all failed: ' + err.message); }
});

// Alignment: the four panels coincidence_peak.py's save_plot() draws, for the chosen pair
let alignInFlight = false;
let lastAlignData = null;

function redrawAlignPlots() {
  const d = lastAlignData;
  drawAlignCoarse(); drawAlignFine(); drawAlignDrift(); drawAlignCorrected();
  if (!d || !d.plots) return;
  setEl('align-plot-v0', Math.max(...d.plots.coarse.y).toFixed(2));
  setEl('align-plot-v1', d.window_ns.toFixed(2) + ' ns');
  setEl('align-plot-v2', d.skew_ppb.toFixed(2) + ' ppb');
  setEl('align-plot-v3', d.corrected_window_ns.toFixed(2) + ' ns');
}

async function refreshAlignment() {
  if (alignInFlight) return;
  alignInFlight = true;
  const a = document.getElementById('align-a').value, b = document.getElementById('align-b').value;
  setBadge('align-badge', 'Checking...');
  try {
    const d = await apiFetch(`/api/align/live?a=${encodeURIComponent(a)}&b=${encodeURIComponent(b)}`);
    if (!d.ready) {
      setBadge('align-badge', 'Waiting for data');
      document.getElementById('align-chips').innerHTML = chipsHTML([['Status', d.reason]]);
      return;
    }
    setBadge('align-badge', d.detected ? 'PEAK' : 'No peak', d.detected ? 'success' : 'warn');
    document.getElementById('align-chips').innerHTML = chipsHTML([
      ['Lag', d.lag_ns.toFixed(1) + ' ns'], ['Window', d.window_ns.toFixed(1) + ' ns'], ['Score', d.score.toFixed(2)],
      ['Skew', d.skew_ppb.toFixed(2) + ' ppb'], ['Skew method', d.skew_method],
      ['Corrected window', d.corrected_window_ns.toFixed(2) + ' ns'],
      ['A events', d.n_a.toLocaleString()], ['B events', d.n_b.toLocaleString()],
    ]);
    if (!lastAlignData || lastAlignData.a !== d.a || lastAlignData.b !== d.b) {
      for (const k of Object.keys(_zoomState)) _zoomState[k] = null;
    }
    lastAlignData = d;
    redrawAlignPlots();
  } catch (e) {
    setBadge('align-badge', 'Error', 'danger');
  } finally {
    alignInFlight = false;
  }
}
for (const id of ['align-a', 'align-b']) document.getElementById(id).addEventListener('change', refreshAlignment);

// Key extraction and calibration jobs
let shownJob = null;

function extractParams() {
  const method = document.getElementById('extract-method').value;
  const p = {
    pairs: extractPairs.filter(q => q.checked).map(q => ({ a: q.a, b: q.b })),
    method,
    filter: document.getElementById('extract-filter').checked,
    asymptotic: document.getElementById('extract-asymptotic').checked,
    delays: document.getElementById('extract-delays').checked,
    link: document.getElementById('extract-link').value,
  };
  if (document.getElementById('extract-soft').checked && ['ldpc5g', 'polar', 'ldpc'].includes(method)) p.soft_bins = 16;
  if (p.asymptotic) p.link = '';  // an asymptotic length certifies nothing, so it never becomes key
  return p;
}

async function startJob(kind) {
  const err = document.getElementById('extract-error');
  err.textContent = '';
  const params = extractParams();
  if (!params.pairs.length) { err.textContent = 'tick at least one recording pair'; return; }
  try {
    const job = await postJSON('/api/jobs', { kind, params });
    shownJob = job.id;
    renderJob(job);
    refreshJobs();
  } catch (e) { err.textContent = e.message; }
}
document.getElementById('extract-run-btn').addEventListener('click', () => startJob('extract'));

// Two-party extraction: this node's own recordings, announced to one neighbor
const tpChosen = new Set();
function renderTwoPartyRecordings() {
  const tb = document.getElementById('tp-recordings');
  tb.innerHTML = recordings.files.length ? recordings.files.map(f => `<tr>
      <td><input type="checkbox" data-tp="${esc(f)}" ${tpChosen.has(f) ? 'checked' : ''}></td>
      <td class="mono">${esc(f.replace('_timestamp_sequence.npy', ''))}</td></tr>`).join('') : emptyRow(2, 'no recordings yet');
}
document.getElementById('tp-recordings').addEventListener('change', e => {
  const f = e.target.dataset.tp;
  if (f) { if (e.target.checked) tpChosen.add(f); else tpChosen.delete(f); }
});
document.getElementById('tp-start-btn').addEventListener('click', async () => {
  const err = document.getElementById('tp-error');
  err.textContent = '';
  const p = extractParams();
  const params = { peer: document.getElementById('tp-peer').value, recordings: [...tpChosen], filter: p.filter, asymptotic: p.asymptotic, delays: p.delays };
  if (p.soft_bins) params.soft_bins = p.soft_bins;
  params.method = p.method === 'polar' ? 'polar' : 'ldpc5g';
  if (p.method !== params.method) { err.textContent = 'two-party extraction runs 5G NR LDPC or polar codes'; return; }
  if (!params.peer) { err.textContent = 'this node has no QKD link'; return; }
  if (!params.recordings.length) { err.textContent = "tick this node's own recordings"; return; }
  try {
    const job = await postJSON('/api/jobs', { kind: 'twoparty-alice', params });
    shownJob = job.id;
    renderJob(job);
    refreshJobs();
  } catch (e) { err.textContent = e.message; }
});
document.getElementById('extract-calibrate-btn').addEventListener('click', () => startJob('calibrate'));

function renderJob(job) {
  setEl('job-title', job.id);
  const kinds = { queued: 'muted', running: 'warn', done: 'success', failed: 'danger' };
  let status = job.status;
  if (job.status === 'done' && (job.kind === 'extract' || job.kind.startsWith('twoparty')) && job.result) {
    status = job.result.final_key_bits > 0 ? (job.result.link ? `key handed to link ${NODE()}-${job.result.link}` : 'key extracted, not saved') : 'no key (too few pairs or QBER too high)';
  }
  setBadge('job-badge', status, kinds[job.status]);
  const items = [['Kind', job.kind], ['Started', fmtTime(job.started)]];
  if (job.kind.startsWith('twoparty')) items.push(['Neighbor', job.params.peer], ['Session', job.params.session || '-'], ['Recordings', (job.params.recordings || []).length]);
  else items.push(['Pairs', (job.params.pairs || []).length]);
  if (job.result && (job.kind === 'extract' || job.kind.startsWith('twoparty'))) items.push(...metricItems(job.result));
  if (job.error) items.push(['Error', job.error]);
  document.getElementById('job-chips').innerHTML = chipsHTML(items);
  const log = document.getElementById('job-log');
  log.textContent = (job.log || []).join('\n');
  log.scrollTop = log.scrollHeight;
}

function NODE() { return lastNode ? lastNode.node_id : ''; }

async function refreshJobs() {
  try {
    const d = await apiFetch('/api/jobs');
    setEl('extract-delays-state', d.delays_calibrated ? '(calibrated)' : '(not calibrated yet)');
    document.getElementById('jobs-table').innerHTML = d.jobs.length ? d.jobs.map(j => `<tr data-job="${esc(j.id)}" style="cursor:pointer">
        <td>${esc(j.id)}</td><td>${esc(j.kind)}</td><td>${esc(j.status)}</td><td>${(j.params.pairs || j.params.recordings || []).length}</td>
        <td>${j.result && j.result.qber != null ? (j.result.qber * 100).toFixed(2) + '%' : '-'}</td>
        <td>${j.result && j.result.final_key_bits != null ? j.result.final_key_bits + ' bits' : '-'}</td>
        <td>${esc(j.result && j.result.link ? j.result.link : '-')}</td><td>${fmtTime(j.finished)}</td>
      </tr>`).join('') : emptyRow(8, 'no jobs yet');
    const target = shownJob || (d.jobs[0] && d.jobs[0].id);
    if (target) renderJob(await apiFetch('/api/jobs/' + encodeURIComponent(target)));
  } catch (e) { /* backend not reachable yet */ }
}
document.getElementById('jobs-table').addEventListener('click', e => {
  const row = e.target.closest('[data-job]');
  if (row) { shownJob = row.dataset.job; refreshJobs(); }
});

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

// Acquisition: config upload, start and stop
async function refreshConfig() {
  try {
    const d = await apiFetch('/api/config');
    const sel = document.getElementById('acq-link');
    if (!d.loaded) { setBadge('config-status', 'No config loaded yet'); sel.innerHTML = ''; return; }
    setBadge('config-status', `${d.sites.length} sites, ${d.links.length} links`, 'success');
    sel.innerHTML = d.links.map(l => `<option value="${esc(l.name)}">${esc(l.name)} (${esc(l.site_a)} + ${esc(l.site_b)})</option>`).join('');
    if (d.default_link) sel.value = d.default_link;
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

document.getElementById('acq-start').addEventListener('click', async () => {
  const errEl = document.getElementById('acq-error');
  errEl.textContent = '';
  try {
    await postJSON('/api/acquire/start', {
      link: document.getElementById('acq-link').value,
      duration_s: Number(document.getElementById('acq-duration').value) || 60,
    });
    refreshAcquireStatus();
  } catch (e) { errEl.textContent = e.message; }
});

document.getElementById('acq-stop').addEventListener('click', async () => {
  const errEl = document.getElementById('acq-error');
  errEl.textContent = '';
  try { await apiFetch('/api/acquire/stop', { method: 'POST' }); } catch (e) { errEl.textContent = e.message; }
});

let wasRecording = false;
async function refreshAcquireStatus() {
  try {
    const d = await apiFetch('/api/acquire/status');
    setBadge('acq-badge', d.running ? (d.kind === 'replay' ? 'Replaying' : 'Recording') : 'Idle', d.running ? 'success' : 'muted');
    document.getElementById('live-replay').disabled = d.running;
    document.getElementById('acq-start').disabled = d.running;
    document.getElementById('acq-stop').disabled = !d.running;
    const logEl = document.getElementById('acq-log');
    logEl.textContent = d.log.join('\n');
    logEl.scrollTop = logEl.scrollHeight;
    if (wasRecording && !d.running) refreshRecordings();
    wasRecording = d.running;
  } catch (e) { /* backend not reachable yet */ }
}

// Live coincidence peak: the tracker's latest status, drawn once a second
let liveLast = null;
const fmtNum = (v, digits) => (v == null || !isFinite(v)) ? '-' : Number(v).toFixed(digits);

function drawLive() {
  const st = liveLast;
  if (!st || st.state !== 'locked') {
    const coarse = st && st.search && st.search.coarse;
    document.getElementById('live-plot-t0').textContent = 'Coarse correlation while searching';
    drawSeriesPlot('live-plot-c0', coarse ? coarse.x_ms : [], coarse ? coarse.y : [], '#2BBDE8', { xUnit: 'ms' });
    ['live-plot-c1', 'live-plot-c2', 'live-plot-c3'].forEach(id => drawSeriesPlot(id, [], []));
    return;
  }
  const h = st.histogram, hi = st.history;
  document.getElementById('live-plot-t0').textContent = 'Clock-corrected peak, recent chunks';
  drawSeriesPlot('live-plot-c0', h.x_ns, h.y, '#22C55E', { band: { center: 0, width: 2 * h.hwhm_ns }, markerX: 0, xUnit: 'ns', yMin: 0 });
  drawDriftPlot('live-plot-c1', hi.t_s, hi.residual_ps, hi.t_s.map(() => 0), '#F59E0B');
  drawSeriesPlot('live-plot-c2', hi.t_s, hi.rate, '#9B5CF5', { xUnit: 's', yMin: 0 });
  drawSeriesPlot('live-plot-c3', hi.t_s, hi.score, '#2BBDE8', { xUnit: 's', yMin: 0 });
}

async function refreshLive() {
  let d;
  try { d = await apiFetch('/api/acquire/live'); } catch (e) { return; }
  const st = d.status;
  liveLast = st;
  const state = st ? st.state : 'idle';
  const kind = { locked: 'success', searching: 'warn', waiting: 'muted', idle: 'muted' }[state] || 'muted';
  setBadge('live-badge', { locked: 'LOCKED', searching: 'Searching', waiting: 'Waiting for data', idle: d.running ? 'Starting' : 'Idle' }[state] || state, kind);
  const chips = [];
  if (st) {
    chips.push(['Data', fmtNum(st.elapsed_s, 1) + ' s'], ['Rate A', fmtNum(st.rate_a / 1e3, 1) + ' kHz'], ['Rate B', fmtNum(st.rate_b / 1e3, 1) + ' kHz']);
    if (state === 'locked') {
      chips.push(['Lag B-A', fmtNum(st.lag_ns / 1e6, 6) + ' ms'], ['Skew', `${fmtNum(st.skew_ppb, 2)} ± ${fmtNum(st.skew_err_ppb, 2)} ppb`],
        ['HWHM', fmtNum(st.hwhm_ps, 0) + ' ps'], ['True pairs', fmtNum(st.pair_rate, 0) + ' /s'],
        ['Accidentals in ±HWHM', st.accidental_share == null ? '-' : fmtNum(100 * st.accidental_share, 1) + '%'],
        ['Last chunk score', fmtNum(st.chunk_score, 1)]);
      setEl('live-plot-v0', '±' + fmtNum(st.hwhm_ps, 0) + ' ps');
      setEl('live-plot-v1', fmtNum(st.skew_ppb, 2) + ' ppb removed');
      setEl('live-plot-v2', fmtNum(st.pair_rate, 0) + ' /s');
      setEl('live-plot-v3', fmtNum(st.chunk_score, 1));
    } else if (st.search && st.search.best_score != null) {
      chips.push(['Best score', `${fmtNum(st.search.best_score, 1)} (locks at ${st.search.lock_score})`], ['Searched', fmtNum(st.search.span_s, 1) + ' s']);
      setEl('live-plot-v0', 'best ' + fmtNum(st.search.best_score, 1));
    }
    document.getElementById('live-events').textContent = (st.events || []).join('\n');
  }
  document.getElementById('live-chips').innerHTML = chipsHTML(chips);
  drawLive();
}

document.getElementById('live-replay').addEventListener('click', async () => {
  const errEl = document.getElementById('live-error');
  errEl.textContent = '';
  try {
    await postJSON('/api/acquire/replay', {
      a: document.getElementById('live-a').value, b: document.getElementById('live-b').value,
      speed: Number(document.getElementById('live-speed').value),
    });
    refreshAcquireStatus();
  } catch (e) { errEl.textContent = e.message; }
});
window.addEventListener('resize', drawLive);

// Continuous QKD: recorder in continuous mode plus the block processor
async function refreshConfigLinks() {
  try {
    const d = await apiFetch('/api/config');
    const sel = document.getElementById('cont-link');
    if (!d.loaded) { sel.innerHTML = '<option value="">load a config on the Acquisition page</option>'; return; }
    const keep = sel.value;
    sel.innerHTML = d.links.map(l => `<option value="${esc(l.name)}">${esc(l.name)} (${esc(l.site_a)} + ${esc(l.site_b)})</option>`).join('');
    sel.value = keep || d.default_link || '';
  } catch (e) { /* backend not reachable yet */ }
}

document.getElementById('cont-source').addEventListener('change', e => {
  const replay = e.target.value === 'replay';
  document.getElementById('cont-replay-row').style.display = replay ? '' : 'none';
  document.getElementById('cont-link').disabled = replay;
});

document.getElementById('cont-start').addEventListener('click', async () => {
  const errEl = document.getElementById('cont-error');
  errEl.textContent = '';
  const replay = document.getElementById('cont-source').value === 'replay';
  const body = {
    segment_s: Number(document.getElementById('cont-segment').value), block_s: Number(document.getElementById('cont-block').value),
    method: document.getElementById('cont-method').value, neighbor: document.getElementById('cont-neighbor').value,
  };
  if (replay) body.replay = { a: document.getElementById('cont-a').value, b: document.getElementById('cont-b').value, speed: Number(document.getElementById('cont-speed').value) };
  else body.link = document.getElementById('cont-link').value;
  try { await postJSON('/api/continuous/start', body); refreshContinuous(); } catch (e) { errEl.textContent = e.message; }
});

document.getElementById('cont-stop').addEventListener('click', async () => {
  const errEl = document.getElementById('cont-error');
  errEl.textContent = '';
  try { await apiFetch('/api/continuous/stop', { method: 'POST' }); refreshContinuous(); } catch (e) { errEl.textContent = e.message; }
});

let contLast = null;
const fmtDur = s => s == null ? '-' : s >= 3600 ? `${(s / 3600).toFixed(1)} h` : s >= 60 ? `${(s / 60).toFixed(1)} min` : `${s.toFixed(0)} s`;

function drawContinuous() {
  const st = contLast && contLast.status;
  const h = st ? st.history : null;
  if (!h || !h.name.length) { ['cont-c0', 'cont-c1', 'cont-c2', 'cont-c3'].forEach(id => drawSeriesPlot(id, [], [])); return; }
  // x axis: data time at the end of each segment, skipped segments drawn as gaps of zero length
  const xs = []; let t = 0;
  h.seconds.forEach(v => { t += v || 0; xs.push(t / 60); });
  const keep = (ys) => { const px = [], py = []; ys.forEach((v, i) => { if (v != null) { px.push(xs[i]); py.push(v); } }); return [px, py]; };
  const [qx, qy] = keep(h.qber_vis.map(v => v == null ? null : 100 * v));
  const [rx, ry] = keep(h.sifted.map((v, i) => v == null || !h.seconds[i] ? null : v / h.seconds[i]));
  const [kx, ky] = keep(h.skew_ppb);
  const [px, py] = keep(h.processing_s);
  drawSeriesPlot('cont-c0', qx, qy, '#F59E0B', { xUnit: ' min', yMin: 0 });
  drawSeriesPlot('cont-c1', rx, ry, '#2BBDE8', { xUnit: ' min', yMin: 0 });
  drawSeriesPlot('cont-c2', kx, ky, '#9B5CF5', { xUnit: ' min' });
  drawSeriesPlot('cont-c3', px, py, '#22C55E', { xUnit: ' min', yMin: 0 });
  const last = (arr) => arr.length ? arr[arr.length - 1] : null;
  setEl('cont-v0', qy.length ? fmtNum(last(qy), 2) + '%' : '-');
  setEl('cont-v1', ry.length ? fmtNum(last(ry), 0) + ' /s' : '-');
  setEl('cont-v2', ky.length ? fmtNum(last(ky), 2) + ' ppb' : '-');
  setEl('cont-v3', py.length ? `${fmtNum(last(py), 1)} s per ${fmtNum(last(h.seconds.filter(Boolean)), 0)} s` : '-');
}

async function refreshContinuous() {
  let d;
  try { d = await apiFetch('/api/continuous/status'); } catch (e) { return; }
  contLast = d;
  const st = d.status;
  const running = d.recording || d.processing;
  setBadge('cont-badge', d.recording ? 'Recording and distilling' : d.processing ? 'Finishing the last block' : st ? 'Finished' : 'Idle',
    d.recording ? 'success' : d.processing ? 'warn' : 'muted');
  document.getElementById('cont-start').disabled = running;
  document.getElementById('cont-stop').disabled = !d.recording;
  const chips = [];
  if (st) {
    const b = st.block;
    document.getElementById('cont-progress').style.width = `${Math.min(100, 100 * b.seconds / st.block_s).toFixed(1)}%`;
    chips.push(['Running', fmtDur(st.uptime_s)], ['Data distilled or in hand', fmtDur(st.data_s)],
      ['Segments', `${st.segments_done} kept, ${st.segments_skipped} skipped`], ['Backlog', `${st.backlog} segment(s)`],
      [`Block ${b.index}`, `${fmtDur(b.seconds)} of ${fmtDur(st.block_s)}, ${b.sifted} sifted bits` + (b.qber_vis != null ? `, QBER ${fmtNum(100 * b.qber_vis, 2)}%` : '')],
      ['Distilling', st.distilling ? `block ${st.distilling.index} (${fmtDur(st.distilling.seconds)})` : 'no'],
      ['Key', `${st.key_bits} bits, ${fmtNum(st.key_rate, 1)} bit/s of data`], ['Keys to the KMS', st.keystore ? st.keys_saved : 'not saved']);
    const blocks = (st.blocks || []).slice().reverse();
    setBadge('cont-blocks-badge', blocks.length ? `${blocks.length} block(s), ${st.key_bits} key bits` : 'none yet', blocks.some(x => x.key_bits > 0) ? 'success' : 'muted');
    document.getElementById('cont-blocks').innerHTML = blocks.length ? blocks.map(x => `<tr>
        <td>${x.index}</td><td>${fmtDur(x.seconds)} (${x.segments} seg.)</td><td>${x.sifted ?? '-'}</td>
        <td>${x.qber != null ? fmtNum(100 * x.qber, 2) + '%' : '-'}</td><td>${x.ec_leak ?? '-'}</td><td>${x.f != null ? fmtNum(x.f, 2) : '-'}</td>
        <td>${x.phase_error_bound != null ? fmtNum(100 * x.phase_error_bound, 1) + '%' : '-'}</td>
        <td class="${x.key_bits > 0 ? 'positive' : ''}">${x.error ? 'failed: ' + esc(x.error) : x.key_bits + ' bits'}</td>
        <td>${x.keys_saved || 0}</td><td>${fmtNum(x.processing_s, 1)} s</td></tr>`).join('') : emptyRow(10, 'no block distilled yet');
  }
  if (d.params) chips.unshift(['Source', d.params.source + (d.params.speed && d.params.speed !== 1 ? ` x${d.params.speed}` : '')],
    ['Blocks', `${fmtDur(d.params.block_s)}, ${d.params.method}`], ['Keys to', d.params.neighbor || 'not saved']);
  document.getElementById('cont-chips').innerHTML = chipsHTML(chips);
  const logEl = document.getElementById('cont-log');
  logEl.textContent = (d.log || []).join('\n');
  logEl.scrollTop = logEl.scrollHeight;
  try {
    const live = (await apiFetch('/api/acquire/live')).status;
    setBadge('cont-live-badge', live && live.state === 'locked' ? `peak locked: skew ${fmtNum(live.skew_ppb, 2)} ppb, HWHM ${fmtNum(live.hwhm_ps, 0)} ps` : `peak: ${live ? live.state : '-'}`,
      live && live.state === 'locked' ? 'success' : 'muted');
  } catch (e) { /* no live status */ }
  drawContinuous();
}
window.addEventListener('resize', drawContinuous);

// Evaluation: evaluate.py on ticked pairs, reports kept on this node
const evalPairs = [];
let evalJob = null;

function renderEvalPairs() {
  for (const p of recordings.pairs) {
    if (!evalPairs.some(q => q.a === p.a && q.b === p.b)) evalPairs.push({ a: p.a, b: p.b, checked: false });
  }
  document.getElementById('eval-pairs').innerHTML = evalPairs.length ? evalPairs.map((p, i) => `<tr>
      <td><input type="checkbox" data-pair="${i}" ${p.checked ? 'checked' : ''}></td>
      <td class="mono">${esc(p.a.replace('_timestamp_sequence.npy', ''))}</td>
      <td class="mono">${esc(p.b.replace('_timestamp_sequence.npy', ''))}</td></tr>`).join('') : emptyRow(3, 'no recording pairs yet');
}
document.getElementById('eval-pairs').addEventListener('change', e => {
  const i = e.target.dataset.pair;
  if (i !== undefined) evalPairs[i].checked = e.target.checked;
});

document.getElementById('eval-run').addEventListener('click', async () => {
  const errEl = document.getElementById('eval-error');
  errEl.textContent = '';
  const pairs = evalPairs.filter(q => q.checked).map(q => ({ a: q.a, b: q.b }));
  if (!pairs.length) { errEl.textContent = 'tick at least one pair'; return; }
  try {
    const j = await postJSON('/api/jobs', { kind: 'evaluate', params: { pairs, live: document.getElementById('eval-live').checked } });
    evalJob = j.id;
    refreshEvalJob();
  } catch (e) { errEl.textContent = e.message; }
});

async function refreshEvalJob() {
  if (!evalJob) return;
  try {
    const j = await apiFetch('/api/jobs/' + encodeURIComponent(evalJob));
    setBadge('eval-badge', j.status === 'running' ? 'Evaluating...' : j.status, j.status === 'done' ? 'success' : j.status === 'failed' ? 'warn' : 'muted');
    const logEl = document.getElementById('eval-log');
    logEl.textContent = (j.log || []).join('\n') + (j.error ? '\n' + j.error : '');
    logEl.scrollTop = logEl.scrollHeight;
    if (j.status === 'done' || j.status === 'failed') {
      const done = evalJob;
      evalJob = null;
      await refreshEvaluations();
      if (j.result && j.result.evaluation) showEvaluation(j.result.evaluation);
    }
  } catch (e) { /* keep polling */ }
}

async function refreshEvaluations() {
  try {
    const d = await apiFetch('/api/evaluations');
    document.getElementById('eval-list').innerHTML = d.evaluations.length ? d.evaluations.map(e => `<tr data-eval="${esc(e.name)}" style="cursor:pointer">
        <td class="mono">${esc(e.name)}</td><td>${e.pairs.length}</td><td>${fmtTime(e.created)}</td></tr>`).join('') : emptyRow(3, 'none yet');
  } catch (e) { /* backend not reachable yet */ }
}
document.getElementById('eval-list').addEventListener('click', e => {
  const row = e.target.closest('[data-eval]');
  if (row) showEvaluation(row.dataset.eval);
});

const EVAL_FIGURES = [['rates.png', 'Detector singles'], ['peak.png', 'Offline peak search'], ['live.png', 'Live tracking'],
  ['window.png', 'Window choice'], ['ec.png', 'Error correction'], ['budget.png', 'Key budget']];

async function showEvaluation(name) {
  let d;
  try { d = await apiFetch('/api/evaluations/' + encodeURIComponent(name)); } catch (e) { return; }
  document.getElementById('eval-view').style.display = '';
  setEl('eval-view-title', 'Evaluation ' + name);
  const pairs = d.pairs || [];
  document.getElementById('eval-summary').innerHTML = pairs.map(p => {
    const b = p.base || {}, pk = p.peak || {}, lv = p.live || {};
    const ec = Object.entries(p.ec || {}).filter(([, v]) => v.ok && v.leak);
    const best = ec.sort((x, y) => x[1].leak - y[1].leak)[0];
    const asym = Math.max(0, ...Object.values(p.ec || {}).map(v => v.key || 0));
    return `<tr><td class="mono">${esc(p.name)}</td><td>${esc(p.link)}</td><td>${fmtNum((p.a || {}).duration_s, 1)} s</td>
      <td>${pk.skew_ppb != null ? fmtNum(pk.skew_ppb, 2) + ' ppb' : '-'}</td><td>${lv.lock_s != null ? fmtNum(lv.lock_s, 1) + ' s' : '-'}</td>
      <td>${b.window_ps ? '±' + b.window_ps + ' ps' : '-'}</td><td>${b.sifted_bits ?? '-'}</td><td>${b.qber != null ? fmtNum(100 * b.qber, 2) + '%' : '-'}</td>
      <td>${best ? esc(best[0]) + ' (' + fmtNum(best[1].leak / p.shannon, 2) + ')' : '-'}</td><td>${asym}</td><td>${b.final_key_bits ?? '-'}</td></tr>`;
  }).join('') || emptyRow(11, (pairs[0] && pairs[0].notes || []).join('; ') || 'no pairs');
  const pooled = Object.entries(d.pooled || {});
  document.getElementById('eval-pooled').innerHTML = chipsHTML(pooled.map(([link, p]) => [`${link} pooled`,
    p.error ? 'failed: ' + p.error : `${p.n} recordings, ${fmtNum(p.seconds, 0)} s, ${p.sifted} sifted, QBER ${fmtNum(100 * p.qber, 2)}%, phase-error bound ${fmtNum(100 * p.bound, 1)}%, finite key ${p.finite} bits`]));
  const url = f => `/api/evaluations/${encodeURIComponent(name)}/${f}`;
  document.getElementById('eval-figures').innerHTML = (d.comparison ? `<h3 class="card-title" style="margin:.4rem 0">Recordings side by side</h3><img class="eval-fig" src="${url('comparison.png')}" alt="comparison">` : '') +
    pairs.map(p => `<h3 class="card-title" style="margin:1rem 0 .4rem">${esc(p.name)}</h3>` +
      EVAL_FIGURES.filter(([f]) => (p.figures || []).includes(f)).map(([f, title]) =>
        `<figure class="eval-figure"><img class="eval-fig" src="${url(encodeURIComponent(p.name) + '/' + f)}" alt="${esc(title)}" loading="lazy"><figcaption>${esc(title)}</figcaption></figure>`).join('')).join('');
}

navigate('home');
