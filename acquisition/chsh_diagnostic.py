#!/usr/bin/env python3
"""Standalone CHSH/Bell diagnostic over a dual TimeTagger recording, per block singles per channel plus ROI1/ROI2 coincidence counts per channel pair"""

from __future__ import annotations

import csv
import json
import math
from dataclasses import dataclass
from pathlib import Path
from typing import Any

import click
import numpy as np
from numba import njit

PS_PER_NS = 1_000
PS_PER_S = 1_000_000_000_000

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
        raise click.ClickException(
            f"{directory}: expected exactly two *_timestamp_sequence.npy files, "
            f"found {len(ts_files)}"
        )
    coincidence_files = sorted(directory.glob("*_coincidence.json"))
    if len(coincidence_files) != 1:
        raise click.ClickException(
            f"{directory}: expected exactly one *_coincidence.json report, found "
            f"{len(coincidence_files)}; run coincidence_peak.py on this directory first"
        )
    report = json.loads(coincidence_files[0].read_text(encoding="utf-8"))
    if not report.get("detected", False):
        raise click.ClickException(
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
    b_rel = (timestamps_b.astype(np.float64) - b0)
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
        raise click.ClickException(f"{ts_path_a}: {len(ts_a)} timestamps but {len(ch_a)} channels")
    if len(ts_b) != len(ch_b):
        raise click.ClickException(f"{ts_path_b}: {len(ts_b)} timestamps but {len(ch_b)} channels")
    if len(ts_a) == 0 or len(ts_b) == 0:
        raise click.ClickException("empty recording")

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
        raise click.ClickException("ROI widths must be positive")

    a_channels = {ch: _channel_slice(ts_a, ch_a, ch) for ch in MEASUREMENT_CHANNELS}
    b_channels = {ch: _channel_slice(ts_b_shifted, ch_b, ch) for ch in MEASUREMENT_CHANNELS}

    block_ps = int(round(block_s * PS_PER_S))
    if block_ps <= 0:
        raise click.ClickException("block duration must be positive")
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
        "accidental-only baseline offset from the peak --> subtract (properly scaled if the "
        "windows differ in width) roi2 from roi1 per pair per block to get the background-"
        "subtracted coincidence count feeding N++/N+-/N-+/N-- per measurement setting.",
    }
    return metadata, columns, rows


def _apply_config_defaults(ctx: click.Context, param: click.Parameter, value: Path | None) -> Path | None:
    """Eager config callback loading the chsh_diagnostic section as defaults, an explicit flag still overrides it"""
    if value is not None:
        try:
            data = json.loads(value.read_text(encoding="utf-8"))
        except (OSError, ValueError) as exc:
            raise click.ClickException(f"{value}: {exc}")
        ctx.default_map = data.get("chsh_diagnostic", {})
    return value


@click.command(context_settings={"show_default": True})
@click.argument("directory", type=click.Path(exists=True, file_okay=False, path_type=Path))
@click.option(
    "--config",
    type=click.Path(exists=True, dir_okay=False, path_type=Path),
    default=None,
    is_eager=True,
    expose_value=False,
    callback=_apply_config_defaults,
    help="JSON file (see acquisition/config.json's \"chsh_diagnostic\" section) "
    "supplying default values for the options below; explicit flags still override it.",
)
@click.option("--block-s", type=click.FloatRange(min=0, min_open=True), default=1.0, help="Integration time per output row.")
@click.option("--roi1-width-ns", type=click.FloatRange(min=0, min_open=True), default=None,
              help="ROI1 width. Defaults to coincidence_peak.py's fitted peak window width.")
@click.option("--roi1-center-ns", type=float, default=None,
              help="ROI1 center, relative to the aligned coincidence peak --> defaults to "
              "coincidence_peak.py's fitted peak center (near 0).")
@click.option("--roi2-offset-ns", type=float, required=True,
              help="ROI2 center offset from ROI1's center, e.g. one laser repetition period. "
              "No safe default --> depends on the source's rep rate.")
@click.option("--roi2-width-ns", type=click.FloatRange(min=0, min_open=True), default=None,
              help="ROI2 width --> defaults to the same width as ROI1.")
@click.option("--output", type=click.Path(path_type=Path), default=None,
              help="Output CSV path --> defaults to <directory>/<directory name>_chsh_blocks.csv.")
def main(
    directory: Path,
    block_s: float,
    roi1_width_ns: float | None,
    roi1_center_ns: float | None,
    roi2_offset_ns: float,
    roi2_width_ns: float | None,
    output: Path | None,
) -> None:
    """Per-block singles and ROI1/ROI2 coincidence counts for the CHSH analysis,
    reads DIRECTORY, a recording already processed by tt_record_dual.py and coincidence_peak.py"""
    metadata, columns, rows = build_blocks(
        directory, block_s, roi1_width_ns, roi1_center_ns, roi2_offset_ns, roi2_width_ns,
    )

    output_path = output if output is not None else directory / f"{directory.name}_chsh_blocks.csv"
    with output_path.open("w", newline="", encoding="utf-8") as f:
        writer = csv.writer(f)
        writer.writerow(columns)
        writer.writerows(rows)

    metadata_path = output_path.with_suffix(".json")
    metadata_path.write_text(json.dumps(metadata, indent=2) + "\n", encoding="utf-8")

    click.echo(f"Blocks: {metadata['n_blocks']} x {block_s:g} s")
    click.echo(f"ROI1: center {metadata['roi1']['center_ps'] / PS_PER_NS:g} ns, width {metadata['roi1']['width_ps'] / PS_PER_NS:g} ns")
    click.echo(f"ROI2: center {metadata['roi2']['center_ps'] / PS_PER_NS:g} ns, width {metadata['roi2']['width_ps'] / PS_PER_NS:g} ns")
    click.echo(f"CSV: {output_path}")
    click.echo(f"Metadata: {metadata_path}")


if __name__ == "__main__":
    main()
