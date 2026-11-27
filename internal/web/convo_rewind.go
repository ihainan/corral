package web

import (
	"bytes"
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Claude Code in-place conversation rewind: HTTP endpoint, immutable herdr
// snapshot reading, cross-connection physical-pane locking, pending rewind
// overlay store (in-memory; lost on gateway restart), and herdr/tmux pane
// drivers that verify full identity before every keypress.

const (
	rewindStoreMax             = 256
	rewindStoreTTL             = 7 * 24 * time.Hour
	rewindClockSlack           = 10 * time.Minute
	rewindMaxSteps             = 200
	rewindConfirmOpt           = "Restore conversation"
	rewindConfirmCodeOpt       = "Restore code and conversation"
	rewindMaxFiles             = 200
	rewindMaxFileBytes         = 512
	rewindMaxFilesTotalBytes   = 4096
	claudeRewindTargetMaxRunes = 2000
)

var (
	claudeRewindStep        = 200 * time.Millisecond
	claudeRewindWait        = 4 * time.Second
	claudeRewindPostWait    = 5 * time.Second
	claudeRewindClearWait   = 2500 * time.Millisecond
	claudeRewindCleanupWait = 8 * time.Second
)

type rewindError struct {
	status int
	code   string
	msg    string
}

func (e *rewindError) Error() string { return e.code + ": " + e.msg }
func rwErr(status int, code, msg string) *rewindError {
	return &rewindError{status: status, code: code, msg: msg}
}

func isIdentityErr(err error) bool {
	var re *rewindError
	if !errors.As(err, &re) {
		return false
	}
	return re.code == "terminal_changed" || re.code == "process_changed" || re.code == "session_changed" || re.code == "not_in_front"
}

func asRewindErr(err error) *rewindError {
	if err == nil {
		return nil
	}
	var re *rewindError
	if errors.As(err, &re) {
		return re
	}
	if errors.Is(err, sshx.ErrBusy) {
		return rwErr(http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	}
	return rwErr(http.StatusBadGateway, "host_error", err.Error())
}

// ---------------------------------------------------------------------------
// herdr snapshot reading (immutable copy merging Panes + Agents)
// ---------------------------------------------------------------------------

// herdrPaneSnapshot merges Panes (AgentSession, Tokens, Label) and Agents
// (AgentStatus, TerminalID, StateSeq) for pane into a fresh copy without
// mutating snap or sharing maps/pointers.
func herdrPaneSnapshot(snap *herdr.Snapshot, pane string) (herdr.Pane, bool) {
	if snap == nil || pane == "" {
		return herdr.Pane{}, false
	}
	var out herdr.Pane
	var found bool
	var tokens map[string]string
	merge := func(src herdr.Pane, prefer bool) {
		if !found {
			out.ID, found = src.ID, true
		}
		pick := func(dst *string, v string) {
			if v != "" && (prefer || *dst == "") {
				*dst = v
			}
		}
		pick(&out.TabID, src.TabID)
		pick(&out.WorkspaceID, src.WorkspaceID)
		pick(&out.AgentStatus, src.AgentStatus)
		pick(&out.Agent, src.Agent)
		pick(&out.Name, src.Name)
		pick(&out.Cwd, src.Cwd)
		pick(&out.Label, src.Label)
		pick(&out.Title, src.Title)
		pick(&out.TerminalID, src.TerminalID)
		out.Focused = out.Focused || src.Focused
		out.StateSeq = max(out.StateSeq, src.StateSeq)
		if src.AgentSession != nil && (prefer || out.AgentSession == nil) {
			cp := *src.AgentSession
			out.AgentSession = &cp
		}
		if len(src.Tokens) > 0 {
			if tokens == nil {
				tokens = make(map[string]string, len(src.Tokens))
			}
			for k, v := range src.Tokens {
				if _, exists := tokens[k]; prefer || !exists {
					tokens[k] = v
				}
			}
		}
	}
	for i := range snap.Panes {
		if snap.Panes[i].ID == pane {
			merge(snap.Panes[i], false)
			break
		}
	}
	for i := range snap.Agents {
		if snap.Agents[i].ID == pane {
			merge(snap.Agents[i], true)
			break
		}
	}
	if !found {
		return herdr.Pane{}, false
	}
	out.Tokens = tokens
	return out, true
}

func (h *herdrSockets) paneDetail(id, pane string) (herdr.Pane, bool) {
	snap, ok := h.snapshotOf(id)
	if !ok {
		return herdr.Pane{}, false
	}
	return herdrPaneSnapshot(snap, pane)
}

// herdrValidatedUUID returns a Claude Code UUID reported by herdr, ignoring
// non-UUID hook values (e.g. Devin CLI's "cuddly-dime") and missing values.
func herdrValidatedUUID(p herdr.Pane) string {
	if v := agentSession(p); v != "" && convoUUID.MatchString(v) {
		return v
	}
	return ""
}

// ---------------------------------------------------------------------------
// Pending rewind record store (in-memory; lost on gateway restart)
// ---------------------------------------------------------------------------

type rewindKey struct {
	paneKey string
	sid     string
	pid     int64
	start   int64
}

type rewindMark struct {
	gen        uint64
	targetID   string
	knownUsers map[string]bool
	at         time.Time // gateway clock for TTL/LRU eviction
	since      time.Time // remote host clock for RewindSettled (or gateway - rewindClockSlack)
}

type rewindStore struct {
	mu      sync.Mutex
	m       map[rewindKey]*rewindMark
	nextGen uint64
	now     func() time.Time
}

func (r *rewindStore) clock() time.Time {
	if r.now != nil {
		return r.now()
	}
	return time.Now()
}

func (r *rewindStore) set(key rewindKey, targetID string, knownUsers map[string]bool, remoteAt time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.m == nil {
		r.m = make(map[rewindKey]*rewindMark)
	}
	now := r.clock()
	if _, exists := r.m[key]; !exists && len(r.m) >= rewindStoreMax {
		cutoff := now.Add(-rewindStoreTTL)
		evicted := false
		for k, v := range r.m {
			if v.at.Before(cutoff) {
				delete(r.m, k)
				evicted = true
			}
		}
		if !evicted && len(r.m) >= rewindStoreMax {
			var oldKey rewindKey
			var oldAt time.Time
			for k, v := range r.m {
				if oldAt.IsZero() || v.at.Before(oldAt) {
					oldKey, oldAt = k, v.at
				}
			}
			delete(r.m, oldKey)
		}
	}
	since := remoteAt
	if since.IsZero() {
		since = now.Add(-rewindClockSlack)
	}
	r.nextGen++
	r.m[key] = &rewindMark{
		gen:        r.nextGen,
		targetID:   targetID,
		knownUsers: maps.Clone(knownUsers),
		at:         now,
		since:      since,
	}
}

func (r *rewindStore) get(key rewindKey) *rewindMark {
	r.mu.Lock()
	defer r.mu.Unlock()
	m := r.m[key]
	if m == nil {
		return nil
	}
	return &rewindMark{
		gen:        m.gen,
		targetID:   m.targetID,
		knownUsers: maps.Clone(m.knownUsers),
		at:         m.at,
		since:      m.since,
	}
}

func (r *rewindStore) deleteIf(key rewindKey, gen uint64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur := r.m[key]; cur != nil && cur.gen == gen {
		delete(r.m, key)
		return true
	}
	return false
}

func (s *Server) syncClaudeRewind(sess *term.Session, st *convoState) string {
	if st.agent != "claude" || st.sub || st.bind == nil {
		if st.rewindTarget != "" {
			st.rewindTarget = ""
			st.gen = convoGens.Add(1)
		}
		return ""
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		return ""
	}
	key := rewindKey{paneKey: s.convoReplyKey(sess, st.bind.Pane), sid: st.bind.Session, pid: st.bind.PID, start: st.bind.Start}
	mark := s.rewinds.get(key)
	for mark != nil && cl.RewindSettled(mark.targetID, mark.since, mark.knownUsers) {
		if s.rewinds.deleteIf(key, mark.gen) {
			mark = nil
			break
		}
		// Another connection replaced the mark with a newer one; re-evaluate it.
		next := s.rewinds.get(key)
		if next == nil || next.gen == mark.gen {
			mark = nil
			break
		}
		mark = next
	}
	want := ""
	if mark != nil {
		want = mark.targetID
	}
	if want != st.rewindTarget {
		st.rewindTarget = want
		st.gen = convoGens.Add(1)
	}
	return want
}

func overlayRewindEntries(st *convoState, targetID string, entries []convo.Entry) []convo.Entry {
	if targetID == "" || len(entries) == 0 {
		return entries
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		return entries
	}
	targetSeq, found := cl.SeqOf(targetID)
	if !found && !st.older {
		return entries
	}
	out := append([]convo.Entry(nil), entries...)
	for i := range out {
		if !found || out[i].Seq >= targetSeq {
			out[i].Gone = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Remote shell scripts & real pane implementations (herdr + tmux)
// ---------------------------------------------------------------------------

// claudeRewindCheckVarsSh sets PATH and identity variables for remote scripts.
func claudeRewindCheckVarsSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid string) string {
	q := sshx.QuoteShell
	var b strings.Builder
	b.WriteString(`PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"` + "\n")
	b.WriteString("pid=" + strconv.FormatInt(pid, 10) + " start=" + strconv.FormatInt(start, 10) + " sid=" + q(sid) + " pane=" + q(pane) + "\n")
	if mode == "tmux" {
		b.WriteString("tpp=" + q(tmuxPanePID) + "\n")
	} else {
		b.WriteString("sock=" + q(socket) + "\n")
	}
	return b.String()
}

// claudeRewindCheckBodySh emits the identity checks (exits 31/32/33/34 on mismatch).
func claudeRewindCheckBodySh(mode string) string {
	var b strings.Builder
	if mode == "tmux" {
		b.WriteString(`[ "$(tmux -u display-message -p -t "$pane" '#{pane_pid} #{pane_dead}' 2>/dev/null 9>&-)" = "$tpp 0" ] || exit 34` + "\n")
		b.WriteString(`tty=$(tmux -u display-message -p -t "$pane" '#{pane_tty}' 2>/dev/null 9>&-)` + "\n")
		b.WriteString(`[ -n "$tty" ] && [ "$(readlink "/proc/$pid/fd/0" 2>/dev/null)" = "$tty" ] || exit 31` + "\n")
	} else {
		b.WriteString(`tr '\0\n' '\n\001' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "HERDR_PANE_ID=$pane" || exit 31` + "\n")
		b.WriteString(`tr '\0\n' '\n\001' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "HERDR_SOCKET_PATH=$sock" || exit 31` + "\n")
	}
	b.WriteString(`stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || exit 31` + "\n")
	b.WriteString(`rest=${stat##*) }` + "\n")
	b.WriteString(`[ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || exit 31` + "\n")
	b.WriteString(`pgrp=$(printf '%s' "$rest" | cut -d' ' -f3); tpgid=$(printf '%s' "$rest" | cut -d' ' -f6)` + "\n")
	b.WriteString(`[ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] || exit 32` + "\n")
	b.WriteString(`c=$(head -c 65536 "$HOME/.claude/sessions/$pid.json" 2>/dev/null | tr -d '\n')` + "\n")
	b.WriteString(`ps=$(printf '%s' "$c" | grep -o '"procStart" *: *"*[0-9]*"*' | head -n 1 | tr -dc '0-9')` + "\n")
	b.WriteString(`[ "$ps" = "$start" ] || exit 31` + "\n")
	b.WriteString(`csid=$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')` + "\n")
	b.WriteString(`[ "$csid" = "$sid" ] || exit 33` + "\n")
	return b.String()
}

// claudeRewindCheckSh verifies full identity on host right before key delivery.
// Exit codes: 31=process_changed, 32=not_in_front, 33=session_changed, 34=terminal_changed.
func claudeRewindCheckSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid string) string {
	return claudeRewindCheckVarsSh(mode, pane, socket, tmuxPanePID, pid, start, sid) + claudeRewindCheckBodySh(mode)
}

var claudeRewindAllowedKeys = map[string]string{
	"esc": "Escape", "up": "Up", "down": "Down", "enter": "Enter", "ctrl+c": "C-c",
}

func claudeRewindPressSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid string, keys []string) (string, error) {
	if len(keys) == 0 {
		return "", errors.New("no keys")
	}
	q := sshx.QuoteShell
	var b strings.Builder
	b.WriteString(claudeRewindCheckVarsSh(mode, pane, socket, tmuxPanePID, pid, start, sid))
	b.WriteString("check_id() {\n")
	b.WriteString(claudeRewindCheckBodySh(mode))
	b.WriteString("}\n")
	for i, k := range keys {
		tk, ok := claudeRewindAllowedKeys[k]
		if !ok {
			return "", errors.New("invalid rewind key: " + k)
		}
		if i > 0 && k == "esc" && keys[i-1] == "esc" {
			b.WriteString("sleep 0.05\n")
		}
		b.WriteString("check_id\n")
		if mode == "tmux" {
			b.WriteString(`tmux send-keys -t "$pane" ` + q(tk) + ` 9>&- || exit 6` + "\n")
		} else {
			b.WriteString(`HERDR_SOCKET_PATH="$sock" herdr pane send-keys "$pane" ` + q(k) + ` >/dev/null 2>&1 9>&- || exit 6` + "\n")
		}
	}
	b.WriteString("echo ok\n")
	return b.String(), nil
}

// claudeRewindAwkCommon provides shared UTF-8-aware (under LC_ALL=C) whitespace
// normalization, overline/rule detection, and ellipsis prefix matching helpers
// used by both claudeRewindPickerAwk and claudeRewindCommitAwk.
const claudeRewindAwkCommon = `
BEGIN {
  want = ENVIRON["TARGET_FIRST"]
  want_trunc = (ENVIRON["TARGET_TRUNC"] == "1")
  arrow = "❯"
  pipe = "│"
  ell = "…"
  footer = "Enter to continue · Esc to cancel"
  header = "Restore to the point before you sent this message:"
  opt_want = "Restore conversation"
  last_bar = 0
}
function normspace(s) {
  gsub(/[\r\n\t\v\f ]+/, " ", s)
  gsub(/\302[\205\240]/, " ", s)
  gsub(/\341\232\200/, " ", s)
  gsub(/\342\200[\200-\212\250\251\257]/, " ", s)
  gsub(/\342\201\237/, " ", s)
  gsub(/\343\200\200/, " ", s)
  return s
}
function trim(s) {
  sub(/\r$/, "", s)
  sub(/^[ \t]+/, "", s)
  sub(/[ \t]+$/, "", s)
  return s
}
function rtrim(s) {
  sub(/[ \t\r]+$/, "", s)
  return s
}
function onespace(s) {
  s = normspace(s)
  sub(/^ +/, "", s)
  sub(/ +$/, "", s)
  gsub(/ +/, " ", s)
  return s
}
function nospace(s) {
  s = normspace(s)
  gsub(/ +/, "", s)
  return s
}
function is_bar(s,    tmp, n) {
  tmp = trim(s)
  n = gsub(/▔/, "", tmp)
  return (n >= 8 && tmp == "")
}
function is_rule_plain(s,    tmp, n) {
  tmp = rtrim(s)
  n = gsub(/─/, "", tmp)
  n += gsub(/━/, "", tmp)
  return (n >= 8 && tmp == "")
}
function is_box_top(s,    tmp) {
  if (is_rule_plain(s)) return 1
  tmp = rtrim(s)
  if (length(tmp) >= 4 && (substr(tmp, length(tmp) - 3) == " ─" || substr(tmp, length(tmp) - 3) == " ━")) {
    gsub(/─/, "", tmp)
    gsub(/━/, "", tmp)
    gsub(/ /, "", tmp)
    return (tmp != "")
  }
  return 0
}
function is_box_foot(top, line) {
  top = rtrim(top)
  line = rtrim(line)
  if (line == top) return 1
  if (is_rule_plain(top)) return 0
  return is_rule_plain(line)
}
function has_normal_box(start, stop,    i, j, t, lim) {
  for (i = stop - 1; i >= start + 1; i--) {
    t = trim(lines[i])
    if (substr(t, 1, length(arrow)) != arrow || !is_box_top(lines[i - 1])) continue
    lim = i + 10
    if (lim > stop) lim = stop
    for (j = i + 1; j <= lim; j++) {
      if (is_box_foot(lines[i - 1], lines[j])) return 1
    }
  }
  return 0
}
function has_ellipsis(g) {
  return ((length(g) >= length(ell) && substr(g, length(g) - length(ell) + 1) == ell) ||
          (length(g) >= 3 && substr(g, length(g) - 2) == "..."))
}
function strip_ellipsis(g) {
  if (length(g) >= length(ell) && substr(g, length(g) - length(ell) + 1) == ell) {
    return substr(g, 1, length(g) - length(ell))
  }
  if (length(g) >= 3 && substr(g, length(g) - 2) == "...") {
    return substr(g, 1, length(g) - 3)
  }
  return ""
}
function match_ellipsis(g, w,    pref, minlen) {
  if (!has_ellipsis(g)) return 0
  pref = strip_ellipsis(g)
  sub(/ +$/, "", pref)
  minlen = 4
  if (length(w) < minlen) minlen = length(w)
  if (length(pref) < minlen || length(pref) == 0) return 0
  return (substr(w, 1, length(pref)) == pref)
}
function is_confirm_opt_label(s,    l) {
  l = tolower(onespace(s))
  return (l == "restore code and conversation" ||
          l == "restore conversation" ||
          l == "restore code" ||
          l == "summarize from here" ||
          l == "summarize up to here" ||
          l == "never mind")
}
{
  sub(/\r$/, "")
  lines[NR] = $0
  if (is_bar($0)) last_bar = NR
}
`

// claudeRewindPickerAwk verifies the captured screen strictly below the last
// ▔ rule: panel is the Rewind picker (not a confirm screen and not followed by
// a normal prompt box), and the sole ❯ row matches ENVIRON["TARGET_FIRST"]
// using the same whitespace and ellipsis rules as Go's matchPickerSelection.
// Exits 36 on any mismatch.
const claudeRewindPickerAwk = claudeRewindAwkCommon + `
END {
  want_os = onespace(want)
  want_ns = nospace(want)
  if (want_os == "" || last_bar == 0) exit 36
  if (has_normal_box(last_bar + 1, NR)) exit 36
  title_idx = 0
  footer_idx = 0
  has_hint = 0
  opt_lines = 0
  known_opts = 0
  sel_count = 0
  sel_idx = 0
  sel_text = ""
  for (i = last_bar + 1; i <= NR; i++) {
    t = trim(lines[i])
    if (title_idx == 0 && t == "Rewind") {
      title_idx = i
    }
    if (index(t, "Restore the code and/or conversation") > 0 || t == footer) {
      has_hint = 1
    }
    if (t == footer) {
      footer_idx = i
    }
    opt_cand = t
    if (substr(opt_cand, 1, length(arrow)) == arrow) {
      sel_count++
      sel_idx = i
      sel_text = trim(substr(t, length(arrow) + 1))
      opt_cand = sel_text
    }
    if (opt_cand ~ /^[0-9]+\.[ \t]+/) {
      opt_lines++
      sub(/^[0-9]+\.[ \t]+/, "", opt_cand)
      if (is_confirm_opt_label(opt_cand)) known_opts++
    }
  }
  if (title_idx == 0 || !has_hint) exit 36
  if (opt_lines >= 2 && known_opts >= 1) exit 36
  if (sel_count != 1 || sel_idx <= title_idx) exit 36
  if (footer_idx > title_idx && sel_idx >= footer_idx) exit 36
  sel_os = onespace(sel_text)
  sel_ns = nospace(sel_text)
  if (sel_os == "" || sel_os == "(current)") exit 36
  p_ok = 0
  if (has_ellipsis(sel_os)) {
    if (match_ellipsis(sel_os, want_os) || match_ellipsis(sel_ns, want_ns)) p_ok = 1
  } else if (sel_os == want_os) {
    p_ok = 1
  } else if (want_trunc && length(sel_os) >= length(want_os) && substr(sel_os, 1, length(want_os)) == want_os) {
    p_ok = 1
  }
  if (!p_ok) exit 36
}
`

// claudeRewindCommitAwk verifies the captured screen strictly below the last
// ▔ rule: confirm header present, quoted first line matches ENVIRON["TARGET_FIRST"]
// via fixed-string space-stripped comparison (never regex; runs under LC_ALL=C
// so UTF-8 Unicode whitespace byte escapes work across gawk/mawk/BSD awk), and
// the sole ❯ line is strictly "❯ <digits>. <OPT_WANT>" where OPT_WANT must be
// either "Restore conversation" or "Restore code and conversation".
// When OPT_WANT is "Restore code and conversation", it also verifies that the
// description line matches Claude Code's exact file-count format against
// ENVIRON["EXPECT_FILES"] (one basename per distinct path, preserving
// duplicates): 1 file "in <f>.", 2 files "in <f1> and <f2>.", 3+ files
// "in <f> and <N> other files." with N+1 == count and <f> in expected basenames
// (exiting 37 on mismatch). Exits 35 on option/quote mismatch without sending Enter.
const claudeRewindCommitAwk = claudeRewindAwkCommon + `
function is_ago_only(s) {
  return (s ~ /^\((just now|[0-9]+[smhd][ \t]+ago|[^()]*[ \t]+ago)\)$/)
}
function strip_ago_tail(s,    r) {
  r = s
  sub(/[ \t]+\((just now|[0-9]+[smhd][ \t]+ago)\)$/, "", r)
  return r
}
function match_code_line(line, exp_list, exp_cnt,    tail, suffix, i) {
  if (exp_cnt < 1) return 0
  if (line !~ /^The code will be restored( [+-][0-9]+(,[0-9]+)*)* in /) return 0
  tail = line
  sub(/^The code will be restored( [+-][0-9]+(,[0-9]+)*)* /, "", tail)
  if (exp_cnt == 1) {
    return (tail == "in " exp_list[1] ".")
  }
  if (exp_cnt == 2) {
    return (tail == "in " exp_list[1] " and " exp_list[2] "." ||
            tail == "in " exp_list[2] " and " exp_list[1] ".")
  }
  suffix = " and " (exp_cnt - 1) " other files."
  for (i = 1; i <= exp_cnt; i++) {
    if (tail == "in " exp_list[i] suffix) return 1
  }
  return 0
}
END {
  opt_want = ENVIRON["OPT_WANT"]
  if (opt_want != "Restore conversation" && opt_want != "Restore code and conversation") exit 35
  want_code = (opt_want == "Restore code and conversation")
  want_ns = nospace(want)
  if (want_ns == "" || last_bar == 0) exit 35
  if (has_normal_box(last_bar + 1, NR)) exit 35
  hdr = 0
  for (i = last_bar + 1; i <= NR; i++) {
    t = trim(lines[i])
    if (index(t, header) > 0) { hdr = i; break }
  }
  if (hdr == 0) exit 35
  in_opts = 0
  ended = 0
  sel_count = 0
  sel_ok = 0
  qn = 0
  code_blocks = 0
  code_line = ""
  in_code = 0
  for (i = last_bar + 1; i <= NR; i++) {
    t = trim(lines[i])
    if (i > hdr && !ended && t == footer) {
      ended = 1
      continue
    }
    if (substr(t, 1, length(arrow)) == arrow) {
      sel_count++
      if (i > hdr && !ended) {
        in_opts = 1
        if (in_code == 1) in_code = 2
        rest = substr(t, length(arrow) + 1)
        sub(/^[ \t]+/, "", rest)
        if (rest ~ /^[0-9]+\.[ \t]+/) {
          sub(/^[0-9]+\.[ \t]+/, "", rest)
          if (rest == opt_want) sel_ok = 1
        }
      }
      continue
    }
    if (ended || i <= hdr) continue
    if (t == "") {
      if (in_code == 1) in_code = 2
      continue
    }
    if (t ~ /^[0-9]+\.[ \t]+/) {
      in_opts = 1
      if (in_code == 1) in_code = 2
      continue
    }
    if (!in_opts && substr(t, 1, length(pipe)) == pipe) {
      if (in_code == 1) in_code = 2
      body = onespace(substr(t, length(pipe) + 1))
      if (body != "") {
        qn++
        q[qn] = body
      }
      continue
    }
    if (!in_opts) {
      os = onespace(lines[i])
      if (os == "The code will be restored" || substr(os, 1, 26) == "The code will be restored ") {
        code_blocks++
        code_line = os
        in_code = 1
      } else if (in_code == 1) {
        if (substr(os, 1, 17) == "The conversation " || substr(os, 1, 9) == "The code " || substr(os, 1, 3) == "⚠") {
          in_code = 2
        } else {
          code_line = code_line " " os
        }
      }
    }
  }
  if (sel_count != 1 || !sel_ok || qn == 0) exit 35
  if (qn > 1 && is_ago_only(q[qn])) {
    qn--
  } else if (qn > 0) {
    tr = strip_ago_tail(q[qn])
    if (tr != "") q[qn] = tr
  }
  if (qn == 0) exit 35
  acc = ""
  q_ok = 0
  for (i = 1; i <= qn; i++) {
    acc = acc nospace(q[i])
    if (acc == want_ns) {
      q_ok = 1
      break
    }
  }
  if (!q_ok) {
    if (match_ellipsis(nospace(q[1]), want_ns) || match_ellipsis(acc, want_ns)) {
      q_ok = 1
    } else if (want_trunc && length(acc) >= length(want_ns) && substr(acc, 1, length(want_ns)) == want_ns) {
      q_ok = 1
    }
  }
  if (!q_ok) exit 35
  if (want_code) {
    if (code_blocks != 1) exit 37
    n_exp = split(ENVIRON["EXPECT_FILES"], exp_arr, "\n")
    exp_cnt = 0
    for (i = 1; i <= n_exp; i++) {
      f = trim(exp_arr[i])
      if (f != "") {
        exp_cnt++
        exp_list[exp_cnt] = f
      }
    }
    if (!match_code_line(code_line, exp_list, exp_cnt)) exit 37
  }
}
`

// claudeRewindTargetPrefix normalizes the first non-empty line of targetText
// (collapsing all Unicode whitespace to single ASCII spaces) and caps it to
// claudeRewindTargetMaxRunes so double-quoted remote commands stay well below
// ARG_MAX / MAX_ARG_STRLEN.
func claudeRewindTargetPrefix(targetText string) (string, bool) {
	want := oneSpaced(firstNonEmptyLine(targetText))
	r := []rune(want)
	if len(r) <= claudeRewindTargetMaxRunes {
		return want, false
	}
	return strings.TrimSpace(string(r[:claudeRewindTargetMaxRunes])), true
}

func parseClaudeRewindCommitOut(out []byte) (bool, time.Time) {
	fields := strings.Fields(string(out))
	if len(fields) == 0 || fields[0] != "ok" {
		return false, time.Time{}
	}
	if len(fields) >= 2 {
		if sec, err := strconv.ParseInt(fields[1], 10, 64); err == nil && sec > 0 {
			return true, time.Unix(sec, 0).UTC()
		}
	}
	return true, time.Time{}
}

// claudeRewindGuardedEnterSh builds the atomic remote script that verifies
// identity, captures the live screen, validates it with awkProg (preserving
// awk's non-zero exit code without sending Enter), and sends Enter.
func claudeRewindGuardedEnterSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid, targetText, awkProg string, mismatchExit int, withTimestamp bool, extraEnv [][2]string) (string, error) {
	wantFirst, wantTrunc := claudeRewindTargetPrefix(targetText)
	if wantFirst == "" {
		return "", errors.New("empty target first line")
	}
	truncFlag := "0"
	if wantTrunc {
		truncFlag = "1"
	}
	exitCode := strconv.Itoa(mismatchExit)
	q := sshx.QuoteShell
	var b strings.Builder
	b.WriteString(claudeRewindCheckSh(mode, pane, socket, tmuxPanePID, pid, start, sid))
	if mode == "tmux" {
		b.WriteString(`scr=$(tmux -u capture-pane -p -t "$pane" 2>/dev/null 9>&-) || exit ` + exitCode + "\n")
	} else {
		b.WriteString(`scr=$(HERDR_SOCKET_PATH="$sock" herdr pane read "$pane" --source visible 2>/dev/null 9>&-) || exit ` + exitCode + "\n")
	}
	var envBuf strings.Builder
	envBuf.WriteString("LC_ALL=C TARGET_FIRST=" + q(wantFirst) + " TARGET_TRUNC=" + truncFlag)
	for _, kv := range extraEnv {
		envBuf.WriteString(" " + kv[0] + "=" + q(kv[1]))
	}
	b.WriteString("printf '%s\\n' \"$scr\" | " + envBuf.String() + " awk " + q(awkProg) + "\n")
	b.WriteString("rc=$?\n[ \"$rc\" -eq 0 ] || exit \"$rc\"\n")
	if withTimestamp {
		b.WriteString("ts=$(date -u +%s 2>/dev/null || true)\n")
	}
	if mode == "tmux" {
		b.WriteString(`tmux send-keys -t "$pane" 'Enter' 9>&- || exit 6` + "\n")
	} else {
		b.WriteString(`HERDR_SOCKET_PATH="$sock" herdr pane send-keys "$pane" 'enter' >/dev/null 2>&1 9>&- || exit 6` + "\n")
	}
	if withTimestamp {
		b.WriteString("printf 'ok %s\\n' \"$ts\"\n")
	} else {
		b.WriteString("echo ok\n")
	}
	return b.String(), nil
}

// claudeRewindPickerEnterSh atomically verifies identity, captures the live
// screen, confirms the Rewind picker is open with target's first line selected
// (not a confirm screen), and only then sends Enter. Exits 36 on mismatch
// without sending Enter.
func claudeRewindPickerEnterSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid, targetText string) (string, error) {
	return claudeRewindGuardedEnterSh(mode, pane, socket, tmuxPanePID, pid, start, sid, targetText, claudeRewindPickerAwk, 36, false, nil)
}

