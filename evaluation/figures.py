'''
Figures for evaluate.py: one PNG per stage of the chain, in one consistent style
'''

import math

import matplotlib
matplotlib.use('Agg')
import matplotlib.pyplot as plt
import numpy as np
from scipy.optimize import curve_fit

INK, MUTED, GRID = '#1F2433', '#6B7385', '#E6E8EF'
BLUE, PURPLE, GREEN, AMBER, RED, CYAN = '#2F6FEB', '#8B5CF6', '#16A34A', '#F59E0B', '#DC2626', '#0891B2'
CHANNEL_COLORS = [BLUE, PURPLE, GREEN, AMBER]
plt.rcParams.update({
    'figure.facecolor': 'white', 'axes.facecolor': 'white', 'savefig.facecolor': 'white',
    'font.family': 'DejaVu Sans', 'font.size': 10, 'axes.edgecolor': GRID, 'axes.labelcolor': INK,
    'axes.titlesize': 11, 'axes.titleweight': 'bold', 'axes.titlecolor': INK, 'axes.titlelocation': 'left',
    'xtick.color': MUTED, 'ytick.color': MUTED, 'axes.grid': True, 'grid.color': GRID, 'grid.linewidth': 0.8,
    'axes.spines.top': False, 'axes.spines.right': False, 'legend.frameon': False,
})


def gauss(x, amp, mu, sigma, bg):
    return bg + amp * np.exp(-0.5 * ((x - mu) / sigma) ** 2)


def save(fig, path, title):
    fig.suptitle(title, x=0.01, ha='left', fontsize=13, fontweight='bold', color=INK)
    fig.tight_layout(rect=(0, 0, 1, 0.95))
    fig.savefig(path, dpi=160)
    plt.close(fig)


def rates(sides, path):
    '''sides: [(label, timestamps, channels)]; singles rate per channel in 50 ms bins'''
    fig, axes = plt.subplots(1, len(sides), figsize=(11, 3.4), sharey=False)
    for ax, (label, ts, ch) in zip(np.atleast_1d(axes), sides):
        t = (ts - ts[0]) / 1e12
        bin_s = max(0.05, float(t[-1]) / 400)
        edges = np.arange(0, t[-1], bin_s)  # whole bins only, the last partial one would read as a drop
        for c in sorted(set(np.unique(ch).tolist()) & {1, 2, 3, 4}):
            n, _ = np.histogram(t[ch == c], edges)
            ax.plot(edges[:-1], n / bin_s / 1e3, color=CHANNEL_COLORS[c - 1], lw=1.3, label=f'channel {c}')
        ax.set_title(f'{label}: {len(ts) / t[-1] / 1e3:,.0f} kHz in total')
        ax.set_xlabel('time (s)')
        ax.set_ylabel(f'singles rate (kHz, {bin_s * 1e3:.0f} ms bins)')
        ax.legend(loc='upper center', bbox_to_anchor=(0.5, -0.2), ncol=4, fontsize=8)
    save(fig, path, 'Detector singles on both sides')


