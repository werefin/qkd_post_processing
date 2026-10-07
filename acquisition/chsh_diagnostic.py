#!/usr/bin/env python3
"""Standalone CHSH/Bell diagnostic over a dual TimeTagger recording, per block singles per channel plus ROI1/ROI2 coincidence counts per channel pair"""

from __future__ import annotations

import argparse
import csv
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Optional

import numpy as np
from numba import njit

PS_PER_NS = 1_000
PS_PER_S = 1_000_000_000_000


class AnalysisError(Exception):
    '''A user-facing input/data problem, as opposed to an internal bug;
    the CLI entry point catches this and prints it as an [ERROR] line'''


def log(level: str, msg: str) -> None:
    '''arnika-style log line: "[LEVEL] message", one severity tag per line'''
    print(f'[{level}] {msg}', flush=True)

# Same physical channel wiring cmd/importtt/main.go channelToBasisBit
# assumes: 1-2 one polarization basis, 3-4 the other
CHANNEL_LABELS = {1: "H", 2: "V", 3: "D", 4: "A"}
MEASUREMENT_CHANNELS = (1, 2, 3, 4)


@njit(cache=True)
def _block_singles(timestamps: np.ndarray, origin_ps: int, block_ps: int, n_blocks: int) -> np.ndarray:
    counts = np.zeros(n_blocks, dtype=np.int64)
    for ts in timestamps:
        index = (ts - origin_ps) // block_ps
        if 0 <= index < n_blocks:
            counts[index] += 1
    return counts


@njit(cache=True)
def _block_roi_coincidences(
    a: np.ndarray,
    b: np.ndarray,
    a_origin_ps: int,
    block_ps: int,
    n_blocks: int,
    roi_center_ps: int,
    half_width_ps: int,
) -> np.ndarray:
    """Counts b timestamps within the ROI window for each a timestamp, bucketed by block,
    both arrays must be sorted ascending and already on a common clock"""
    counts = np.zeros(n_blocks, dtype=np.int64)
    left = 0
    right = 0
    n_b = len(b)
    for ts_a in a:
        lower = ts_a + roi_center_ps - half_width_ps
        upper = ts_a + roi_center_ps + half_width_ps
        while left < n_b and b[left] < lower:
            left += 1
        if right < left:
            right = left
        while right < n_b and b[right] < upper:
            right += 1
        block_index = (ts_a - a_origin_ps) // block_ps
        if 0 <= block_index < n_blocks:
            counts[block_index] += right - left
    return counts


@dataclass
class ROI:
    center_ps: int
    width_ps: int

    @property
    def half_width_ps(self) -> int:
        return self.width_ps // 2


def _find_recording(directory: Path) -> tuple[Path, Path, dict[str, Any]]:
    """Finds the two timestamp npy files and the coincidence json report in directory,
    mirrors cmd/importtt/main.go findRecording"""
    ts_files = sorted(directory.glob("*_timestamp_sequence.npy"))
    if len(ts_files) != 2:
        raise AnalysisError(
            f"{directory}: expected exactly two *_timestamp_sequence.npy files, "
            f"found {len(ts_files)}"
        )
    coincidence_files = sorted(directory.glob("*_coincidence.json"))
    if len(coincidence_files) != 1:
        raise AnalysisError(
            f"{directory}: expected exactly one *_coincidence.json report, found "
            f"{len(coincidence_files)}; run coincidence_peak.py on this directory first"
        )
    report = json.loads(coincidence_files[0].read_text(encoding="utf-8"))
    if not report.get("detected", False):
        raise AnalysisError(
            f"{coincidence_files[0]}: no significant coincidence peak was detected; "
            "the clock alignment in this report is not trustworthy"
        )

    a_base = Path(report["input_a"]).name
    b_base = Path(report["input_b"]).name
    ts_a = ts_b = None
    for path in ts_files:
        if path.name == a_base:
            ts_a = path
        elif path.name == b_base:
            ts_b = path
    if ts_a is None or ts_b is None:
        ts_a, ts_b = ts_files
    return ts_a, ts_b, report