// claudeRewindCommitSh atomically verifies identity, captures the live screen,
// confirms "Restore conversation" and target's first line on the confirm panel,
// and only then sends Enter in the same remote script. Exits 35 on mismatch
// without sending Enter. Prints "ok <remote_unix_sec>" on success.
func claudeRewindCommitSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid, targetText string) (string, error) {
	return claudeRewindCommitOptSh(mode, pane, socket, tmuxPanePID, pid, start, sid, targetText, false, nil)
}

// claudeRewindCommitOptSh builds the atomic remote commit script for either
// conversation-only rewind (restoreCode=false) or code+conversation rewind
// (restoreCode=true, verifying the exact expected filename set on the confirm
// screen and exiting 37 on code mismatch without sending Enter).
func claudeRewindCommitOptSh(mode, pane, socket, tmuxPanePID string, pid, start int64, sid, targetText string, restoreCode bool, expectedFiles []string) (string, error) {
	optWant := rewindConfirmOpt
	var expectEnv string
	if restoreCode {
		optWant = rewindConfirmCodeOpt
		norm, _, err := normalizeRewindFiles(expectedFiles)
		if err != nil {
			return "", err
		}
		expectEnv = strings.Join(norm, "\n")
	}
	extraEnv := [][2]string{
		{"OPT_WANT", optWant},
		{"EXPECT_FILES", expectEnv},
	}
	return claudeRewindGuardedEnterSh(mode, pane, socket, tmuxPanePID, pid, start, sid, targetText, claudeRewindCommitAwk, 35, true, extraEnv)
}

