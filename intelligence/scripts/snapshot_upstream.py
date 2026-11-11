"""Read-only git export, pinned inputs; never checks out or edits other components."""
from pathlib import Path
import hashlib
import json
import subprocess

ROOT = Path(__file__).resolve().parents[1]
SOURCES = {
    "platform": "2fe2700620c946e236377c79886e511743d7608c",
    "runner": "75f2454",
    "environment": "c9932fea7eca48d6fccb2e06a997478c8c59da62",
}
FILES = {
    "platform": ["platform/contracts/schemas/assessment.json", "platform/contracts/schemas/scenario.json",
        "platform/contracts/schemas/execution-manifest.json", "platform/contracts/schemas/artifact-ref.json",
        "platform/contracts/proto/poligon/control/v1/control.proto", "platform/contracts/proto/validation.proto"],
    "runner": ["runner/contracts/proto/poligon/control/v1/control.proto"],
    "environment": ["environment/pkg/api/models.go"],
}


def main():
    lock = []
    for owner, files in FILES.items():
        commit = subprocess.check_output(["git", "rev-parse", SOURCES[owner]], cwd=ROOT).decode().strip()
        for path in files:
            data = subprocess.check_output(["git", "show", f"{commit}:{path}"], cwd=ROOT)
            target = ROOT / "contracts" / "upstream" / path
            target.parent.mkdir(parents=True, exist_ok=True)
            target.write_bytes(data)
            lock.append(dict(commit=commit, path=path, sha256=hashlib.sha256(data).hexdigest()))
            if owner == "platform" and path.endswith("control.proto"):
                dest = ROOT / "contracts" / "proto" / "poligon" / "control" / "v1" / "control.proto"
                dest.parent.mkdir(parents=True, exist_ok=True)
                dest.write_bytes(data)
    (ROOT / "contracts" / "upstream.lock.json").write_text(json.dumps(lock, indent=2) + "\n", encoding="utf-8")


if __name__ == "__main__":
    main()