def peak(report, diag, min_score, path):
    '''The offline search: coarse correlation, constant-lag peak, skew fit, clock-corrected peak'''
    fig, axes = plt.subplots(2, 2, figsize=(11, 7))
    ax = axes[0, 0]
    x, y = np.asarray(diag['coarse_lags_ps']) / 1e9, np.asarray(diag['coarse_scores'])
    lag_ms = report['constant_shift']['relative_lag_ps'] / 1e9
    ax.axhline(min_score, color=RED, ls='--', lw=1, label=f'detection threshold {min_score:g}')
    if len(y):
        ax.plot(x, y, color=CYAN, lw=0.7)
        ax.annotate(f'peak at {lag_ms:.3f} ms', (lag_ms, y.max()), xytext=(0.55, 0.92), textcoords='axes fraction', color=INK,
                    arrowprops=dict(arrowstyle='->', color=MUTED))
    else:
        # no coarse candidate refined: the joint lag and skew search found the peak, so there is no curve to show
        ax.text(0.5, 0.5, f'no unambiguous coarse peak\njoint lag and skew search: {lag_ms:.3f} ms', transform=ax.transAxes,
                ha='center', va='center', color=INK)
    ax.set_title('Coarse FFT correlation over every lag')
    ax.set_xlabel('B minus A lag (ms)')
    ax.set_ylabel('robust z-score')
    ax.legend(loc='lower left', fontsize=8)

    ax = axes[0, 1]
    f = diag['fine_histogram']
    xs = (-f.radius_ps + (np.arange(len(f.counts)) + 0.5) * f.bin_ps) / 1e3
    w = report['constant_shift']['window']
    span = max(4 * w['width_ps'] / 1e3, 20)
    keep = np.abs(xs) <= span
    ax.fill_between(xs[keep], f.counts[keep], step='mid', color=PURPLE, alpha=0.25)
    ax.step(xs[keep], f.counts[keep], where='mid', color=PURPLE, lw=1)
    ax.axvspan((w['center_ps'] - w['width_ps'] / 2) / 1e3, (w['center_ps'] + w['width_ps'] / 2) / 1e3, color=AMBER, alpha=0.18)
    ax.set_title(f'Constant lag only: best window {w["width_ps"] / 1e3:g} ns')
    ax.set_xlabel('residual around the lag (ns)')
    ax.set_ylabel(f'pairs per {f.bin_ps} ps')

    ax = axes[1, 0]
    sk = report['clock_skew']
    if sk.get('segment_elapsed_ps'):
        t = np.asarray(sk['segment_elapsed_ps']) / 1e12
        lag = (np.asarray(sk['segment_lags_ps']) - sk['lag_at_reference_ps']) / 1e3
        ref = sk['reference_elapsed_ps'] / 1e12
        tt = np.linspace(0, t.max() + (t[1] - t[0] if len(t) > 1 else 0.5) / 2, 50)
        ax.plot(tt, sk['slope_ps_per_s'] * (tt - ref) / 1e3, color=AMBER, lw=2, label=f'fit: {sk["skew_ppb"]:.2f} ppb')
        ax.scatter(t, lag, color=INK, s=28, zorder=3, label='segment peaks')
        ax.legend(loc='upper left')
    else:
        ax.text(0.5, 0.5, f'skew from a {sk.get("skew_method", "none")} search: {sk["skew_ppb"]:.2f} ppb',
                ha='center', va='center', transform=ax.transAxes, color=MUTED)
    ax.set_title('Clock skew from per-segment peaks')
    ax.set_xlabel('time (s)')
    ax.set_ylabel('lag minus lag at reference (ns)')

    ax = axes[1, 1]
    counts = np.asarray(diag['corrected_counts'], float)
    xs = (-f.radius_ps + (np.arange(len(counts)) + 0.5) * f.bin_ps)
    keep = np.abs(xs) <= 3000
    xs, counts = xs[keep], counts[keep]
    ax.fill_between(xs / 1e3, counts, step='mid', color=GREEN, alpha=0.25)
    ax.step(xs / 1e3, counts, where='mid', color=GREEN, lw=1, label='pairs, clocks corrected')
    fit = None
    try:
        p, _ = curve_fit(gauss, xs, counts, p0=(max(counts.max() - np.median(counts), 1), 0, 300, np.median(counts)), maxfev=5000)
        hwhm = abs(p[2]) * math.sqrt(2 * math.log(2))
        fine = np.linspace(xs[0], xs[-1], 600)
        ax.plot(fine / 1e3, gauss(fine, *p), color=INK, lw=1.4, label=f'Gaussian, σ = {abs(p[2]):.0f} ps')
        ax.axvspan((p[1] - hwhm) / 1e3, (p[1] + hwhm) / 1e3, color=AMBER, alpha=0.2, label=f'±HWHM = ±{hwhm:.0f} ps')
        fit = {'sigma_ps': float(abs(p[2])), 'hwhm_ps': float(hwhm)}
    except (RuntimeError, ValueError):
        pass
    ax.set_title('Skew removed')
    ax.set_xlabel('residual (ns)')
    ax.set_ylabel(f'pairs per {f.bin_ps} ps')
    ax.legend(loc='upper right', fontsize=8)
    save(fig, path, 'Offline coincidence-peak search (coincidence_peak.py)')
    return fit


def window(rows, chosen_ps, path):
    '''rows: {'drift tracked': [(window ps, sifted, qber)], 'skew only': [...]}'''
    fig, axes = plt.subplots(1, 2, figsize=(11, 3.8))
    for (label, rs), color in zip(rows.items(), (GREEN, MUTED)):
        rs = [r for r in rs if r[1] is not None]
        axes[0].plot([r[0] for r in rs], [100 * r[2] for r in rs], 'o-', color=color, label=label)
        axes[1].plot([r[0] for r in rs], [r[1] for r in rs], 'o-', color=color, label=label)
    for ax in axes:
        ax.axvline(chosen_ps, color=AMBER, lw=2, alpha=0.6, label=f'HWHM chosen: ±{chosen_ps} ps')
        ax.set_xscale('log')
        ax.set_xlabel('coincidence window half-width (ps)')
        ax.legend(loc='upper left', fontsize=8)
    axes[0].set_ylabel('QBER over all sifted pairs (%)')
    axes[0].set_title('Wider windows admit accidentals')
    axes[1].set_ylabel('sifted bits')
    axes[1].set_title('Narrower windows lose true pairs')
    save(fig, path, 'Coincidence filtering: choosing the window')