func rewindFileBase(p string) string {
	s := strings.TrimSpace(p)
	s = strings.TrimRight(s, "/\\")
	if s == "" {
		return ""
	}
	if i := strings.LastIndexAny(s, "/\\"); i >= 0 {
		s = s[i+1:]
	}
	return strings.TrimSpace(s)
}

// normalizeRewindFiles validates the client-supplied file paths for code
// restore, deduplicating by distinct path while preserving basename
// multiplicity when two distinct paths share a filename (e.g. src/b.txt and
// test/b.txt count as 2 files). Returns (sorted basenames per distinct path
// for screen matching, cleaned distinct paths for audit logging).
func normalizeRewindFiles(files []string) (bases []string, cleaned []string, err error) {
	if len(files) == 0 {
		return nil, nil, errors.New("files list is required when restore_code is true")
	}
	if len(files) > rewindMaxFiles {
		return nil, nil, errors.New("too many files")
	}
	seenPath := make(map[string]bool, len(files))
	totalBytes := 0
	for _, raw := range files {
		if strings.ContainsAny(raw, "\r\n\x00") {
			return nil, nil, errors.New("invalid file name")
		}
		trimmed := strings.TrimRight(strings.TrimSpace(raw), "/\\")
		if trimmed == "" || len(trimmed) > rewindMaxFileBytes {
			return nil, nil, errors.New("invalid file name")
		}
		base := rewindFileBase(trimmed)
		if base == "" {
			return nil, nil, errors.New("invalid file name")
		}
		if seenPath[trimmed] {
			continue
		}
		seenPath[trimmed] = true
		cleaned = append(cleaned, trimmed)
		totalBytes += len(base) + 1
		if totalBytes > rewindMaxFilesTotalBytes {
			return nil, nil, errors.New("files list too large")
		}
		bases = append(bases, base)
	}
	if len(bases) == 0 {
		return nil, nil, errors.New("files list is required when restore_code is true")
	}
	slices.Sort(bases)
	return bases, cleaned, nil
}

