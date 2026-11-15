#!/usr/bin/env python3
"""Exercise a real Codex TUI against a local scripted service, without personal credentials."""
import argparse
from contextlib import ExitStack
import datetime
import itertools
import json
import os
from pathlib import Path
import platform
import re
import shlex
import shutil
import signal
import struct
import subprocess
import sys
import tempfile
import threading
import time
import tomllib
import uuid
import zlib
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

from codex_checks import ROOT, source_state


class Fixture(ThreadingHTTPServer):
    daemon_threads = True

    def __init__(self):
        super().__init__(("127.0.0.1", 0), Handler)
        self.requests = []
        self.lock = threading.Lock()
        self.ids = itertools.count(1)


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *_):
        pass

    def do_GET(self):
        body = json.dumps({"object": "list", "data": [{"id": "gpt-test", "object": "model"}]}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def do_POST(self):
        req = json.loads(self.rfile.read(int(self.headers.get("Content-Length", "0"))))
        with self.server.lock:
            self.server.requests.append(req)
        items = req.get("input") or []
        words = [part.get("text", "") for item in items if isinstance(item, dict) and item.get("role") == "user"
                 for part in item.get("content", []) if isinstance(part, dict)]
        # Codex prepends an image marker to text in a message with attachments.
        topic = "#side"
        for word in reversed(words):
            match = re.search(r"#(?:multiline|approval|decline|image|new|second|after-rewind)\b", word)
            if match:
                topic = word[match.start():]
                break
        last = items[-1] if items else {}
        number = next(self.server.ids)
        if topic in ("#approval", "#decline") and last.get("role") == "user":
            marker = "approved.txt" if topic == "#approval" else "declined.txt"
            output = {"type": "function_call", "id": f"fc_{number}", "call_id": f"call_{number}", "name": "exec_command",
                      "arguments": json.dumps({"cmd": f"printf contract-marker > {marker}",
                                               "sandbox_permissions": "require_escalated",
                                               "justification": "Create a marker in the isolated contract directory"}), "status": "completed"}
        else:
            output = {"type": "message", "id": f"msg_{number}", "role": "assistant", "status": "completed",
                      "content": [{"type": "output_text", "text": "contract-reply " + topic, "annotations": []}]}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()

        def event(kind, **fields):
            self.wfile.write(("event: " + kind + "\ndata: " + json.dumps({"type": kind, **fields}) + "\n\n").encode())
            self.wfile.flush()

        try:
            event("response.created", response={"id": f"resp_{number}"})
            event("response.output_item.added", output_index=0, item=output)
            if output["type"] == "message":
                event("response.output_text.delta", output_index=0, item_id=output["id"], content_index=0,
                      delta=output["content"][0]["text"])
            event("response.output_item.done", output_index=0, item=output)
            event("response.completed", response={"id": f"resp_{number}", "usage": {
                "input_tokens": 1200, "output_tokens": 40, "total_tokens": 1240,
                "input_tokens_details": {"cached_tokens": 0}, "output_tokens_details": {"reasoning_tokens": 0}}})
        except (BrokenPipeError, ConnectionResetError):
            pass
        self.close_connection = True


class Terminal:
    def __init__(self, binary, directory, port):
        self.socket = "corral-contract-" + uuid.uuid4().hex
        self.root = directory
        self.home = directory / ".codex"
        self.home.mkdir()
        self.config = f'''model = "gpt-6.1-sol"
model_provider = "contract"
approval_policy = "on-request"
sandbox_mode = "workspace-write"
check_for_update_on_startup = false

[model_providers.contract]
name = "Local contract fixture"
base_url = "http://127.0.0.1:{port}/v1"
env_key = "CORRAL_CONTRACT_KEY"
wire_api = "responses"

[projects.{json.dumps(str(directory))}]
trust_level = "trusted"
'''
        (self.home / "config.toml").write_text(self.config)
        # An allowlist, rather than copying credentials, proxies or pane IDs.
        environment = {"PATH": os.environ.get("PATH", os.defpath), "TERM": "xterm-256color", "LANG": "C.UTF-8",
                       "CODEX_HOME": str(self.home), "CORRAL_CONTRACT_KEY": "fixture-only",
                       "XDG_CONFIG_HOME": str(directory / "config"), "XDG_CACHE_HOME": str(directory / "cache"),
                       "XDG_DATA_HOME": str(directory / "data"), "SHELL": "/bin/sh"}
        command = shlex.join(["env", "-i", *[key + "=" + value for key, value in environment.items()],
                              binary, "--no-daemon", "--no-alt-screen", "-C", str(directory)])
        try:
            self.tmux("new-session", "-d", "-s", "contract", "-x", "120", "-y", "40", command)
        except (OSError, subprocess.SubprocessError):
            self.close()
            raise

    def tmux(self, *args, data=None):
        return subprocess.check_output(["tmux", "-L", self.socket, "-f", "/dev/null", *args],
                                       input=data, stderr=subprocess.PIPE, timeout=10).decode()

    def screen(self):
        return self.tmux("capture-pane", "-p", "-t", "contract", "-S", "-200")

    def keys(self, *keys):
        self.tmux("send-keys", "-t", "contract", *keys)

    def paste(self, text):
        self.tmux("load-buffer", "-b", "contract", "-", data=text.encode())
        self.tmux("paste-buffer", "-p", "-d", "-b", "contract", "-t", "contract")

    def send(self, text):
        self.paste(text)
        time.sleep(0.3)
        self.keys("Enter")

    def rows(self):
        result = []
        for path in sorted((self.home / "sessions").rglob("*.jsonl")):
            for line in path.read_text().splitlines():
                try:
                    result.append(json.loads(line))
                except ValueError:
                    pass  # A concurrently written final line is read on the next poll.
        return result

    def close(self):
        subprocess.run(["tmux", "-L", self.socket, "kill-server"], capture_output=True, timeout=10)


def wait_for(test, description, timeout=25):
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if test():
            return
        time.sleep(0.15)
    raise AssertionError("timeout: " + description)


def run(binary, output):
    report = {"kind": "real-cli-local-fixture", "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "source": source_state(), "platform": platform.platform(), "passed": False, "cases": [],
              "not_covered": ["Corral browser/SSH integration", "herdr", "daemon", "real model providers", "physical devices"]}
    terminal = None
    server = Fixture()
    threading.Thread(target=server.serve_forever, daemon=True).start()
    began = time.monotonic()
    try:
        report["codex_version"] = subprocess.check_output([binary, "--version"], timeout=15).decode().strip()
        with ExitStack() as resources:
            temp = resources.enter_context(tempfile.TemporaryDirectory(prefix="corral-codex-contract-"))
            terminal = Terminal(binary, Path(temp), server.server_port)
            resources.callback(terminal.close)

            def evidence():
                try:
                    (output / "last-screen.txt").write_text(terminal.screen().replace(temp, "<fixture>"))
                    logs = output / "rollouts"
                    logs.mkdir(exist_ok=True)
                    for path in (terminal.home / "sessions").rglob("*.jsonl"):
                        (logs / path.name).write_text(path.read_text().replace(temp, "/home/fixture"))
                    report["rollout_names"] = sorted(path.name for path in logs.iterdir())
                    with server.lock:
                        report["request_inputs"] = [[{"role": item.get("role"),
                                                      "types": [part.get("type") for part in item.get("content", []) if isinstance(part, dict)]}
                                                     for item in req.get("input", []) if isinstance(item, dict) and item.get("role") == "user"]
                                                    for req in server.requests]
                except (OSError, subprocess.SubprocessError) as error:
                    report["evidence_error"] = str(error)

            resources.callback(evidence)

            def case(name, action):
                record = {"name": name, "status": "failed"}
                report["cases"].append(record)
                start = time.monotonic()
                try:
                    action()
                    record["status"] = "passed"
                    (output / (name + ".txt")).write_text(terminal.screen().replace(temp, "<fixture>"))
                finally:
                    record["seconds"] = round(time.monotonic() - start, 3)

            def said(text):
                return any(row.get("type") == "response_item" and row.get("payload", {}).get("type") == "message"
                           and any(part.get("text") == "contract-reply " + text for part in row["payload"].get("content", []))
                           for row in terminal.rows())

            def send_and_wait(text):
                terminal.send(text)
                wait_for(lambda: said(text), "CLI log contains the response to " + text)

            case("startup", lambda: wait_for(lambda: "›" in terminal.screen(), "Codex composer"))

            def model_picker():
                for attempt in range(10):
                    terminal.send("/model")
                    try:
                        wait_for(lambda: "Select Model" in terminal.screen(), "model picker", timeout=2)
                        break
                    except AssertionError:
                        if "Model selection is disabled until startup completes" not in terminal.screen():
                            raise
                else:
                    raise AssertionError("Codex startup never enabled model selection")
                report["startup_menu_retries"] = attempt
                (output / "model-picker-open.txt").write_text(terminal.screen().replace(temp, "/home/fixture"))
                terminal.keys("Escape")
                wait_for(lambda: "Select Model" not in terminal.screen().split("OpenAI Codex")[-1], "model picker closed")

            case("model-picker", model_picker)

            def model_flow():
                environment = {**os.environ, "CORRAL_CODEX_CONTRACT_TMUX": terminal.socket}
                result = subprocess.run(["go", "test", "./internal/web", "-count=1", "-timeout=60s", "-v", "-run", "^TestCodexContractModelFlow$"],
                                        cwd=ROOT, env=environment, capture_output=True, text=True, timeout=90)
                (output / "model-flow.txt").write_text(result.stdout + result.stderr)
                if result.returncode or "--- PASS: TestCodexContractModelFlow" not in result.stdout:
                    raise AssertionError("Corral's real model picker flow failed; see model-flow.txt")

            case("model-flow", model_flow)
            case("multiline", lambda: send_and_wait("#multiline 中文  连续空格\n第二行"))

            def approval(allow):
                topic = "#approval" if allow else "#decline"
                terminal.send(topic)
                wait_for(lambda: "Would you like to run" in terminal.screen(), "approval dialog")
                (output / ("approval-dialog.txt" if allow else "decline-dialog.txt")).write_text(terminal.screen().replace(temp, "<fixture>"))
                terminal.keys("1", "Enter") if allow else terminal.keys("Escape")
                marker = terminal.root / ("approved.txt" if allow else "declined.txt")
                if allow:
                    wait_for(lambda: marker.exists() and marker.read_text() == "contract-marker", "approved command ran")
                    wait_for(lambda: said(topic), "tool result returned to the fixture")
                else:
                    wait_for(lambda: "Would you like to run" not in terminal.screen(), "approval dismissed")
                    if marker.exists():
                        raise AssertionError("declined command created its marker")

            case("approval", lambda: approval(True))
            case("decline", lambda: approval(False))

            def image():
                # Generate all chunk checksums; permissive image decoders can
                # accept sample PNGs that Codex's stricter decoder rejects.
                path = terminal.root / "image.png"
                def chunk(kind, content):
                    return struct.pack("!I", len(content)) + kind + content + struct.pack("!I", zlib.crc32(kind + content))
                path.write_bytes(b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack("!2I5B", 16, 16, 8, 2, 0, 0, 0))
                                 + chunk(b"IDAT", zlib.compress((b"\0" + b"\xff\0\0" * 16) * 16)) + chunk(b"IEND", b""))
                terminal.paste(str(path))
                time.sleep(0.3)
                send_and_wait("#image")
                with server.lock:
                    received = any(part.get("type") == "input_image" for req in server.requests
                                   for item in req.get("input", []) if isinstance(item, dict) and item.get("role") == "user"
                                   for part in item.get("content", []) if isinstance(part, dict))
                if not received:
                    raise AssertionError("model fixture did not receive an image")

            case("image", image)

            def new_session():
                before = {str(path) for path in (terminal.home / "sessions").rglob("*.jsonl")}
                terminal.send("/new")
                wait_for(lambda: "contract-reply #image" not in terminal.screen().split("OpenAI Codex")[-1], "new conversation")
                send_and_wait("#new")
                after = {str(path) for path in (terminal.home / "sessions").rglob("*.jsonl")}
                if not after - before:
                    raise AssertionError("/new did not start a new rollout")

            case("new-session", new_session)
            case("second-turn", lambda: send_and_wait("#second"))

            def rewind(topic="#after-rewind", removed="#second"):
                terminal.keys("Escape")
                time.sleep(0.3)
                terminal.keys("Escape")
                wait_for(lambda: "rewind" in terminal.screen().lower(), "transcript browser")
                terminal.keys("Enter")
                wait_for(lambda: "reverted" in terminal.screen().lower(), "rewind applied")
                terminal.keys("C-u")
                send_and_wait(topic)
                with server.lock:
                    candidates = [req for req in server.requests if any(part.get("text") == topic
                                  for item in req.get("input", []) if isinstance(item, dict) and item.get("role") == "user"
                                  for part in item.get("content", []) if isinstance(part, dict))]
                if not candidates:
                    raise AssertionError("no model request carried the post-rewind message")
                request = candidates[-1]
                retained = [part.get("text", "") for item in request.get("input", [])
                            if isinstance(item, dict) and item.get("role") == "user"
                            for part in item.get("content", []) if isinstance(part, dict)]
                if any(text == removed for text in retained):
                    raise AssertionError("rewound message is still in the model context")

            case("rewind", rewind)
            case("rewind-again", lambda: rewind("#after-rewind-2", "#after-rewind"))
            before = tomllib.loads(terminal.config)
            after = tomllib.loads((terminal.home / "config.toml").read_text())
            defaults = ("model", "model_provider", "model_reasoning_effort", "approval_policy", "sandbox_mode", "model_providers")
            if any(before.get(key) != after.get(key) for key in defaults):
                raise AssertionError("test changed its model, provider, effort or permission defaults")
            report["defaults_unchanged"] = True
        environment = {**os.environ, "CORRAL_CODEX_CONTRACT_DIR": str(output / "rollouts")}
        result = subprocess.run(["go", "test", "./internal/web", "-count=1", "-timeout=60s", "-v", "-run", "^TestCodexContractArtifacts$"],
                                cwd=ROOT, env=environment, capture_output=True, text=True, timeout=90)
        (output / "parser-validation.txt").write_text(result.stdout + result.stderr)
        report["parser_validation"] = {"returncode": result.returncode, "passed": result.returncode == 0 and "--- PASS: TestCodexContractArtifacts" in result.stdout}
        if not report["parser_validation"]["passed"]:
            report["passed"] = False
            raise AssertionError("Corral did not accept the real CLI's artifacts; see parser-validation.txt")
        report["passed"] = True
    except (OSError, AssertionError, subprocess.SubprocessError) as error:
        report["passed"] = False
        report["error"] = str(error)
    finally:
        server.shutdown()
        server.server_close()
        report["seconds"] = round(time.monotonic() - began, 3)
        report["request_count"] = len(server.requests)
        (output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({key: report[key] for key in ("passed", "codex_version", "cases", "parser_validation", "error", "not_covered") if key in report},
                     ensure_ascii=False, indent=2))
    return report["passed"]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--codex", default="codex")
    parser.add_argument("--output", type=Path, required=True, help="new directory for this run's evidence")
    args = parser.parse_args()
    binary = shutil.which(args.codex)
    if not binary or not shutil.which("tmux"):
        parser.error("codex and tmux are required; nothing was started")
    args.output.mkdir(parents=True, exist_ok=False)
    for signum in (signal.SIGTERM, signal.SIGINT):
        signal.signal(signum, lambda *_: sys.exit(130))
    return 0 if run(binary, args.output) else 1


if __name__ == "__main__":
    sys.exit(main())
