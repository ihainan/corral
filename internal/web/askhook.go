package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/askhook"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// Claude Code's question hook in a host's own settings (internal/askhook):
// what ~/.claude/settings.json of the host's account says of it, and — only
// when the person asks, from the question card or the host's page — the
// hook put in or taken out. Done over a terminal already open to the host
// (as the port listing is): no other connection is made for it.
//
// Only ~/.claude is meant: the conversation view reads Claude Code's
// sessions and logs there alone (conversation.go), so a Claude Code with a
// config directory of its own has no card to fix.
//
// Writing (askHookWrite): the new file to a temporary one beside it; then,
// the file found as it was read (its sha256), the old one copied aside to
// settings.json.corral-bak and the new one renamed into place. Claude Code
// takes no lock on the file: a write of its own between that check and the
// rename (milliseconds) would be lost — the backup has the file as it was.

const askHookFile = "~/.claude/settings.json"

// askHookTTL: how long what a host's file says is taken as so (the card asks
// only when it falls back on the screen; the page, when it is opened).
const askHookTTL = 5 * time.Minute

// What the host's file is, as read by askHookRead's script:
// kind=none|file|link|other, links=<hard links>, sum=1 (a sha256 tool
// there), settings=<base64>, managed=<base64> (the system's managed settings).
const askHookRead = `f="$HOME/.claude/settings.json"
if [ -L "$f" ]; then echo kind=link
elif [ ! -e "$f" ]; then echo kind=none
elif [ -f "$f" ]; then echo kind=file; echo "links=$(ls -ldn "$f" | awk '{print $2}')"; printf 'settings='; base64 < "$f" | tr -d '\n'; echo
else echo kind=other; fi
if command -v sha256sum >/dev/null 2>&1 || command -v shasum >/dev/null 2>&1; then echo sum=1; fi
for m in /etc/claude-code/managed-settings.json "/Library/Application Support/ClaudeCode/managed-settings.json"; do
  if [ -f "$m" ] && [ -r "$m" ]; then printf 'managed='; base64 < "$m" | tr -d '\n'; echo; break; fi
done
true`

// askHookWrite: $1 the file's sha256 as read ("none": there was no file),
// $2 write (the new file on stdin) or remove. Exit 3: the file is not as read
// (or a link, or hard-linked) now; 4: it cannot be written so; 5: a step
// failed (the file as it was). Prints "done" once in place.
const askHookWrite = `set -u
d="$HOME/.claude"; f="$d/settings.json"; b="$f.corral-bak"; was="$1"; op="$2"
sum() { { sha256sum "$1" 2>/dev/null || shasum -a 256 "$1" 2>/dev/null; } | cut -c1-64; }
if [ -L "$f" ]; then exit 4; fi
if [ -e "$f" ]; then
  [ -f "$f" ] || exit 4
  [ "$(ls -ldn "$f" | awk '{print $2}')" = 1 ] || exit 4
fi
if [ ! -d "$d" ]; then mkdir -m 700 "$d" || exit 5; fi
tmp=""; tb=""
trap 'rm -f "$tmp" "$tb"' EXIT
if [ "$op" = write ]; then
  tmp=$(mktemp "$d/.settings.json.corral.XXXXXX") || exit 5
  if [ -e "$f" ]; then cp -p "$f" "$tmp" || exit 5; fi
  cat > "$tmp" || exit 5
fi
if [ "$was" = none ]; then
  [ -e "$f" ] && exit 3
else
  [ -f "$f" ] && [ ! -L "$f" ] && [ "$(sum "$f")" = "$was" ] || exit 3
fi
if [ -e "$f" ]; then
  tb=$(mktemp "$d/.settings.json.corral-bak.XXXXXX") || exit 5
  cp -p "$f" "$tb" || exit 5
  mv -f "$tb" "$b" || exit 5
  tb=""
fi
if [ "$op" = write ]; then
  mv -f "$tmp" "$f" || exit 5
  tmp=""
else
  rm -f "$f" || exit 5
fi
echo done`

// askHookInfo is what the card and the host's page are told.
type askHookInfo struct {
	State    askhook.State `json:"state,omitempty"`  // on, on-own, off, blocked; empty when not read
	Why      string        `json:"why,omitempty"`    // blocked: the setting; else why it cannot be written (see cannot)
	Cannot   string        `json:"cannot,omitempty"` // link, not_file, hard_link, invalid, no_sum: not written by the gateway (Snippet is for the person)
	File     string        `json:"file"`
	Seen     string        `json:"seen"`    // the file as read (sha256, or "none"): a change is refused when it is no longer so
	Snippet  string        `json:"snippet"` // what goes into hooks.PreToolUse, for the confirmation (and to add by hand)
	Declined bool          `json:"declined"`
}

type askHookFileRead struct {
	kind, settings, managed []byte
	links                   string
	sum                     bool
}

type askHooks struct {
	mu    sync.Mutex
	busy  map[[2]int64]bool // a change on the host under way, till its command has really ended
	cache map[[2]int64]askHookCached
}

type askHookCached struct {
	info    askHookInfo
	version int64 // the host's ConnVersion it was read with
	at      time.Time
}

