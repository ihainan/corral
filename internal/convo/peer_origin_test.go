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

// Regression #3: a later unverified/partial peer message on the same socket
// address must invalidate the old verified (pid, start) rather than inheriting
// it onto subsequent SendMessage or queue-operation entries.
func TestClaudePeerOriginPartialDoesNotInheritOldSocketIdentity(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// 1. Initial verified message on socket X.
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","name":"ai-old"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-old\">\nfrom old\n</cross-session-message>"}}` + "\n" +
			// 2. Later message on the SAME socket X with invalid/missing verifiedPeerProcStart.
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","timestamp":"2026-11-28T03:01:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"","name":"ai-new"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-new\">\nfrom new unverified\n</cross-session-message>"}}` + "\n" +
			// 3. Subsequent SendMessage to socket X must NOT inherit old pid/start.
			`{"type":"assistant","uuid":"a1","timestamp":"2026-11-28T03:01:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"SendMessage","input":{"to":"uds:/tmp/cc-socks/530048.sock","summary":"Reply","message":"reply"}}]}}` + "\n" +
			// 4. Subsequent queued message on socket X must NOT inherit old pid/start.
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:01:08Z","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-new\">\nqueued after partial\n</cross-session-message>"}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	ev2 := entries[1].Parts[0].Event
	if ev2 == nil || ev2.From != "ai-new" || ev2.PeerPID != 0 || ev2.PeerStart != 0 {
		t.Fatalf("ev2 = %+v, want unverified ai-new with zero pid/start", ev2)
	}
	tool := entries[2].Parts[0].Tool
	if tool == nil || tool.Title != "ai-new" || tool.PeerPID != 0 || tool.PeerStart != 0 {
		t.Fatalf("SendMessage after partial origin = %+v, must NOT inherit old verified pid/start", tool)
	}
	queued := c.Queued()
	if len(queued) != 1 || queued[0].From != "ai-new" || queued[0].PeerPID != 0 || queued[0].PeerStart != 0 {
		t.Fatalf("queued after partial origin = %+v, must NOT inherit old verified pid/start", queued)
	}
}

// Regression #7: missing isMeta with complete peer origin is parsed as EventPeer,
// and plain-text content with verified origin (no XML wrapper and no origin.body)
// is preserved rather than dropped; normal user rows stay RoleUser.
func TestClaudePeerOriginNonMetaAndPlainTextContent(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// 1. Missing isMeta, complete peer origin, plain-text content (no XML, no origin.body).
		`{"type":"user","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/777.sock","verifiedPeerPid":777,"verifiedPeerProcStart":"88888","name":"ai-77"},` +
			`"message":{"role":"user","content":"plain peer update without xml"}}` + "\n" +
			// 2. isMeta=true, complete peer origin, plain-text content (no XML, no origin.body).
			`{"type":"user","isMeta":true,"uuid":"u2","timestamp":"2026-11-28T03:00:05Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/778.sock","verifiedPeerPid":778,"verifiedPeerProcStart":"99999","name":"ai-78"},` +
			`"message":{"role":"user","content":"second plain peer message"}}` + "\n" +
			// 3. Normal user message (no isMeta, no origin): must remain RoleUser.
			`{"type":"user","uuid":"u3","timestamp":"2026-11-28T03:00:10Z",` +
			`"message":{"role":"user","content":"hello from human"}}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 3 {
		t.Fatalf("got %d entries, want 3: %+v", len(entries), entries)
	}
	if entries[0].Role != RoleNote || entries[0].Parts[0].Event == nil || entries[0].Parts[0].Event.Kind != EventPeer {
		t.Fatalf("entry 0 = %+v, want RoleNote EventPeer", entries[0])
	}
	ev0 := entries[0].Parts[0].Event
	if ev0.From != "ai-77" || ev0.PeerPID != 777 || ev0.PeerStart != 88888 || ev0.Text != "plain peer update without xml" {
		t.Fatalf("ev0 = %+v", ev0)
	}
	ev1 := entries[1].Parts[0].Event
	if ev1 == nil || ev1.From != "ai-78" || ev1.PeerPID != 778 || ev1.PeerStart != 99999 || ev1.Text != "second plain peer message" {
		t.Fatalf("ev1 = %+v", ev1)
	}
	if entries[2].Role != RoleUser || entries[2].Parts[0].Text != "hello from human" {
		t.Fatalf("entry 2 = %+v, want normal RoleUser", entries[2])
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
