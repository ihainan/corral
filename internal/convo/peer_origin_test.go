package convo

import (
	"slices"
	"testing"
)

func TestClaudePeerOriginPerMessageOnlyAndNoInheritance(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","msg_id":"m1","name":"ai-68","fromMode":"prompting","body":"hello from peer"},` +
			`"message":{"role":"user","content":"Another Claude session sent a message:\n<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\" from-mode=\"prompting\">\nhello from peer\n</cross-session-message>"}}` + "\n" +
			// SendMessage shows the peer's display name only (no Corral link / no PeerPID).
			`{"type":"assistant","uuid":"a1","timestamp":"2026-11-28T03:00:05Z","message":{"role":"assistant","content":[{"type":"tool_use","id":"tu1","name":"SendMessage","input":{"to":"uds:/tmp/cc-socks/530048.sock","summary":"Ack","message":"got it"}}]}}` + "\n" +
			// Queue-operation WITH its own verified origin carries (pid, start).
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:08Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/530048.sock","verifiedPeerPid":530048,"verifiedPeerProcStart":"193322812","name":"ai-68"},` +
			`"content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\" from-mode=\"prompting\">\nverified queued\n</cross-session-message>"}` + "\n" +
			// Queue-operation WITHOUT origin must NOT inherit old (pid, start) even with same socket and name.
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:09Z","content":"<cross-session-message from=\"uds:/tmp/cc-socks/530048.sock\" from-name=\"ai-68\" from-mode=\"prompting\">\nunverified queued same name\n</cross-session-message>"}` + "\n",
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
	if tool == nil || tool.Name != "SendMessage" || tool.Title != "ai-68" {
		t.Fatalf("SendMessage tool = %+v, want display name ai-68 only", tool)
	}
	queued := c.Queued()
	if len(queued) != 2 {
		t.Fatalf("queued = %+v, want 2", queued)
	}
	if queued[0].From != "ai-68" || queued[0].PeerPID != 530048 || queued[0].PeerStart != 193322812 {
		t.Fatalf("queued[0] = %+v, want own verified origin", queued[0])
	}
	if queued[1].From != "ai-68" || queued[1].PeerPID != 0 || queued[1].PeerStart != 0 {
		t.Fatalf("queued[1] = %+v, unverified queue-operation must have zero pid/start", queued[1])
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

// Regression fix3 #5 & fix4 B#6: XML address parsing accepts whitespace around
// "=" (e.g. from = "B"); both disagreement AND an XML wrapper whose from
// attribute is missing/unparseable discard verified (pid, start).
func TestClaudePeerOriginXMLAddrWhitespaceAndMismatchRejected(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// 1. Direct mismatch: XML from="...999.sock" vs origin.from="...111.sock".
		`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/111.sock","verifiedPeerPid":111,"verifiedPeerProcStart":"222","name":"ai-origin"},` +
			`"message":{"role":"user","content":"<cross-session-message from=\"uds:/tmp/cc-socks/999.sock\" from-name=\"ai-xml\">\nmismatched addr\n</cross-session-message>"}}` + "\n" +
			// 2. Whitespace around "=" with mismatched address: from = "uds:...888.sock".
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/111.sock","verifiedPeerPid":111,"verifiedPeerProcStart":"222","name":"ai-origin"},` +
			`"message":{"role":"user","content":"<cross-session-message from = \"uds:/tmp/cc-socks/888.sock\" from-name = \"ai-xml\">\nspaced mismatch\n</cross-session-message>"}}` + "\n" +
			// 3. XML wrapper present but missing/unparseable from attribute -> treated as mismatch.
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u3","timestamp":"2026-11-28T03:00:02Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/111.sock","verifiedPeerPid":111,"verifiedPeerProcStart":"222","name":"ai-origin"},` +
			`"message":{"role":"user","content":"<cross-session-message from-name=\"ai-xml\">\nmissing xml from\n</cross-session-message>"}}` + "\n" +
			// 4. Whitespace around "=" with MATCHING address: accepted.
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u4","timestamp":"2026-11-28T03:00:03Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/333.sock","verifiedPeerPid":333,"verifiedPeerProcStart":"444","name":"ai-ok"},` +
			`"message":{"role":"user","content":"<cross-session-message from = \"uds:/tmp/cc-socks/333.sock\" from-name = \"ai-ok\">\nspaced match\n</cross-session-message>"}}` + "\n" +
			// 5. Queue-operation with mismatched XML addr.
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:05Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/111.sock","verifiedPeerPid":111,"verifiedPeerProcStart":"222","name":"ai-origin"},` +
			`"content":"<cross-session-message from = \"uds:/tmp/cc-socks/999.sock\" from-name=\"ai-xml\">\nqueued mismatch\n</cross-session-message>"}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 4 {
		t.Fatalf("got %d entries, want 4: %+v", len(entries), entries)
	}
	for i := 0; i < 3; i++ {
		ev := entries[i].Parts[0].Event
		if ev == nil || ev.Kind != EventPeer || ev.PeerPID != 0 || ev.PeerStart != 0 {
			t.Fatalf("entry %d = %+v, want zero pid/start", i, ev)
		}
	}
	ev3 := entries[3].Parts[0].Event
	if ev3 == nil || ev3.PeerPID != 333 || ev3.PeerStart != 444 || ev3.From != "ai-ok" {
		t.Fatalf("entry 3 (spaced match) = %+v, want pid=333 start=444", ev3)
	}
	queued := c.Queued()
	if len(queued) != 1 || queued[0].PeerPID != 0 || queued[0].PeerStart != 0 {
		t.Fatalf("mismatched queued = %+v, want zero pid/start", queued)
	}
	refs := PeerRefs(c)
	if !slices.Equal(refs, []PeerRef{{PID: 333, Start: 444}}) {
		t.Fatalf("PeerRefs = %+v, want only 333:444", refs)
	}
}

// Regression fix4 B#7 & B#8:
//   - Plain-text enqueue with origin.kind=="peer" must be classified as a peer
//     queued note (Note=true, From!="") — never as a human user queued message.
//   - When a plain-text peer message arrives, metaArrived must NOT clear a
//     human user's queued message with identical text, and MUST clear the
//     matching peer queued message.
func TestClaudePeerPlainTextQueueAndMetaArrivedDoNotClearUserQueue(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// 1. Human user queues "run tests".
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:00Z","content":"run tests"}` + "\n" +
			// 2. Peer enqueues plain text "run tests" with complete peer origin (no XML).
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/777.sock","verifiedPeerPid":777,"verifiedPeerProcStart":"888","name":"ai-peer"},` +
			`"content":"run tests"}` + "\n",
	))

	queued := c.Queued()
	if len(queued) != 2 {
		t.Fatalf("queued = %+v, want 2 (1 user + 1 peer)", queued)
	}
	if queued[0].Note || queued[0].From != "" || queued[0].Text != "run tests" {
		t.Fatalf("queued[0] (user) = %+v, want Note=false From=\"\"", queued[0])
	}
	if !queued[1].Note || queued[1].From != "ai-peer" || queued[1].PeerPID != 777 || queued[1].PeerStart != 888 || queued[1].Text != "run tests" {
		t.Fatalf("queued[1] (plain-text peer) = %+v, want Note=true From=\"ai-peer\" pid=777 start=888", queued[1])
	}

	// 3. Peer message "run tests" arrives (plain text, origin.kind="peer").
	//    Must clear ONLY the peer queued item and keep the human user's queued item!
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-28T03:00:05Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/777.sock","verifiedPeerPid":777,"verifiedPeerProcStart":"888","name":"ai-peer"},` +
			`"message":{"role":"user","content":"run tests"}}` + "\n",
	))

	after := c.Queued()
	if len(after) != 1 || after[0].Note || after[0].From != "" || after[0].Text != "run tests" {
		t.Fatalf("after peer arrival, queued = %+v, want human user's queued message preserved and peer item cleared", after)
	}
}

