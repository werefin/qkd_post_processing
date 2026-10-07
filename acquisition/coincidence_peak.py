#!/usr/bin/env python3
"""Find a coincidence peak and clock skew in two photon timestamp streams"""

from __future__ import annotations

import argparse
import json
import math
import os
import sys
from concurrent.futures import ThreadPoolExecutor
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

import numpy as np
from numba import njit
from scipy import fft


PS_PER_NS = 1_000
PS_PER_MS = 1_000_000_000
PS_PER_S = 1_000_000_000_000
MIN_PEAK_SCORE = 6.0
MIN_SEGMENT_SCORE = 5.0


class AnalysisError(Exception):
    '''A user-facing input/data problem, as opposed to an internal bug;
    the CLI entry point catches this and prints it as an [ERROR] line'''


def log(level: str, msg: str) -> None:
    '''arnika-style log line: "[LEVEL] message", one severity tag per line'''
    print(f'[{level}] {msg}', flush=True)


@dataclass
class WindowResult:
    center_ps: float
    width_ps: int
    count: int
    background: float
    excess: float
    score: float
    background_per_bin: float


@dataclass
class HistogramResult:
    lag_ps: int
    radius_ps: int
    bin_ps: int
    counts: np.ndarray
    window: WindowResult


@njit(cache=True)
def _bin_timestamps(
    timestamps: np.ndarray, origin_ps: int, bin_ps: int, n_bins: int
) -> np.ndarray:
    counts = np.zeros(n_bins, dtype=np.float32)
    for timestamp in timestamps:
        index = (timestamp - origin_ps) // bin_ps
        if 0 <= index < n_bins:
            counts[index] += 1.0
    return counts


@njit(cache=True)
def _difference_histogram(
    a: np.ndarray,
    b: np.ndarray,
    a_origin_ps: int,
    b_origin_ps: int,
    lag_ps: int,
    radius_ps: int,
    bin_ps: int,
) -> np.ndarray:
    """Histogram (b_rel - a_rel - lag) for all pairs in the local range"""
    n_bins = (2 * radius_ps + bin_ps - 1) // bin_ps
    histogram = np.zeros(n_bins, dtype=np.int64)
    left = 0
    right = 0
    n_b = len(b)

    for timestamp_a in a:
        a_rel = timestamp_a - a_origin_ps
        lower = b_origin_ps + a_rel + lag_ps - radius_ps
        upper = b_origin_ps + a_rel + lag_ps + radius_ps

        while left < n_b and b[left] < lower:
            left += 1
        if right < left:
            right = left
        while right < n_b and b[right] < upper:
            right += 1

        for index_b in range(left, right):
            residual = (b[index_b] - b_origin_ps) - a_rel - lag_ps
            bin_index = (residual + radius_ps) // bin_ps
            if 0 <= bin_index < n_bins:
                histogram[bin_index] += 1

    return histogram


@njit(cache=True)
def _drift_corrected_histogram(
    a: np.ndarray,
    b: np.ndarray,
    a_origin_ps: int,
    b_origin_ps: int,
    lag_at_reference_ps: float,
    slope_ps_per_s: float,
    reference_elapsed_ps: int,
    radius_ps: int,
    bin_ps: int,
) -> np.ndarray:
    n_bins = (2 * radius_ps + bin_ps - 1) // bin_ps
    histogram = np.zeros(n_bins, dtype=np.int64)
    left = 0
    right = 0
    n_b = len(b)

    for timestamp_a in a:
        a_rel = timestamp_a - a_origin_ps
        lag = lag_at_reference_ps + slope_ps_per_s * (
            (a_rel - reference_elapsed_ps) / PS_PER_S
        )
        target = b_origin_ps + a_rel + lag
        lower = math.ceil(target - radius_ps)
        upper = math.ceil(target + radius_ps)

        while left < n_b and b[left] < lower:
            left += 1
        if right < left:
            right = left
        while right < n_b and b[right] < upper:
            right += 1

        for index_b in range(left, right):
            residual = (b[index_b] - b_origin_ps) - a_rel - lag
            bin_index = int(math.floor((residual + radius_ps) / bin_ps))
            if 0 <= bin_index < n_bins:
                histogram[bin_index] += 1

    return histogram


@njit(cache=True)
def _pair_elapsed_moments(
    a: np.ndarray,
    b: np.ndarray,
    a_origin_ps: int,
    b_origin_ps: int,
    lag_ps: int,
    half_window_ps: int,
) -> tuple[int, float]:
    """Return the pair count and summed A elapsed time in a lag window"""
    left = 0
    right = 0
    n_b = len(b)
    pair_count = 0
    elapsed_sum = 0.0
    for timestamp_a in a:
        a_rel = timestamp_a - a_origin_ps
        lower = b_origin_ps + a_rel + lag_ps - half_window_ps
        upper = b_origin_ps + a_rel + lag_ps + half_window_ps
        while left < n_b and b[left] < lower:
            left += 1
        if right < left:
            right = left
        while right < n_b and b[right] < upper:
            right += 1
        for _ in range(left, right):
            elapsed_sum += a_rel
            pair_count += 1
    return pair_count, elapsed_sum


def _validate_timestamps(path: Path) -> np.ndarray:
    timestamps = np.load(path, mmap_mode="r", allow_pickle=False)
    if timestamps.ndim != 1:
        raise AnalysisError(f"{path}: expected a one-dimensional array")
    if not np.issubdtype(timestamps.dtype, np.integer):
        raise AnalysisError(f"{path}: expected integer picosecond timestamps")
    if len(timestamps) < 2:
        raise AnalysisError(f"{path}: at least two timestamps are required")
    if np.any(timestamps[1:] < timestamps[:-1]):
        raise AnalysisError(f"{path}: timestamps are not sorted")
    return timestamps


def resolve_inputs(inputs: tuple[Path, ...]) -> tuple[Path, Path, str]:
    if len(inputs) == 1 and inputs[0].is_dir():
        paths = sorted(inputs[0].glob("*_timestamp_sequence.npy"))
        if len(paths) != 2:
            raise AnalysisError(
                f"{inputs[0]}: expected exactly two *_timestamp_sequence.npy files, "
                f"found {len(paths)}"
            )
        return paths[0], paths[1], inputs[0].name
    if len(inputs) == 2 and all(path.is_file() for path in inputs):
        name_a = inputs[0].name.removesuffix("_timestamp_sequence.npy")
        name_b = inputs[1].name.removesuffix("_timestamp_sequence.npy")
        return inputs[0], inputs[1], f"{name_a}_vs_{name_b}"
    raise AnalysisError("provide one recording directory or two timestamp NPY files")


def _channel_path(path: Path) -> Path:
    '''Sibling *_channel_sequence.npy for a *_timestamp_sequence.npy path'''
    return path.with_name(path.name.removesuffix("_timestamp_sequence.npy") + "_channel_sequence.npy")