func claudeRewindTmuxPaneSh(pane string) string {
	return `tmux -u display-message -p -t ` + sshx.QuoteShell(pane) + ` '#{pane_pid} #{pane_dead} #{pane_tty}' 2>/dev/null 9>&-`
}

func claudeRewindRowCommand(log, uuid string) string {
	q := sshx.QuoteShell
	return "grep -m " + strconv.Itoa(imageRowsTried) + " -i -F -e " + q(`"uuid":"`+uuid+`"`) + " -e " + q(`"uuid": "`+uuid+`"`) + " -- " + q(log)
}

func claudeRewindTextFromRows(out []byte, uuid string) (string, *rewindError) {
	for _, line := range bytes.Split(out, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		if text, ok := convo.ClaudeUserTextOf(line, uuid); ok && strings.TrimSpace(text) != "" {
			if len(text) > maxSendText {
				return "", rwErr(http.StatusConflict, "message_too_long", "message exceeds the 100 KiB composer limit and cannot be restored completely")
			}
			return text, nil
		}
	}
	return "", rwErr(http.StatusConflict, "message_text_unavailable", "could not read the full original text of this message from the session log")
}

func (s *Server) claudeRewindFullText(ctx context.Context, sess *term.Session, inflight *inFlightCounter, logPath, uuid string) (string, *rewindError) {
	if logPath == "" {
		return "", rwErr(http.StatusConflict, "message_text_unavailable", "could not read the full original text of this message from the session log")
	}
	out, err := claudeRewindExec(ctx, sess, inflight, claudeRewindRowCommand(logPath, uuid), imageLineMax, 3*time.Second)
	if err != nil {
		if errors.Is(err, sshx.ErrOutputTooLarge) {
			return "", rwErr(http.StatusConflict, "message_too_long", "message exceeds the 100 KiB composer limit and cannot be restored completely")
		}
		var ee *sshx.ExitError
		if errors.As(err, &ee) && ee.Status == 1 {
			return "", rwErr(http.StatusConflict, "message_text_unavailable", "could not read the full original text of this message from the session log")
		}
		return "", asRewindErr(err)
	}
	return claudeRewindTextFromRows(out, uuid)
}