func (a *askHooks) cached(key [2]int64, version int64) (askHookInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.cache[key]
	return c.info, ok && c.version == version && time.Since(c.at) < askHookTTL
}

func (a *askHooks) keep(key [2]int64, version int64, info askHookInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.cache == nil {
		a.cache = map[[2]int64]askHookCached{}
	}
	a.cache[key] = askHookCached{info, version, time.Now()}
}

func (a *askHooks) forget(key [2]int64) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.cache, key)
}

// take: the host's turn to be changed; release gives it back (called once
// the command has really ended, however the request went).
func (a *askHooks) take(key [2]int64) (release func(), ok bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.busy[key] {
		return nil, false
	}
	if a.busy == nil {
		a.busy = map[[2]int64]bool{}
	}
	a.busy[key] = true
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			delete(a.busy, key)
			a.mu.Unlock()
		})
	}, true
}

// askHookClient: the connection of a terminal open to h as it is now.
func (s *Server) askHookClient(h *store.Host) *ssh.Client {
	cfg := hostConfig(h)
	for _, sess := range s.terms.Running(h.OwnerID, h.ID) {
		if sess.HostConfig == cfg {
			if c := sess.Client(); c != nil {
				return c
			}
		}
	}
	return nil
}

func readAskHookFile(ctx context.Context, c *ssh.Client) (*askHookFileRead, error) {
	out, err := sshx.Output(ctx, c, askHookRead, 4<<20)
	if err != nil {
		return nil, err
	}
	return parseAskHookRead(out)
}

// parseAskHookRead: askHookRead's output.
func parseAskHookRead(out []byte) (*askHookFileRead, error) {
	f := &askHookFileRead{}
	for _, l := range strings.Split(string(out), "\n") {
		k, v, _ := strings.Cut(strings.TrimRight(l, "\r"), "=")
		switch k {
		case "kind":
			f.kind = []byte(v)
		case "links":
			f.links = v
		case "sum":
			f.sum = v == "1"
		case "settings", "managed":
			b, err := base64.StdEncoding.DecodeString(v)
			if err != nil {
				return nil, errors.New("the settings file could not be read")
			}
			if k == "settings" {
				f.settings = b
			} else {
				f.managed = b
			}
		}
	}
	switch string(f.kind) {
	case "none", "file", "link", "other":
	default:
		return nil, errors.New("the settings file could not be read")
	}
	return f, nil
}

// askHookInfoOf: what the file read says, and whether the gateway may write it.
func askHookInfoOf(f *askHookFileRead) askHookInfo {
	info := askHookInfo{File: askHookFile, Seen: "none", Snippet: askHookSnippet()}
	switch string(f.kind) {
	case "link":
		info.Cannot = "link"
	case "other":
		info.Cannot = "not_file"
	case "file":
		sum := sha256.Sum256(f.settings)
		info.Seen = hex.EncodeToString(sum[:])
		if f.links != "1" {
			info.Cannot = "hard_link"
		}
	}
	if info.Cannot == "" && !f.sum {
		info.Cannot = "no_sum"
	}
	if string(f.kind) == "file" || string(f.kind) == "none" {
		st, why, err := askhook.Read(f.settings, f.managed)
		if err != nil {
			if info.Cannot == "" {
				info.Cannot = "invalid"
			}
		} else {
			info.State, info.Why = st, why
		}
	}
	return info
}

// askHookSnippet: the hook as it reads in the file (under "hooks").
func askHookSnippet() string {
	var b bytes.Buffer
	_ = json.Indent(&b, []byte(askhook.Inline()), "", "  ")
	return b.String()
}

// GET /api/hosts/{id}/ask-hook: what the host's Claude Code settings say of
// the hook (read over a terminal open to it; 409 no_connection without one).
func (s *Server) apiAskHook(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	info, status, code, msg := s.askHookState(r.Context(), h, r.URL.Query().Get("fresh") == "1")
	if status != 0 {
		writeError(w, status, code, msg)
		return
	}
	writeJSON(w, http.StatusOK, info)
}

func (s *Server) askHookState(ctx context.Context, h *store.Host, fresh bool) (askHookInfo, int, string, string) {
	key := [2]int64{h.OwnerID, h.ID}
	declined, err := s.store.AskHookDeclined(ctx, h.OwnerID, h.ID)
	if err != nil {
		return askHookInfo{}, http.StatusNotFound, "not_found", "no such host"
	}
	if info, ok := s.askHook.cached(key, h.ConnVersion); ok && !fresh {
		info.Declined = declined
		return info, 0, "", ""
	}
	c := s.askHookClient(h)
	if c == nil {
		return askHookInfo{}, http.StatusConflict, "no_connection", "open a terminal to this host first"
	}
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f, err := readAskHookFile(rctx, c)
	if errors.Is(err, sshx.ErrBusy) {
		return askHookInfo{}, http.StatusServiceUnavailable, "busy", "too many commands are running on this host; try again in a moment"
	}
	if err != nil {
		return askHookInfo{}, http.StatusBadGateway, "read_failed", "could not read the Claude Code settings on the host"
	}
	info := askHookInfoOf(f)
	s.askHook.keep(key, h.ConnVersion, info)
	info.Declined = declined
	return info, 0, "", ""
}