def _meta_for(path: Path) -> dict[str, Any] | None:
    """Load the sibling *_meta.json recording manifest, if present"""
    if path.name.endswith("_timestamp_sequence.npy"):
        prefix = path.name.removesuffix("_timestamp_sequence.npy")
        candidate = path.with_name(prefix + "_meta.json")
    else:
        candidate = path.with_suffix(".json")
    if not candidate.is_file():
        return None
    try:
        return json.loads(candidate.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return None


def _wall_interval(meta: dict[str, Any]) -> tuple[float, float, int, int]:
    """Return (wall_start_unix, wall_end_unix, first_frame_ps, last_frame_ps)"""
    frames = meta.get("frames") or []
    if not frames:
        raise ValueError("meta file contains no frames")
    first = frames[0]
    last = frames[-1]
    wall_end = float(last["unixtime_readout"])
    wall_start = float(first["unixtime_readout"]) - (
        int(first["frame_end_ps"]) - int(first["frame_start_ps"])
    ) / PS_PER_S
    return wall_start, wall_end, int(first["frame_start_ps"]), int(last["frame_end_ps"])


def slice_to_wall_overlap(
    a: np.ndarray,
    b: np.ndarray,
    path_a: Path,
    path_b: Path,
    margin_s: float = 2.0,
) -> tuple[np.ndarray, np.ndarray, dict[str, Any]]:
    """Restrict both streams to their wall-clock overlap (from *_meta.json)
    Returns the sliced arrays plus an info dict; raises AnalysisError
    when the overlap is empty or a slice would be left with fewer than two timestamps
    """
    meta_a = _meta_for(path_a)
    meta_b = _meta_for(path_b)
    if meta_a is None or meta_b is None:
        missing = str(path_a) if meta_a is None else str(path_b)
        raise AnalysisError(f"{missing}: sibling *_meta.json not found")
    start_a, end_a, frame_start_a, _ = _wall_interval(meta_a)
    start_b, end_b, frame_start_b, _ = _wall_interval(meta_b)
    overlap_start = max(start_a, start_b) - margin_s
    overlap_end = min(end_a, end_b) + margin_s
    if overlap_end <= overlap_start:
        raise AnalysisError("wall-clock recordings do not overlap")

    a0, b0 = int(a[0]), int(b[0])
    wall_a0 = start_a + (a0 - frame_start_a) / PS_PER_S
    wall_b0 = start_b + (b0 - frame_start_b) / PS_PER_S

    def _slice(
        timestamps: np.ndarray, origin: int, wall_origin: float
    ) -> tuple[np.ndarray, int, int]:
        lo = origin + int(round((overlap_start - wall_origin) * PS_PER_S))
        hi = origin + int(round((overlap_end - wall_origin) * PS_PER_S))
        left = int(np.searchsorted(timestamps, max(lo, origin), side="left"))
        right = int(
            np.searchsorted(timestamps, min(hi, int(timestamps[-1])), side="right")
        )
        return np.ascontiguousarray(timestamps[left:right]), left, right

    a_cut, a_left, a_right = _slice(a, a0, wall_a0)
    b_cut, b_left, b_right = _slice(b, b0, wall_b0)
    if len(a_cut) < 2 or len(b_cut) < 2:
        raise AnalysisError(
            "wall-overlap slice left fewer than two timestamps; "
            "increase --overlap-margin-s or use --no-meta"
        )
    info = {
        "wall_start_a_unix": start_a,
        "wall_end_a_unix": end_a,
        "wall_start_b_unix": start_b,
        "wall_end_b_unix": end_b,
        "wall_overlap_start_unix": max(start_a, start_b),
        "wall_overlap_end_unix": min(end_a, end_b),
        "margin_s": float(margin_s),
        "index_range_a": [a_left, a_right],
        "index_range_b": [b_left, b_right],
        "kept_fraction_a": float(len(a_cut) / len(a)),
        "kept_fraction_b": float(len(b_cut) / len(b)),
    }
    return a_cut, b_cut, info


def _upper_limits(
    a: np.ndarray, b: np.ndarray, lag_ps: int
) -> list[dict[str, Any]]:
    """Poisson detection floor per coincidence window width
    Accidentals assume uniform singles rates over the lag overlap:
    bg = R_A * R_B * W * T; min_excess is the excess needed for a local score of MIN_PEAK_SCORE
    """
    duration_a = int(a[-1]) - int(a[0])
    duration_b = int(b[-1]) - int(b[0])
    overlap = min(duration_a, duration_b - lag_ps) - max(0, -lag_ps)
    overlap_s = max(overlap, 0) / PS_PER_S
    rate_a = len(a) / (duration_a / PS_PER_S) if duration_a > 0 else 0.0
    rate_b = len(b) / (duration_b / PS_PER_S) if duration_b > 0 else 0.0
    limits = []
    for width_ns in (0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1_000):
        width_ps = width_ns * PS_PER_NS
        background = rate_a * rate_b * (width_ps / PS_PER_S) * overlap_s
        limits.append(
            {
                "width_ps": int(width_ps),
                "expected_accidentals": float(background),
                "min_detectable_excess": float(
                    MIN_PEAK_SCORE * math.sqrt(max(background, 1.0))
                ),
                "overlap_s": float(overlap_s),
            }
        )
    return limits


def _coarse_search(
    a: np.ndarray,
    b: np.ndarray,
    coarse_bin_ps: int,
    max_lag_ps: int,
    candidate_count: int,
    max_memory_gib: float = 4.0,
) -> dict[str, Any]:
    a_origin = int(a[0])
    b_origin = int(b[0])
    duration_ps = max(int(a[-1]) - a_origin, int(b[-1]) - b_origin)
    # Memory guard: widen the coarse bin until the FFT working set fits
    while True:
        n_bins = duration_ps // coarse_bin_ps + 2
        n_fft = fft.next_fast_len(2 * n_bins - 1)
        estimated_gib = (2 * n_bins * 4 + 3 * (n_fft // 2 + 1) * 8) / (1024**3)
        if (
            estimated_gib <= max_memory_gib
            or coarse_bin_ps >= max_lag_ps // 4
            or coarse_bin_ps >= 10_000 * PS_PER_NS
        ):
            break
        coarse_bin_ps *= 2
        log('WARN',
            f"the coarse search at this bin width would need about {estimated_gib:.1f} GiB of memory "
            f"(over the --max-memory-gib budget); widening the bin to {coarse_bin_ps / PS_PER_NS:g} ns to fit, "
            f"which trades timing resolution for a smaller search")
    n_bins = duration_ps // coarse_bin_ps + 2
    max_lag_bins = min(max_lag_ps // coarse_bin_ps, n_bins - 1)
    if max_lag_bins < 1:
        raise AnalysisError("maximum lag is smaller than the coarse bin width")

    n_fft = fft.next_fast_len(2 * n_bins - 1)
    estimated_gib = (2 * n_bins * 4 + 3 * (n_fft // 2 + 1) * 8) / (1024**3)
    log('INFO',
        f"coarse search: {n_bins} bins at {coarse_bin_ps / PS_PER_NS:g} ns/bin, "
        f"about {estimated_gib:.2f} GiB of working memory")

    counts_a = _bin_timestamps(a, a_origin, coarse_bin_ps, n_bins)
    counts_b = _bin_timestamps(b, b_origin, coarse_bin_ps, n_bins)
    counts_a -= counts_a.mean(dtype=np.float64)
    counts_b -= counts_b.mean(dtype=np.float64)

    spectrum_a = fft.rfft(counts_a, n_fft, workers=-1)
    spectrum_b = fft.rfft(counts_b, n_fft, workers=-1)
    del counts_a, counts_b
    spectrum_a *= np.conj(spectrum_b)
    del spectrum_b
    correlation = fft.irfft(spectrum_a, n_fft, workers=-1)
    del spectrum_a

    lag_bins = np.arange(-max_lag_bins, max_lag_bins + 1, dtype=np.int64)
    values = correlation[(-lag_bins) % n_fft]
    del correlation
    median = float(np.median(values))
    mad = float(np.median(np.abs(values - median)))
    scale = 1.4826 * mad
    if not math.isfinite(scale) or scale <= 0:
        scale = float(np.std(values)) or 1.0
    scores = (values - median) / scale

    pool_size = min(len(scores), max(10_000, candidate_count * 100))
    pool = np.argpartition(scores, len(scores) - pool_size)[-pool_size:]
    pool = pool[np.argsort(scores[pool])[::-1]]
    # Adjacent coarse bins represent the same peak, but candidates farther away
    # than two bins must survive for independent exact refinement
    separation_bins = 2
    selected: list[int] = []
    for index in pool:
        lag_bin = int(lag_bins[index])
        if all(abs(lag_bin - existing) >= separation_bins for existing in selected):
            selected.append(lag_bin)
            if len(selected) == candidate_count:
                break

    plot_limit = 12_000
    step = max(1, math.ceil(len(scores) / plot_limit))
    starts = np.arange(0, len(scores), step)
    plot_indices = np.empty(len(starts), dtype=np.int64)
    for plot_index, start in enumerate(starts):
        end = min(int(start) + step, len(scores))
        plot_indices[plot_index] = int(start) + int(np.argmax(scores[start:end]))
    plot_scores = scores[plot_indices]
    plot_lags = lag_bins[plot_indices]

    return {
        "candidate_lags_ps": [lag_bin * coarse_bin_ps for lag_bin in selected],
        "candidate_scores": [
            float(scores[np.searchsorted(lag_bins, lag_bin)]) for lag_bin in selected
        ],
        "plot_lags_ps": plot_lags * coarse_bin_ps,
        "plot_scores": plot_scores,
        "coarse_bin_ps": int(coarse_bin_ps),
        "n_bins": int(n_bins),
        "n_fft": int(n_fft),
        "estimated_memory_gib": estimated_gib,
    }


def _window_widths(bin_ps: int, radius_ps: int) -> list[int]:
    requested_ps = np.array(
        [0.5, 1, 2, 5, 10, 20, 50, 100, 200, 500, 1_000], dtype=np.float64
    ) * PS_PER_NS
    bins = np.unique(np.maximum(1, np.rint(requested_ps / bin_ps).astype(int)))
    return [int(width) for width in bins if width * bin_ps <= radius_ps]


def _best_window(
    counts: np.ndarray,
    radius_ps: int,
    bin_ps: int,
    center_limit_ps: int,
) -> WindowResult:
    centers = -radius_ps + (np.arange(len(counts)) + 0.5) * bin_ps
    sideband = np.abs(centers) >= max(radius_ps // 2, center_limit_ps * 2)
    side_counts = counts[sideband] if np.any(sideband) else counts
    # MEAN, not median: with fine bins, counts/bin are small integers and the median quantizes (e.g., 4 vs a true rate of 4.65)
    # that error, scaled by a wide window, fabricates huge fake excesses
    # real peak is localized at the center, so it can't bias the far sidebands either way
    background_per_bin = float(np.mean(side_counts))

    best: WindowResult | None = None
    for width_bins in _window_widths(bin_ps, radius_ps):
        rolling = np.convolve(counts, np.ones(width_bins, dtype=np.int64), mode="valid")
        width_ps = width_bins * bin_ps
        background = background_per_bin * width_bins
        window_centers = -radius_ps + (
            np.arange(len(rolling)) + width_bins / 2
        ) * bin_ps
        valid = np.abs(window_centers) <= center_limit_ps
        if not np.any(valid):
            continue
        valid_indices = np.flatnonzero(valid)
        index = int(valid_indices[np.argmax(rolling[valid])])
        count = int(rolling[index])
        excess = count - background
        score = excess / math.sqrt(max(background, 1.0))
        result = WindowResult(
            center_ps=float(window_centers[index]),
            width_ps=width_ps,
            count=count,
            background=background,
            excess=excess,
            score=score,
            background_per_bin=background_per_bin,
        )
        if best is None or result.score > best.score:
            best = result
    if best is None:
        raise RuntimeError("could not evaluate a coincidence window")
    return best


def _refine_candidate(
    a: np.ndarray,
    b: np.ndarray,
    lag_ps: int,
    radius_ps: int,
    fine_bin_ps: int,
    center_limit_ps: int,
) -> HistogramResult:
    counts = _difference_histogram(
        a, b, int(a[0]), int(b[0]), lag_ps, radius_ps, fine_bin_ps
    )
    window = _best_window(counts, radius_ps, fine_bin_ps, center_limit_ps)
    return HistogramResult(lag_ps, radius_ps, fine_bin_ps, counts, window)


def calibrate_channel_delays(
    ts_a: np.ndarray, ch_a: np.ndarray, ts_b: np.ndarray, ch_b: np.ndarray,
    shift_b_to_a, radius_ps: int = 3000, fine_bin_ps: int = 20, center_limit_ps: int = 500,
) -> dict[str, Any]:
    '''Per-channel residual delay on top of the already-fitted lag+skew; our
    best-effort match to the vendor's delay_ch0_ps..delay_ch3_ps, not confirmed-identical
    Each channel is matched only against its own same-numbered channel on
    the other side --> pooling in the other 3 just adds background, no signal'''
    ts_b_aligned = shift_b_to_a(ts_b)
    results: dict[str, Any] = {}
    for label, ts_side, ch_side, other_ts, other_ch in (
        ('a', ts_a, ch_a, ts_b_aligned, ch_b),
        ('b', ts_b_aligned, ch_b, ts_a, ch_a),
    ):
        per_channel = {}
        for ch in (1, 2, 3, 4):
            subset = np.sort(ts_side[ch_side == ch].astype(np.int64))
            other_sorted = np.sort(other_ts[other_ch == ch].astype(np.int64))
            if subset.size < 2 or other_sorted.size < 2:
                per_channel[str(ch)] = {'offset_ps': 0, 'score': 0.0, 'n': int(subset.size)}
                continue
            lag_ps = int(subset[0]) - int(other_sorted[0])
            hist = _refine_candidate(subset, other_sorted, lag_ps, radius_ps, fine_bin_ps, center_limit_ps)
            per_channel[str(ch)] = {
                'offset_ps': int(round(hist.window.center_ps)),
                'score': float(hist.window.score),
                'n': int(subset.size),
            }
        results[label] = per_channel
    return results


class _SkewUnavailable(Exception):
    pass


def _fit_clock_skew(
    a: np.ndarray,
    b: np.ndarray,
    lag_ps: int,
    radius_ps: int,
    fine_bin_ps: int,
    segments: int,
) -> dict[str, Any]:
    a_origin = int(a[0])
    b_origin = int(b[0])
    duration_a = int(a[-1]) - a_origin
    duration_b = int(b[-1]) - b_origin
    overlap_start = max(0, -lag_ps)
    overlap_end = min(duration_a, duration_b - lag_ps)
    if overlap_end <= overlap_start:
        raise AnalysisError("the selected lag has no timestamp overlap")

    boundaries = np.linspace(overlap_start, overlap_end, segments + 1, dtype=np.int64)

    def _fit_one_segment(start_ps: int, end_ps: int) -> tuple[int, float, float] | None:
        left = int(np.searchsorted(a, a_origin + int(start_ps), side="left"))
        right = int(np.searchsorted(a, a_origin + int(end_ps), side="left"))
        if right - left < 2:
            return None
        histogram = _difference_histogram(
            a[left:right], b, a_origin, b_origin, lag_ps, radius_ps, fine_bin_ps
        )
        window = _best_window(
            histogram,
            radius_ps,
            fine_bin_ps,
            center_limit_ps=min(radius_ps // 2, 500 * PS_PER_NS),
        )
        if window.score < MIN_SEGMENT_SCORE:
            return None
        local_lag_ps = int(round(lag_ps + window.center_ps))
        half_window_ps = max(fine_bin_ps, window.width_ps // 2)
        central_count, central_sum = _pair_elapsed_moments(
            a[left:right],
            b,
            a_origin,
            b_origin,
            local_lag_ps,
            half_window_ps,
        )
        side_offset_ps = min(
            radius_ps - half_window_ps,
            max(5 * window.width_ps, 200 * PS_PER_NS),
        )
        left_count, left_sum = _pair_elapsed_moments(
            a[left:right],
            b,
            a_origin,
            b_origin,
            local_lag_ps - side_offset_ps,
            half_window_ps,
        )
        right_count, right_sum = _pair_elapsed_moments(
            a[left:right],
            b,
            a_origin,
            b_origin,
            local_lag_ps + side_offset_ps,
            half_window_ps,
        )
        background_count = (left_count + right_count) / 2
        excess_count = central_count - background_count
        if excess_count <= 0:
            return None
        pair_elapsed_ps = (
            central_sum - (left_sum + right_sum) / 2
        ) / excess_count
        if not start_ps <= pair_elapsed_ps < end_ps:
            return None
        return int(round(pair_elapsed_ps)), float(local_lag_ps), window.score
    # Same independence + GIL-releasing-numba reasoning as the candidate
    # refinement above: each segment's fit only touches its own slice of a
    # (plus the full, read-only b), so segments run concurrently in threads
    with ThreadPoolExecutor(max_workers=min(32, os.cpu_count() or 4)) as pool:
        segment_results = list(pool.map(
            lambda bounds: _fit_one_segment(*bounds),
            zip(boundaries[:-1].tolist(), boundaries[1:].tolist()),
        ))

    elapsed_ps: list[int] = []
    local_lags_ps: list[float] = []
    scores: list[float] = []
    for result in segment_results:
        if result is None:
            continue
        segment_elapsed, segment_lag, segment_score = result
        elapsed_ps.append(segment_elapsed)
        local_lags_ps.append(segment_lag)
        scores.append(segment_score)

    if len(elapsed_ps) < 3:
        raise _SkewUnavailable(
            "fewer than three segments contain a significant coincidence peak; "
            "clock skew cannot be estimated reliably"
        )

    x_ps = np.asarray(elapsed_ps, dtype=np.float64)
    y_ps = np.asarray(local_lags_ps, dtype=np.float64)
    weights = np.sqrt(np.maximum(np.asarray(scores), 1.0))
    reference_ps = int(round(float(np.mean(x_ps))))
    x_s = (x_ps - reference_ps) / PS_PER_S
    slope, intercept = np.polyfit(x_s, y_ps, 1, w=weights)
    residuals = y_ps - (intercept + slope * x_s)
    if len(y_ps) >= 5:
        residual_mad = np.median(np.abs(residuals - np.median(residuals)))
        if residual_mad > 0:
            keep = np.abs(residuals - np.median(residuals)) <= 4 * 1.4826 * residual_mad
            if np.count_nonzero(keep) >= 3 and not np.all(keep):
                slope, intercept = np.polyfit(
                    x_s[keep], y_ps[keep], 1, w=weights[keep]
                )

    return {
        "reference_elapsed_ps": reference_ps,
        "lag_at_reference_ps": float(intercept),
        "slope_ps_per_s": float(slope),
        "skew_fraction": float(slope / PS_PER_S),
        "skew_ppb": float(slope / 1_000.0),
        "skew_method": "segment_fit",
        "segment_elapsed_ps": [int(value) for value in elapsed_ps],
        "segment_lags_ps": [float(value) for value in local_lags_ps],
        "segment_scores": [float(value) for value in scores],
    }


def _grid_search_clock_skew(
    a: np.ndarray,
    b: np.ndarray,
    lag_ps: int,
    fine_bin_ps: int,
    segments: int,
    slope_range_ppb: float = 200.0,
    grid_steps: int = 25,
) -> dict[str, Any]:
    """Fallback skew estimate maximizing the drift-corrected window score"""
    a_origin = int(a[0])
    b_origin = int(b[0])
    duration_a = int(a[-1]) - a_origin
    boundaries = np.linspace(0, duration_a, segments + 1, dtype=np.int64)
    reference_ps = int(round(sum((int(s) + int(e)) / 2 for s, e in zip(boundaries[:-1], boundaries[1:])) / segments))
    radius_ps = max(2_000 * PS_PER_NS, 200 * fine_bin_ps)
    best: dict[str, Any] | None = None
    for slope_ppb in np.linspace(-slope_range_ppb, slope_range_ppb, grid_steps):
        slope_ps_per_s = float(slope_ppb * 1_000.0)
        counts = _drift_corrected_histogram(
            a, b, a_origin, b_origin,
            float(lag_ps), slope_ps_per_s, reference_ps,
            radius_ps, fine_bin_ps,
        )
        window = _best_window(counts, radius_ps, fine_bin_ps, center_limit_ps=200 * PS_PER_NS)
        if best is None or window.score > best["_score"]:
            best = {"_score": window.score, "slope_ps_per_s": slope_ps_per_s, "window": window}
    assert best is not None
    window = best["window"]
    slope = best["slope_ps_per_s"]
    return {
        "reference_elapsed_ps": reference_ps,
        "lag_at_reference_ps": float(lag_ps) + float(window.center_ps),
        "slope_ps_per_s": float(slope),
        "skew_fraction": float(slope / PS_PER_S),
        "skew_ppb": float(slope / 1_000.0),
        "skew_method": "grid_search",
        "segment_elapsed_ps": [],
        "segment_lags_ps": [],
        "segment_scores": [],
    }


def _fallback_skew(a: np.ndarray, lag_ps: int) -> dict[str, Any]:
    """Placeholder skew block when no significant peak was found"""
    reference_ps = (int(a[-1]) - int(a[0])) // 2
    return {
        "reference_elapsed_ps": int(reference_ps),
        "lag_at_reference_ps": float(lag_ps),
        "slope_ps_per_s": 0.0,
        "skew_fraction": 0.0,
        "skew_ppb": 0.0,
        "skew_method": "none",
        "segment_elapsed_ps": [],
        "segment_lags_ps": [],
        "segment_scores": [],
    }


def _window_dict(window: WindowResult) -> dict[str, Any]:
    return {
        "center_ps": float(window.center_ps),
        "width_ps": int(window.width_ps),
        "count": int(window.count),
        "estimated_accidentals": float(window.background),
        "estimated_excess": float(window.excess),
        "local_score": float(window.score),
    }


def analyze_arrays(
    a: np.ndarray,
    b: np.ndarray,
    *,
    max_lag_ps: int = PS_PER_S,
    coarse_bin_ps: int = 100 * PS_PER_NS,
    fine_bin_ps: int = 100,
    candidate_count: int = 8,
    segments: int = 6,
    refine_radius_ps: int = 2_000 * PS_PER_NS,
    max_memory_gib: float = 4.0,
    slope_range_ppb: float = 200.0,
    fit_skew: bool = True,
    strict: bool = True,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Core coincidence-peak search over two already-loaded timestamp arrays
    analyze_files wraps this with file loading/validation; a live monitor
    running alongside acquisition can call this directly on an in-memory
    snapshot of the recording buffered so far
    fit_skew=False skips the segment fit and its O(N) grid-search fallback,
    for a live check that must stay fast even with no peak found yet
    """
    if fine_bin_ps > refine_radius_ps:
        raise AnalysisError(
            "fine bin width must not exceed the refinement radius"
        )
    if (2 * refine_radius_ps) % fine_bin_ps:
        raise AnalysisError(
            "twice the refinement radius must be divisible by the fine bin width"
        )

    coarse = _coarse_search(
        a,
        b,
        coarse_bin_ps,
        max_lag_ps,
        candidate_count=candidate_count,
        max_memory_gib=max_memory_gib,
    )
    log('INFO', "refining the coarse search's top candidate lags using exact (not binned) timestamp differences")
    # 80% of total runtime, measured; candidates are independent and the
    # @njit scan releases the GIL, so threads give real parallelism here
    center_limit_ps = max(2 * coarse_bin_ps, 200 * PS_PER_NS)
    with ThreadPoolExecutor(max_workers=min(32, os.cpu_count() or 4)) as pool:
        refined = list(pool.map(
            lambda lag: _refine_candidate(a, b, lag, refine_radius_ps, fine_bin_ps, center_limit_ps),
            coarse["candidate_lags_ps"],
        ))
    refined = [
        item
        for item in refined
        if abs(item.lag_ps + item.window.center_ps) <= max_lag_ps
    ]
    if not refined:
        raise AnalysisError("no refined candidate remains inside the lag range")
    selected = max(refined, key=lambda item: item.window.score)
    detected = selected.window.score >= MIN_PEAK_SCORE
    lag_ps = int(round(selected.lag_ps + selected.window.center_ps))
    for _ in range(3):
        selected = _refine_candidate(
            a,
            b,
            lag_ps,
            refine_radius_ps,
            fine_bin_ps,
            center_limit_ps=max(2 * coarse_bin_ps, 200 * PS_PER_NS),
        )
        adjustment_ps = int(round(selected.window.center_ps))
        lag_ps += adjustment_ps
        if abs(adjustment_ps) <= fine_bin_ps // 2:
            break
    selected = _refine_candidate(
        a,
        b,
        lag_ps,
        refine_radius_ps,
        fine_bin_ps,
        center_limit_ps=max(2 * coarse_bin_ps, 200 * PS_PER_NS),
    )
    final_adjustment_ps = int(round(selected.window.center_ps))
    if abs(final_adjustment_ps) > fine_bin_ps // 2:
        lag_ps += final_adjustment_ps
        selected = _refine_candidate(
            a,
            b,
            lag_ps,
            refine_radius_ps,
            fine_bin_ps,
            center_limit_ps=max(2 * coarse_bin_ps, 200 * PS_PER_NS),
        )
    constant_lag_ps = lag_ps
    if abs(constant_lag_ps) > max_lag_ps:
        raise AnalysisError("refined coincidence peak lies outside the lag range")

    if detected and fit_skew:
        try:
            skew = _fit_clock_skew(
                a,
                b,
                constant_lag_ps,
                refine_radius_ps,
                fine_bin_ps,
                segments,
            )
        except _SkewUnavailable as exc:
            log('WARN', f"could not fit clock skew from per-segment peaks ({exc}); falling back to a slower slope grid search")
            skew = _grid_search_clock_skew(
                a,
                b,
                constant_lag_ps,
                fine_bin_ps,
                segments,
                slope_range_ppb=slope_range_ppb,
            )
    else:
        skew = _fallback_skew(a, constant_lag_ps)
    corrected_counts = _drift_corrected_histogram(
        a,
        b,
        int(a[0]),
        int(b[0]),
        skew["lag_at_reference_ps"],
        skew["slope_ps_per_s"],
        skew["reference_elapsed_ps"],
        refine_radius_ps,
        fine_bin_ps,
    )
    corrected_window = _best_window(
        corrected_counts,
        refine_radius_ps,
        fine_bin_ps,
        center_limit_ps=200 * PS_PER_NS,
    )
    skew["lag_at_reference_ps"] += corrected_window.center_ps
    corrected_counts = _drift_corrected_histogram(
        a,
        b,
        int(a[0]),
        int(b[0]),
        skew["lag_at_reference_ps"],
        skew["slope_ps_per_s"],
        skew["reference_elapsed_ps"],
        refine_radius_ps,
        fine_bin_ps,
    )
    corrected_window = _best_window(
        corrected_counts,
        refine_radius_ps,
        fine_bin_ps,
        center_limit_ps=200 * PS_PER_NS,
    )

    origin_difference_ps = int(b[0]) - int(a[0])
    constant_delay_ps = origin_difference_ps + constant_lag_ps
    affine_delay_ps = origin_difference_ps + int(round(skew["lag_at_reference_ps"]))
    report: dict[str, Any] = {
        "sign_convention": "delay is B minus A; add shift_to_add_to_b_ps to B",
        "detected": detected,
        "detection_threshold": MIN_PEAK_SCORE,
        "parameters": {
            "max_lag_ps": int(max_lag_ps),
            "coarse_bin_ps": int(coarse["coarse_bin_ps"]),
            "requested_coarse_bin_ps": int(coarse_bin_ps),
            "fine_bin_ps": int(fine_bin_ps),
            "candidate_count": int(candidate_count),
            "segments": int(segments),
            "refine_radius_ps": int(refine_radius_ps),
            "max_memory_gib": float(max_memory_gib),
            "slope_range_ppb": float(slope_range_ppb),
        },
        "constant_shift": {
            "relative_lag_ps": constant_lag_ps,
            "delay_b_minus_a_ps": constant_delay_ps,
            "shift_to_add_to_b_ps": -constant_delay_ps,
            "window": _window_dict(selected.window),
        },
        "clock_skew": {
            **skew,
            "delay_b_minus_a_at_reference_ps": affine_delay_ps,
            "shift_to_add_to_b_at_reference_ps": -affine_delay_ps,
            "corrected_window": _window_dict(corrected_window),
            "alignment_formula": (
                "A_est = A0 + tref + ((B - B0) - (tref + lag_ref)) / "
                "(1 + skew_fraction)"
            ),
        },
        "detection_limits": _upper_limits(a, b, constant_lag_ps),
        "coarse_search": {
            "candidate_lags_ps": coarse["candidate_lags_ps"],
            "candidate_scores": coarse["candidate_scores"],
            "n_bins": coarse["n_bins"],
            "n_fft": coarse["n_fft"],
            "estimated_memory_gib": coarse["estimated_memory_gib"],
        },
    }
    diagnostics = {
        "coarse_lags_ps": coarse["plot_lags_ps"],
        "coarse_scores": coarse["plot_scores"],
        "fine_histogram": selected,
        "corrected_counts": corrected_counts,
        "corrected_window": corrected_window,
    }
    if strict and not detected:
        raise AnalysisError(
            f"no significant coincidence peak found (best local score "
            f"{selected.window.score:.2f}, required {MIN_PEAK_SCORE:.2f})"
        )
    return report, diagnostics


def analyze_files(
    path_a: Path,
    path_b: Path,
    *,
    use_meta: bool = False,
    overlap_margin_s: float = 2.0,
    **kwargs: Any,
) -> tuple[dict[str, Any], dict[str, Any]]:
    """Loads and validates two timestamp NPY files, then runs analyze_arrays
    over them, adding the file/overlap-specific report fields it doesn't know
    about (input_a/input_b, overlap, use_meta, overlap_margin_s)
    """
    raw_a = _validate_timestamps(path_a)
    raw_b = _validate_timestamps(path_b)
    log('INFO', f"A: {path_a} ({len(raw_a)} timestamps)")
    log('INFO', f"B: {path_b} ({len(raw_b)} timestamps)")
    overlap_info: dict[str, Any] | None = None
    if use_meta:
        a, b, overlap_info = slice_to_wall_overlap(
            np.asarray(raw_a), np.asarray(raw_b), path_a, path_b, overlap_margin_s
        )
        log('INFO',
            f"kept only the wall-clock overlap between the two recordings: A {len(a)}/{len(raw_a)} "
            f"({overlap_info['kept_fraction_a']:.1%}), B {len(b)}/{len(raw_b)} ({overlap_info['kept_fraction_b']:.1%})")
    else:
        a = np.asarray(raw_a)
        b = np.asarray(raw_b)

    report, diagnostics = analyze_arrays(a, b, **kwargs)
    report["input_a"] = str(path_a)
    report["input_b"] = str(path_b)
    report["overlap"] = overlap_info
    report["parameters"]["use_meta"] = bool(overlap_info is not None)
    report["parameters"]["overlap_margin_s"] = float(overlap_margin_s)

    ch_path_a, ch_path_b = _channel_path(path_a), _channel_path(path_b)
    if ch_path_a.is_file() and ch_path_b.is_file():
        ch_a = np.load(ch_path_a, allow_pickle=False)
        ch_b = np.load(ch_path_b, allow_pickle=False)
        shift = _shift_to_a_from_report(report, int(raw_a[0]), int(raw_b[0]))
        report["channel_delays"] = calibrate_channel_delays(np.asarray(raw_a), ch_a, np.asarray(raw_b), ch_b, shift)
        for side, chs in report["channel_delays"].items():
            summary = ", ".join(f"ch{ch}={d['offset_ps']:+d}ps(score={d['score']:.1f})" for ch, d in chs.items())
            log('INFO', f"per-channel delay calibration, side {side}: {summary}")
    else:
        report["channel_delays"] = None
    return report, diagnostics


def _shift_to_a_from_report(report: dict[str, Any], a0: int, b0: int):
    '''Builds the same B-onto-A timestamp shift cmd/importtt applies, from
    this report's own fitted lag+skew, for calibrate_channel_delays to reuse'''
    skew = report["clock_skew"]
    ref, lag_ref, frac = skew["reference_elapsed_ps"], skew["lag_at_reference_ps"], skew["skew_fraction"]

    def shift(ts_b: np.ndarray) -> np.ndarray:
        # int64 subtraction first: raw ts are 1e18, past float64's exact
        # range, so casting before subtracting b0 rounds to the nearest 512ps
        b_rel = (ts_b.astype(np.int64) - np.int64(b0)).astype(np.float64)
        a_rel = ref + (b_rel - (ref + lag_ref)) / (1 + frac)
        return (a0 + np.round(a_rel)).astype(np.int64)

    return shift


def _format_span(ps: float) -> str:
    """Human-readable duration: ps/ns/us/ms as appropriate"""
    value = float(ps)
    for unit, factor in (("ms", PS_PER_MS), ("us", 1_000 * PS_PER_NS), ("ns", PS_PER_NS)):
        if value >= factor and value % factor == 0:
            return f"{value / factor:g} {unit}"
    if value >= PS_PER_NS:
        return f"{value / PS_PER_NS:g} ns"
    return f"{value:g} ps"


def _peak_xlim_ns(
    center_ps: float, width_ps: float, bin_ps: int, radius_ps: int
) -> tuple[float, float]:
    """X limits (ns) for a peak panel, scaled to the detected window
    The shaded window occupies about 25% of the panel width (half-span is four half-widths)
    with a floor of 5 ns / 50 bins so sub-ns windows still show surrounding background, clipped to histogram radius
    """
    half_width_ps = width_ps / 2
    half_span_ps = max(4 * half_width_ps, 5 * PS_PER_NS, 50 * bin_ps)
    half_span_ps = min(half_span_ps, radius_ps)
    lo_ps = max(center_ps - half_span_ps, -radius_ps)
    hi_ps = min(center_ps + half_span_ps, radius_ps)
    return lo_ps / PS_PER_NS, hi_ps / PS_PER_NS


def save_plot(report: dict[str, Any], diagnostics: dict[str, Any], path: Path) -> None:
    import matplotlib

    matplotlib.use("Agg")
    import matplotlib.pyplot as plt

    fine: HistogramResult = diagnostics["fine_histogram"]
    corrected_counts: np.ndarray = diagnostics["corrected_counts"]
    corrected_window: WindowResult = diagnostics["corrected_window"]
    skew = report["clock_skew"]

    figure, axes = plt.subplots(2, 2, figsize=(13, 9), constrained_layout=True)
    coarse_bin = _format_span(report["parameters"]["coarse_bin_ps"])
    fine_bin = _format_span(fine.bin_ps)
    coarse_lags_ms = diagnostics["coarse_lags_ps"] / PS_PER_MS
    axes[0, 0].plot(coarse_lags_ms, diagnostics["coarse_scores"], linewidth=0.7)
    axes[0, 0].axvline(
        report["constant_shift"]["relative_lag_ps"] / PS_PER_MS,
        color="tab:red",
        linewidth=1,
    )
    axes[0, 0].set(title=f"Coarse correlation ({coarse_bin}/bin)", xlabel="Relative lag B - A (ms)", ylabel="Robust score")

    centers_ns = (
        -fine.radius_ps + (np.arange(len(fine.counts)) + 0.5) * fine.bin_ps
    ) / PS_PER_NS
    axes[0, 1].plot(centers_ns, fine.counts, linewidth=0.8)
    half_width_ns = fine.window.width_ps / (2 * PS_PER_NS)
    center_ns = fine.window.center_ps / PS_PER_NS
    axes[0, 1].axvspan(center_ns - half_width_ns, center_ns + half_width_ns, color="tab:red", alpha=0.2)
    axes[0, 1].set_xlim(*_peak_xlim_ns(fine.window.center_ps, fine.window.width_ps, fine.bin_ps, fine.radius_ps))
    axes[0, 1].set(title=f"Best constant-shift peak ({fine_bin}/bin, window {_format_span(fine.window.width_ps)})", xlabel="Residual (ns)", ylabel="Pair count / bin")

    elapsed_s = np.asarray(skew["segment_elapsed_ps"], dtype=np.float64) / PS_PER_S
    local_ns = (
        np.asarray(skew["segment_lags_ps"], dtype=np.float64) - skew["lag_at_reference_ps"]
    ) / PS_PER_NS
    reference_s = skew["reference_elapsed_ps"] / PS_PER_S
    fit_ns = skew["slope_ps_per_s"] * (elapsed_s - reference_s) / PS_PER_NS
    if len(elapsed_s):
        axes[1, 0].scatter(elapsed_s, local_ns, color="black", s=28, label="Segment peaks")
        axes[1, 0].plot(elapsed_s, fit_ns, color="tab:red", label=f"{skew['skew_ppb']:.2f} ppb")
    else:
        axes[1, 0].plot(elapsed_s, fit_ns, color="tab:red", label=f"{skew['skew_ppb']:.2f} ppb ({skew.get('skew_method', '?')})")
    axes[1, 0].legend()
    axes[1, 0].set(title="Clock-rate drift", xlabel="Elapsed time (s)", ylabel="Lag relative to reference (ns)")

    corrected_centers_ns = (
        -fine.radius_ps + (np.arange(len(corrected_counts)) + 0.5) * fine.bin_ps
    ) / PS_PER_NS
    axes[1, 1].plot(corrected_centers_ns, corrected_counts, linewidth=0.8)
    half_width_ns = corrected_window.width_ps / (2 * PS_PER_NS)
    center_ns = corrected_window.center_ps / PS_PER_NS
    axes[1, 1].axvspan(center_ns - half_width_ns, center_ns + half_width_ns, color="tab:green", alpha=0.2)
    axes[1, 1].set_xlim(*_peak_xlim_ns(corrected_window.center_ps, corrected_window.width_ps, fine.bin_ps, fine.radius_ps))
    axes[1, 1].set(title=f"Clock-corrected peak ({fine_bin}/bin, window {_format_span(corrected_window.width_ps)})", xlabel="Residual (ns)", ylabel="Pair count / bin")

    input_a = Path(report["input_a"])
    input_b = Path(report["input_b"])
    if input_a.parent == input_b.parent:
        title = input_a.parent.name
    else:
        title = f"{input_a.stem} vs {input_b.stem}"
    status = "DETECTED" if report.get("detected", True) else "NO SIGNIFICANT PEAK"
    figure.suptitle(f"Coincidence analysis: {title} [{status}]")
    figure.savefig(path, dpi=160)
    plt.close(figure)


def parse_args(argv: Optional[list] = None) -> argparse.Namespace:
    '''CLI for analyze_files: one recording directory or two timestamp NPY
    files, plus the search tuning options. Each tuning option defaults to
    None here so --config's values (if given) can supply the real default
    before this module's own hardcoded fallback applies; see main()'''
    parser = argparse.ArgumentParser(description="Analyze one recording directory or two timestamp sequence NPY files")
    parser.add_argument("inputs", nargs="*", type=Path)
    parser.add_argument("--config", type=Path, default=None,
                         help='JSON file (see acquisition/config.json\'s "coincidence_peak" section) '
                              'supplying default values for the options below; explicit flags still override it.')
    parser.add_argument("--max-lag-ms", type=float, default=None)
    parser.add_argument("--coarse-bin-ns", type=float, default=None)
    parser.add_argument("--fine-bin-ps", type=int, default=None)
    parser.add_argument("--candidates", type=int, default=None)
    parser.add_argument("--segments", type=int, default=None)
    parser.add_argument("--refine-radius-ns", type=float, default=None)
    parser.add_argument("--output-dir", type=Path, default=None,
                         help="Directory for the JSON/PNG reports. Defaults to the input "
                              "recording directory; required when passing two files explicitly.")
    plot_group = parser.add_mutually_exclusive_group()
    plot_group.add_argument("--plot", dest="plot", action="store_true", default=None)
    plot_group.add_argument("--no-plot", dest="plot", action="store_false")
    meta_group = parser.add_mutually_exclusive_group()
    meta_group.add_argument("--meta", dest="use_meta", action="store_true", default=None,
                             help="Restrict both streams to their wall-clock overlap from *_meta.json.")
    meta_group.add_argument("--no-meta", dest="use_meta", action="store_false")
    parser.add_argument("--overlap-margin-s", type=float, default=None)
    parser.add_argument("--max-memory-gib", type=float, default=None)
    parser.add_argument("--slope-range-ppb", type=float, default=None)
    strict_group = parser.add_mutually_exclusive_group()
    strict_group.add_argument("--strict", dest="strict", action="store_true", default=None,
                               help="Exit nonzero when no peak passes the detection threshold.")
    strict_group.add_argument("--no-strict", dest="strict", action="store_false")
    return parser.parse_args(argv)


def _resolve_option(cli_value: Any, cfg: dict, key: str, default: Any) -> Any:
    '''--flag (if given) overrides --config's value (if given), which
    overrides this module's own hardcoded default'''
    return cli_value if cli_value is not None else cfg.get(key, default)


def main(argv: Optional[list] = None) -> int:
    """Analyze one recording directory or two timestamp sequence NPY files"""
    args = parse_args(argv)

    cfg: dict = {}
    if args.config is not None:
        try:
            cfg = json.loads(args.config.read_text(encoding="utf-8")).get("coincidence_peak", {})
        except (OSError, ValueError) as exc:
            log('ERROR', f"{args.config}: {exc}")
            return 1

    for path in args.inputs:
        if not path.exists():
            log('ERROR', f"{path}: no such file or directory")
            return 1

    max_lag_ms = _resolve_option(args.max_lag_ms, cfg, "max_lag_ms", 1_000.0)
    coarse_bin_ns = _resolve_option(args.coarse_bin_ns, cfg, "coarse_bin_ns", 100.0)
    fine_bin_ps = _resolve_option(args.fine_bin_ps, cfg, "fine_bin_ps", 100)
    candidates = _resolve_option(args.candidates, cfg, "candidates", 16)
    segments = _resolve_option(args.segments, cfg, "segments", 6)
    refine_radius_ns = _resolve_option(args.refine_radius_ns, cfg, "refine_radius_ns", 2_000.0)
    output_dir = args.output_dir
    plot = _resolve_option(args.plot, cfg, "plot", True)
    use_meta = _resolve_option(args.use_meta, cfg, "use_meta", False)
    overlap_margin_s = _resolve_option(args.overlap_margin_s, cfg, "overlap_margin_s", 2.0)
    max_memory_gib = _resolve_option(args.max_memory_gib, cfg, "max_memory_gib", 4.0)
    slope_range_ppb = _resolve_option(args.slope_range_ppb, cfg, "slope_range_ppb", 200.0)
    strict = _resolve_option(args.strict, cfg, "strict", True)

    if max_lag_ms <= 0:
        log('ERROR', "--max-lag-ms must be positive")
        return 1
    if coarse_bin_ns <= 0:
        log('ERROR', "--coarse-bin-ns must be positive")
        return 1
    if fine_bin_ps < 1:
        log('ERROR', "--fine-bin-ps must be at least 1")
        return 1
    if candidates < 1:
        log('ERROR', "--candidates must be at least 1")
        return 1
    if segments < 3:
        log('ERROR', "--segments must be at least 3")
        return 1
    if refine_radius_ns < 10:
        log('ERROR', "--refine-radius-ns must be at least 10")
        return 1
    if overlap_margin_s < 0:
        log('ERROR', "--overlap-margin-s cannot be negative")
        return 1
    if max_memory_gib < 0.1:
        log('ERROR', "--max-memory-gib must be at least 0.1")
        return 1
    if slope_range_ppb <= 0:
        log('ERROR', "--slope-range-ppb must be positive")
        return 1

    try:
        path_a, path_b, result_name = resolve_inputs(args.inputs)
        if len(args.inputs) == 1 and args.inputs[0].is_dir():
            resolved_output_dir = output_dir if output_dir is not None else path_a.parent
        elif output_dir is None:
            raise AnalysisError("--output-dir is required when passing two timestamp files explicitly")
        else:
            resolved_output_dir = output_dir
        report, diagnostics = analyze_files(
            path_a,
            path_b,
            max_lag_ps=int(round(max_lag_ms * PS_PER_MS)),
            coarse_bin_ps=int(round(coarse_bin_ns * PS_PER_NS)),
            fine_bin_ps=fine_bin_ps,
            candidate_count=candidates,
            segments=segments,
            refine_radius_ps=int(round(refine_radius_ns * PS_PER_NS)),
            use_meta=use_meta,
            overlap_margin_s=overlap_margin_s,
            max_memory_gib=max_memory_gib,
            slope_range_ppb=slope_range_ppb,
            strict=strict,
        )
    except AnalysisError as exc:
        log('ERROR', str(exc))
        return 1

    resolved_output_dir.mkdir(parents=True, exist_ok=True)
    report_path = resolved_output_dir / f"{result_name}_coincidence.json"
    report_path.write_text(json.dumps(report, indent=2) + "\n", encoding="utf-8")
    plot_path = resolved_output_dir / f"{result_name}_coincidence.png"
    if plot:
        save_plot(report, diagnostics, plot_path)

    constant = report["constant_shift"]
    skew = report["clock_skew"]
    detected = report.get("detected", True)
    print()
    if detected:
        log('INFO', f"[OK] coincidence peak found (score {constant['window']['local_score']:.2f}, "
                    f"required {MIN_PEAK_SCORE:.2f}); the two recordings are the same quantum link")
    else:
        log('WARN', f"no coincidence peak found (best score {constant['window']['local_score']:.2f}, "
                        f"required {MIN_PEAK_SCORE:.2f}); treat the numbers below as unreliable")
    log('INFO', f"relative lag (B minus A): {constant['relative_lag_ps'] / PS_PER_MS:.9f} ms")
    log('INFO', f"shift to add to B's timestamps to align them onto A's clock: {constant['shift_to_add_to_b_ps']} ps")
    log('INFO', f"coincidence window used for the lag search: {constant['window']['width_ps'] / PS_PER_NS:g} ns")
    log('INFO', f"clock skew between the two TimeTaggers: {skew['skew_ppb']:.3f} ppb ({skew['slope_ps_per_s']:.1f} ps/s), fitted by {skew.get('skew_method', '?')}")
    log('INFO', f"tighter window achievable once clock skew is corrected for: {skew['corrected_window']['width_ps'] / PS_PER_NS:g} ns")
    log('INFO', f"full report written to {report_path}")
    if plot:
        log('INFO', f"plot written to {plot_path}")
    if strict and not detected:
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
