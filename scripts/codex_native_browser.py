#!/usr/bin/env python3
"""Exercise the native Chat page with the real app and controlled engine events."""
import argparse
import json
import os
from pathlib import Path
import signal
import socket
import subprocess
import time
import urllib.parse
import urllib.request

from codex_checks import ROOT, source_state

THREAD = "01a1cd1d-b2d0-7341-95d1-b5323eefbb97"
OTHER = "01a1cd90-b507-78e1-983f-f03098fca3c3"
STREAM = """
window.__sources = [];
window.EventSource = class {
  constructor(url) { this.url=url; this.closed=false; window.__sources.push(this);
    setTimeout(()=>{if(!this.closed)this.onmessage?.({data:JSON.stringify({method:'ready',seq:10,requests:[]})})},20);
  }
  close() { this.closed=true; }
};
window.__emit = e => { for(const s of window.__sources) if(!s.closed)s.onmessage?.({data:JSON.stringify({...e,thread:window.location.pathname.split('/').pop()})}); };
"""


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    args.output.mkdir(parents=True, exist_ok=False)
    report = {"kind": "native-chat-browser-controlled-rpc", "source": source_state(), "passed": False, "cases": [],
              "not_covered": ["physical phones", "real provider inference", "SSH transport (covered by Go integration tests)"]}
    process = None
    try:
        from playwright.sync_api import sync_playwright
        with socket.socket() as reserve:
            reserve.bind(("127.0.0.1", 0)); port = reserve.getsockname()[1]
        base = f"http://127.0.0.1:{port}"
        with (args.output / "vite.log").open("w") as log:
            process = subprocess.Popen([str(ROOT / "web/node_modules/.bin/vite"), "--host", "127.0.0.1", "--port", str(port), "--strictPort"],
                                       cwd=ROOT / "web", stdout=log, stderr=subprocess.STDOUT, start_new_session=True)
        for _ in range(100):
            try:
                urllib.request.urlopen(base, timeout=1).close(); break
            except OSError:
                if process.poll() is not None: raise RuntimeError("Vite failed")
                time.sleep(.1)
        with sync_playwright() as playwright:
            browser = playwright.chromium.launch()
            try:
                for width in (390, 768, 1280):
                    for theme in ("light", "dark"):
                        for lang in ("zh-CN", "en"):
                            case = {"width": width, "theme": theme, "language": lang, "status": "failed"}
                            report["cases"].append(case)
                            context = browser.new_context(viewport={"width": width, "height": 900}, color_scheme=theme)
                            page = context.new_page(); errors = []; posts = []; state = {"connected": False, "unknown": False}
                            page.on("pageerror", lambda error: errors.append(str(error)))
                            context.add_init_script(STREAM + f"localStorage.setItem('corral.lang',{json.dumps(lang)});")
                            def route_api(route):
                                request = route.request; path = urllib.parse.urlparse(request.url).path
                                data = {}; status = 200
                                if path == "/api/bootstrap":
                                    data = {"version": "native-fixture", "setup_needed": False, "default_language": lang, "user": {"id": 1, "username": "fixture", "role": "member"}}
                                elif path == "/api/sessions":
                                    data = [{"id": "fixture", "host_id": 1, "host_name": "Local fixture", "state": "running"}]
                                elif path.endswith("/models"):
                                    data = {"data": [{"id": "fixture", "model": "fixture", "displayName": "Fixture", "supportedReasoningEfforts": [{"reasoningEffort": "low"}, {"reasoningEffort": "high"}]}]}
                                elif request.method == "POST" and "/codex/threads" in path:
                                    body = request.post_data_json; posts.append(body)
                                    if body["action"] == "connect": state["connected"] = True
                                    if body["action"] == "send" and state["unknown"]:
                                        status = 409; data = {"error": "outcome_unknown", "message": "Refresh before sending again"}
                                    else: data = {"accepted": True, "thread": {"id": THREAD}}
                                elif path.endswith("/upload"):
                                    status = 201; data = {"path": "/tmp/fixture.png"}
                                elif "/codex/threads" in path:
                                    thread = {"id": path.split("/")[-1] if not path.endswith("/threads") else THREAD, "name": "Native test", "preview": "Review project", "cwd": "/tmp/project", "model": "fixture", "reasoningEffort": "high", "status": {"type": "idle"}}
                                    if path.endswith("/threads"):
                                        data = {"data": [thread, {**thread, "id": OTHER, "name": "Other conversation"}], "nextCursor": None}
                                    else:
                                        data = {"thread": thread, "connected": state["connected"], "seq": 10, "turn": "", "requests": [], "page": {"nextCursor": None, "data": [
                                            {"turnId": "t1", "completedAtMs": 1, "item": {"id": "a1", "type": "agentMessage", "text": "**Readable answer**\n\n```go\nfmt.Println(1)\n```"}},
                                            {"turnId": "t1", "completedAtMs": 1, "item": {"id": "tool1", "type": "agentMessage", "text": "[external agent tool call: Bash]\ndescription: Inspect repository\ncommand: " + "long_script_line\n" * 200}},
                                            {"turnId": "t1", "completedAtMs": 1, "item": {"id": "r1", "type": "reasoning", "summary": ["**Checking native stream**"]}},
                                            {"turnId": "t1", "completedAtMs": 1, "item": {"id": "u1", "type": "userMessage", "content": [{"type": "text", "text": '<send_user_message_question_reply>[{"question":"Continue?","answer":"Yes"}]</send_user_message_question_reply>'}]}}]}}
                                route.fulfill(status=status, content_type="application/json", body=json.dumps(data))
                            page.route("**/api/**", route_api)
                            try:
                                page.goto(base + "/codex/fixture/" + THREAD)
                                page.locator(".native-answer strong").wait_for()
                                assert page.locator(".native-answer strong").inner_text() == "Readable answer"
                                assert "send_user_message" not in page.locator(".native-user").inner_text()
                                assert not page.locator(".native-tool pre").first.is_visible()
                                page.locator(".native-steps > summary").click()
                                assert page.locator(".native-thought > summary").inner_text() == "Checking native stream"
                                page.locator(".native-steps > summary").click()
                                page.locator(".native-connect button").click()
                                page.wait_for_function("window.__sources.some(s=>!s.closed)")
                                page.locator(".native-composer button[type=submit]").wait_for(state="visible")
                                page.wait_for_timeout(60)
                                page.evaluate("window.__emit({seq:11,method:'item/agentMessage/delta',params:{itemId:'live',turnId:'t2',delta:'Stream '}})")
                                page.get_by_text("Stream", exact=True).wait_for()
                                page.evaluate("window.__emit({seq:12,method:'item/agentMessage/delta',params:{itemId:'live',turnId:'t2',delta:'continues'}})")
                                page.get_by_text("Stream continues", exact=True).wait_for()
                                page.evaluate("window.__emit({seq:13,method:'item/completed',params:{turnId:'t2',item:{id:'live',type:'agentMessage',text:'Stream complete'}}})")
                                page.get_by_text("Stream complete", exact=True).wait_for()
                                assert page.get_by_text("Stream complete", exact=True).count() == 1
                                composer = page.locator(".native-composer textarea")
                                composer.fill("one submission"); composer.press("Enter"); composer.press("Enter")
                                page.wait_for_function("document.querySelector('.native-composer textarea').value === ''")
                                assert len([p for p in posts if p["action"] == "send"]) == 1
                                page.locator('input[type=file]').set_input_files({"name": "image.png", "mimeType": "image/png", "buffer": b"fixture"})
                                page.locator(".native-attachments button").wait_for(); composer.fill("with image"); composer.press("Enter")
                                page.wait_for_function("document.querySelector('.native-composer textarea').value === ''")
                                assert [p for p in posts if p["action"] == "send"][-1]["images"] == ["/tmp/fixture.png"]
                                state["unknown"] = True; composer.fill("uncertain"); composer.press("Enter")
                                page.locator(".native-error").wait_for(); assert composer.input_value() == "uncertain"
                                page.wait_for_timeout(50); composer.press("Enter"); page.wait_for_timeout(100)
                                unknown = [p for p in posts if p["action"] == "send" and p["text"] == "uncertain"]
                                assert len(unknown) == 2 and unknown[0]["key"] == unknown[1]["key"]
                                overflow = page.evaluate("document.documentElement.scrollWidth - innerWidth")
                                assert overflow <= 1, overflow
                                assert not errors, errors
                                page.screenshot(path=str(args.output / f"{width}-{theme}-{lang}.png"))
                                case["status"] = "passed"
                            except Exception as error:
                                case["error"] = str(error); case["page_errors"] = errors
                                page.screenshot(path=str(args.output / f"{width}-{theme}-{lang}-failed.png"))
                            finally:
                                context.close()
            finally:
                browser.close()
        report["passed"] = len(report["cases"]) == 12 and all(case["status"] == "passed" for case in report["cases"])
    except Exception as error:
        report["error"] = str(error)
    finally:
        if process and process.poll() is None:
            os.killpg(process.pid, signal.SIGTERM)
            try: process.wait(timeout=10)
            except subprocess.TimeoutExpired: os.killpg(process.pid, signal.SIGKILL); process.wait(timeout=10)
        (args.output / "report.json").write_text(json.dumps(report, ensure_ascii=False, indent=2) + "\n")
    print(json.dumps(report, ensure_ascii=False, indent=2))
    return 0 if report["passed"] else 1


if __name__ == "__main__":
    raise SystemExit(main())
