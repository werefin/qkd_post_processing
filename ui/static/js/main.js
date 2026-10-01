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
  // A canvas in a display:none section (the other gauge, while this one's
  // section is the active one) has zero layout size, so r below would go
  // negative and ctx.arc() throws, aborting whatever the caller runs next
  // Skip it; navigate() redraws from cached state once that section is shown
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

// Snapshot plots (coincidence_peak.py's save_plot() panels, redrawn fresh on
// every check instead of accumulated as history like the sparklines above),
// each zoomable: scroll to zoom around the cursor, drag to zoom to a range,
// double-click to reset -- see attachZoom() below
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

// Zoom state per canvas id: null (full range) or [lo, hi] in that plot's
// own x units. Wheel zooms around the cursor, drag selects a range,
// double-click resets -- attached once per canvas in attachZoom() below
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

// Per-canvas draw wrappers: read the last-fetched alignment data (see
// refreshAlignment()) plus this canvas's own zoom state, so both the 5s
// poll and every zoom/hover interaction redraw from the same source
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

// Navigation (same pattern as skQCI: toggle .active on nav items + sections,
// re-arm per-section polling so a hidden section's expensive checks stop)
let alignPollInterval = null;
let acquirePollInterval = null;

function navigate(sectionName) {
  document.querySelectorAll('.nav-item').forEach(el => {
    el.classList.toggle('active', el.dataset.section === sectionName);
  });
  document.querySelectorAll('.section').forEach(el => {
    el.classList.toggle('active', el.id === 'section-' + sectionName);
  });

  if (alignPollInterval) { clearInterval(alignPollInterval); alignPollInterval = null; }
  if (acquirePollInterval) { clearInterval(acquirePollInterval); acquirePollInterval = null; }

  // Canvases drawn while their section was display:none have zero size;
  // redraw from cached state now that the section is visible again
  if (sectionName === 'home') {
    redrawHomeGauge();
  } else if (sectionName === 'run') {
    redrawRunPlots();
  } else if (sectionName === 'align') {
    redrawAlignPlots();
    refreshAlignment();
    alignPollInterval = setInterval(refreshAlignment, 5000);
  } else if (sectionName === 'acquire') {
    refreshAcquireStatus();
    acquirePollInterval = setInterval(refreshAcquireStatus, 2000);
  } else if (sectionName === 'recordings') {
    refreshRecordingsList();
  }
}

document.getElementById('main-nav').addEventListener('click', e => {
  const item = e.target.closest('.nav-item');
  if (item) { e.preventDefault(); navigate(item.dataset.section); }
});
document.querySelectorAll('.nav-card').forEach(card => {
  card.addEventListener('click', e => { e.preventDefault(); navigate(card.dataset.section); });
});

// Run and metrics
const METRICS_MAX_HISTORY = 30;
const history = { qber: [], key_rate: [], compression: [], leaked: [] };
let lastMetrics = null;

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

  PLOTS.forEach(p => history[p.key].push(p.val(m) * 1) > METRICS_MAX_HISTORY && history[p.key].shift());
  lastMetrics = m;
  redrawRunPlots();

  if (m.keys_in_store != null) lastKeysSaved = m.keys_in_store;
  redrawHomeGauge();
}

// Redraws the run section's plots/gauge from cached state, needed both
// after a run and when switching back to a section that was display:none
// (a canvas drawn while hidden has zero size)
function redrawRunPlots() {
  PLOTS.forEach((p, i) => {
    sparkline('plot-c' + i, history[p.key], p.color);
    if (lastMetrics) setEl('plot-v' + i, p.val(lastMetrics));
  });
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

// Status (sidebar dot, always polled) + the keys-in-store gauge, shown on
// both Home and the Run section
let lastKeysSaved = 0;
let lastStatusOnline = false;
const KEYSTORE_MAX = 30000;  // matches app.py's KEYSTORE_MAX / -keystore-max

function redrawHomeGauge() {
  drawGauge('home-gauge-canvas', lastKeysSaved, KEYSTORE_MAX);
  setEl('home-gauge-value', lastKeysSaved);
  drawGauge('gauge-canvas', lastKeysSaved, KEYSTORE_MAX);
  setEl('gauge-value', lastKeysSaved);
  setEl('sidebar-key-count', lastKeysSaved);
  const chips = document.getElementById('home-status-chips');
  if (chips) {
    chips.innerHTML = [
      ['Backend', lastStatusOnline ? 'reachable' : 'unreachable'],
      ['Keys in store', lastKeysSaved],
    ].map(([k, v]) => `<div class="chip"><span class="chip-k">${k}</span><span class="chip-v">${v}</span></div>`).join('');
  }
}

async function refreshStatus() {
  const dot = document.getElementById('status-dot');
  const label = document.getElementById('status-label');
  try {
    const d = await apiFetch('/api/status');
    dot.className = 'status-dot online';
    label.textContent = 'Backend reachable';
    lastStatusOnline = true;
    lastKeysSaved = d.keys_saved;
  } catch (e) {
    dot.className = 'status-dot offline';
    label.textContent = 'Backend unreachable';
    lastStatusOnline = false;
  }
  redrawHomeGauge();
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
      refreshRecordingsList();
    } catch (e) {
      list.innerHTML = `<div class="bad">upload failed</div>`;
    }
  };
  xhr.onerror = () => { track.classList.remove('active'); list.innerHTML = `<div class="bad">upload failed</div>`; };
  xhr.send(fd);
}
setupDropzone('rec-drop', 'rec-file-input', uploadRecordings);

