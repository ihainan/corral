package web

import "testing"

// A Codex subagent's log is read as a Codex log: its words and its
// command (it was read as a Claude subagent's, nothing in it then).
func TestCodexSubParser(t *testing.T) {
	st := &convoState{sub: true, agent: "codex"}
	p := st.newParser()
	for _, l := range []string{
		`{"timestamp":"2026-10-31T06:00:00Z","type":"session_meta","payload":{"id":"01a1909b-aa1e-76f1-ad4d-0bf3d35d3293","parent_thread_id":"01a1909b-93b8-7781-a6eb-8b5dd01fde0b","thread_source":"subagent"}}`,
		`{"timestamp":"2026-10-31T06:00:01Z","type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"#sub 数一数文件"}]}}`,
		`{"timestamp":"2026-10-31T06:00:02Z","type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"子代理开工，先列目录。"}]}}`,
		`{"timestamp":"2026-10-31T06:00:03Z","type":"response_item","payload":{"type":"function_call","name":"exec_command","arguments":"{\"cmd\": \"ls\"}","call_id":"call_12"}}`,
	} {
		p.Feed([]byte(l + "\n"))
	}
	var words, cmd bool
	for _, e := range p.Since(0) {
		for _, pt := range e.Parts {
			words = words || pt.Text == "子代理开工，先列目录。"
			cmd = cmd || (pt.Tool != nil && pt.Tool.Name == "Bash")
		}
	}
	if !words || !cmd {
		t.Errorf("its words %v, its command %v", words, cmd)
	}
}
