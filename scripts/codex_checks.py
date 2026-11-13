#!/usr/bin/env python3
"""Run required, portable Codex regressions and retain their actual outcomes."""
import argparse
import datetime
import hashlib
import json
import platform
from pathlib import Path
import re
import subprocess
import sys
import time

ROOT = Path(__file__).resolve().parents[1]
PACKAGES = ["./internal/convo", "./internal/web"]
PATTERN = "Codex|ParseConvoLocate|DaemonLog|TurnIndex"
# These are opt-in inspections of personal logs, not portable regressions.
EXTERNAL = {"TestCodexBackReal", "TestCodexTurnErrorReal", "TestCodexContractArtifacts", "TestCodexContractModelFlow", "TestCodexDaemonLiveBinding"}
REQUIRED = {"TestParseCodexPicker", "TestCodexModelFlow", "TestCodexBacktrack", "TestCodexFds"}


def source_state():
    def git(*args):
        return subprocess.check_output(["git", *args], cwd=ROOT)
    digest = hashlib.sha256()
    for name in sorted(git("ls-files", "-z", "--cached", "--others", "--exclude-standard").split(b"\0")):
        if name:
            path = ROOT / name.decode()
            digest.update(name + b"\0")
            if path.is_file():
                digest.update(path.read_bytes())
    return {"commit": git("rev-parse", "HEAD").decode().strip(),
            "dirty": bool(git("status", "--porcelain")), "source_sha256": digest.hexdigest()}


def outcomes(lines, returncode, expected):
    tests, failures = {}, []
    for line in lines.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        name, action = event.get("Test"), event.get("Action")
        if name and "/" not in name and action in ("pass", "fail", "skip"):
            tests[event.get("Package", "") + "/" + name] = action
        if action == "skip" or action == "fail":
            failures.append({"package": event.get("Package"), "test": name, "status": action})
    observed = {key.rsplit("/", 1)[-1] for key in tests}
    missing = sorted(set(expected) - observed)
    ok = returncode == 0 and bool(tests) and not failures and not missing
    return {"passed": ok, "returncode": returncode, "tests": tests, "problems": failures, "missing": missing}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, help="JSON report (parent directory must exist)")
    args = parser.parse_args()
    began = time.monotonic()
    report = {"kind": "codex-regression", "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "source": source_state(), "platform": platform.platform(), "excluded_external_inspections": sorted(EXTERNAL),
              "passed": False}
    try:
        listed = subprocess.run(["go", "test", *PACKAGES, "-list", PATTERN], cwd=ROOT,
                                capture_output=True, text=True, timeout=180)
        if listed.returncode:
            raise RuntimeError("test discovery failed:\n" + listed.stdout + listed.stderr)
        names = sorted({s for s in listed.stdout.splitlines() if re.fullmatch(r"Test\w+", s)} - EXTERNAL)
        if not REQUIRED.issubset(names):
            raise RuntimeError("required tests not discovered: " + str(sorted(REQUIRED - set(names))))
        command = ["go", "test", *PACKAGES, "-count=1", "-timeout=180s", "-json", "-run", "^(" + "|".join(names) + ")$"]
        result = subprocess.run(command, cwd=ROOT, capture_output=True, text=True, timeout=240)
        report.update(outcomes(result.stdout, result.returncode, names))
        report["command"] = command
        if not report["passed"]:
            report["diagnostics"] = (result.stdout + result.stderr)[-16000:]
    except (OSError, RuntimeError, subprocess.SubprocessError) as error:
        report["error"] = str(error)
    report["seconds"] = round(time.monotonic() - began, 3)
    encoded = json.dumps(report, ensure_ascii=False, indent=2) + "\n"
    if args.output:
        args.output.write_text(encoded)
        print(json.dumps({"passed": report["passed"], "tests": len(report.get("tests", {})), "report": str(args.output),
                          "error": report.get("error")}, ensure_ascii=False))
    else:
        print(encoded, end="")
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
