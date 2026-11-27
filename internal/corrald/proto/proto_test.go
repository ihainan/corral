package proto

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFrameRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	want := Request{
		ID:     42,
		Method: MethodDaemonHello,
		Params: json.RawMessage(`{"gateway":"gw.example"}`),
	}
	if err := WriteJSON(&buf, want); err != nil {
		t.Fatalf("WriteJSON: %v", err)
	}
	var got Request
	if err := ReadJSON(&buf, &got); err != nil {
		t.Fatalf("ReadJSON: %v", err)
	}
	if got.ID != want.ID || got.Method != want.Method || string(got.Params) != string(want.Params) {
		t.Fatalf("round-trip mismatch: got %+v, want %+v", got, want)
	}
}

func TestFrameLargePayloadUpToLimit(t *testing.T) {
	// Test payload larger than the 64 KiB fast-path buffer and up to MaxFrameSize.
	pad := strings.Repeat("a", 128<<10)
	payload, err := json.Marshal(map[string]string{"pad": pad})
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFrame(&buf, payload); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	got, err := ReadFrame(&buf)
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("payload mismatch: len %d vs %d", len(got), len(payload))
	}
}

func TestFrameRejectsOversized(t *testing.T) {
	// WriteFrame rejects > 4 MiB.
	tooBig := make([]byte, MaxFrameSize+1)
	if err := WriteFrame(&bytes.Buffer{}, tooBig); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("WriteFrame oversized: got %v, want ErrFrameTooLarge", err)
	}

	// ReadFrame rejects header claiming > 4 MiB without trying to allocate.
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], MaxFrameSize+1)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame oversized: got %v, want ErrFrameTooLarge", err)
	}

	// Zero-length frame is rejected on both sides.
	if err := WriteFrame(&bytes.Buffer{}, nil); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("WriteFrame empty: got %v, want ErrEmptyFrame", err)
	}
	binary.BigEndian.PutUint32(hdr[:], 0)
	if _, err := ReadFrame(bytes.NewReader(hdr[:])); !errors.Is(err, ErrEmptyFrame) {
		t.Fatalf("ReadFrame zero: got %v, want ErrEmptyFrame", err)
	}
}

func TestFramePartialAndEOF(t *testing.T) {
	// Clean EOF before header.
	if _, err := ReadFrame(bytes.NewReader(nil)); !errors.Is(err, io.EOF) {
		t.Fatalf("empty reader: got %v, want io.EOF", err)
	}

	// Partial header (2 bytes).
	if _, err := ReadFrame(bytes.NewReader([]byte{0x00, 0x00})); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial header: got %v, want io.ErrUnexpectedEOF", err)
	}

	// Header claims 20 bytes, only 5 bytes present.
	var buf bytes.Buffer
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], 20)
	buf.Write(hdr[:])
	buf.WriteString(`{"id"`)
	if _, err := ReadFrame(&buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("partial body: got %v, want io.ErrUnexpectedEOF", err)
	}

	// Header claims 5 bytes, 0 body bytes present.
	buf.Reset()
	binary.BigEndian.PutUint32(hdr[:], 5)
	buf.Write(hdr[:])
	if _, err := ReadFrame(&buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("missing body: got %v, want io.ErrUnexpectedEOF", err)
	}
}

func TestFrameRejectsInvalidJSONAndUTF8(t *testing.T) {
	cases := []struct {
		name    string
		payload []byte
	}{
		{"truncated json", []byte(`{"id":1`)},
		{"garbage", []byte(`not-json`)},
		{"invalid utf8", []byte{'"', 0xff, 0xfe, '"'}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			if _, err := ReadFrame(&buf); !errors.Is(err, ErrInvalidJSON) {
				t.Fatalf("ReadFrame(%s): got %v, want ErrInvalidJSON", tc.name, err)
			}
		})
	}
}