def _channel_path(timestamp_path: Path) -> Path:
    return timestamp_path.with_name(
        timestamp_path.name.removesuffix("_timestamp_sequence.npy") + "_channel_sequence.npy"
    )


def _shift_to_a_clock(timestamps_b: np.ndarray, a0: int, b0: int, clock_skew: dict[str, Any]) -> np.ndarray:
    """Maps a B timestamp onto A's clock using the fitted lag and clock skew,
    same affine model as cmd/importtt/main.go toAliceClock"""
    reference_ps = clock_skew["reference_elapsed_ps"]
    lag_at_reference_ps = clock_skew["lag_at_reference_ps"]
    skew_fraction = clock_skew["skew_fraction"]
    # int64 subtraction first: raw ts are ~1e18, past float64's exact
    # range, so casting before subtracting b0 rounds to the nearest 512ps
    b_rel = (timestamps_b.astype(np.int64) - np.int64(b0)).astype(np.float64)
    a_rel = reference_ps + (b_rel - (reference_ps + lag_at_reference_ps)) / (1 + skew_fraction)
    return (a0 + np.round(a_rel)).astype(np.int64)


def _channel_slice(timestamps: np.ndarray, channels: np.ndarray, channel: int) -> np.ndarray:
    return np.ascontiguousarray(timestamps[channels == channel])


