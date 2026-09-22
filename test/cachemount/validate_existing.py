#!/usr/bin/env python3
"""Read-only validation of an existing, complete OLAH cache. No payload export.

Full direct-payload and FUSE SHA256 passes plus bounded random/mmap/concurrent
reads. Source is opened O_RDONLY only; all output goes to a new work directory.
No global cache eviction. Timings include the host's existing cache state.
"""
import argparse
import concurrent.futures
import hashlib
import json
import mmap
import os
from pathlib import Path
import random
import struct
import subprocess
import time


def stamp(path):
    s = os.stat(path)
    return {"size": s.st_size, "mtime_ns": s.st_mtime_ns, "inode": s.st_ino,
            "allocated_bytes": s.st_blocks * 512}


def full_pass(path, offset, size, label):
    h = hashlib.sha256()
    read_seconds = 0
    count = 0
    next_log = 4 * 1024**3
    start = time.perf_counter()
    with open(path, "rb", buffering=0) as f:
        f.seek(offset)
        while count < size:
            tick = time.perf_counter()
            b = f.read(min(4 * 1024**2, size-count))
            read_seconds += time.perf_counter()-tick
            if not b:
                raise RuntimeError("unexpected EOF")
            h.update(b)
            count += len(b)
            if count >= next_log:
                print(f"{label}: {count/1024**3:.0f}/{size/1024**3:.0f} GiB, elapsed {time.perf_counter()-start:.1f}s", flush=True)
                next_log += 4 * 1024**3
    elapsed = time.perf_counter()-start
    result = {"bytes": count, "sha256": h.hexdigest(), "elapsed_seconds": elapsed,
              "read_call_seconds": read_seconds, "read_call_mib_per_second": size/1024**2/read_seconds,
              "hash_and_read_mib_per_second": size/1024**2/elapsed}
    print(label + " complete: " + json.dumps(result), flush=True)
    return result


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--source", required=True)
    ap.add_argument("--binary", required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--expected-sha256", required=True)
    args = ap.parse_args()
    source = Path(args.source).resolve(strict=True)
    before = stamp(source)
    with open(source, "rb") as f:
        header = f.read(36)
        version, block, size, bits = struct.unpack("<QQQQ", header[4:])
        assert header[:4] == b"OLAH" and version == 8 and block > 0 and 0 < bits <= 8*1024*1024
        mask = f.read((bits+7)//8)
    offset = 36+len(mask)
    count = (size+block-1)//block
    assert len(mask) == (bits+7)//8 and count <= bits and before["size"] >= offset+size
    assert all(mask[i//8] & (1 << (i%8)) for i in range(count)), "incomplete cache"
    work = Path(args.work)
    work.mkdir(parents=True, exist_ok=False)
    mount = work/"mount"
    mount.mkdir()
    manifest = work/"manifest.json"
    manifest.write_text(json.dumps({"payload.bin": str(source)}))
    report = {"source": str(source), "source_before": before, "payload_bytes": size,
              "header_bytes": offset, "block_bytes": block, "complete_blocks": count,
              "environment": "WSL Ubuntu; backing file on Windows D: via /mnt/d (DrvFS/9p)",
              "cache_policy": "no cache flush; full passes direct then FUSE, not controlled cold-disk tests",
              "expected_sha256": args.expected_sha256}
    log = open(work/"mount.log", "w")
    start = time.perf_counter()
    proc = subprocess.Popen([args.binary, "-manifest", str(manifest), str(mount)], stdout=log, stderr=log)
    try:
        while not os.path.ismount(mount):
            if proc.poll() is not None or time.perf_counter()-start > 15:
                raise RuntimeError("mount failed")
            time.sleep(0.001)
        report["mount_ready_seconds"] = time.perf_counter()-start
        mounted = mount/"payload.bin"
        assert mounted.stat().st_size == size
        print(f"Mounted {size/1024**3:.0f} GiB in {report['mount_ready_seconds']:.6f}s", flush=True)
        report["direct_full"] = full_pass(source, offset, size, "direct")
        (work/"progress.json").write_text(json.dumps(report, indent=2))
        report["fuse_full"] = full_pass(mounted, 0, size, "fuse")
        assert report["direct_full"]["sha256"] == report["fuse_full"]["sha256"] == args.expected_sha256
        rng = random.Random(20260918)
        ranges = [(0, 8192), (size-8192, 8192), (block-4096, 8192), (size, 1)]
        ranges += [(rng.randrange(size-1048576), rng.choice([4096, 65536, 1048576])) for _ in range(128)]
        with open(source, "rb", buffering=0) as raw, open(mounted, "rb", buffering=0) as view:
            for off, n in ranges:
                assert os.pread(raw.fileno(), n, offset+off) == os.pread(view.fileno(), n, off)
            with mmap.mmap(view.fileno(), 0, access=mmap.ACCESS_READ) as mm:
                for off, n in ranges:
                    assert mm[off:off+n] == os.pread(raw.fileno(), n, offset+off)
        report["random_and_mmap_ranges"] = len(ranges)
        def check(part):
            with open(source, "rb", buffering=0) as a, open(mounted, "rb", buffering=0) as b:
                for off, n in part:
                    assert os.pread(a.fileno(), n, offset+off) == os.pread(b.fileno(), n, off)
            return len(part)
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            report["concurrent_ranges"] = sum(pool.map(check, [ranges[i::4] for i in range(4)]))
        report["source_after"] = stamp(source)
        assert before == report["source_after"], "source metadata changed during validation"
        with open(source, "rb") as f:
            assert f.read(offset) == header+mask, "header changed"
        report["mount_process_memory"] = [line.strip() for line in Path(f"/proc/{proc.pid}/status").read_text().splitlines() if line.startswith(("VmRSS:", "VmHWM:"))]
        report["output_bytes_excluding_mount"] = sum(p.stat().st_size for p in work.iterdir() if p.is_file())
        report["source_unchanged"] = True
        report["validation"] = "passed"
    finally:
        if os.path.ismount(mount):
            subprocess.run(["fusermount3", "-u", str(mount)], check=True)
        proc.wait(timeout=15)
        log.close()
    (work/"report.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2), flush=True)


if __name__ == "__main__":
    main()