// POST /api/hosts/{id}/ask-hook {on, seen}: the hook put in (on) or taken
// out, the file being as it was seen; or {declined}: the card's "no".
func (s *Server) apiSetAskHook(w http.ResponseWriter, r *http.Request) {
	h, ok := s.ownedHost(w, r)
	if !ok {
		return
	}
	var b struct {
		On       *bool  `json:"on"`
		Seen     string `json:"seen"`
		Declined *bool  `json:"declined"`
	}
	if err := readJSON(r, &b); err != nil || (b.On == nil) == (b.Declined == nil) {
		writeError(w, http.StatusBadRequest, "bad_request", "say on or declined")
		return
	}
	if b.Declined != nil {
		if err := s.store.SetAskHookDeclined(r.Context(), h.OwnerID, h.ID, *b.Declined); err != nil {
			writeError(w, http.StatusNotFound, "not_found", "no such host")
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"declined": *b.Declined})
		return
	}
	on := *b.On
	key := [2]int64{h.OwnerID, h.ID}
	release, ok := s.askHook.take(key)
	if !ok {
		writeError(w, http.StatusConflict, "busy", "a change to this host's settings is already under way")
		return
	}
	held := false // (handed on to the command: given back once it has really ended)
	defer func() {
		if !held {
			release()
		}
	}()
	c := s.askHookClient(h)
	if c == nil {
		writeError(w, http.StatusConflict, "no_connection", "open a terminal to this host first")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	f, err := readAskHookFile(ctx, c)
	if err != nil {
		writeError(w, http.StatusBadGateway, "read_failed", "could not read the Claude Code settings on the host")
		return
	}
	info := askHookInfoOf(f)
	audit := func(outcome string) {
		s.audit(r, "host.ask_hook", hostTarget(h), map[string]any{"on": on, "file": askHookFile, "outcome": outcome})
	}
	if b.Seen != info.Seen {
		s.askHook.keep(key, h.ConnVersion, info)
		writeErrorExtra(w, http.StatusConflict, "changed", "the settings file changed since it was shown; look at it again", map[string]any{"info": info})
		return
	}
	if info.Cannot != "" || info.State == "" {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "cannot", "the gateway does not change this settings file", map[string]any{"info": info})
		return
	}
	var next []byte
	var changed, remove bool
	if on {
		next, changed, err = askhook.Enable(f.settings)
	} else {
		next, remove, changed, err = askhook.Disable(f.settings)
		remove = remove && string(f.kind) == "file"
	}
	if err != nil {
		writeErrorExtra(w, http.StatusUnprocessableEntity, "cannot", "the gateway does not change this settings file", map[string]any{"info": info})
		return
	}
	if !changed {
		s.answerAskHook(w, r.Context(), h, key, on, c)
		return
	}
	op := "write"
	if remove {
		op, next = "remove", nil
	}
	cmd := "sh -c " + sshx.QuoteShell(askHookWrite) + " sh " + sshx.QuoteShell(info.Seen) + " " + op
	held = true
	res, err := sshx.Exec(ctx, c, cmd, 4096, sshx.ExecOptions{Stdin: bytes.NewReader(next), Release: release})
	s.askHook.forget(key)
	var ee *sshx.ExitError
	switch {
	case err == nil && strings.TrimSpace(string(res.Out)) == "done":
		audit("done")
	case errors.As(err, &ee) && ee.Status == 3:
		audit("changed")
		writeError(w, http.StatusConflict, "changed", "the settings file changed meanwhile; nothing was written")
		return
	case errors.As(err, &ee) && (ee.Status == 4 || ee.Status == 5):
		audit("failed")
		writeError(w, http.StatusBadGateway, "write_failed", "the settings file could not be changed; it is as it was")
		return
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusServiceUnavailable, "busy", "too many commands are running on this host; try again in a moment")
		return
	default:
		// (Not known to have ended, or ended unlike the script does: what the
		// file is now is not known from here — read it again later.)
		audit("uncertain")
		writeError(w, http.StatusBadGateway, "uncertain", "it is not known whether the settings file was changed; look at it again in a moment")
		return
	}
	if on {
		_ = s.store.SetAskHookDeclined(r.Context(), h.OwnerID, h.ID, false)
	}
	s.answerAskHook(w, r.Context(), h, key, on, c)
}

// answerAskHook: the file read again after a change, what it says now.
func (s *Server) answerAskHook(w http.ResponseWriter, ctx context.Context, h *store.Host, key [2]int64, on bool, c *ssh.Client) {
	rctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	f, err := readAskHookFile(rctx, c)
	if err != nil {
		writeError(w, http.StatusBadGateway, "uncertain", "it is not known whether the settings file was changed; look at it again in a moment")
		return
	}
	info := askHookInfoOf(f)
	s.askHook.keep(key, h.ConnVersion, info)
	info.Declined, _ = s.store.AskHookDeclined(ctx, h.OwnerID, h.ID)
	writeJSON(w, http.StatusOK, info)
}