function formatBytes(n) {
  if (!n) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB', 'TB'];
  let u = 0;
  while (n >= 1024 && u < units.length - 1) { n /= 1024; u++; }
  return (u === 0 ? n : n.toFixed(1)) + ' ' + units[u];
}

async function refreshRecordingsList() {
  const badge = document.getElementById('recordings-usage-badge');
  const tbody = document.getElementById('recordings-table-body');
  if (!badge || !tbody) return;
  try {
    const d = await apiFetch('/api/recordings');
    const capBytes = d.max_gb > 0 ? d.max_gb * 1024 ** 3 : null;
    badge.textContent = formatBytes(d.total_bytes) + (capBytes ? ` / ${d.max_gb}GB cap` : ' (no cap set)');
    badge.className = 'badge ' + (capBytes && d.total_bytes > capBytes * 0.9 ? 'badge-warn' : 'badge-muted');

    if (!d.items.length) {
      tbody.innerHTML = `<tr><td colspan="3" style="text-align:center;color:var(--text3);padding:1.5rem">none uploaded yet</td></tr>`;
      return;
    }
    tbody.innerHTML = d.items.map(it => `
      <tr>
        <td>${it.name}</td>
        <td>${formatBytes(it.bytes)}</td>
        <td><button class="btn btn-outline" style="padding:.3rem .6rem;font-size:.72rem" data-delete-recording="${it.base}">Delete</button></td>
      </tr>
    `).join('');
  } catch (e) { /* backend not reachable yet */ }
}

document.getElementById('recordings-table-body').addEventListener('click', async e => {
  const btn = e.target.closest('[data-delete-recording]');
  if (!btn) return;
  const base = btn.dataset.deleteRecording;
  if (!confirm(`Delete recording "${base}"? This removes the raw TimeTagger files from disk (not reversible).`)) return;
  btn.disabled = true;
  try {
    await apiFetch(`/api/recordings/${encodeURIComponent(base)}`, { method: 'DELETE' });
    refreshRecordingsList();
  } catch (err) {
    alert('Delete failed: ' + err.message);
    btn.disabled = false;
  }
});

document.getElementById('recordings-delete-all-btn').addEventListener('click', async () => {
  const btn = document.getElementById('recordings-delete-all-btn');
  if (!confirm('Delete ALL stored recordings? This removes every raw TimeTagger file from disk (not reversible).')) return;
  btn.disabled = true;
  try {
    await apiFetch('/api/recordings', { method: 'DELETE' });
    refreshRecordingsList();
  } catch (err) {
    alert('Delete all failed: ' + err.message);
  } finally {
    btn.disabled = false;
  }
});

// Live alignment: same 4 diagnostic panels coincidence_peak.py's save_plot()
// draws (coarse correlation, best constant-shift peak, clock-rate drift,
// clock-corrected peak), redrawn fresh on every recheck --> these are
// snapshots of one analysis, not a metric evolving over time, so no history
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
      document.getElementById('align-run-btn').disabled = true;
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

    lastAlignData = d;
    redrawAlignPlots();
    // Only a confirmed peak makes the real recording's clock alignment
    // trustworthy enough to run the real chain on, same guard the backend
    // (analyze_arrays(strict=True)) and cmd/importtt's findRecording() apply
    document.getElementById('align-run-btn').disabled = !d.detected;
  } catch (e) {
    badge.textContent = 'Error';
    badge.className = 'badge badge-danger';
  } finally {
    alignInFlight = false;
  }
}