def build_blocks(
    directory: Path,
    block_s: float,
    roi1_width_ns: float | None,
    roi1_center_ns: float | None,
    roi2_offset_ns: float,
    roi2_width_ns: float | None,
) -> tuple[dict[str, Any], list[str], list[list[int]]]:
    ts_path_a, ts_path_b, report = _find_recording(directory)
    # mmap_mode='r': a real recording's arrays can be large; np.asarray() on a
    # memmap shares its buffer rather than copying, so this only pulls pages
    # into RAM as block_singles/block_roi_coincidences actually touch them,
    # same approach ui/app.py's _load_timestamps already uses
    ts_a = np.asarray(np.load(ts_path_a, allow_pickle=False, mmap_mode='r'))
    ch_a = np.asarray(np.load(_channel_path(ts_path_a), allow_pickle=False, mmap_mode='r'))
    ts_b = np.asarray(np.load(ts_path_b, allow_pickle=False, mmap_mode='r'))
    ch_b = np.asarray(np.load(_channel_path(ts_path_b), allow_pickle=False, mmap_mode='r'))
    if len(ts_a) != len(ch_a):
        raise AnalysisError(f"{ts_path_a}: {len(ts_a)} timestamps but {len(ch_a)} channels")
    if len(ts_b) != len(ch_b):
        raise AnalysisError(f"{ts_path_b}: {len(ts_b)} timestamps but {len(ch_b)} channels")
    if len(ts_a) == 0 or len(ts_b) == 0:
        raise AnalysisError("empty recording")

    a0, b0 = int(ts_a[0]), int(ts_b[0])
    ts_b_shifted = _shift_to_a_clock(ts_b, a0, b0, report["clock_skew"])

    corrected_window = report["clock_skew"]["corrected_window"]
    roi1 = ROI(
        center_ps=int(round(roi1_center_ns * PS_PER_NS)) if roi1_center_ns is not None else int(round(corrected_window["center_ps"])),
        width_ps=int(round(roi1_width_ns * PS_PER_NS)) if roi1_width_ns is not None else int(corrected_window["width_ps"]),
    )
    roi2 = ROI(
        center_ps=roi1.center_ps + int(round(roi2_offset_ns * PS_PER_NS)),
        width_ps=int(round(roi2_width_ns * PS_PER_NS)) if roi2_width_ns is not None else roi1.width_ps,
    )
    if roi1.width_ps <= 0 or roi2.width_ps <= 0:
        raise AnalysisError("ROI widths must be positive")

    a_channels = {ch: _channel_slice(ts_a, ch_a, ch) for ch in MEASUREMENT_CHANNELS}
    b_channels = {ch: _channel_slice(ts_b_shifted, ch_b, ch) for ch in MEASUREMENT_CHANNELS}

    block_ps = int(round(block_s * PS_PER_S))
    if block_ps <= 0:
        raise AnalysisError("block duration must be positive")
    origin_ps = min(a0, int(ts_b_shifted[0]))
    end_ps = max(int(ts_a[-1]), int(ts_b_shifted[-1]))
    n_blocks = max(1, math.ceil((end_ps - origin_ps + 1) / block_ps))

    columns = ["t_start_s"]
    columns += [f"A_{CHANNEL_LABELS[ch]}_singles" for ch in MEASUREMENT_CHANNELS]
    columns += [f"B_{CHANNEL_LABELS[ch]}_singles" for ch in MEASUREMENT_CHANNELS]
    pair_names = [
        f"A_{CHANNEL_LABELS[a_ch]}_B_{CHANNEL_LABELS[b_ch]}"
        for a_ch in MEASUREMENT_CHANNELS
        for b_ch in MEASUREMENT_CHANNELS
    ]
    columns += [f"roi1_{name}" for name in pair_names]
    columns += [f"roi2_{name}" for name in pair_names]

    singles_columns = []
    for ch in MEASUREMENT_CHANNELS:
        singles_columns.append(_block_singles(a_channels[ch], origin_ps, block_ps, n_blocks))
    for ch in MEASUREMENT_CHANNELS:
        singles_columns.append(_block_singles(b_channels[ch], origin_ps, block_ps, n_blocks))

    roi1_columns = []
    roi2_columns = []
    for a_ch in MEASUREMENT_CHANNELS:
        for b_ch in MEASUREMENT_CHANNELS:
            roi1_columns.append(
                _block_roi_coincidences(
                    a_channels[a_ch], b_channels[b_ch], origin_ps, block_ps, n_blocks,
                    roi1.center_ps, roi1.half_width_ps,
                )
            )
            roi2_columns.append(
                _block_roi_coincidences(
                    a_channels[a_ch], b_channels[b_ch], origin_ps, block_ps, n_blocks,
                    roi2.center_ps, roi2.half_width_ps,
                )
            )

    rows: list[list[int]] = []
    t_starts = (np.arange(n_blocks) * block_ps) / PS_PER_S
    for i in range(n_blocks):
        row = [t_starts[i]]
        row += [int(col[i]) for col in singles_columns]
        row += [int(col[i]) for col in roi1_columns]
        row += [int(col[i]) for col in roi2_columns]
        rows.append(row)

    metadata = {
        "recording_dir": str(directory),
        "input_a": str(ts_path_a),
        "input_b": str(ts_path_b),
        "channel_labels": {str(ch): label for ch, label in CHANNEL_LABELS.items()},
        "block_s": block_s,
        "n_blocks": n_blocks,
        "roi1": {"center_ps": roi1.center_ps, "width_ps": roi1.width_ps},
        "roi2": {"center_ps": roi2.center_ps, "width_ps": roi2.width_ps, "offset_from_roi1_ps": roi2.center_ps - roi1.center_ps},
        "pair_order": pair_names,
        "clock_skew_ppb": report["clock_skew"]["skew_ppb"],
        "note": "roi1 is the real+accidental coincidence window; roi2 is an equal-width "
        "accidental-only baseline offset from the peak; subtract (properly scaled if the "
        "windows differ in width) roi2 from roi1 per pair per block to get the background-"
        "subtracted coincidence count feeding N++/N+-/N-+/N-- per measurement setting.",
    }
    return metadata, columns, rows


def _resolve_option(cli_value: Any, cfg: dict, key: str, default: Any) -> Any:
    '''--flag (if given) overrides --config's value (if given), which
    overrides this module's own hardcoded default'''
    return cli_value if cli_value is not None else cfg.get(key, default)


