#!/usr/bin/env python3
"""Isolated, real-FUSE validation. No production cache or global cache eviction.

python3 validate.py --binary /path/cachemount --work /tmp/new-validation-dir
Optional torch installation enables actual mmap load/train/checkpoint/resume.
"""
import argparse
import concurrent.futures
import hashlib
import gc
import json
import mmap
import os
from pathlib import Path
import statistics
import struct
import subprocess
import time


def digest(path):
    h = hashlib.sha256()
    with open(path, "rb") as f:
        while b := f.read(4 * 1024 * 1024):
            h.update(b)
    return h.hexdigest()


def wrap(source, target):
    size = source.stat().st_size
    block = 1024 * 1024
    count = (size + block - 1) // block
    bits = max(8, ((count + 7) // 8) * 8)
    with open(source, "rb") as src, open(target, "wb") as dst:
        dst.write(b"OLAH" + struct.pack("<QQQQ", 8, block, size, bits))
        dst.write(b"\xff" * (bits // 8))
        while b := src.read(4 * 1024 * 1024):
            dst.write(b)
    return 36 + bits // 8


def scan(path):
    start = time.perf_counter()
    with open(path, "rb") as f:
        while f.read(4 * 1024 * 1024):
            pass
    return time.perf_counter() - start


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--mib", type=int, default=256)
    ap.add_argument("--hf", action="store_true", help="exercise automatic cached HF manifest mapping")
    args = ap.parse_args()
    work = Path(args.work)
    work.mkdir(exist_ok=False, parents=True)
    raw, cache, mount = [work / n for n in ("raw", "cache", "mount")]
    for p in (raw, cache, mount):
        p.mkdir()
    hfroot = work / "hf"
    if args.hf:
        cache = hfroot / "files/models/demo/tiny/blobs"
        cache.mkdir(parents=True)
    with open(raw / "weights.bin", "wb") as f:
        chunk = bytes(range(256)) * 4096
        for _ in range(args.mib):
            f.write(chunk)
    (raw / "config.json").write_text('{"test":true}')
    try:
        import torch
    except ImportError:
        torch = None
    if torch:
        torch.manual_seed(1)
        model = torch.nn.Sequential(torch.nn.Linear(32, 64), torch.nn.ReLU(), torch.nn.Linear(64, 8))
        torch.save(model.state_dict(), raw / "model.pt")
    try:
        from transformers import AutoModelForCausalLM, GPT2Config, GPT2LMHeadModel
    except ImportError:
        AutoModelForCausalLM = None
    if torch and AutoModelForCausalLM:
        tiny = GPT2LMHeadModel(GPT2Config(vocab_size=64, n_positions=32, n_embd=32, n_layer=1, n_head=2, bos_token_id=0, eos_token_id=1))
        tiny.save_pretrained(raw, safe_serialization=True)
        del tiny
    files = {}
    offsets = {}
    for source in raw.iterdir():
        target = cache / source.name
        offsets[source.name] = wrap(source, target)
        files[source.name] = str(target)
    manifest = work / "manifest.json"
    manifest.write_text(json.dumps(files))
    command = [args.binary, "-manifest", str(manifest), str(mount)]
    if args.hf:
        meta = {"sha": "fixedcommit", "private": False, "gated": False,
                "siblings": [{"rfilename": name, "blobId": name, "size": (raw/name).stat().st_size} for name in files]}
        meta_path = hfroot / "api/models/demo/tiny/revision/main/meta_get.json"
        meta_path.parent.mkdir(parents=True)
        meta_path.write_text(json.dumps({"status_code": 200, "content": json.dumps(meta).encode().hex()}))
        command = [args.binary, "-cache-root", str(hfroot), "-repo", "demo/tiny", str(mount)]
    before = {p.name: (digest(p), p.stat().st_size, p.stat().st_mtime_ns) for p in cache.iterdir()}
    report = {"payload_mib": args.mib, "torch": torch.__version__ if torch else "unavailable", "source_mode": "hf" if args.hf else "manifest"}
    started = time.perf_counter()
    log = open(work / "mount.log", "w")
    proc = subprocess.Popen(command, stdout=log, stderr=log)
    try:
        deadline = time.monotonic() + 10
        while not os.path.ismount(mount):
            if proc.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("mount failed; inspect mount.log")
            time.sleep(0.01)
        report["mount_ready_seconds"] = time.perf_counter() - started
        for name in files:
            assert digest(raw / name) == digest(mount / name), name
            assert (raw / name).stat().st_size == (mount / name).stat().st_size
        with open(mount / "weights.bin", "rb") as f, mmap.mmap(f.fileno(), 0, access=mmap.ACCESS_READ) as mm:
            for off in [0, 1, 4095, 1024 * 1024 - 3, len(mm) - 9]:
                assert mm[off:off+9] == bytes((i % 256 for i in range(off, off+9)))
        with concurrent.futures.ProcessPoolExecutor(max_workers=4) as pool:
            hashes = list(pool.map(digest, [mount / "weights.bin"] * 8))
        assert len(set(hashes)) == 1
        for operation in [lambda: (mount / "new").write_bytes(b"x"), lambda: (mount / "config.json").unlink()]:
            try:
                operation()
            except OSError:
                pass
            else:
                raise AssertionError("write accepted")
        timings = {"plain": [], "fuse": []}
        for _ in range(5):
            timings["plain"].append(scan(raw / "weights.bin"))
            timings["fuse"].append(scan(mount / "weights.bin"))
        report["warm_scan_seconds"] = timings
        report["warm_scan_median_seconds"] = {k: statistics.median(v) for k, v in timings.items()}
        start = time.perf_counter()
        with open(cache / "weights.bin", "rb") as src, open(work / "export.bin", "wb") as dst:
            src.seek(offsets["weights.bin"])
            while b := src.read(4 * 1024 * 1024):
                dst.write(b)
            dst.flush()
            os.fsync(dst.fileno())
        report["export_fsync_seconds"] = time.perf_counter() - start
        report["export_extra_bytes"] = (work / "export.bin").stat().st_size
        if torch:
            state = torch.load(mount / "model.pt", mmap=True, weights_only=True)
            model.load_state_dict(state)
            reference = torch.nn.Sequential(torch.nn.Linear(32, 64), torch.nn.ReLU(), torch.nn.Linear(64, 8))
            reference.load_state_dict(torch.load(raw / "model.pt", mmap=True, weights_only=True))
            x, y = torch.randn(4, 32), torch.randn(4, 8)
            assert torch.equal(model(x), reference(x))
            opt = torch.optim.SGD(model.parameters(), lr=0.01)
            losses = []
            for _ in range(5):
                opt.zero_grad()
                loss = ((model(x)-y)**2).mean()
                loss.backward()
                opt.step()
                losses.append(loss.item())
            torch.save({"model": model.state_dict(), "optimizer": opt.state_dict()}, work / "checkpoint.pt")
            saved = torch.load(work / "checkpoint.pt", weights_only=True)
            reference.load_state_dict(saved["model"])
            resumed_opt = torch.optim.SGD(reference.parameters(), lr=0.01)
            resumed_opt.load_state_dict(saved["optimizer"])
            assert torch.equal(model(x), reference(x))
            resumed_opt.zero_grad()
            ((reference(x)-y)**2).mean().backward()
            resumed_opt.step()
            report["training"] = {"steps": 5, "losses": losses, "checkpoint_resume": "passed"}
            # torch.load(mmap=True) keeps its source mapping alive with state.
            del state
            gc.collect()
        if torch and AutoModelForCausalLM:
            hf_model = AutoModelForCausalLM.from_pretrained(mount, local_files_only=True)
            hf_reference = AutoModelForCausalLM.from_pretrained(raw, local_files_only=True)
            hf_model.eval()
            hf_reference.eval()
            ids = torch.arange(16).reshape(2, 8)
            with torch.no_grad():
                assert torch.equal(hf_model(ids).logits, hf_reference(ids).logits)
            hf_model.train()
            hf_opt = torch.optim.SGD(hf_model.parameters(), lr=0.001)
            hf_losses = []
            for _ in range(3):
                hf_opt.zero_grad()
                loss = hf_model(input_ids=ids, labels=ids).loss
                loss.backward()
                hf_opt.step()
                hf_losses.append(loss.item())
            checkpoint = work / "hf-checkpoint"
            hf_model.save_pretrained(checkpoint)
            hf_resumed = AutoModelForCausalLM.from_pretrained(checkpoint, local_files_only=True)
            hf_resumed.eval()
            hf_model.eval()
            with torch.no_grad():
                assert torch.equal(hf_model(ids).logits, hf_resumed(ids).logits)
            hf_resumed.train()
            resume_opt = torch.optim.SGD(hf_resumed.parameters(), lr=0.001)
            resume_opt.load_state_dict(hf_opt.state_dict())
            hf_resumed(input_ids=ids, labels=ids).loss.backward()
            resume_opt.step()
            report["transformers"] = {"offline_from_pretrained": "passed", "format": "safetensors", "steps": 3,
                                      "losses": hf_losses, "checkpoint_resume": "passed", "model": "random tiny GPT2"}
            del hf_model, hf_reference, hf_resumed, hf_opt, resume_opt, loss
            gc.collect()
        after = {p.name: (digest(p), p.stat().st_size, p.stat().st_mtime_ns) for p in cache.iterdir()}
        assert before == after, "backing cache changed"
        report["source_unchanged"] = True
        report["validation"] = "passed"
    finally:
        if os.path.ismount(mount):
            subprocess.run(["fusermount3", "-u", str(mount)], check=True)
        proc.wait(timeout=10)
        log.close()
    (work / "report.json").write_text(json.dumps(report, indent=2))
    print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