// Regression fix5 #3: a peer queue-operation "remove" must ONLY remove a peer
// queued item — NEVER a human user's queued message with identical text (and
// vice versa).
func TestClaudePeerRemoveDoesNotDeleteMatchingUserQueueItem(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// 1. Human user queues "deploy now".
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:00Z","content":"deploy now"}` + "\n" +
			// 2. Peer queues plain-text "deploy now" with peer origin.
			`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/500.sock","verifiedPeerPid":500,"verifiedPeerProcStart":"900","name":"ai-peer"},` +
			`"content":"deploy now"}` + "\n" +
			// 3. Peer remove arrives with the same content "deploy now" and peer origin.
			`{"type":"queue-operation","operation":"remove","timestamp":"2026-11-28T03:00:02Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/500.sock","name":"ai-peer"},` +
			`"content":"deploy now"}` + "\n",
	))

	queued := c.Queued()
	if len(queued) != 1 || queued[0].Note || queued[0].From != "" || queued[0].Text != "deploy now" {
		t.Fatalf("after peer remove, queued = %+v, want human user's queued item preserved and peer item removed", queued)
	}
}

// Regression fix5 #4: <cross-session-message ...> XML envelope is ONLY parsed
// when it appears at the start of the content (allowing leading whitespace or
// Claude Code's fixed prefix line). An example tag appearing mid-body is
// treated as ordinary text (does NOT trigger addr mismatch or strip prose).
func TestClaudePeerXMLMidBodyExampleTreatedAsPlainText(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		// Plain-text verified peer message whose body discusses an example XML tag in the middle.
		`{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/777.sock","verifiedPeerPid":777,"verifiedPeerProcStart":"888","name":"ai-real"},` +
			`"message":{"role":"user","content":"Please check <cross-session-message from=\"uds:/tmp/cc-socks/999.sock\" from-name=\"fake\">demo</cross-session-message> in docs."}}` + "\n" +
			// Leading whitespace before XML envelope IS still parsed as an envelope.
			`{"type":"user","isMeta":true,"promptSource":"system","uuid":"u2","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/333.sock","verifiedPeerPid":333,"verifiedPeerProcStart":"444","name":"ai-lead"},` +
			`"message":{"role":"user","content":"  \n\t<cross-session-message from=\"uds:/tmp/cc-socks/333.sock\" from-name=\"ai-lead\">\nleading space ok\n</cross-session-message>"}}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2: %+v", len(entries), entries)
	}
	ev0 := entries[0].Parts[0].Event
	wantBody := `Please check <cross-session-message from="uds:/tmp/cc-socks/999.sock" from-name="fake">demo</cross-session-message> in docs.`
	if ev0 == nil || ev0.Kind != EventPeer || ev0.From != "ai-real" || ev0.PeerPID != 777 || ev0.PeerStart != 888 || ev0.Text != wantBody {
		t.Fatalf("mid-body XML example misparsed: %+v", ev0)
	}
	ev1 := entries[1].Parts[0].Event
	if ev1 == nil || ev1.Kind != EventPeer || ev1.From != "ai-lead" || ev1.PeerPID != 333 || ev1.PeerStart != 444 || ev1.Text != "leading space ok" {
		t.Fatalf("leading-whitespace XML envelope = %+v", ev1)
	}
}

// Regression fix5 #6: a peer message enqueued via origin.Body (with empty
// content) is cleanly matched and removed from c.queue on arrival using the
// same normalized text.
func TestClaudePeerOriginBodyEnqueuedRemovedOnArrival(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"queue-operation","operation":"enqueue","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/600.sock","verifiedPeerPid":600,"verifiedPeerProcStart":"700","name":"ai-body","body":"  sync schema now\n\n"},` +
			`"content":""}` + "\n",
	))
	if q := c.Queued(); len(q) != 1 || !q[0].Note || q[0].From != "ai-body" || q[0].Text != "sync schema now" {
		t.Fatalf("before arrival queued = %+v, want 1 origin.Body peer item", q)
	}
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-28T03:00:05Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/600.sock","verifiedPeerPid":600,"verifiedPeerProcStart":"700","name":"ai-body","body":"sync schema now"},` +
			`"message":{"role":"user","content":""}}` + "\n",
	))
	if q := c.Queued(); len(q) != 0 {
		t.Fatalf("after arrival queued = %+v, want empty (origin.Body item must be removed)", q)
	}
}