def ec(runs, n_bits, qber, path):
    '''runs: {method: {'leak', 'ok', 'key'}}; bars of disclosed bits against the Shannon limit'''
    names = list(runs)
    fig, ax = plt.subplots(figsize=(11, 0.65 * len(names) + 1.4))
    y = np.arange(len(names))[::-1]
    leaks = [runs[n]['leak'] or 0 for n in names]
    ok = [runs[n]['ok'] for n in names]
    ax.barh(y, leaks, color=[GREEN if o else '#E5E7EB' for o in ok], edgecolor=[GREEN if o else MUTED for o in ok], height=0.6)
    for yi, n in zip(y, names):
        r = runs[n]
        if r.get('error'):
            text = f'did not run: {r["error"]}'
        else:
            tag = 'all errors corrected' if r['ok'] else 'failed, dropped by the CRC'
            text = f'{r["leak"]:,} bits · {tag} · asymptotic key {r["key"]:,} · finite key {r["finite"]:,}'
        ax.text((r['leak'] or 0) + max(leaks) * 0.01, yi, text, va='center', fontsize=9, color=INK)
    h = -qber * math.log2(qber) - (1 - qber) * math.log2(1 - qber) if 0 < qber < 1 else 0
    shannon = n_bits * h
    ax.axvline(shannon, color=RED, ls='--', lw=1.2)
    ax.text(shannon, y[-1] - 0.6, f' Shannon limit {shannon:,.0f} bits', color=RED, fontsize=8)
    ax.set_yticks(y, names)
    ax.set_xlim(0, max(max(leaks), shannon) * 1.9)
    ax.set_xlabel('bits disclosed for error correction (syndromes, frozen values, tags, verdicts)')
    ax.grid(axis='y', visible=False)
    save(fig, path, f'Error correction on the same {n_bits:,} bits (sample QBER {100 * qber:.1f}%)')
    return shannon


def budget(sifted, sample, ec_leak, crc, asym, finite, label, path):
    '''Waterfall from sifted bits to the asymptotic and finite key'''
    stages = [('sifted', sifted, BLUE), ('QBER sample', -sample, MUTED), (label, -ec_leak, AMBER), ('CRC', -crc, AMBER)]
    fig, ax = plt.subplots(figsize=(11, 3.6))
    level = 0
    for i, (name, d, color) in enumerate(stages):
        lo, hi = (level, level + d) if d > 0 else (level + d, level)
        ax.bar(i, hi - lo, bottom=lo, color=color, width=0.62)
        ax.text(i, hi + sifted * 0.02, f'{d:+,d}' if i else f'{d:,}', ha='center', color=INK, fontsize=9)
        level += d
    pa = max(level - asym, 0)
    ax.bar(len(stages), pa, bottom=asym, color=PURPLE, width=0.62)
    ax.text(len(stages), max(level, asym) + sifted * 0.02, f'-{pa:,}', ha='center', fontsize=9)
    for k, (v, color) in enumerate(((asym, GREEN), (finite, GREEN if finite else RED))):
        ax.bar(len(stages) + 1 + k, max(v, 0.001), color=color, width=0.62)
        ax.text(len(stages) + 1 + k, v + sifted * 0.02, f'{v:,}', ha='center', fontsize=9, fontweight='bold', color=color)
    ax.set_xticks(range(len(stages) + 3), [s[0] for s in stages] + ['privacy\namplification', 'asymptotic\nkey', 'finite\nkey'])
    ax.set_ylabel('bits')
    ax.grid(axis='x', visible=False)
    ax.set_ylim(min(0, level) * 1.1, sifted * 1.15)
    save(fig, path, f'Key budget with {label}')


def comparison(rows, path):
    '''One point per recording: QBER, sifted rate, skew and key, to compare recordings of a run'''
    names = [r['name'] for r in rows]
    x = np.arange(len(names))
    fig, axes = plt.subplots(2, 2, figsize=(11, 6.5), sharex=True)
    panels = (('qber', 'sample QBER (%)', 100, AMBER), ('sifted_rate', 'sifted bits per second', 1, BLUE),
              ('skew_ppb', 'clock skew (ppb)', 1, PURPLE), ('asym_key', 'asymptotic key (bits)', 1, GREEN))
    for ax, (key, label, scale, color) in zip(axes.flat, panels):
        vals = [np.nan if r.get(key) is None else r[key] * scale for r in rows]
        ax.bar(x, vals, color=color, width=0.6)
        ax.set_title(label)
        ax.grid(axis='x', visible=False)
    for ax in axes[1]:
        ax.set_xticks(x, names, rotation=30, ha='right', fontsize=8)
    save(fig, path, 'Recordings side by side')