func mapRewindExitErr(err error) error {
	var exitErr *sshx.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.Status {
		case 31:
			return rwErr(http.StatusConflict, "process_changed", "the Claude Code process in this pane changed")
		case 32:
			return rwErr(http.StatusConflict, "not_in_front", "Claude Code is not in front in its pane")
		case 33:
			return rwErr(http.StatusConflict, "session_changed", "the Claude Code session in this pane changed")
		case 34:
			return rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed")
		case 35:
			return rwErr(http.StatusConflict, "confirm_mismatch", "confirmation screen did not match expected option and target")
		case 36:
			return rwErr(http.StatusConflict, "target_mismatch", "selected message in rewind list does not match target")
		case 37:
			return rwErr(http.StatusConflict, "code_mismatch", "files to restore do not match the expected list")
		}
	}
	return err
}

// inFlightCounter tracks in-flight remote commands with cancellable Wait(ctx).
// Unlike sync.WaitGroup, a Wait that times out can safely be followed by new
// Add/Done calls on the same counter without panicking.
type inFlightCounter struct {
	mu   sync.Mutex
	n    int
	zero chan struct{}
}

func (c *inFlightCounter) Add() {
	c.mu.Lock()
	if c.n == 0 {
		c.zero = make(chan struct{})
	}
	c.n++
	c.mu.Unlock()
}