func TestScopeGrewAllRules(t *testing.T) {
	base := DefaultManifest()

	// Identical manifest -> no growth.
	if got := ScopeGrew(base, DefaultManifest()); len(got) != 0 {
		t.Fatalf("identical manifest reported growth: %v", got)
	}

	// Reordered slices -> no growth.
	reordered := DefaultManifest()
	reordered.Reads = make([]string, len(base.Reads))
	for i := range base.Reads {
		reordered.Reads[i] = base.Reads[len(base.Reads)-1-i]
	}
	reordered.Actions = []string{ActionAnswerCard, ActionPaneInput}
	if got := ScopeGrew(base, reordered); len(got) != 0 {
		t.Fatalf("reordered manifest reported growth: %v", got)
	}

	// Shrinking reads/actions or lowering budget -> no growth.
	shrunk := DefaultManifest()
	shrunk.Reads = shrunk.Reads[:3]
	shrunk.Actions = shrunk.Actions[:1]
	shrunk.Budget = Budget{RSSMB: 40, DiskMB: 20, LogMB: 5}
	if got := ScopeGrew(base, shrunk); len(got) != 0 {
		t.Fatalf("shrunk manifest reported growth: %v", got)
	}

	cases := []struct {
		name   string
		mutate func(*Manifest)
		want   []string
	}{
		{
			name:   "new read category",
			mutate: func(m *Manifest) { m.Reads = append(m.Reads, "agent-logs:cursor") },
			want:   []string{"reads:agent-logs:cursor"},
		},
		{
			name:   "new action category",
			mutate: func(m *Manifest) { m.Actions = append(m.Actions, "model-switch") },
			want:   []string{"actions:model-switch"},
		},
		{
			name:   "new write outside self",
			mutate: func(m *Manifest) { m.Writes = append(m.Writes, "claude-settings") },
			want:   []string{"writes:claude-settings"},
		},
		{
			name:   "new egress destination",
			mutate: func(m *Manifest) { m.Egress = append(m.Egress, "internet") },
			want:   []string{"egress:internet"},
		},
		{
			name:   "rss budget increased",
			mutate: func(m *Manifest) { m.Budget.RSSMB = 60 },
			want:   []string{"budget:rss_mb"},
		},
		{
			name:   "disk budget increased",
			mutate: func(m *Manifest) { m.Budget.DiskMB = 50 },
			want:   []string{"budget:disk_mb"},
		},
		{
			name:   "log budget increased",
			mutate: func(m *Manifest) { m.Budget.LogMB = 20 },
			want:   []string{"budget:log_mb"},
		},
		{
			name:   "new optional capability",
			mutate: func(m *Manifest) { m.Optional = append(m.Optional, "approval-hook") },
			want:   []string{"optional:approval-hook"},
		},
		{
			name:   "schema bump",
			mutate: func(m *Manifest) { m.Schema = 2 },
			want:   []string{"schema:2"},
		},
		{
			name: "multiple expansions deduplicated and ordered",
			mutate: func(m *Manifest) {
				m.Reads = append(m.Reads, "env:FOO", "env:BAR", "env:FOO")
				m.Actions = append(m.Actions, "mode-switch")
				m.Budget.RSSMB = 100
				m.Optional = append(m.Optional, "hook:ask")
			},
			want: []string{
				"reads:env:BAR",
				"reads:env:FOO",
				"actions:mode-switch",
				"budget:rss_mb",
				"optional:hook:ask",
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := DefaultManifest()
			tc.mutate(&next)
			got := ScopeGrew(base, next)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("ScopeGrew: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestManifestSerializationStable(t *testing.T) {
	m1, err := MarshalManifest(DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	m2, err := MarshalManifest(DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m1, m2) {
		t.Fatalf("manifest JSON not stable:\n%s\nvs\n%s", m1, m2)
	}

	// Ensure nil slices serialize as [] rather than null.
	nilManifest := Manifest{Schema: 1}
	b, err := MarshalManifest(nilManifest)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "null") {
		t.Fatalf("nil slices serialized as null: %s", b)
	}

	// Check exact expected shape of DefaultManifest.
	const want = `{"schema":1,"reads":["proc","agent-logs:claude","agent-logs:codex","agent-logs:opencode","agent-logs:devin","agent-logs:agy","pane-screen:tmux","env:CLAUDE_CONFIG_DIR","env:CODEX_HOME","env:HERDR_PANE_ID","env:HERDR_SOCKET_PATH","env:TMUX","env:TMUX_PANE","env:CORRAL_SIDE_COPY","env:ANTHROPIC_BASE_URL","env:ANTHROPIC_AUTH_TOKEN","env:ANTHROPIC_API_KEY","env:CLAUDE_CODE_USE_BEDROCK","env:CLAUDE_CODE_USE_VERTEX","env:CLAUDE_AUTOCOMPACT_PCT_OVERRIDE","env:OPENAI_BASE_URL","env:XDG_DATA_HOME"],"actions":["pane-input","answer-card"],"writes":["self"],"egress":["gateway"],"budget":{"rss_mb":50,"disk_mb":30,"log_mb":10},"optional":[]}`
	if string(m1) != want {
		t.Fatalf("default manifest JSON:\ngot:  %s\nwant: %s", m1, want)
	}

	d1, err := ManifestSHA256(DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	d2, err := ManifestSHA256(DefaultManifest())
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 || len(d1) != 64 {
		t.Fatalf("unexpected digest: %q vs %q", d1, d2)
	}
}

func TestReleaseComparisonAndProtocolRange(t *testing.T) {
	if CompareRelease(100, 200) != -1 {
		t.Fatal("100 < 200 should be -1")
	}
	if CompareRelease(200, 200) != 0 {
		t.Fatal("200 == 200 should be 0")
	}
	if CompareRelease(300, 200) != 1 {
		t.Fatal("300 > 200 should be 1")
	}

	if !ShouldUpgrade(100, 200) {
		t.Fatal("newer release should upgrade")
	}
	if ShouldUpgrade(200, 200) {
		t.Fatal("equal release must not upgrade")
	}
	if ShouldUpgrade(200, 100) {
		t.Fatal("older release must never downgrade")
	}
	if ShouldUpgrade(0, 0) {
		t.Fatal("dev 0 -> 0 must not upgrade")
	}
	if !ShouldUpgrade(0, 1) {
		t.Fatal("0 -> 1 should upgrade")
	}

	if v, err := ParseRelease("1760000000"); err != nil || v != 1760000000 {
		t.Fatalf("ParseRelease valid: %d, %v", v, err)
	}
	if v, err := ParseRelease(""); err != nil || v != 0 {
		t.Fatalf("ParseRelease empty: %d, %v", v, err)
	}
	if _, err := ParseRelease("-1"); err == nil {
		t.Fatal("ParseRelease negative should fail")
	}
	if _, err := ParseRelease("abc"); err == nil {
		t.Fatal("ParseRelease invalid should fail")
	}

	if !ProtocolInRange(1, 1, 1) || !ProtocolInRange(2, 1, 3) {
		t.Fatal("valid protocol in range rejected")
	}
	if ProtocolInRange(2, 1, 1) || ProtocolInRange(0, 1, 2) || ProtocolInRange(1, 2, 1) || ProtocolInRange(1, 0, 2) {
		t.Fatal("invalid protocol range accepted")
	}
}

func TestRenderReadmeContainsRequiredDetails(t *testing.T) {
	txt := RenderReadme(ReadmeData{
		Version:    "0.9.3",
		Release:    1760000000,
		Gateway:    "ssh.inner.example",
		EnabledBy:  "alice",
		EnabledAt:  "2026-11-27T10:00:00Z",
		MachineID:  "aabbccddeeff",
		LibDir:     "/home/alice/.local/lib/corral",
		StateDir:   "/home/alice/.local/state/corral/corrald/aabbccddeeff",
		SocketPath: "/home/alice/.local/state/corral/corrald/aabbccddeeff/corrald.sock",
	})
	for _, want := range []string{
		"0.9.3",
		"1760000000",
		"ssh.inner.example",
		"alice",
		"/home/alice/.local/lib/corral/corrald status",
		"/home/alice/.local/lib/corral/corrald stop",
		"/home/alice/.local/lib/corral/corrald uninstall",
		"CLAUDE_CONFIG_DIR",
		"CODEX_HOME",
		"10 分钟",
		"10 minutes",
		"5 MB",
		"code-server",
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("README missing %q", want)
		}
	}
}

func TestOpIDGenerateAndParse(t *testing.T) {
	now := time.Date(2026, 11, 27, 10, 0, 0, 123000000, time.UTC)
	id := NewOpID(now)
	ts, err := ParseOpID(id)
	if err != nil {
		t.Fatalf("ParseOpID(%q): %v", id, err)
	}
	if !ts.Equal(now) {
		t.Fatalf("parsed timestamp %v, want %v", ts, now)
	}

	for _, bad := range []string{"", "no-dash", "abc-12345678", "0-12345678", "-5-12345678", "1700000000000-ab", "1700000000000-bad/char"} {
		if _, err := ParseOpID(bad); err == nil {
			t.Errorf("ParseOpID(%q) unexpectedly succeeded", bad)
		}
	}
	if OpDedupWindow != 30*time.Minute {
		t.Fatalf("unexpected OpDedupWindow: %v", OpDedupWindow)
	}
}
