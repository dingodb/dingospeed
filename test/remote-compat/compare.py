"""Compare two built DingoSpeed binaries against a deterministic upstream.

Only writes to --output; servers bind loopback and processes are stopped on exit.
Usage: python compare.py --baseline old.exe --candidate new.exe --output result-dir
"""
import argparse
import hashlib
import http.client
import json
import os
from pathlib import Path
import shutil
import socket
import subprocess
import threading
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlsplit

PAYLOAD = b"remote fixture bytes\n" * 200
SHA = hashlib.sha256(PAYLOAD).hexdigest()
COMMIT = "a" * 40


class Upstream(BaseHTTPRequestHandler):
    def log_message(self, *_):
        pass

    def do_HEAD(self):
        self.do_GET()

    def do_POST(self):
        self.rfile.read(int(self.headers.get("Content-Length", "0")))
        self.do_GET()

    def do_GET(self):
        path = urlsplit(self.path).path
        code = 200
        headers = {"Content-Type": "application/json"}
        if "/missing/" in path:
            code, body = 404, b'{"error":"missing repository"}'
        elif "/denied/" in path:
            code, body = 403, b'{"error":"access denied"}'
        elif "/resolve/" in path or path.endswith("/repo"):
            body = PAYLOAD
            headers.update({"Content-Type": "application/octet-stream", "ETag": '"' + SHA + '"', "X-Repo-Commit": COMMIT, "Accept-Ranges": "bytes"})
            if self.headers.get("Range"):
                start, end = self.headers["Range"].removeprefix("bytes=").split("-")
                start, end = int(start), min(int(end) if end else len(body)-1, len(body)-1)
                headers["Content-Range"] = f"bytes {start}-{end}/{len(body)}"
                body, code = body[start:end+1], 206
        elif path.startswith("/api/v1/"):
            data = {"Files": [{"Type": "blob", "Path": "weights.bin", "Name": "weights.bin", "Size": len(PAYLOAD), "Sha256": SHA}], "RevisionMap": {"Branches": [{"Revision": "master"}], "Tags": []}}
            body = json.dumps({"Code": 200, "Success": True, "Data": data}).encode()
        elif "/tree/" in path or "/paths-info/" in path:
            body = json.dumps([{"type": "file", "path": "weights.bin", "oid": SHA, "size": len(PAYLOAD), "lfs": {"oid": SHA, "size": len(PAYLOAD)}}]).encode()
        elif path.endswith("/refs"):
            body = json.dumps({"branches": [{"name": "main", "ref": "refs/heads/main", "targetCommit": COMMIT}], "tags": [], "converts": []}).encode()
        else:
            body = json.dumps({"id": "team/demo", "sha": COMMIT, "siblings": [{"rfilename": "weights.bin"}], "usedStorage": len(PAYLOAD)}).encode()
        headers["Content-Length"] = str(len(body))
        self.send_response(code)
        for key, value in headers.items():
            self.send_header(key, value)
        self.end_headers()
        if self.command != "HEAD":
            self.wfile.write(body)


def port():
    with socket.socket() as sock:
        sock.bind(("127.0.0.1", 0))
        return sock.getsockname()[1]


def request(base_port, method, path, headers=None):
    conn = http.client.HTTPConnection("127.0.0.1", base_port, timeout=20)
    try:
        conn.request(method, path, headers=headers or {})
        response = conn.getresponse()
        body = response.read()
        selected = {k.lower(): v for k, v in response.getheaders() if k.lower() in {"content-type", "content-length", "content-range", "etag", "x-repo-commit", "accept-ranges"}}
        return {"status": response.status, "headers": selected, "sha256": hashlib.sha256(body).hexdigest(), "size": len(body)}
    finally:
        conn.close()


def start(binary, directory, upstream_port, repos=None, online=True):
    directory.mkdir(parents=True, exist_ok=True)
    read_port, upload_port = port(), port()
    config = {"server": {"host": "127.0.0.1", "port": read_port, "online": online, "repos": str(repos or directory / "repos"), "hfScheme": "http", "hfNetLoc": f"127.0.0.1:{upstream_port}", "bpHfNetLoc": f"127.0.0.1:{upstream_port}"}, "tokenBucketLimit": {"handlerCapacity": 50}, "scheduler": {"mode": "standalone"}, "upload": {"host": "127.0.0.1", "port": upload_port, "namespace": "dingo-local"}, "download": {"blockSize": 1048576, "respChunkSize": 1024, "remoteFileBufferSize": 8192, "goroutineMaxNumPerFile": 2, "reqTimeout": 10}, "cache": {"defaultExpiration": 30, "cleanupInterval": 60, "readBlock": {"collectTimePeriod": 5, "prefetchMemoryUsedThreshold": 90, "prefetchBlocks": 8, "prefetchBlockTTL": 30}}, "retry": {"attempts": 1, "delay": 1}, "modelscope": {"officialBaseURL": f"http://127.0.0.1:{upstream_port}", "chunkSize": 1024, "maxRetry": 1, "retryDelay": 1}}
    path = directory / "config.json"
    path.write_text(json.dumps(config), encoding="utf-8")
    log = (directory / "server.log").open("w", encoding="utf-8")
    proc = subprocess.Popen([str(binary), "-config", str(path)], cwd=directory, stdout=log, stderr=subprocess.STDOUT, creationflags=subprocess.CREATE_NO_WINDOW if os.name == "nt" else 0)
    for _ in range(100):
        if proc.poll() is not None:
            log.close()
            raise RuntimeError(f"server exited: {directory / 'server.log'}")
        try:
            assert request(read_port, "GET", "/info")["status"] == 200
            return proc, log, read_port
        except (OSError, http.client.HTTPException):
            time.sleep(.1)
    proc.terminate()
    proc.wait()
    log.close()
    raise RuntimeError("server startup timed out")