func (c *inFlightCounter) Done() {
	c.mu.Lock()
	c.n--
	if c.n < 0 {
		c.mu.Unlock()
		panic("inFlightCounter: negative counter")
	}
	if c.n == 0 && c.zero != nil {
		close(c.zero)
		c.zero = nil
	}
	c.mu.Unlock()
}

func (c *inFlightCounter) Wait(ctx context.Context) error {
	if c == nil {
		return nil
	}
	for {
		c.mu.Lock()
		if c.n == 0 {
			c.mu.Unlock()
			return nil
		}
		ch := c.zero
		c.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func claudeRewindExecResult(ctx context.Context, sess *term.Session, inflight *inFlightCounter, cmd string, limit int, await time.Duration) (sshx.ExecResult, error) {
	inflight.Add()
	res, err := sshx.Exec(ctx, sess.Client(), cmd, limit, sshx.ExecOptions{AwaitSlot: await, Release: inflight.Done})
	return res, err
}

func claudeRewindExec(ctx context.Context, sess *term.Session, inflight *inFlightCounter, cmd string, limit int, await time.Duration) ([]byte, error) {
	res, err := claudeRewindExecResult(ctx, sess, inflight, cmd, limit, await)
	return res.Out, err
}

// evalCommitExec interprets the outcome of running claudeRewindCommitSh.
// Known pre-Enter exits (identity mismatch 31-34, confirm mismatch 35, or shell
// unable to launch 126-127) and pre-dispatch exec failures report started=false
// so the caller treats them as Phase-1 failures rather than rewind_uncertain.
func evalCommitExec(res sshx.ExecResult, err error) (claudeRewindCommit, error) {
	if err != nil {
		var exitErr *sshx.ExitError
		if res.Ended && errors.As(err, &exitErr) {
			switch exitErr.Status {
			case 31, 32, 33, 34, 35, 37, 126, 127:
				return claudeRewindCommit{started: false}, mapRewindExitErr(err)
			}
		}
		return claudeRewindCommit{started: res.Started}, mapRewindExitErr(err)
	}
	ok, remoteAt := parseClaudeRewindCommitOut(res.Out)
	if !ok {
		return claudeRewindCommit{started: true}, errors.New("key delivery unconfirmed")
	}
	return claudeRewindCommit{started: true, remoteAt: remoteAt}, nil
}

type herdrClaudeRewindPane struct {
	sess                     *term.Session
	inflight                 *inFlightCounter
	socket, pane, terminalID string
	pid, start               int64
	sid                      string
}

func (p *herdrClaudeRewindPane) checkHerdrSnapshot(ctx context.Context) error {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return err
	}
	defer c.Close()
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	info, ok := herdrPaneSnapshot(snap, p.pane)
	if !ok || (p.terminalID != "" && info.TerminalID != p.terminalID) {
		return rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed")
	}
	if hsid := herdrValidatedUUID(info); hsid != "" && !strings.EqualFold(hsid, p.sid) {
		return rwErr(http.StatusConflict, "session_changed", "herdr reports a different session in this pane")
	}
	return nil
}

func (p *herdrClaudeRewindPane) verify(ctx context.Context) error {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return err
	}
	_, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(claudeRewindCheckSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid)), 128, 3*time.Second)
	return mapRewindExitErr(err)
}

