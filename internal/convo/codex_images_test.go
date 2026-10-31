package convo

import "testing"

// A message of the person's with pictures in it (real Codex 0.164 rows,
// cxprobe6.py): the pictures, and their words without "[Image #N]".
func TestCodexUserPictures(t *testing.T) {
	c := feedAll(
		`{"timestamp":"2026-10-31T08:10:00.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u1","content":[{"type":"local_image","path":"/home/alice/cxreal/p1.png"},{"type":"local_image","path":"/home/alice/cxreal/p2.png"},{"type":"text","text":"[Image #1]  [Image #2]  两张\n第二行","text_elements":[]}]}}}`+"\n",
		`{"timestamp":"2026-10-31T08:10:01.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u2","content":[{"type":"local_image","path":"/home/alice/cxreal/p1.png"},{"type":"text","text":"先说两句 [Image #1]"}]}}}`+"\n",
		`{"timestamp":"2026-10-31T08:10:02.000Z","type":"event_msg","payload":{"type":"item_completed","item":{"type":"UserMessage","id":"u3","content":[{"type":"text","text":"no picture: [Image #1] stays"}]}}}`+"\n",
	)
	es := c.Since(0)
	if len(es) != 3 {
		t.Fatalf("entries: %d", len(es))
	}
	want := []struct {
		pics int
		text string
	}{{2, "两张\n第二行"}, {1, "先说两句"}, {0, "no picture: [Image #1] stays"}}
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
