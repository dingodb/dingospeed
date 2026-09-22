#!/usr/bin/env python3
"""Mount an existing repository tree, verify it, and load real cached models.
Outputs and small reference models go to a new isolated work directory.
"""
import argparse
import gc
import json
import os
from pathlib import Path
import subprocess
import time

from accept_tree import read_payload


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--binary", required=True)
    ap.add_argument("--root", required=True)
    ap.add_argument("--work", required=True)
    ap.add_argument("--mount", default="/mnt/dingo-models")
    ap.add_argument("--load-models", action="store_true")
    args = ap.parse_args()
    work = Path(args.work)
    work.mkdir(parents=True, exist_ok=False)
    log = open(work/"mount.log", "w")
    report = work/"mapping.json"
    summary = {"root": args.root, "mount": args.mount, "models": []}
    proc = subprocess.Popen([args.binary, "-tree-root", args.root, "-report", str(report), args.mount], stdout=log, stderr=log)
    try:
        for _ in range(600):
            if report.exists() and report.stat().st_size:
                break
            if proc.poll() is not None:
                raise RuntimeError((work/"mount.log").read_text())
            time.sleep(.05)
        versions = json.loads(report.read_text())
        summary["versions"] = len(versions)
        summary["ready"] = sum(v["status"] == "ready" for v in versions)
        summary["unavailable"] = [{"path": v["source_file_path"], "reason": v.get("error")} for v in versions if v["status"] != "ready"]
        summary["files"] = sum(len(v["files"]) for v in versions if v["status"] == "ready")
        subprocess.run(["python3", str(Path(__file__).with_name("accept_tree.py")), "check", "--report", str(report), "--output", str(work/"acceptance.html")], check=True)
        summary["tree_and_content_check"] = "passed; >16 MiB files sampled, smaller files fully checked"
        if args.load_models:
            import torch
            from transformers import AutoTokenizer, AutoModelForCausalLM
            targets = [("huggingface", "sshleifer/tiny-gpt2"), ("dingo-local", "hf-tiny-gpt2")]
            for ns, repo in targets:
                v = next(v for v in versions if v["namespace"] == ns and v["repo"] == repo and v["revision"] == "main")
                assert v["status"] == "ready"
                reference = work/"reference"/ns/repo
                reference.mkdir(parents=True)
                source_stamps = {}
                for f in v["files"]:
                    source = Path(f["backing"])
                    st = source.stat()
                    source_stamps[str(source)] = (st.st_size, st.st_mtime_ns)
                    assert 0 <= f["size"] <= 32*1024*1024, "reference copy restricted to small test models"
                    out = reference/f["path"]
                    out.parent.mkdir(parents=True, exist_ok=True)
                    out.write_bytes(read_payload(source, 0, f["size"]))
                mounted = Path(v["source_file_path"])
                tokenizer = AutoTokenizer.from_pretrained(mounted, local_files_only=True)
                reference_tokenizer = AutoTokenizer.from_pretrained(reference, local_files_only=True)
                model = AutoModelForCausalLM.from_pretrained(mounted, local_files_only=True)
                original = AutoModelForCausalLM.from_pretrained(reference, local_files_only=True)
                x = tokenizer("Hello, this is a local model directory.", return_tensors="pt")
                baseline_x = reference_tokenizer("Hello, this is a local model directory.", return_tensors="pt")
                assert torch.equal(x["input_ids"], baseline_x["input_ids"])
                model.eval(); original.eval()
                with torch.no_grad():
                    assert torch.equal(model(**x).logits, original(**x).logits)
                    generated = tokenizer.decode(model.generate(**x, max_new_tokens=8, do_sample=False, pad_token_id=tokenizer.eos_token_id)[0])
                model.train()
                optimizer = torch.optim.SGD(model.parameters(), lr=0.001)
                optimizer.zero_grad()
                loss = model(**x, labels=x["input_ids"]).loss
                loss.backward(); optimizer.step()
                checkpoint = work/"checkpoint"/ns/repo
                model.save_pretrained(checkpoint)
                restored = AutoModelForCausalLM.from_pretrained(checkpoint, local_files_only=True)
                model.eval(); restored.eval()
                with torch.no_grad():
                    assert torch.equal(model(**x).logits, restored(**x).logits)
                for source, before in source_stamps.items():
                    st = Path(source).stat()
                    assert before == (st.st_size, st.st_mtime_ns)
                summary["models"].append({"source_file_path": str(mounted), "tokenizer": "passed", "logits_identical_to_plain_reference": True,
                    "generated_text": generated, "training_loss": loss.item(), "training_step": "passed", "checkpoint_reload": "passed", "source_metadata_unchanged": True})
                del model, original, restored, optimizer, loss
                gc.collect()
        summary["result"] = "passed"
    finally:
        if os.path.ismount(args.mount):
            subprocess.run(["fusermount3", "-u", args.mount], check=True)
        proc.wait(timeout=10)
        log.close()
    (work/"summary.json").write_text(json.dumps(summary, indent=2, ensure_ascii=False))
    print(json.dumps(summary, indent=2, ensure_ascii=False))


if __name__ == "__main__":
    main()