func (p *herdrClaudeRewindPane) press(ctx context.Context, keys ...string) error {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return err
	}
	sh, err := claudeRewindPressSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid, keys)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *herdrClaudeRewindPane) pickerEnter(ctx context.Context, targetText string) error {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return err
	}
	sh, err := claudeRewindPickerEnterSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid, targetText)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *herdrClaudeRewindPane) commitEnter(ctx context.Context, targetText string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error) {
	if err := p.checkHerdrSnapshot(ctx); err != nil {
		return claudeRewindCommit{started: false}, err
	}
	sh, err := claudeRewindCommitOptSh("herdr", p.pane, p.socket, "", p.pid, p.start, p.sid, targetText, restoreCode, expectedFiles)
	if err != nil {
		return claudeRewindCommit{started: false}, err
	}
	res, err := claudeRewindExecResult(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	return evalCommitExec(res, err)
}

func (p *herdrClaudeRewindPane) waitInFlight(ctx context.Context) error {
	return p.inflight.Wait(ctx)
}

func (p *herdrClaudeRewindPane) screen(ctx context.Context) (string, error) {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.ReadPaneSource(ctx, p.pane, "visible", 0)
}

func (p *herdrClaudeRewindPane) colorScreen(ctx context.Context) (string, error) {
	c, err := herdr.DialWait(ctx, p.sess.Client(), p.socket, 5*time.Second)
	if err != nil {
		return "", err
	}
	defer c.Close()
	return c.ReadPaneANSI(ctx, p.pane, "visible", 0)
}

type tmuxClaudeRewindPane struct {
	sess          *term.Session
	inflight      *inFlightCounter
	pane, panePID string
	pid, start    int64
	sid           string
}

func (p *tmuxClaudeRewindPane) verify(ctx context.Context) error {
	_, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid)), 128, 3*time.Second)
	return mapRewindExitErr(err)
}

func (p *tmuxClaudeRewindPane) press(ctx context.Context, keys ...string) error {
	sh, err := claudeRewindPressSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid, keys)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *tmuxClaudeRewindPane) pickerEnter(ctx context.Context, targetText string) error {
	sh, err := claudeRewindPickerEnterSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid, targetText)
	if err != nil {
		return err
	}
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	if err != nil {
		return mapRewindExitErr(err)
	}
	if strings.TrimSpace(string(out)) != "ok" {
		return errors.New("key delivery unconfirmed")
	}
	return nil
}

func (p *tmuxClaudeRewindPane) commitEnter(ctx context.Context, targetText string, restoreCode bool, expectedFiles []string) (claudeRewindCommit, error) {
	sh, err := claudeRewindCommitOptSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid, targetText, restoreCode, expectedFiles)
	if err != nil {
		return claudeRewindCommit{started: false}, err
	}
	res, err := claudeRewindExecResult(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 128, 0)
	return evalCommitExec(res, err)
}

func (p *tmuxClaudeRewindPane) waitInFlight(ctx context.Context) error {
	return p.inflight.Wait(ctx)
}

func (p *tmuxClaudeRewindPane) screen(ctx context.Context) (string, error) {
	sh := claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid) + `tmux -u capture-pane -p -t "$pane" 2>/dev/null 9>&-`
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 256<<10, 5*time.Second)
	return string(out), mapRewindExitErr(err)
}

func (p *tmuxClaudeRewindPane) colorScreen(ctx context.Context) (string, error) {
	sh := claudeRewindCheckSh("tmux", p.pane, "", p.panePID, p.pid, p.start, p.sid) + `tmux -u capture-pane -p -e -t "$pane" 2>/dev/null 9>&-`
	out, err := claudeRewindExec(ctx, p.sess, p.inflight, "sh -c "+sshx.QuoteShell(sh), 512<<10, 5*time.Second)
	return string(out), mapRewindExitErr(err)
}

// releaseClaudeRewind waits for all in-flight remote commands to complete,
// runs a final bounded panel cleanup if a Phase-1 failure/cancellation did not
// finish cleaning up in time, waits for any cleanup command to complete, and
// only then releases the pane lock.
func releaseClaudeRewind(running *inFlightCounter, pane claudeRewindPane, needsCleanup bool, unlock func()) {
	if running != nil {
		_ = running.Wait(context.Background())
	}
	if needsCleanup && pane != nil {
		cctx, cancel := context.WithTimeout(context.Background(), claudeRewindCleanupWait)
		closeRewindPanel(cctx, pane, true)
		cancel()
		if running != nil {
			_ = running.Wait(context.Background())
		}
	}
	if unlock != nil {
		unlock()
	}
}

