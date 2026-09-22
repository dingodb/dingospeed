#!/usr/bin/env python3
"""User-operated acceptance, never run automatically by the build.

demo --work NEW_DIRECTORY creates isolated HF/hosted/partial examples.
check --report MOUNT_REPORT --output NEW_HTML [--full] inspects a live mount.
The report is real filesystem evidence, not pre-rendered expected success.
"""
import argparse
import errno
import hashlib
import html
import json
import os
from pathlib import Path
import struct


def wrapper(path, body):
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps({"status_code": 200, "content": json.dumps(body).encode().hex()}))


def demo(work):
    work.mkdir(parents=True, exist_ok=False)
    root = work / "repos"
    selections = []
    for namespace, repo, revision, broken in [("huggingface", "demo/HFModel", "main", False),
                                               ("datacanvas", "team/UploadModel", "v1", False),
                                               ("datacanvas", "team/Incomplete", "v1", True)]:
        storage = repo if namespace == "huggingface" else "dingo-local/" + namespace + "/" + repo
        api = root / "api/models" / storage
        blobs = root / "files/models" / storage / "blobs"
        blobs.mkdir(parents=True)
        data = {"config.json": json.dumps({"model_type": "acceptance-demo", "namespace": namespace}, indent=2).encode(),
                "tokenizer/tokenizer.json": b'{"demo": "nested path retained"}',
                "model.safetensors": struct.pack("<Q", 64) + b'{}' + b' ' * 62 + bytes(range(256)) * 32}
        entries = []
        for name, payload in data.items():
            digest = hashlib.sha256(payload).hexdigest()
            count = (len(payload) + 4095)//4096
            bits = max(8, (count+7)//8*8)
            mask = bytearray(b"\xff" * (bits//8))
            if broken and name == "model.safetensors":
                mask[0] &= 0xfe
            (blobs/digest).write_bytes(b"OLAH" + struct.pack("<QQQQ", 8, 4096, len(payload), bits) + mask + payload)
            original = work/"originals"/namespace/repo/revision/name
            original.parent.mkdir(parents=True, exist_ok=True)
            original.write_bytes(payload)
            entries.append({"path": name, "sha256": digest, "size": len(payload)})
        commit = "snapshot1"
        meta = {"sha": commit, "private": False, "gated": False,
                "siblings": [{"rfilename": e["path"], "lfs": {"sha256": e["sha256"], "size": e["size"]}} for e in entries]}
        wrapper(api/"revision"/revision/"meta_get.json", meta)
        wrapper(api/"revision"/commit/"meta_get.json", meta)
        if namespace != "huggingface":
            (api/"repository.json").write_text(json.dumps({"namespace": namespace, "repoType": "models", "repo": repo,
                "version": 1, "source": "hosted", "persistent": True, "format": "dingcache"}))
            (api/"revision"/commit/"dingo-local-manifest.json").write_text(json.dumps(entries))
        selections.append({"cache_root": str(root), "namespace": namespace, "repo": repo, "revision": revision})
    (work/"selection.json").write_text(json.dumps(selections, indent=2))
    print("样本已生成；未挂载、未验收。后续命令：")
    print(f"./cachemount -tree-config '{work}/selection.json' -report '{work}/mapping.json' /mnt/dingo-models")
    print(f"python3 test/cachemount/accept_tree.py check --report '{work}/mapping.json' --originals '{work}/originals' --output '{work}/acceptance.html' --full")
    print("model.safetensors 是用于字节映射验收的合成文件，不是可训练模型。")


def read_payload(path, off, length):
    with open(path, "rb") as f:
        h = f.read(36)
        if h[:4] == b"OLAH":
            _, _, size, bits = struct.unpack("<QQQQ", h[4:])
            start = 36+(bits+7)//8
        else:
            size, start = os.fstat(f.fileno()).st_size, 0
        f.seek(start+off)
        return f.read(min(length, max(0, size-off)))


def original_manifest(v):
    """Independent metadata read: do not use the FUSE implementation's file list
    as the sole oracle. Compare the pinned commit to repository source data."""
    ns, repo = v["namespace"], v["repo"]
    storage = repo if ns == "huggingface" else ("dingo-local/" + repo if ns == "dingo-local" else "dingo-local/"+ns+"/"+repo)
    api = Path(v["cache_root"])/"api/models"/storage/"revision"
    if ns != "huggingface":
        entries = json.loads((api/v["commit"]/"dingo-local-manifest.json").read_text())
        return {e["path"]: (e["size"], e["sha256"]) for e in entries}
    p = api/v["commit"]/"meta_get.json"
    if not p.exists():
        p = api/v["revision"]/"meta_get.json"
    envelope = json.loads(p.read_text())
    assert envelope["status_code"] == 200
    meta = json.loads(bytes.fromhex(envelope["content"]))
    assert meta["sha"] == v["commit"], "验收时版本已变化"
    return {e["rfilename"]: (e.get("lfs", {}).get("size", e.get("size", -1)),
                             e.get("lfs", {}).get("sha256", e.get("blobId", ""))) for e in meta["siblings"]}


def check(args):
    versions = json.loads(Path(args.report).read_text())
    sections = []
    all_pass = bool(versions) and any(v["status"] == "ready" for v in versions)
    esc = lambda x: html.escape(str(x))
    for v in versions:
        root = Path(v["source_file_path"])
        title = f'{v["namespace"]}/{v["repo"]}/{v["revision"]}'
        if v["status"] != "ready":
            try:
                root.stat()
                list(root.iterdir())
                result = "失败：不可用版本却可以正常列目录"
                all_pass = False
            except OSError as e:
                if e.errno == errno.EIO:
                    result = f"负向场景通过：明确返回 I/O 错误（{e}）"
                else:
                    result = f"负向场景失败：预期 EIO，实际 {e}"
                    all_pass = False
            sections.append(f"<section><h2>{esc(title)}</h2><p>{esc(v['error'])}</p><p>{esc(result)}</p></section>")
            continue
        expected = {f["path"]: f for f in v["files"]}
        actual = {}
        errors = []
        try:
            independent = original_manifest(v)
            assert set(independent) == set(expected), "挂载清单与原仓库文件清单不一致"
            for name, (size, identity) in independent.items():
                if size >= 0:
                    assert expected[name]["size"] == size, name + " 清单大小不一致"
                if identity:
                    assert expected[name]["content_id"] == identity, name + " 内容身份不一致"
        except (OSError, AssertionError, ValueError, KeyError) as e:
            errors.append("独立读取原仓库清单失败：" + str(e))
        try:
            for base, _, files in os.walk(root, onerror=lambda e: errors.append(str(e))):
                for name in files:
                    p = Path(base)/name
                    actual[p.relative_to(root).as_posix()] = p.stat().st_size
        except OSError as e:
            errors.append(str(e))
        missing = sorted(set(expected)-set(actual))
        extra = sorted(set(actual)-set(expected))
        rows = []
        for name, f in expected.items():
            status = "未读取"
            try:
                p = root/name
                size = p.stat().st_size
                assert size == f["size"] or f["size"] == -1, "大小不同"
                mode_full = args.full or size <= 16*1024*1024
                with open(p, "rb") as view:
                    if mode_full:
                        digest = hashlib.sha256()
                        # Independently compare against backing payload, not the
                        # mount's own read routine or its claimed checksum.
                        at = 0
                        while b := view.read(4*1024*1024):
                            assert b == read_payload(f["backing"], at, len(b)), "正文不同"
                            digest.update(b)
                            at += len(b)
                        assert at == size, "正文读取长度不足"
                        content_id = f["content_id"]
                        if len(content_id) == 64:
                            assert digest.hexdigest() == content_id, "与仓库 SHA256 不同"
                        status = "全量正文一致"
                    else:
                        for at in [0, max(0, size//2-2048), max(0, size-4096)]:
                            view.seek(at)
                            assert view.read(4096) == read_payload(f["backing"], at, 4096), "抽样不同"
                        status = "头/中/尾抽样一致；未做全量校验"
                if args.originals:
                    original = Path(args.originals)/title/name
                    assert original.read_bytes() == p.read_bytes(), "与独立原始样本不同"
                    status += "；与原始样本一致"
            except (OSError, AssertionError) as e:
                status = "失败：" + str(e)
                errors.append(name + ": " + str(e))
            rows.append(f"<tr><td>{esc(name)}</td><td>{esc(f['size'])}</td><td>{esc(actual.get(name,'缺失'))}</td><td>{esc(status)}</td></tr>")
        passed = not (missing or extra or errors)
        all_pass &= passed
        preview = ""
        if "config.json" in expected and "config.json" in actual:
            f = expected["config.json"]
            with open(f["backing"], "rb") as stream:
                head = stream.read(36)
            with open(root/"config.json", "rb") as stream:
                body = stream.read(2048)
            preview = f"<h3>直接看差别</h3><p>内部缓存开头：</p><pre>{esc(head.hex(' '))}</pre><p>上层读取 config.json：</p><pre>{esc(body.decode('utf-8',errors='replace'))}</pre>"
        sections.append(f"<section><h2>{esc(title)} — {'通过' if passed else '失败'}</h2><p>source_file_path：<code>{esc(root)}</code></p>"
            f"<div class='trees'><div><h3>仓库清单（挂载时固定）</h3><pre>{esc(chr(10).join(sorted(expected)))}</pre></div>"
            f"<div><h3>实际文件夹（现场读取）</h3><pre>{esc(chr(10).join(sorted(actual)))}</pre></div></div>"
            f"<p>已独立读取原仓库元数据核对映射。缺失：{esc(missing)}；额外：{esc(extra)}；错误：{esc(errors)}</p>"
            f"<table><tr><th>文件路径</th><th>预期大小</th><th>实际大小</th><th>读取结果</th></tr>{''.join(rows)}</table>{preview}</section>")
    page = "<!doctype html><meta charset='utf-8'><title>模型目录现场验收</title><style>body{font:16px system-ui;max-width:1200px;margin:32px auto;padding:16px;background:#f3f6fa;color:#172b4d}section{background:white;padding:24px;margin:24px 0;border-radius:12px}.trees{display:grid;grid-template-columns:1fr 1fr;gap:24px}pre{white-space:pre-wrap;overflow-wrap:anywhere;background:#eef2f6;padding:16px}table{border-collapse:collapse;width:100%}td,th{border:1px solid #ccc;padding:10px;text-align:left}code{overflow-wrap:anywhere}</style>"
    page += f"<h1>模型目录现场验收：{'通过' if all_pass else '有失败项'}</h1><p>报告由当前真实挂载读取生成。预期树来自挂载时固定的版本清单；可与仓库页面核对。合成样本不证明真实训练通过。大文件未指定 --full 时仅抽样，不能视为全量验收。</p>" + ''.join(sections)
    with open(args.output, "x", encoding="utf-8") as f:
        f.write(page)
    print(args.output)
    if not all_pass:
        raise SystemExit(1)


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="command", required=True)
    d = sub.add_parser("demo")
    d.add_argument("--work", required=True)
    c = sub.add_parser("check")
    c.add_argument("--report", required=True)
    c.add_argument("--output", required=True)
    c.add_argument("--originals")
    c.add_argument("--full", action="store_true")
    args = ap.parse_args()
    if args.command == "demo":
        demo(Path(args.work).resolve())
    else:
        check(args)


if __name__ == "__main__":
    main()
