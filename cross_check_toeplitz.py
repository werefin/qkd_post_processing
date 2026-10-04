#!/usr/bin/env python3
"""Independent NumPy re-derivation of Go's Toeplitz hash (toeplitz.go) over
testdata/toeplitz_vectors.json (see toeplitz_vectors_test.go), cross-language
validation for the one step with the actual security claim: privacy
amplification"""

import base64
import json
import sys
from pathlib import Path

import numpy as np


def _b64_bits(s: str) -> list[int]:
    """Go's encoding/json base64-encodes []byte fields by default; each
    decoded byte here is already a 0/1 bit value, not packed"""
    return list(base64.b64decode(s))


def toeplitz_hash(data_bits: list[int], seed_bits: list[int], l: int) -> list[int]:
    """out[i] = XOR over j of data[j] AND seed[i - j + n - 1], independently
    derived straight from the mathematical definition of a Toeplitz matrix,
    not translated from Go's implementation"""
    data = np.asarray(data_bits, dtype=np.uint8)
    seed = np.asarray(seed_bits, dtype=np.uint8)
    n = len(data)
    out = np.zeros(l, dtype=np.uint8)
    for i in range(l):
        window = seed[i:i + n][::-1]  # seed[i+n-1], seed[i+n-2], ..., seed[i] <-> j=0..n-1
        out[i] = np.bitwise_xor.reduce(data & window) if n > 0 else 0
    return out.tolist()


def main() -> int:
    vectors_path = Path(__file__).parent / "testdata" / "toeplitz_vectors.json"
    if not vectors_path.is_file():
        print(f"{vectors_path} not found --> generate it first:")
        print("  GENERATE_VECTORS=1 go test -run TestGenerateToeplitzCrossCheckVectors -v .")
        return 2

    vectors = json.loads(vectors_path.read_text())
    all_ok = True
    for idx, v in enumerate(vectors):
        data_bits = _b64_bits(v["data_bits"])
        got = toeplitz_hash(data_bits, _b64_bits(v["seed_bits"]), v["l"])
        want = _b64_bits(v["want_hash"])
        ok = got == want
        all_ok = all_ok and ok
        status = "OK" if ok else "MISMATCH"
        print(f"vector {idx}: n={len(data_bits):5d}  l={v['l']:5d}  {status}")
        if not ok:
            print(f"  got:  {got}")
            print(f"  want: {want}")

    print()
    print("ALL VECTORS MATCH (independent NumPy re-derivation agrees with Go):", all_ok)
    return 0 if all_ok else 1


if __name__ == "__main__":
    sys.exit(main())
