#!/usr/bin/env python3
"""Run cachemount and a consumer in the same mount namespace and UID.
Exit on either child's failure. Never keep the application healthy after the
mount helper dies. Each boot uses a fresh mapping report; no stale readiness.
"""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

stopping = False


def stop(signum, frame):
    global stopping
    stopping = True


def terminate(p):
    if p and p.poll() is None:
        p.terminate()
        try:
            p.wait(timeout=15)
        except subprocess.TimeoutExpired:
            p.kill()
            p.wait()


def main():
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    root = os.environ.get("DINGO_CACHE_ROOT", "/repos")
    mount = os.environ.get("DINGO_MODEL_MOUNT", "/mnt/dingo-models")
    state = Path(os.environ.get("DINGO_MOUNT_STATE", "/run/cachemount"))
    state.mkdir(parents=True, exist_ok=True)
    ready = state/"ready.json"
    ready.unlink(missing_ok=True)
    session = Path(tempfile.mkdtemp(prefix="session-", dir=state))
    report = session/"mapping.json"
    helper = app = None
    try:
        helper = subprocess.Popen(["/usr/local/bin/cachemount", "-tree-root", root, "-report", str(report), mount])
        deadline = time.monotonic()+60
        while not stopping:
            if helper.poll() is not None:
                raise RuntimeError("cachemount exited before readiness")
            if os.path.ismount(mount) and report.exists():
                try:
                    versions = json.loads(report.read_text())
                    break
                except json.JSONDecodeError:
                    pass
            if time.monotonic() >= deadline:
                raise RuntimeError("mount readiness timed out")
            time.sleep(.1)
        if stopping:
            return 0
        if not any(v["status"] == "ready" for v in versions):
            raise RuntimeError("no usable model version")
        app = subprocess.Popen(sys.argv[1:] or ["/modelfleet"])
        temp = session/"ready.json"
        temp.write_text(json.dumps({"uid": os.getuid(), "helper_pid": helper.pid, "consumer_pid": app.pid,
                                    "mount": mount, "mapping": str(report)}))
        temp.replace(ready)
        print("mount ready; consumer started", flush=True)
        while not stopping:
            if helper.poll() is not None or app.poll() is not None or not os.path.ismount(mount):
                raise RuntimeError("mount or consumer exited; stopping the pair")
            time.sleep(.2)
        return 0
    finally:
        ready.unlink(missing_ok=True)
        terminate(app)
        terminate(helper)


if __name__ == "__main__":
    try:
        sys.exit(main())
    except Exception as e:
        print(str(e), file=sys.stderr, flush=True)
        sys.exit(1)
