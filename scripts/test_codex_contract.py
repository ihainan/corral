import json
import threading
import unittest
import urllib.request

from codex_contract import Fixture


class ScriptedService(unittest.TestCase):
    def test_image_marker_does_not_replay_previous_approval(self):
        server = Fixture()
        thread = threading.Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            # Codex preserves the earlier conversation and prepends a marker
            # to an attached image's message. It must start the new topic.
            body = {"input": [
                {"role": "user", "content": [{"type": "input_text", "text": "#decline"}]},
                {"role": "user", "content": [{"type": "input_text", "text": "[Image #1] #image"},
                                              {"type": "input_image", "image_url": "data:image/png;base64,fixture"}]},
            ]}
            req = urllib.request.Request(f"http://127.0.0.1:{server.server_port}/v1/responses", data=json.dumps(body).encode(),
                                         headers={"Content-Type": "application/json"})
            with urllib.request.urlopen(req, timeout=3) as response:
                events = [json.loads(line[6:]) for line in response.read().decode().splitlines() if line.startswith("data: ")]
            items = [event["item"] for event in events if event["type"] == "response.output_item.done"]
            self.assertEqual(len(items), 1)
            self.assertEqual(items[0]["type"], "message")
            self.assertEqual(items[0]["content"][0]["text"], "contract-reply #image")
        finally:
            server.shutdown()
            server.server_close()
            thread.join(timeout=1)


if __name__ == "__main__":
    unittest.main()