def parse_args(argv: Optional[list] = None) -> argparse.Namespace:
    '''CLI for build_blocks: a recording directory plus the ROI tuning
    options, each defaulting to None here so --config's values (if given)
    can supply the real default before this module's own fallback applies'''
    parser = argparse.ArgumentParser(
        description="Per-block singles and ROI1/ROI2 coincidence counts for the CHSH analysis; "
                     "DIRECTORY is a recording already processed by tt_record_dual.py and coincidence_peak.py")
    parser.add_argument("directory", type=Path)
    parser.add_argument("--config", type=Path, default=None,
                         help='JSON file (see acquisition/config.json\'s "chsh_diagnostic" section) '
                              'supplying default values for the options below; explicit flags still override it.')
    parser.add_argument("--block-s", type=float, default=None, help="Integration time per output row (default 1.0).")
    parser.add_argument("--roi1-width-ns", type=float, default=None,
                         help="ROI1 width. Defaults to coincidence_peak.py's fitted peak window width.")
    parser.add_argument("--roi1-center-ns", type=float, default=None,
                         help="ROI1 center, relative to the aligned coincidence peak; defaults to "
                              "coincidence_peak.py's fitted peak center (near 0).")
    parser.add_argument("--roi2-offset-ns", type=float, default=None,
                         help="ROI2 center offset from ROI1's center, e.g. one laser repetition period. "
                              "Required (here or via --config); depends on the source's rep rate.")
    parser.add_argument("--roi2-width-ns", type=float, default=None,
                         help="ROI2 width; defaults to the same width as ROI1.")
    parser.add_argument("--output", type=Path, default=None,
                         help="Output CSV path; defaults to <directory>/<directory name>_chsh_blocks.csv.")
    return parser.parse_args(argv)


def main(argv: Optional[list] = None) -> int:
    args = parse_args(argv)

    if not args.directory.is_dir():
        log('ERROR', f"{args.directory}: no such directory")
        return 1

    cfg: dict = {}
    if args.config is not None:
        try:
            cfg = json.loads(args.config.read_text(encoding="utf-8")).get("chsh_diagnostic", {})
        except (OSError, ValueError) as exc:
            log('ERROR', f"{args.config}: {exc}")
            return 1

    block_s = _resolve_option(args.block_s, cfg, "block_s", 1.0)
    roi1_width_ns = _resolve_option(args.roi1_width_ns, cfg, "roi1_width_ns", None)
    roi1_center_ns = _resolve_option(args.roi1_center_ns, cfg, "roi1_center_ns", None)
    roi2_offset_ns = _resolve_option(args.roi2_offset_ns, cfg, "roi2_offset_ns", None)
    roi2_width_ns = _resolve_option(args.roi2_width_ns, cfg, "roi2_width_ns", None)
    output = args.output

    if roi2_offset_ns is None:
        log('ERROR', "--roi2-offset-ns is required (directly, or via --config's chsh_diagnostic.roi2_offset_ns)")
        return 1
    if block_s <= 0:
        log('ERROR', "--block-s must be positive")
        return 1
    for name, value in (("--roi1-width-ns", roi1_width_ns), ("--roi2-width-ns", roi2_width_ns)):
        if value is not None and value <= 0:
            log('ERROR', f"{name} must be positive")
            return 1

    try:
        metadata, columns, rows = build_blocks(
            args.directory, block_s, roi1_width_ns, roi1_center_ns, roi2_offset_ns, roi2_width_ns,
        )
    except AnalysisError as exc:
        log('ERROR', str(exc))
        return 1

    output_path = output if output is not None else args.directory / f"{args.directory.name}_chsh_blocks.csv"
    with output_path.open("w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow(columns)
        writer.writerows(rows)

    metadata_path = output_path.with_suffix(".json")
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")

    log('INFO', f"[OK] {metadata['n_blocks']} blocks of {block_s:g}s each")
    log('INFO', f"ROI1 (signal + accidentals): center {metadata['roi1']['center_ps'] / PS_PER_NS:g} ns, width {metadata['roi1']['width_ps'] / PS_PER_NS:g} ns")
    log('INFO', f"ROI2 (accidentals-only baseline): center {metadata['roi2']['center_ps'] / PS_PER_NS:g} ns, width {metadata['roi2']['width_ps'] / PS_PER_NS:g} ns")
    log('INFO', f"block counts written to {output_path}")
    log('INFO', f"metadata written to {metadata_path}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
