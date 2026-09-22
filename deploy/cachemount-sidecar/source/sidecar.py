"""Expose the repository tree. No Spinfield or inference process starts here."""
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import tempfile
import time

STATE = Path("/run/cachemount")
READY = STATE / "ready.json"
stopping = False


def stop(signum, frame):
    global stopping
    stopping = True


def configuration():
    names = ("DINGO_CACHE_ROOT", "DINGO_MODEL_MOUNT")
    values = [os.environ.get(n, "") for n in names]
    if not all(values):
        raise ValueError("required environment: " + ", ".join(names))
    raw, target = values
    if not Path(raw).is_absolute() or not Path(target).is_absolute():
        raise ValueError("input and output must be absolute container paths")
    root = Path(raw).resolve(strict=True)
    mount = Path(target).resolve()
    if not root.is_dir() or root == mount or root in mount.parents or mount in root.parents:
        raise ValueError("input and output must be separate directory trees")
    if not os.path.ismount(mount.parent):
        raise ValueError("output must be a child of the dedicated shared volume mount")
    return root, mount


def check():
    state = json.loads(READY.read_text())
    os.kill(state["pid"], 0)
    if not os.path.ismount(state["mount"]):
        raise RuntimeError("mount missing")
    list(Path(state["mount"]).iterdir())
    for version in state["versions"]:
        model = Path(version["source_file_path"])
        list(model.iterdir())
        if version["files"]:
            with (model / version["files"][0]["path"]).open("rb") as f:
                f.read(1)


def run():
    signal.signal(signal.SIGTERM, stop)
    signal.signal(signal.SIGINT, stop)
    STATE.mkdir(parents=True, exist_ok=True)
    READY.unlink(missing_ok=True)
    root, mount = configuration()
    if os.path.ismount(mount):
        raise RuntimeError("mount already exists; recreate this Pod after a mount failure")
    mount.mkdir(exist_ok=True)
    work = Path(tempfile.mkdtemp(prefix="session-", dir=STATE))
    report = work / "mapping.json"
    helper = subprocess.Popen(["/usr/local/bin/cachemount", "-tree-root", str(root), "-report", str(report), str(mount)])
    try:
        deadline = time.monotonic() + 120
        while not stopping:
            if helper.poll() is not None:
                raise RuntimeError("cachemount exited during startup")
            if report.exists():
                try:
                    versions = json.loads(report.read_text())
                    break
                except json.JSONDecodeError:
                    pass
            if time.monotonic() > deadline:
                raise RuntimeError("mount startup timed out")
            time.sleep(.1)
        if stopping:
            return
        usable = [v for v in versions if v["status"] == "ready"]
        if not usable:
            raise RuntimeError("no usable repository versions; inspect cachemount logs")
        state = {"pid": helper.pid, "mount": str(mount), "versions": usable}
        pending = work / "ready.json"
        pending.write_text(json.dumps(state))
        pending.replace(READY)
        check()
        print(json.dumps({"ready": True, "model_root": str(mount), "available_versions": len(usable),
                          "unavailable_versions": len(versions)-len(usable)}), flush=True)
        while not stopping:
            if helper.poll() is not None:
                raise RuntimeError("cachemount exited; recreate the Pod before reloading the model")
            time.sleep(.2)
    finally:
        READY.unlink(missing_ok=True)
        if helper.poll() is None:
            helper.terminate()
            try:
                helper.wait(timeout=20)
            except subprocess.TimeoutExpired:
                print("unmount busy; stopping helper; recreate Pod", file=sys.stderr)
                helper.kill()
                helper.wait()
                raise RuntimeError("clean unmount failed")


if __name__ == "__main__":
    try:
        if sys.argv[1:] == ["check"]:
            check()
        elif not sys.argv[1:]:
            run()
        else:
            raise ValueError("usage: sidecar.py [check]")
    except Exception as exc:
        print(str(exc), file=sys.stderr)
        sys.exit(1)