def main():
    parser = argparse.ArgumentParser()
    for name in ("baseline", "candidate", "output"):
        parser.add_argument("--" + name, required=True, type=Path)
    parser.add_argument("--spinfield", type=Path, help="Also run console HTTP upload integration and HF SDK downloads")
    args = parser.parse_args()
    output = args.output.resolve()
    output.mkdir(parents=True, exist_ok=False)
    upstream = ThreadingHTTPServer(("127.0.0.1", 0), Upstream)
    threading.Thread(target=upstream.serve_forever, daemon=True).start()
    processes = []
    report = []
    try:
        ports = []
        for name, binary in (("baseline", args.baseline), ("candidate", args.candidate)):
            running = start(binary.resolve(), output / name, upstream.server_port)
            processes.append(running)
            ports.append(running[2])
        cases = [(method, path, headers) for method, path, headers in [
            ("GET", "/api/models/team/demo/revision/main", {}),
            ("HEAD", "/api/models/team/demo/revision/main", {}),
            ("GET", "/api/models/team/demo/tree/main", {}),
            ("GET", "/api/models/team/demo/refs", {}),
            ("HEAD", "/team/demo/resolve/main/weights.bin", {}),
            ("GET", "/team/demo/resolve/main/weights.bin", {}),
            ("GET", "/team/demo/resolve/main/weights.bin", {"Range": "bytes=7-39"}),
            ("GET", "/api/datasets/team/demo/revision/main", {}),
            ("GET", "/datasets/team/demo/resolve/main/weights.bin", {}),
            ("GET", "/api/models/missing/demo/revision/main", {}),
            ("GET", "/api/models/denied/demo/revision/main", {}),
            ("GET", "/api/v1/models/team/demo", {}),
            ("GET", "/api/v1/models/team/demo/repo?Revision=master&FilePath=weights.bin", {}),
            ("GET", "/api/v1/models/team/demo/repo?Revision=master&FilePath=weights.bin", {"Range": "bytes=7-39"}),
        ]]
        for method, path, headers in cases:
            results = [request(p, method, path, headers) for p in ports]
            expected_status = 404 if "/missing/" in path else 403 if "/denied/" in path else 206 if "Range" in headers or path.startswith("/api/v1/") and "/repo?" in path else 200
            if results[0]["status"] != expected_status:
                raise AssertionError(f"fixture did not exercise expected baseline behavior: {method} {path}: {results}")
            report.append({"method": method, "path": path, "requestHeaders": headers, "baseline": results[0], "candidate": results[1], "equal": results[0] == results[1]})
        if args.spinfield:
            config = json.loads((output / "candidate/config.json").read_text())
            env = os.environ.copy()
            env["DINGOSPEED_E2E_UPLOAD_BASE"] = f"http://127.0.0.1:{config['upload']['port']}"
            env["DINGOSPEED_E2E_DOWNLOAD_BASE"] = f"http://127.0.0.1:{ports[1]}"
            result = subprocess.run(["go", "test", "./modelfleet/internal/modelasset/api", "-run", "TestDingoSpeedConsoleUploadNamespaceIsolation", "-count=1", "-v"], cwd=args.spinfield.resolve(), env=env, capture_output=True, text=True, timeout=180)
            (output / "console-http-test.log").write_text(result.stdout + result.stderr, encoding="utf-8")
            if result.returncode:
                raise RuntimeError(result.stdout + result.stderr)
            from huggingface_hub import snapshot_download
            sdk_results = []
            for ns in ("alice", "bob", "datacanvas"):
                repo = next((output / "candidate/repos/files/models/dingo-local" / ns).iterdir()).name
                dest = output / "sdk-download" / ns
                snapshot_download(repo_id=ns+"/"+repo, revision="main", endpoint=env["DINGOSPEED_E2E_DOWNLOAD_BASE"]+"/dingo-local", local_dir=dest, token=False)
                data = (dest / "weights.bin").read_bytes()
                assert data == (ns+" owns these bytes").encode()
                sdk_results.append({"namespace": ns, "repo": repo, "sha256": hashlib.sha256(data).hexdigest()})
            (output / "sdk-results.json").write_text(json.dumps(sdk_results, indent=2), encoding="utf-8")
        # Transfer unmodified baseline caches, with no new repository descriptors.
        baseline, log, _ = processes.pop(0)
        baseline.terminate()
        baseline.wait()
        log.close()
        old_cache = output / "legacy-cache"
        shutil.copytree(output / "baseline/repos", old_cache)
        running = start(args.candidate.resolve(), output / "legacy-reader", upstream.server_port, old_cache, False)
        processes.append(running)
        for method, path, headers in (cases[0], cases[4], cases[5], cases[6], cases[8], cases[12]):
            value = request(running[2], method, path, headers)
            expected = next(row["baseline"] for row in report if row["method"] == method and row["path"] == path and row["requestHeaders"] == headers)
            report.append({"legacyCache": True, "method": method, "path": path, "baseline": expected, "candidate": value, "equal": value == expected})
        (output / "report.json").write_text(json.dumps(report, indent=2), encoding="utf-8")
        failed = [row for row in report if not row["equal"]]
        print(json.dumps({"cases": len(report), "passed": len(report)-len(failed), "report": str(output / "report.json"), "failures": failed}, indent=2))
        if failed:
            raise SystemExit(1)
    finally:
        for proc, log, _ in processes:
            proc.terminate()
            proc.wait(timeout=10)
            log.close()
        upstream.shutdown()
        upstream.server_close()


if __name__ == "__main__":
    main()
