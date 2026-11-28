package convo

import (
	"slices"
	"testing"
)

func TestClaudePeerOriginAndVerifiedProcess(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","msg_id":"m1","name":"ai-68","fromMode":"prompting","body":"hello from peer"},` +
			`"message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\" from-mode=\"prompting\">\nhello from peer\n</cross-session-message>"}}` + "\n" +
			`{"type":"assistant","uuid":"a1","timestamp":"2026-11-28T03:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"SendMessage","input":{"to":"uds:/tmp/cc-socks/530048.sock","summary":"Ack","message":"got it"}}]}}` + "\n" +
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:08Z","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\" from-mode=\"prompting\">\nsecond note\n</cross-session-message>"}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	ev := entries[0].Parts[0].Event
	if ev == nil || ev.Kind != EventPeer {
		t.Fatalf("entry 0 event = %+v, want EventPeer", ev)
	}
	if ev.From != "ai-68" || ev.PeerPID != 530048 || ev.PeerStart != 193322812 || ev.Text != "hello from peer" {
		t.Fatalf("EventPeer = %+v", ev)
	}
	tool := entries[1].Parts[0].Tool
	if tool == nil || tool.Name != "SendMessage" {
		t.Fatalf("entry 1 tool = %+v", tool)
	}
	if tool.Title != "ai-68" || tool.PeerPID != 530048 || tool.PeerStart != 193322812 {
		t.Fatalf("SendMessage tool = %+v, want ai-68 + verified peer proc", tool)
	}
	queued := c.Queued()
	if len(queued) != 1 {
		t.Fatalf("queued = %+v, want 1", queued)
	}
	if queued[0].From != "ai-68" || queued[0].PeerPID != 530048 || queued[0].PeerStart != 193322812 {
		t.Fatalf("queued[0] = %+v, want verified peer proc from c.peers", queued[0])
	}
	refs := PeerRefs(c)
	wantRefs := []PeerRef{{PID: 530048, Start: 193322812}}
	if !slices.Equal(refs, wantRefs) {
		t.Fatalf("PeerRefs = %+v, want %+v", refs, wantRefs)
	}
}

func TestClaudePeerOriginRequiresBothPidAndStart(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/123.sock","verifiedPeerPid":123,"verifiedPeerProcStart":"not-a-num","name":"ai-12","body":"body fallback"},` +
			`"message":{"role":"user","content":""}}` + "\n" +
			`{"type":"user","isMeta":true,"uuid":"u2","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/456.sock","verifiedPeerPid":"456","verifiedPeerProcStart":98765,"name":"ai-34","body":"numeric start"},` +
			`"message":{"role":"user","content":[{"type":"text","text":"<cross-session-message from=\"uds:/tmp/cc-socks/456.sock\" from-name=\"ai-34\">\nnumeric start\n</cross-session-message>"}]}}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2", len(entries))
	}
	ev0 := entries[0].Parts[0].Event
	if ev0.From != "ai-12" || ev0.PeerPID != 0 || ev0.PeerStart != 0 || ev0.Text != "body fallback" {
		t.Fatalf("ev0 = %+v, want unverified pid/start zeroed", ev0)
	}
	ev1 := entries[1].Parts[0].Event
	if ev1.From != "ai-34" || ev1.PeerPID != 456 || ev1.PeerStart != 98765 || ev1.Text != "numeric start" {
		t.Fatalf("ev1 = %+v, want parsed pid=456 start=98765", ev1)
	}
	refs := PeerRefs(c)
	if len(refs) != 1 || refs[0] != (PeerRef{PID: 456, Start: 98765}) {
		t.Fatalf("PeerRefs = %+v", refs)
	}
}

func TestClaudePeerOriginPrependMergesRefs(t *testing.T) {
	older := NewClaude()
	older.Feed([]byte(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u0","timestamp":"2026-11-28T02:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/111.sock","verifiedPeerPid":111,"verifiedPeerProcStart":"222","name":"ai-01"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/111.sock\" from-name=\"ai-01\">\nold\n</cross-session-message>"}}` + "\n",
	))
	newer := NewClaude()
	newer.Feed([]byte(
		`{"type":"user","uuid":"u1","timestamp":"2026-11-28T03:00:00Z","message":{"role":"user","content":"hi"}}` + "\n",
	))
	if refs := PeerRefs(newer); len(refs) != 0 {
		t.Fatalf("before Prepend: PeerRefs = %+v, want empty", refs)
	}
	newer.Prepend(older)
	refs := PeerRefs(newer)
	if len(refs) != 1 || refs[0] != (PeerRef{PID: 111, Start: 222}) {
		t.Fatalf("after Prepend: PeerRefs = %+v", refs)
	}
}
