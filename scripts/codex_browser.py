#!/usr/bin/env python3
"""Browser component regressions using production conversation code and synthetic API responses."""
import argparse
import datetime
import json
import os
from pathlib import Path
import platform
import shlex
import signal
import socket
import struct
import subprocess
import sys
import time
import urllib.request
import zlib

from codex_checks import ROOT, source_state


def png():
    def chunk(kind, data):
        return struct.pack("!I", len(data)) + kind + data + struct.pack("!I", zlib.crc32(kind + data))
    return (b"\x89PNG\r\n\x1a\n" + chunk(b"IHDR", struct.pack("!2I5B", 16, 16, 8, 2, 0, 0, 0))
            + chunk(b"IDAT", zlib.compress((b"\0" + b"\xff\0\0" * 16) * 16)) + chunk(b"IEND", b""))


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    parser.add_argument("--engines", nargs="+", choices=["chromium", "firefox", "webkit"], default=["chromium", "firefox", "webkit"])
    parser.add_argument("--smoke", action="store_true", help="one phone/dark/Chinese case per engine")
    parser.add_argument("--webkit-library-path", type=Path, help="optional extracted Linux runtime libraries; no system installation")
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {"kind": "browser-component-synthetic-api", "source": source_state(), "passed": False, "cases": [],
              "platform": platform.platform(), "browsers": {}, "expected_cases": len(args.engines) * (1 if args.smoke else 12),
              "started_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "not_covered": ["SSH and real CLI integration", "physical devices", "real provider performance"]}
    process = None
    try:
        from playwright.sync_api import sync_playwright
        with socket.socket() as reservation:
            reservation.bind(("127.0.0.1", 0))
            port = reservation.getsockname()[1]
        base = f"http://127.0.0.1:{port}"
        with (args.output / "vite.log").open("w") as log:
            process = subprocess.Popen([str(ROOT / "web/node_modules/.bin/vite"), "--host", "127.0.0.1", "--port", str(port), "--strictPort"],
                                       cwd=ROOT / "web", stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        deadline = time.monotonic() + 30
        while True:
            if process.poll() is not None or time.monotonic() > deadline:
                raise RuntimeError("fixture server failed to start; see vite.log")
            try:
                with urllib.request.urlopen(base + "/test/codex.html", timeout=1) as response:
                    if response.status == 200:
                        break
            except OSError:
                time.sleep(0.1)

        entries = [
            {"seq": 1, "rev": 1, "id": "u1", "role": "user", "parts": [{"kind": "text", "text": "检查文件并查看图片"}, {"kind": "image", "path": "/home/fixture/image.png"}]},
            {"seq": 2, "rev": 1, "id": "steps", "role": "assistant", "parts": [
                {"kind": "thinking", "text": "**Checking native stream**\n\nKeep the selected conversation stable."},
                {"kind": "thinking", "text": "**Planning event replay**"},
                {"kind": "tool", "tool": {"name": "Bash", "detail": "python3 -B -c 'inspect selected conversation'", "result": "checked", "done": True}},
            ]},
            {"seq": 3, "rev": 1, "id": "a1", "role": "assistant", "parts": [{"kind": "text", "text": "Checked [file.go:12](/home/fixture/project/file.go:12).\n\n```go\nfmt.Println(\"done\")\n```"}]},
        ]
        with sync_playwright() as playwright:
            for engine in args.engines:
                browser_type = getattr(playwright, engine)
                launch = {}
                if engine == "webkit" and args.webkit_library_path:
                    # The bundled launcher replaces LD_LIBRARY_PATH. Use a
                    # wrapper of our own, confined to this evidence directory.
                    bundle = Path(browser_type.executable_path).parent / "minibrowser-wpe"
                    wrapper = args.output / "webkit-launcher.sh"
                    environment = {"WEBKIT_EXEC_PATH": str(bundle / "bin"), "WEBKIT_INJECTED_BUNDLE_PATH": str(bundle / "lib"),
                                   "WEBKIT_INSPECTOR_RESOURCES_PATH": str(bundle / "share"), "WEBKIT_FORCE_COMPLEX_TEXT": "1",
                                   "LD_LIBRARY_PATH": ":".join(map(str, (args.webkit_library_path.resolve(), bundle / "lib", bundle / "sys/lib")))}
                    wrapper.write_text("#!/bin/sh\n" + "\n".join("export " + name + "=" + shlex.quote(value) for name, value in environment.items())
                                       + "\nexec " + shlex.quote(str(bundle / "bin/MiniBrowser")) + ' "$@"\n')
                    wrapper.chmod(0o700)
                    launch["executable_path"] = str(wrapper)
                browser = browser_type.launch(**launch)
                report["browsers"][engine] = browser.version
                try:
                    for size, dimensions in {"phone": (390, 844), "tablet": (820, 1180), "desktop": (1440, 900)}.items():
                        for theme in ("light", "dark"):
                            for language in ("zh", "en"):
                                if args.smoke and (size, theme, language) != ("phone", "dark", "zh"):
                                    continue
                                record = {"name": f"{engine}-{size}-{theme}-{language}", "status": "failed"}
                                report["cases"].append(record)
                                start = time.monotonic()
                                context = browser.new_context(viewport={"width": dimensions[0], "height": dimensions[1]},
                                                              locale="zh-CN" if language == "zh" else "en-US", has_touch=size != "desktop")
                                page = context.new_page()
                                page.set_default_timeout(10000)
                                errors, calls = [], []
                                page.on("pageerror", lambda error: errors.append(str(error)))
                                flags = {"history_partial": False, "forked": False}

                                def route_api(route):
                                    url = route.request.url
                                    if "/files/view?" in url:
                                        route.fulfill(status=200, content_type="image/png", body=png())
                                    elif "/conversation/unlook" in url:
                                        route.fulfill(status=200, json={"ok": True})
                                    elif "/conversation?" in url:
                                        calls.append(url)
                                        route.fulfill(status=200, json={"available": True, "gen": 1, "rev": 1,
                                            "bind": {"pane": "p1", "pid": 7, "start": 70, "session": "01a1cdc3-eed2-7582-b2a4-5acb0d4ec0e3", "agent": "codex"},
                                            "entries": entries if "rev=0" in url else [], "meta": {"model": "gpt-test", **flags}})
                                    else:
                                        errors.append("unexpected API: " + url)
                                        route.fulfill(status=500, json={"error": "unexpected_fixture_request"})

                                context.route("**/api/**", route_api)
                                try:
                                    page.goto(base + f"/test/codex.html?theme={theme}&lang={language}")
                                    page.locator(".convo-user").wait_for()
                                    notice = page.locator(".convo-older")
                                    flags["history_partial"] = True
                                    page.evaluate("window.dispatchEvent(new Event('fixture-refresh'))")
                                    expected = "回退后的新消息" if language == "zh" else "Only new messages after the rewind"
                                    page.wait_for_function("text => document.querySelector('.convo-older')?.textContent.includes(text)", arg=expected)
                                    if len(calls) < 2:
                                        raise AssertionError("metadata was not refreshed")
                                    page.locator(".convo-user .convo-pic").click()
                                    image = page.locator(".picture-img")
                                    image.wait_for()
                                    page.wait_for_function("() => document.querySelector('.picture-img')?.naturalWidth === 16")
                                    page.get_by_role("button", name="关闭" if language == "zh" else "Close", exact=True).click()
                                    if "[file.go:12](/" in page.locator(".convo").inner_text():
                                        raise AssertionError("file link rendered as raw Markdown")
                                    page.get_by_role("button", name="file.go:12", exact=True).click()
                                    page.get_by_role("menu").wait_for()
                                    page.keyboard.press("Escape")
                                    page.locator(".convo-work-head").first.click()
                                    thought = page.locator(".convo-step.is-thought .convo-step-head").first
                                    if thought.inner_text().strip() != "Checking native stream":
                                        raise AssertionError("reasoning summary contains a repeated label or raw Markdown")
                                    thought.click()
                                    if page.locator(".convo-thinking strong").first.inner_text() != "Checking native stream":
                                        raise AssertionError("expanded reasoning does not render Markdown")
                                    overflow = page.evaluate("Math.max(document.documentElement.scrollWidth - innerWidth, document.querySelector('.convo').scrollWidth - document.querySelector('.convo').clientWidth)")
                                    if overflow > 1:
                                        raise AssertionError(f"horizontal overflow: {overflow}px")
                                    if errors:
                                        raise AssertionError(str(errors))
                                    page.screenshot(path=str(args.output / (record["name"] + ".png")))
                                    record["status"] = "passed"
                                    record["history_notice"] = notice.inner_text()
                                except Exception as error:
                                    record["error"] = str(error)
                                    record["page_errors"] = errors
                                    page.screenshot(path=str(args.output / (record["name"] + "-failed.png")))
                                finally:
                                    record["seconds"] = round(time.monotonic() - start, 3)
                                    context.close()
                finally:
                    browser.close()
        report["passed"] = len(report["cases"]) == report["expected_cases"] and all(case["status"] == "passed" for case in report["cases"])
    except Exception as error:
        report["error"] = str(error)
    finally:
        if process and process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try:
                process.wait(timeout=10)
            except subprocess.TimeoutExpired:
                os.killpg(process.pid, signal.SIGKILL)
                process.wait(timeout=10)
        (args.output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps({"passed": report["passed"], "cases": len(report["cases"]),
                      "failures": [case for case in report["cases"] if case["status"] != "passed"], "error": report.get("error")}, ensure_ascii=False, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    sys.exit(main())
