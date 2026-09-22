import json
import os
from pathlib import Path
import urllib.request

state = json.loads(Path("/run/cachemount/ready.json").read_text())
assert os.path.ismount(state["mount"])
os.kill(state["helper_pid"], 0)
os.kill(state["consumer_pid"], 0)
with urllib.request.urlopen("http://127.0.0.1:8082/api/v1/status", timeout=3) as response:
    assert response.status == 200