func (s *Server) apiConversationRewind(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Pane        string   `json:"pane"`
		PID         int64    `json:"pid"`
		Start       int64    `json:"start"`
		Session     string   `json:"session"`
		TerminalID  string   `json:"terminal_id"`
		MessageID   string   `json:"message_id"`
		RestoreCode bool     `json:"restore_code"`
		Files       []string `json:"files"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeError(w, http.StatusConflict, "unsupported", "only in tmux and herdr sessions")
		return
	}
	if b.PID <= 0 || b.Start <= 0 || !convoUUID.MatchString(b.Session) || !convoUUID.MatchString(b.MessageID) ||
		(sess.Persistence == "herdr" && !herdrPane.MatchString(b.Pane)) ||
		(sess.Persistence == "tmux" && !tmuxPane.MatchString(b.Pane)) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid pane, process, session and message_id are required")
		return
	}
	var auditFiles []string
	if b.RestoreCode && len(b.Files) > 0 {
		var ferr error
		_, auditFiles, ferr = normalizeRewindFiles(b.Files)
		if ferr != nil {
			writeError(w, http.StatusBadRequest, "bad_request", ferr.Error())
			return
		}
	}
	outcome, reason := "refused", ""
	defer func() {
		if reason == "terminal_changed" || reason == "process_changed" || reason == "session_changed" {
			s.convoRelocNow(sess, b.Pane)
		}
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer acancel()
		detail := map[string]any{"session": sess.ID, "pane": b.Pane, "pid": b.PID, "start": b.Start, "claude_session": b.Session, "message_id": b.MessageID, "restore_code": b.RestoreCode, "outcome": outcome}
		if b.RestoreCode && len(auditFiles) > 0 {
			detail["files"] = auditFiles
		}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r.WithContext(actx), "convo.rewind", sess.HostName, detail)
	}()

	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		reason = "signed_out"
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if b.RestoreCode && len(b.Files) == 0 {
		reason = "no_code_changes"
		writeError(w, http.StatusConflict, "no_code_changes", "no restorable code changes after this message")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	ctx, cancelStep := context.WithTimeout(ctx, 45*time.Second)
	defer cancelStep()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()

	paneKey := s.convoReplyKey(sess, b.Pane)
	lock, err := s.replies.lock(ctx, paneKey)
	if err != nil {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another operation on this pane is still under way")
		return
	}
	turn, ok := convoSending.take(ctx, convoTurnKey(sess.ID, b.Pane))
	if !ok {
		s.replies.unlock(lock)
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another operation on this pane is still under way")
		return
	}
	var running inFlightCounter
	var pane claudeRewindPane
	cleanupNeeded := false
	defer func() {
		go releaseClaudeRewind(&running, pane, cleanupNeeded, func() {
			turn()
			s.replies.unlock(lock)
		})
	}()
	fail := func(re *rewindError) {
		reason = re.code
		writeError(w, re.status, re.code, re.msg)
	}

	// Verify physical pane (herdr live snapshot or tmux pane_pid).
	socket := s.herdrSocks.get(sess.ID)
	var liveTermID, liveHerdrSID, liveHerdrStatus, tmuxPanePID string
	if sess.Persistence == "herdr" {
		if socket == "" {
			fail(rwErr(http.StatusConflict, "terminal_changed", "herdr socket is not available"))
			return
		}
		hc, derr := herdr.DialWait(ctx, sess.Client(), socket, 5*time.Second)
		if derr != nil {
			fail(asRewindErr(derr))
			return
		}
		snap, serr := hc.Snapshot(ctx)
		hc.Close()
		if serr != nil {
			fail(asRewindErr(serr))
			return
		}
		info, found := herdrPaneSnapshot(snap, b.Pane)
		if !found || (b.TerminalID != "" && info.TerminalID != b.TerminalID) {
			fail(rwErr(http.StatusConflict, "terminal_changed", "the terminal in this pane changed"))
			return
		}
		liveTermID, liveHerdrSID, liveHerdrStatus = info.TerminalID, herdrValidatedUUID(info), info.AgentStatus
	} else {
		out, terr := claudeRewindExec(ctx, sess, &running, claudeRewindTmuxPaneSh(b.Pane), 512, 3*time.Second)
		if terr != nil {
			fail(asRewindErr(terr))
			return
		}
		f := strings.Fields(string(out))
		if len(f) < 2 || !isDigits(f[0]) || f[1] != "0" {
			fail(rwErr(http.StatusConflict, "terminal_changed", "tmux pane no longer exists or is dead"))
			return
		}
		tmuxPanePID = f[0]
	}

	// Locate real Claude Code process via /proc scan and verify identity & idle state.
	tmuxTarget := ""
	if sess.Persistence == "tmux" {
		tmuxTarget = b.Pane
	}
	tLoc := time.Now()
	locOut, lerr := claudeRewindExec(ctx, sess, &running, convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax, 3*time.Second)
	if lerr != nil {
		s.perfLocate("locate.full", tLoc, lerr, 0, sess.HostID)
		fail(asRewindErr(lerr))
		return
	}
	located := parseConvoLocate(string(locOut))
	s.perfLocate("locate.full", tLoc, nil, len(located.procs), sess.HostID)
	front := pickConvoProc(located.procs, sess.Persistence, b.Pane, socket, located.tmuxPID, located.paneTTY, liveHerdrSID)
	switch {
	case front == nil || front.pid != b.PID || front.start != b.Start:
		fail(rwErr(http.StatusConflict, "process_changed", "the Claude Code process in this pane changed"))
		return
	case front.agent != "claude":
		fail(rwErr(http.StatusConflict, "unsupported", "rewind is only supported for Claude Code"))
		return
	case front.sid != b.Session || (liveHerdrSID != "" && !strings.EqualFold(liveHerdrSID, b.Session)):
		fail(rwErr(http.StatusConflict, "session_changed", "the Claude Code session in this pane changed"))
		return
	case !front.fg:
		fail(rwErr(http.StatusConflict, "not_in_front", "Claude Code is not in front in its pane"))
		return
	case liveHerdrStatus == herdr.StatusWorking || liveHerdrStatus == herdr.StatusBlocked:
		fail(rwErr(http.StatusConflict, "agent_busy", "the agent is busy"))
		return
	}
	if status := s.convoFreshStatus(ctx, sess, &convoBind{PID: front.pid, Start: front.start, Session: front.sid}); status != "" && status != "idle" {
		fail(rwErr(http.StatusConflict, "agent_busy", "the agent is busy or waiting for input"))
		return
	}

	// Refresh log state and build rewind plan.
	stateKey := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		stateKey += b.Pane
	}
	st := s.convos.get(stateKey)
	defer st.done()
	st.mu.Lock()
	if st.sid != front.sid || st.log != front.log || st.agent != "claude" {
		st.bind = &convoBind{PID: front.pid, Start: front.start, Session: front.sid, Agent: "claude", Pane: b.Pane, TerminalID: liveTermID}
		st.adopt(front.sid, front.log, "claude", "")
	}
	_, rerr := s.readConvo(sess, st)
	if rerr != nil {
		st.mu.Unlock()
		fail(asRewindErr(rerr))
		return
	}
	cl, ok := st.parser.(*convo.Claude)
	if !ok {
		st.mu.Unlock()
		fail(rwErr(http.StatusConflict, "unsupported", "rewind is only supported for Claude Code"))
		return
	}
	allEntries := overlayRewindEntries(st, s.syncClaudeRewind(sess, st), cl.Since(0))
	knownUsers := cl.UserIDs()
	logPath := st.log
	st.mu.Unlock()

	if !hasActiveRewindTarget(allEntries, b.MessageID) {
		fail(rwErr(http.StatusConflict, "message_not_found", "that message is no longer in the active conversation"))
		return
	}
	fullText, terr := s.claudeRewindFullText(ctx, sess, &running, logPath, b.MessageID)
	if terr != nil {
		fail(terr)
		return
	}

	plan, perr := buildClaudeRewindPlan(allEntries, b.MessageID, fullText)
	if perr != nil {
		fail(perr)
		return
	}
	if b.RestoreCode {
		verifiedPaths, verr := verifyRewindCodePaths(allEntries, b.MessageID, auditFiles)
		if verr != nil {
			fail(verr)
			return
		}
		plan.RestoreCode = true
		plan.ExpectedFiles = verifiedPaths
	}

	if sess.Persistence == "herdr" {
		pane = &herdrClaudeRewindPane{sess: sess, inflight: &running, socket: socket, pane: b.Pane, terminalID: liveTermID, pid: front.pid, start: front.start, sid: front.sid}
	} else {
		pane = &tmuxClaudeRewindPane{sess: sess, inflight: &running, pane: b.Pane, panePID: tmuxPanePID, pid: front.pid, start: front.start, sid: front.sid}
	}
	res, rwerr := claudeRewind(ctx, pane, plan)
	if rwerr != nil {
		cleanupNeeded = res.needsCleanup
		if rwerr.code == "rewind_uncertain" {
			outcome = "unknown"
		}
		fail(rwerr)
		return
	}
	s.rewinds.set(rewindKey{paneKey: paneKey, sid: front.sid, pid: front.pid, start: front.start}, b.MessageID, knownUsers, res.remoteAt)
	outcome = "rewound"
	if res.Warning != "" {
		reason = res.Warning
	}
	writeJSON(w, http.StatusOK, res)
}