// Run the real chain (sift, error-correct, privacy-amplify) on the same
// recording pair once a peak is confirmed above, and save the distilled key
document.getElementById('align-run-btn').addEventListener('click', async () => {
  const btn = document.getElementById('align-run-btn');
  const badge = document.getElementById('align-run-badge');
  const chips = document.getElementById('align-run-chips');
  const errEl = document.getElementById('align-run-error');
  errEl.textContent = '';
  btn.disabled = true;
  badge.textContent = 'Running...';
  badge.className = 'badge badge-muted';
  try {
    const method = document.getElementById('align-run-method').value;
    const m = await apiFetch('/api/align/run', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ method }),
    });
    const items = [['Sifted bits', m.sifted_bits]];
    if (m.method === 'winnow') {
      items.push(['Corrections', m.winnow_corrections], ['Leaked (syndromes)', m.winnow_leaked_bits]);
    } else if (m.method === 'ldpc') {
      items.push(['Blocks converged', `${m.ldpc_converged || 0} / ${m.ldpc_blocks}`], ['Leaked (syndromes)', m.ldpc_leaked_bits]);
    } else {
      items.push(['Blocks kept', `${m.blocks_kept} / ${m.blocks_total}`]);
    }
    items.push(
      ['QBER', (m.qber * 100).toFixed(2) + '%'],
      ['Final key', m.final_key_bits + ' bits'],
      ['Compression', m.compression_rate.toFixed(4)],
      ['Keys saved', m.keys_saved],
      ['Keys in store', m.keys_in_store],
      ['Elapsed', m.elapsed_ms + ' ms'],
    );
    // final_key_bits is what the pipeline distilled; keys_saved*256 is what
    // actually got written --> a gap between them means -keystore-max held
    // some of it back (the keystore is at its cap), see cmd/importtt's
    // saveToKeystore(). Real key bits, so nothing here silently drops them
    // they just never leave the process
    const heldBackBits = m.final_key_bits - m.keys_saved * 256;
    if (heldBackBits > 0) {
      items.push(['Held back (keystore cap)', heldBackBits + ' bits']);
    }
    chips.innerHTML = items.map(([k, v]) => `<div class="chip"><span class="chip-k">${k}</span><span class="chip-v">${v}</span></div>`).join('');
    if (heldBackBits > 0) {
      badge.textContent = 'Keystore at cap';
      badge.className = 'badge badge-warn';
    } else if (m.final_key_bits > 0) {
      badge.textContent = 'Key saved';
      badge.className = 'badge badge-success';
    } else {
      badge.textContent = 'No key (QBER/leakage too high)';
      badge.className = 'badge badge-warn';
    }
    if (m.keys_in_store != null) lastKeysSaved = m.keys_in_store;
    redrawHomeGauge();
  } catch (e) {
    errEl.textContent = e.message;
    badge.textContent = 'Error';
    badge.className = 'badge badge-danger';
  } finally {
    btn.disabled = !(lastAlignData && lastAlignData.detected);
  }
});

// CHSH/Bell diagnostic: per-block singles + ROI1/ROI2 coincidence counts per
// channel pair over the same two recordings, see acquisition/chsh_diagnostic.py
document.getElementById('chsh-run-btn').addEventListener('click', async () => {
  const btn = document.getElementById('chsh-run-btn');
  const badge = document.getElementById('chsh-badge');
  const errEl = document.getElementById('chsh-error');
  const chips = document.getElementById('chsh-chips');
  const tbody = document.getElementById('chsh-table-body');
  const downloadBtn = document.getElementById('chsh-download-btn');
  errEl.textContent = '';
  btn.disabled = true;
  badge.textContent = 'Running...';
  badge.className = 'badge badge-muted';
  try {
    const numOrNull = id => {
      const v = document.getElementById(id).value;
      return v === '' ? null : Number(v);
    };
    const body = {
      block_s: numOrNull('chsh-block-s'),
      roi1_width_ns: numOrNull('chsh-roi1-width'),
      roi1_center_ns: numOrNull('chsh-roi1-center'),
      roi2_offset_ns: numOrNull('chsh-roi2-offset'),
      roi2_width_ns: numOrNull('chsh-roi2-width'),
    };
    const d = await apiFetch('/api/chsh/run', {
      method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(body),
    });
    badge.textContent = `${d.metadata.n_blocks} blocks`;
    badge.className = 'badge badge-success';
    chips.innerHTML = [
      ['A', d.a], ['B', d.b],
      ['Blocks', `${d.metadata.n_blocks} x ${d.metadata.block_s}s`],
      ['ROI1', `${(d.metadata.roi1.center_ps / 1000).toFixed(2)} ns ± ${(d.metadata.roi1.width_ps / 2000).toFixed(2)} ns`],
      ['ROI2', `${(d.metadata.roi2.center_ps / 1000).toFixed(2)} ns ± ${(d.metadata.roi2.width_ps / 2000).toFixed(2)} ns` + (d.roi2_offset_auto ? ' (auto)' : '')],
      ['Clock skew', d.metadata.clock_skew_ppb.toFixed(2) + ' ppb'],
    ].map(([k, v]) => `<div class="chip"><span class="chip-k">${k}</span><span class="chip-v">${v}</span></div>`).join('');

    tbody.innerHTML = d.pairs.map(p => `<tr>
      <td>${p.pair}</td><td>${p.roi1_total.toLocaleString()}</td><td>${p.roi2_total.toLocaleString()}</td>
      <td class="${p.subtracted > 0 ? 'positive' : ''}">${p.subtracted.toLocaleString()}</td>
    </tr>`).join('');

    downloadBtn.style.display = '';
  } catch (e) {
    errEl.textContent = e.message;
    badge.textContent = 'Error';
    badge.className = 'badge badge-danger';
  } finally {
    btn.disabled = false;
  }
});

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

// Land on Home; per-section polling (alignment, acquire status) starts
// only once the user actually opens that section, see navigate() above
navigate('home');
