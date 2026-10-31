package convo

import "testing"

// A message of the person's with pictures in it (as real Codex 0.164 rows,
// cxprobe6.py): the pictures, and their words without the "[Image #N]"
// Codex put where it says it did (text_elements) — the same words the
// person wrote stay (review505).
func TestCodexUserPictures(t *testing.T) {
	c := feedAll(
		"{\"timestamp\": \"2026-10-31T08:10:01Z\", \"type\": \"event_msg\", \"payload\": {\"type\": \"item_completed\", \"item\": {\"type\": \"UserMessage\", \"id\": \"u1\", \"content\": [{\"type\": \"local_image\", \"path\": \"/home/alice/cxreal/p1.png\"}, {\"type\": \"local_image\", \"path\": \"/home/alice/cxreal/p2.png\"}, {\"type\": \"text\", \"text\": \"[Image #1]  [Image #2]  两张\\n第二行\", \"text_elements\": [{\"byte_range\": {\"start\": 0, \"end\": 10}, \"placeholder\": \"[Image #1]\"}, {\"byte_range\": {\"start\": 12, \"end\": 22}, \"placeholder\": \"[Image #2]\"}]}]}}}\n",
		"{\"timestamp\": \"2026-10-31T08:10:02Z\", \"type\": \"event_msg\", \"payload\": {\"type\": \"item_completed\", \"item\": {\"type\": \"UserMessage\", \"id\": \"u2\", \"content\": [{\"type\": \"local_image\", \"path\": \"/home/alice/cxreal/p1.png\"}, {\"type\": \"text\", \"text\": \"先说两句 [Image #1]\", \"text_elements\": [{\"byte_range\": {\"start\": 13, \"end\": 23}, \"placeholder\": \"[Image #1]\"}]}]}}}\n",
		"{\"timestamp\": \"2026-10-31T08:10:03Z\", \"type\": \"event_msg\", \"payload\": {\"type\": \"item_completed\", \"item\": {\"type\": \"UserMessage\", \"id\": \"u3\", \"content\": [{\"type\": \"text\", \"text\": \"no picture: [Image #1] stays\"}]}}}\n",
		"{\"timestamp\": \"2026-10-31T08:10:04Z\", \"type\": \"event_msg\", \"payload\": {\"type\": \"item_completed\", \"item\": {\"type\": \"UserMessage\", \"id\": \"u4\", \"content\": [{\"type\": \"local_image\", \"path\": \"/home/alice/cxreal/p1.png\"}, {\"type\": \"text\", \"text\": \"[Image #1]  请保留字面量 [Image #1]，这是示例\", \"text_elements\": [{\"byte_range\": {\"start\": 0, \"end\": 10}, \"placeholder\": \"[Image #1]\"}]}]}}}\n",
		"{\"timestamp\": \"2026-10-31T08:10:05Z\", \"type\": \"event_msg\", \"payload\": {\"type\": \"item_completed\", \"item\": {\"type\": \"UserMessage\", \"id\": \"u5\", \"content\": [{\"type\": \"local_image\", \"path\": \"/home/alice/cxreal/p1.png\"}, {\"type\": \"text\", \"text\": \"[Image #1] 旧版没有位置\", \"text_elements\": []}]}}}\n",
	)
	es := c.Since(0)
	want := []struct {
		pics int
		text string
	}{{2, "两张\n第二行"}, {1, "先说两句"}, {0, "no picture: [Image #1] stays"}, {1, "请保留字面量 [Image #1]，这是示例"}, {1, "[Image #1] 旧版没有位置"}}
	if len(es) != len(want) {
		t.Fatalf("entries: %d", len(es))
	}
	for i, e := range es {
		pics, text := 0, ""
		for _, p := range e.Parts {
			if p.Kind == KindImage {
				pics++
			}
			if p.Kind == KindText {
				text = p.Text
			}
		}
		if pics != want[i].pics || text != want[i].text {
			t.Errorf("%d: %d pictures, %q", i, pics, text)
		}
	}
}