// Regression fix4 B#9: origin.kind=="peer" with non-empty body but missing both
// name and verified (pid, start) must still be kept as EventPeer (rendered as
// unknown sender + "not in Corral") rather than dropped.
func TestClaudePeerOriginNamelessUnverifiedMessageKept(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"user","isMeta":true,"uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","body":"anonymous peer body"},` +
			`"message":{"role":"user","content":""}}` + "\n" +
			`{"type":"user","uuid":"u2","timestamp":"2026-11-28T03:00:01Z",` +
			`"origin":{"kind":"peer"},` +
			`"message":{"role":"user","content":"anonymous plain content"}}` + "\n",
	))

	entries := c.Since(0)
	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (nameless unverified peer messages must not be dropped): %+v", len(entries), entries)
	}
	for i, want := range []string{"anonymous peer body", "anonymous plain content"} {
		ev := entries[i].Parts[0].Event
		if ev == nil || ev.Kind != EventPeer || ev.From != "" || ev.PeerPID != 0 || ev.PeerStart != 0 || ev.Text != want {
			t.Fatalf("entry %d = %+v, want nameless unverified EventPeer text=%q", i, ev, want)
		}
	}
}

// Requirement #4: missing isMeta or plain-text peer content is still recognized
// as EventPeer (with own verified fields if complete, or zero if incomplete);
// normal human user rows stay RoleUser.
func TestClaudePeerOriginNonMetaAndPlainTextContent(t *testing.T) {
	c := NewClaude()
	c.Feed([]byte(
		`{"type":"user","uuid":"u1","timestamp":"2026-11-28T03:00:00Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/777.sock","verifiedPeerPid":777,"verifiedPeerProcStart":"88888","name":"ai-77"},` +
			`"message":{"role":"user","content":"plain peer update without xml"}}` + "\n" +
			`{"type":"user","uuid":"u2","timestamp":"2026-11-28T03:00:05Z",` +
			`"origin":{"kind":"peer","from":"uds:/tmp/cc-socks/778.sock","verifiedPeerPid":778,"name":"ai-78"},` +
			`"message":{"role":"user","content":"unverified plain peer message"}}` + "\n" +
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
	if ev1 == nil || ev1.Kind != EventPeer || ev1.From != "ai-78" || ev1.PeerPID != 0 || ev1.PeerStart != 0 || ev1.Text != "unverified plain peer message" {
		t.Fatalf("ev1 = %+v, want unverified EventPeer", ev1)
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
