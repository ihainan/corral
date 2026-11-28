package web

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/corrald/gw"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Switching an Antigravity CLI (agy) session's model and reasoning effort from
// the agents page, for this session only.
//
// Unlike Claude Code and Codex (whose models are only read by opening an
// interactive picker), agy exposes `agy models` on stdout as TSV
// (`<id>\t<Display Name>`) and accepts `/model <id>` and `/effort <level>`
// directly as slash commands without driving an interactive TUI. Both commands
// persist the chosen model to ~/.gemini/antigravity-cli/settings.json, so
// Corral saves the top-level "model" before switching and restores it via
// compare-and-swap afterward (same contract as Claude Code's settings.json).

const (
	agyModelsCommand = `PATH="$HOME/.local/bin:$HOME/bin:/usr/local/bin:$PATH" agy models 2>/dev/null`

	agySettingsReadCommand = `f="$HOME/.gemini/antigravity-cli/settings.json"
if [ -L "$f" ]; then f=$(readlink -f "$f") && [ -n "$f" ] || exit 5; fi
if [ ! -e "$f" ]; then d=${f%/*}; { [ ! -e "$d" ] || { [ -r "$d" ] && [ -x "$d" ]; }; } && exit 3; exit 5; fi
head -c 1048577 "$f" || exit 5`

	agyModelCacheTTL     = 12 * time.Hour
	agyModelRefreshAfter = 10 * time.Minute
)

func agySettingsWriteCommand(was, now []byte) string {
	script := `f="$HOME/.gemini/antigravity-cli/settings.json"
if [ -L "$f" ]; then f=$(readlink -f "$f") && [ -n "$f" ] || exit 4; fi
[ -f "$f" ] || exit 4
sum() { sha256sum <"$1" | cut -d' ' -f1; }
[ "$(sum "$f")" = "$1" ] || exit 9
t=$(mktemp "$f.corral.XXXXXX") || exit 4
cp -p "$f" "$t" || { rm -f "$t"; exit 4; }
cat >"$t" || { rm -f "$t"; exit 4; }
[ "$(wc -c <"$t" | tr -d ' ')" = "$2" ] && [ "$(sum "$t")" = "$3" ] || { rm -f "$t"; exit 4; }
[ "$(sum "$f")" = "$1" ] || { rm -f "$t"; exit 9; }
mv -f "$t" "$f" || { rm -f "$t"; exit 4; }`
	a, b := sha256.Sum256(was), sha256.Sum256(now)
	return "sh -c " + sshx.QuoteShell(script) + " corral " + hex.EncodeToString(a[:]) + " " + strconv.Itoa(len(now)) + " " + hex.EncodeToString(b[:])
}

var (
	agyModelIDRe     = regexp.MustCompile(`^[A-Za-z0-9._-]{2,80}$`)
	agyModelSetRe    = regexp.MustCompile(`Model set to\s+([^\r\n]+)`)
	agyEffortSetRe   = regexp.MustCompile(`Effort set to\s+([a-z]+)`)
	agyEffortNoRe    = regexp.MustCompile(`has no "([a-z]+)" effort(?:\s*\([^)]*\))?`)
	agyStatusTaskRe  = regexp.MustCompile(`^\d+\s+task\(s\)$`)
	agyStatusWideGap = regexp.MustCompile(`\s{2,}`)
	agyEffortOrder   = map[string]int{"low": 0, "medium": 1, "high": 2, "xhigh": 3, "max": 4}
)

type agyModelVariant struct {
	id      string
	display string
	effort  string // "" when the model has no effort suffix
}

type agyModelGroup struct {
	baseID   string
	display  string
	variants []agyModelVariant
}

// agySplitEffort splits a trailing effort suffix from an agy model ID and its
// display name when both carry the same effort level (e.g. "gemini-4-neon-high"
// and "Gemini 4 Neon (High)").
func agySplitEffort(id, display string) (baseID, baseDisplay, effort string, ok bool) {
	for _, lvl := range effortLevels {
		if !strings.HasSuffix(id, "-"+lvl) {
			continue
		}
		if len(display) > len(lvl)+3 && strings.HasSuffix(display, ")") {
			if idx := strings.LastIndex(display, " ("); idx > 0 {
				paren := strings.ToLower(strings.TrimSpace(display[idx+2 : len(display)-1]))
				if paren == lvl {
					return strings.TrimSuffix(id, "-"+lvl), strings.TrimSpace(display[:idx]), lvl, true
				}
			}
		}
	}
	return id, display, "", false
}

// parseAgyModels parses `agy models` stdout into grouped models preserving
// first-seen order and sorting each group's efforts canonically.
func parseAgyModels(out string) []agyModelGroup {
	var groups []agyModelGroup
	byKey := map[string]int{}
	seenID := map[string]bool{}
	for _, raw := range strings.Split(strings.ReplaceAll(out, "\r", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		var id, display string
		if i := strings.IndexByte(line, '\t'); i > 0 {
			id = strings.TrimSpace(line[:i])
			display = strings.TrimSpace(line[i+1:])
		} else if loc := agyStatusWideGap.FindStringIndex(line); loc != nil {
			id = strings.TrimSpace(line[:loc[0]])
			display = strings.TrimSpace(line[loc[1]:])
		} else if i := strings.IndexByte(line, ' '); i > 0 {
			id = strings.TrimSpace(line[:i])
			display = strings.TrimSpace(line[i+1:])
		} else {
			continue
		}
		if !agyModelIDRe.MatchString(id) || display == "" || seenID[id] {
			continue
		}
		seenID[id] = true
		baseID, baseDisp, lvl, hasEffort := agySplitEffort(id, display)
		key := id
		if hasEffort {
			key = "base:" + baseID
		}
		idx, exists := byKey[key]
		if !exists {
			idx = len(groups)
			byKey[key] = idx
			gDisp := display
			if hasEffort {
				gDisp = baseDisp
			}
			groups = append(groups, agyModelGroup{baseID: baseID, display: gDisp})
		}
		groups[idx].variants = append(groups[idx].variants, agyModelVariant{
			id:      id,
			display: display,
			effort:  lvl,
		})
	}
	for i := range groups {
		slices.SortFunc(groups[i].variants, func(a, b agyModelVariant) int {
			return agyEffortOrder[a.effort] - agyEffortOrder[b.effort]
		})
	}
	return groups
}

// agyModelOpts turns parsed groups into menu rows, marking the current model
// from the live status bar (preferred) or transcript model ID.
func agyModelOpts(groups []agyModelGroup, liveModel string) []modelOpt {
	live := strings.ToLower(strings.TrimSpace(liveModel))
	opts := make([]modelOpt, 0, len(groups))
	for _, g := range groups {
		if len(g.variants) >= 2 {
			effs := make([]string, 0, len(g.variants))
			cur := live != "" && (strings.EqualFold(g.display, live) || strings.EqualFold(g.baseID, live))
			for _, v := range g.variants {
				if v.effort != "" {
					effs = append(effs, v.effort)
				}
				if !cur && live != "" && (strings.EqualFold(v.id, live) || strings.EqualFold(v.display, live)) {
					cur = true
				}
			}
			opts = append(opts, modelOpt{
				Label:   g.display,
				Desc:    g.baseID,
				Current: cur,
				Efforts: effs,
			})
			continue
		}
		if len(g.variants) == 1 {
			v := g.variants[0]
			cur := live != "" && (strings.EqualFold(v.display, live) || strings.EqualFold(g.display, live) || strings.EqualFold(v.id, live))
			opts = append(opts, modelOpt{
				Label:   v.display,
				Desc:    v.id,
				Current: cur,
			})
		}
	}
	return opts
}

// resolveAgyModelID finds the exact agy model variant for (target, effort).
// fallbackEffort is used when target names a multi-effort group without an
// explicit effort (e.g. picking a model without touching the effort pills).
func resolveAgyModelID(groups []agyModelGroup, target, effort, fallbackEffort string) (agyModelVariant, string, bool) {
	t := strings.TrimSpace(target)
	e := strings.ToLower(strings.TrimSpace(effort))
	fb := strings.ToLower(strings.TrimSpace(fallbackEffort))
	if t == "" {
		return agyModelVariant{}, "", false
	}
	for _, g := range groups {
		// Exact variant match by ID or full display name.
		for _, v := range g.variants {
			if strings.EqualFold(v.id, t) || strings.EqualFold(v.display, t) {
				if e != "" && len(g.variants) >= 2 {
					for _, alt := range g.variants {
						if alt.effort == e {
							return alt, g.display, true
						}
					}
					return agyModelVariant{}, "", false
				}
				return v, g.display, true
			}
		}
		// Group match by base display name or base ID.
		if strings.EqualFold(g.display, t) || strings.EqualFold(g.baseID, t) {
			if len(g.variants) == 1 {
				if e == "" || g.variants[0].effort == "" || g.variants[0].effort == e {
					return g.variants[0], g.display, true
				}
				return agyModelVariant{}, "", false
			}
			if e != "" {
				for _, v := range g.variants {
					if v.effort == e {
						return v, g.display, true
					}
				}
				return agyModelVariant{}, "", false
			}
			// No explicit effort: prefer current effort, then high/medium, then first.
			for _, want := range []string{fb, "high", "medium"} {
				if want == "" {
					continue
				}
				for _, v := range g.variants {
					if v.effort == want {
						return v, g.display, true
					}
				}
			}
			return g.variants[0], g.display, true
		}
	}
	return agyModelVariant{}, "", false
}

// agyStatusModelEffort parses the current model display name and reasoning
// effort from the status bar below agy's input box, e.g.:
//
//	"? for shortcuts   auto · Gemini 4 Neon · high" -> ("Gemini 4 Neon", "high")
//	"esc to cancel     auto · Gemini 4 Neon · high · 1 task(s) · /tasks" -> ("Gemini 4 Neon", "high")
//	"? for shortcuts   auto · Orchestrator (Neon)" -> ("Orchestrator (Neon)", "")
func agyStatusModelEffort(screen string) (modelDisplay, effort string) {
	plain := sgr.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(screen), "")
	lines := strings.Split(strings.TrimRight(plain, "\n"), "\n")
	_, bot, ok := agyBoxBounds(lines)
	if !ok || bot+1 >= len(lines) {
		return "", ""
	}
	var status string
	for i := len(lines) - 1; i > bot; i-- {
		t := strings.TrimSpace(lines[i])
		if t == "" || agyRule.MatchString(t) {
			continue
		}
		if strings.Contains(t, "·") || strings.Contains(t, "? for shortcuts") || strings.Contains(t, "esc to cancel") {
			status = t
			break
		}
	}
	if status == "" {
		return "", ""
	}
	parts := strings.Split(status, "·")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		if s := strings.TrimSpace(p); s != "" {
			segs = append(segs, s)
		}
	}
	// Strip trailing task/shortcut chips.
	for len(segs) > 0 {
		last := segs[len(segs)-1]
		low := strings.ToLower(last)
		if low == "/tasks" || agyStatusTaskRe.MatchString(low) || strings.HasPrefix(low, "ctrl+") || strings.HasPrefix(low, "esc to ") {
			segs = segs[:len(segs)-1]
			continue
		}
		break
	}
	if len(segs) == 0 {
		return "", ""
	}
	// Check if the last segment is an effort level.
	if _, isEff := agyEffortOrder[strings.ToLower(segs[len(segs)-1])]; isEff {
		effort = strings.ToLower(segs[len(segs)-1])
		segs = segs[:len(segs)-1]
	}
	if len(segs) == 0 {
		return "", effort
	}
	// Model display is now the last remaining segment; strip any left-side hint
	// separated by 2+ spaces (e.g. "? for shortcuts   Gemini 4 Neon" in default mode).
	m := segs[len(segs)-1]
	if chunks := agyStatusWideGap.Split(m, -1); len(chunks) > 1 {
		m = strings.TrimSpace(chunks[len(chunks)-1])
	}
	for _, prefix := range []string{"? for shortcuts ", "esc to cancel "} {
		if strings.HasPrefix(strings.ToLower(m), prefix) {
			m = strings.TrimSpace(m[len(prefix):])
		}
	}
	lowM := strings.ToLower(m)
	if lowM == "auto" || lowM == "plan" || lowM == "accept-edits" || lowM == "? for shortcuts" || lowM == "esc to cancel" {
		return "", effort
	}
	return m, effort
}

// agyModelSetResult returns the last "Model set to <name>" confirmation shown
// above the input box, or "".
func agyModelSetResult(screen string) string {
	plain := sgr.ReplaceAllString(screen, "")
	all := agyModelSetRe.FindAllStringSubmatch(plain, -1)
	if len(all) == 0 {
		return ""
	}
	return strings.TrimSpace(all[len(all)-1][1])
}

// agyEffortSetResult inspects the latest /effort output on screen.
func agyEffortSetResult(screen string) (setTo, unsupported string) {
	plain := sgr.ReplaceAllString(screen, "")
	lines := strings.Split(plain, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := lines[i]
		if m := agyEffortNoRe.FindStringSubmatch(l); m != nil {
			return "", strings.TrimSpace(l)
		}
		if m := agyEffortSetRe.FindStringSubmatch(l); m != nil {
			return strings.ToLower(m[1]), ""
		}
	}
	return "", ""
}

type agyModelCacheEntry struct {
	groups []agyModelGroup
	at     time.Time
}

type agyModelFlightCall struct {
	done   chan struct{}
	groups []agyModelGroup
	err    error
}

type storedAgyVariant struct {
	ID      string `json:"id"`
	Display string `json:"display"`
	Effort  string `json:"effort,omitempty"`
}

type storedAgyGroup struct {
	BaseID   string             `json:"base_id"`
	Display  string             `json:"display"`
	Variants []storedAgyVariant `json:"variants"`
}

var (
	agyModelsCacheMu sync.Mutex
	agyModelsCache   = map[int64]agyModelCacheEntry{}
	agyModelsFlight  = map[int64]*agyModelFlightCall{}
	agyModelFronts   sync.Map // modelFrontKey → time.Time

	agySettingsMu  sync.Mutex
	agySettingsSem = map[int64]chan struct{}{}
)

func agyModelStoreKey(hostID int64) string {
	return "agy:" + strconv.FormatInt(hostID, 10)
}

func noteAgyModelFront(key string, at time.Time) {
	agyModelFronts.Range(func(k, v any) bool {
		if at.Sub(v.(time.Time)) > agyModelCacheTTL {
			agyModelFronts.Delete(k)
		}
		return true
	})
	agyModelFronts.Store(key, at)
}

func isAgyModelFront(key string) bool {
	v, ok := agyModelFronts.Load(key)
	if !ok {
		return false
	}
	if time.Since(v.(time.Time)) > agyModelCacheTTL {
		agyModelFronts.Delete(key)
		return false
	}
	return true
}

func marshalAgyGroups(groups []agyModelGroup) ([]byte, error) {
	out := make([]storedAgyGroup, 0, len(groups))
	for _, g := range groups {
		vs := make([]storedAgyVariant, 0, len(g.variants))
		for _, v := range g.variants {
			vs = append(vs, storedAgyVariant{ID: v.id, Display: v.display, Effort: v.effort})
		}
		out = append(out, storedAgyGroup{BaseID: g.baseID, Display: g.display, Variants: vs})
	}
	return json.Marshal(out)
}

func unmarshalAgyGroups(raw []byte) ([]agyModelGroup, bool) {
	var in []storedAgyGroup
	if err := json.Unmarshal(raw, &in); err != nil || len(in) == 0 {
		return nil, false
	}
	out := make([]agyModelGroup, 0, len(in))
	for _, g := range in {
		vs := make([]agyModelVariant, 0, len(g.Variants))
		for _, v := range g.Variants {
			vs = append(vs, agyModelVariant{id: v.ID, display: v.Display, effort: v.Effort})
		}
		out = append(out, agyModelGroup{baseID: g.BaseID, display: g.Display, variants: vs})
	}
	return out, true
}

func agySettingsTurn(ctx context.Context, key int64) (func(), bool) {
	agySettingsMu.Lock()
	ch := agySettingsSem[key]
	if ch == nil {
		ch = make(chan struct{}, 1)
		agySettingsSem[key] = ch
	}
	agySettingsMu.Unlock()
	select {
	case ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-ch }) }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (s *Server) agyCachedModels(ctx context.Context, hostID int64) ([]agyModelGroup, time.Time, bool) {
	agyModelsCacheMu.Lock()
	ent, ok := agyModelsCache[hostID]
	agyModelsCacheMu.Unlock()
	if ok && time.Since(ent.at) < agyModelCacheTTL && len(ent.groups) > 0 {
		return ent.groups, ent.at, true
	}
	if s.store == nil {
		return nil, time.Time{}, false
	}
	atUnix, raw, found, err := s.store.ModelList(ctx, agyModelStoreKey(hostID))
	if err != nil || !found {
		return nil, time.Time{}, false
	}
	at := time.Unix(atUnix, 0)
	if time.Since(at) >= agyModelCacheTTL {
		return nil, time.Time{}, false
	}
	groups, ok := unmarshalAgyGroups(raw)
	if !ok {
		return nil, time.Time{}, false
	}
	agyModelsCacheMu.Lock()
	if cur, exists := agyModelsCache[hostID]; !exists || cur.at.Before(at) {
		agyModelsCache[hostID] = agyModelCacheEntry{groups: groups, at: at}
	}
	agyModelsCacheMu.Unlock()
	return groups, at, true
}

func (s *Server) fetchAgyModels(ctx context.Context, sess *term.Session, fresh bool) ([]agyModelGroup, error) {
	key := sess.HostID
	if !fresh {
		if groups, _, ok := s.agyCachedModels(ctx, key); ok {
			return groups, nil
		}
	}
	// Singleflight per host so warm-up and concurrent UI opens share one `agy models` call.
	agyModelsCacheMu.Lock()
	if call, ok := agyModelsFlight[key]; ok {
		agyModelsCacheMu.Unlock()
		select {
		case <-call.done:
			return call.groups, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &agyModelFlightCall{done: make(chan struct{})}
	agyModelsFlight[key] = call
	agyModelsCacheMu.Unlock()

	go func() {
		defer func() {
			agyModelsCacheMu.Lock()
			if agyModelsFlight[key] == call {
				delete(agyModelsFlight, key)
			}
			agyModelsCacheMu.Unlock()
			close(call.done)
		}()
		mctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		var await time.Duration
		if !fresh {
			await = 2 * time.Second
		}
		res, err := sshx.Exec(mctx, sess.Client(), agyModelsCommand, 128<<10, sshx.ExecOptions{Interactive: !fresh, AwaitSlot: await})
		out := res.Out
		if err != nil {
			call.err = err
			return
		}
		groups := parseAgyModels(string(out))
		if len(groups) == 0 {
			call.err = errors.New("agy models returned no models")
			return
		}
		now := time.Now()
		agyModelsCacheMu.Lock()
		agyModelsCache[key] = agyModelCacheEntry{groups: groups, at: now}
		agyModelsCacheMu.Unlock()
		if s.store != nil {
			if raw, merr := marshalAgyGroups(groups); merr == nil {
				_ = s.store.PutModelList(context.Background(), agyModelStoreKey(key), now.Unix(), raw)
			}
		}
		call.groups = groups
	}()

	select {
	case <-call.done:
		return call.groups, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// warmAgyModels refreshes the host's Antigravity model list in the background
// when missing or older than agyModelRefreshAfter, so opening the model picker
// is instant even right after a gateway restart.
func (s *Server) warmAgyModels(sess *term.Session) {
	if sess == nil || sess.Client() == nil {
		return
	}
	if _, at, ok := s.agyCachedModels(context.Background(), sess.HostID); ok && time.Since(at) < agyModelRefreshAfter {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = s.fetchAgyModels(ctx, sess, true)
	}()
}

// keptAgyModels serves cached Antigravity models immediately without any SSH
// round-trip (neither locateScan nor `agy models`), mirroring Claude Code's
// keptModelsOf. If the cached list is older than agyModelRefreshAfter, a
// background stale-while-revalidate refresh is kicked off.
func (s *Server) keptAgyModels(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane string, pid, start int64, sid string, outcome, reason *string) bool {
	if !isAgyModelFront(modelFrontKey(sess.ID, pane, pid, start, sid)) && !s.isAgyHerdrPane(sess.ID, pane) {
		return false
	}
	groups, at, ok := s.agyCachedModels(ctx, sess.HostID)
	if !ok {
		return false
	}
	opts := agyModelOpts(groups, "")
	*outcome, *reason = "kept", "list_kept"
	writeJSON(w, http.StatusOK, map[string]any{"models": opts, "kept": true})
	if time.Since(at) >= agyModelRefreshAfter {
		s.warmAgyModels(sess)
	}
	return true
}

func readAgySettings(ctx context.Context, sess *term.Session) (doc []byte, none bool, err error) {
	res, err := sshx.Exec(ctx, sess.Client(), agySettingsReadCommand, maxSettings+16, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	out := res.Out
	var exitErr *sshx.ExitError
	switch {
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		return nil, true, nil
	case err != nil:
		return nil, false, err
	case len(out) > maxSettings:
		return nil, false, errors.New("settings too long")
	}
	return out, false, nil
}

// convoAgyModels handles POST /conversation/models for agy on a cold cache:
// runs `agy models` (cached per host with singleflight) and reads the live
// status bar in parallel to mark the current model and effort.
func (s *Server) convoAgyModels(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid string, outcome, reason *string, release func()) {
	defer release()
	noteAgyModelFront(modelFrontKey(sess.ID, pane, pid, start, sid), time.Now())

	var (
		groups     []agyModelGroup
		err        error
		liveModel  string
		liveEffort string
		wg         sync.WaitGroup
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		groups, err = s.fetchAgyModels(ctx, sess, false)
	}()
	go func() {
		defer wg.Done()
		sctx, scancel := context.WithTimeout(ctx, 8*time.Second)
		defer scancel()
		scrRes, serr := sshx.Exec(sctx, sess.Client(), agyScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
		scr := scrRes.Out
		if serr == nil {
			liveModel, liveEffort = agyStatusModelEffort(string(scr))
		}
	}()
	wg.Wait()

	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*reason = "models_failed"
		writeError(w, http.StatusBadGateway, "models_failed", "could not list Antigravity models")
		return
	}
	opts := agyModelOpts(groups, liveModel)
	*outcome = "read"
	resp := map[string]any{"models": opts}
	if liveEffort != "" {
		resp["effort"] = liveEffort
	}
	writeJSON(w, http.StatusOK, resp)
}

// convoAgySwitch sends `/model <id>` or `/effort <level>` to an idle agy pane,
// verifies the status bar updated, and restores ~/.gemini/antigravity-cli/settings.json
// so the change applies to this session only.
func (s *Server) convoAgyModel(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, target, effort string, outcome, reason *string, release func()) {
	doneLock, ok := agySettingsTurn(ctx, sess.HostID)
	if !ok {
		release()
		*reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another Antigravity model switch is under way")
		return
	}
	defer func() { doneLock(); release() }()

	groups, err := s.fetchAgyModels(ctx, sess, false)
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*reason = "models_failed"
		writeError(w, http.StatusBadGateway, "models_failed", "could not list Antigravity models")
		return
	}
	// Read live colored screen once to verify idle + empty input AND learn current effort.
	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	rawRes, err := sshx.Exec(sctx, sess.Client(), agyColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	rawAnsi := rawRes.Out
	scancel()
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*reason = "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "could not read Antigravity's screen: nothing sent")
		return
	}
	plain := sgr.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(string(rawAnsi)), "")
	if ierr := agyIdleInputColor(string(rawAnsi)); ierr != nil {
		switch {
		case errors.Is(ierr, errAgyAsks):
			*outcome, *reason = "refused", "agent_waiting"
			writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		case errors.Is(ierr, errDraft):
			*outcome, *reason = "refused", "draft"
			writeError(w, http.StatusConflict, "draft", "its input holds text: clear it in the terminal first")
		default:
			*outcome, *reason = "refused", "cannot_verify"
			writeError(w, http.StatusConflict, "cannot_verify", "could not see Antigravity's input box: nothing sent")
		}
		return
	}
	digest, okBox := agyBoxDigest([]byte(plain))
	if !okBox || digest == "" {
		*outcome, *reason = "refused", "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "could not verify Antigravity's input box: nothing sent")
		return
	}
	_, curEffort := agyStatusModelEffort(plain)

	variant, groupDisp, found := resolveAgyModelID(groups, target, effort, curEffort)
	if !found || !agyModelIDRe.MatchString(variant.id) {
		*outcome, *reason = "refused", "no_such_model"
		writeError(w, http.StatusBadRequest, "bad_request", "no such Antigravity model or effort")
		return
	}

	s.runAgySlashSwitch(w, ctx, sess, pane, socket, pid, start, sid, digest, "/model "+variant.id, func(scr string) (done bool, failMsg string) {
		mDisp, mEff := agyStatusModelEffort(scr)
		if mDisp != "" && (strings.EqualFold(mDisp, groupDisp) || strings.EqualFold(mDisp, variant.display)) &&
			(variant.effort == "" || mEff == variant.effort) {
			return true, ""
		}
		if set := agyModelSetResult(scr); set != "" && strings.EqualFold(set, variant.display) {
			return true, ""
		}
		return false, ""
	}, variant.id, variant.effort, outcome, reason)
}

func (s *Server) convoAgyEffort(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane, socket string, pid, start int64, sid, level string, outcome, reason *string, release func()) {
	if effortLevel(level) < 0 {
		release()
		*outcome, *reason = "refused", "no_such_effort"
		writeError(w, http.StatusBadRequest, "bad_request", "no such effort level")
		return
	}
	doneLock, ok := agySettingsTurn(ctx, sess.HostID)
	if !ok {
		release()
		*reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another Antigravity model switch is under way")
		return
	}
	defer func() { doneLock(); release() }()

	sctx, scancel := context.WithTimeout(ctx, 10*time.Second)
	rawRes, err := sshx.Exec(sctx, sess.Client(), agyColorScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	rawAnsi := rawRes.Out
	scancel()
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			*reason = "ssh_channels_busy"
			writeSSHChannelsBusy(w)
			return
		}
		*reason = "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "could not read Antigravity's screen: nothing sent")
		return
	}
	plain := sgr.ReplaceAllString(strings.NewReplacer("\x00", "", "\r", "").Replace(string(rawAnsi)), "")
	if ierr := agyIdleInputColor(string(rawAnsi)); ierr != nil {
		switch {
		case errors.Is(ierr, errAgyAsks):
			*outcome, *reason = "refused", "agent_waiting"
			writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		case errors.Is(ierr, errDraft):
			*outcome, *reason = "refused", "draft"
			writeError(w, http.StatusConflict, "draft", "its input holds text: clear it in the terminal first")
		default:
			*outcome, *reason = "refused", "cannot_verify"
			writeError(w, http.StatusConflict, "cannot_verify", "could not see Antigravity's input box: nothing sent")
		}
		return
	}
	digest, okBox := agyBoxDigest([]byte(plain))
	if !okBox || digest == "" {
		*outcome, *reason = "refused", "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "could not verify Antigravity's input box: nothing sent")
		return
	}

	beforeNo := strings.Count(plain, `has no "`+level+`" effort`)
	s.runAgySlashSwitch(w, ctx, sess, pane, socket, pid, start, sid, digest, "/effort "+level, func(scr string) (done bool, failMsg string) {
		plain := sgr.ReplaceAllString(scr, "")
		if strings.Count(plain, `has no "`+level+`" effort`) > beforeNo {
			if _, unsup := agyEffortSetResult(plain); unsup != "" {
				return false, unsup
			}
			return false, `this model does not support "` + level + `" effort`
		}
		if _, mEff := agyStatusModelEffort(plain); mEff == level {
			return true, ""
		}
		if setTo, _ := agyEffortSetResult(plain); setTo == level {
			return true, ""
		}
		return false, ""
	}, "", level, outcome, reason)
}

func (s *Server) runAgySlashSwitch(
	w http.ResponseWriter, ctx context.Context, sess *term.Session,
	pane, socket string, pid, start int64, sid, digest, slashCmd string,
	check func(screen string) (done bool, failMsg string),
	resModel, resEffort string, outcome, reason *string,
) {
	// Snapshot ~/.gemini/antigravity-cli/settings.json "model" before typing.
	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	saved, none, err := readAgySettings(rctx, sess)
	rcancel()
	was, had, perr := settingsModel(saved, none)
	if errors.Is(err, sshx.ErrBusy) {
		*reason = "ssh_channels_busy"
		writeSSHChannelsBusy(w)
		return
	}
	if err != nil || perr != nil {
		*reason = "settings"
		writeError(w, http.StatusBadGateway, "settings", "Antigravity's settings could not be read: nothing done")
		return
	}

	restore := func() (changed bool, rerr error) {
		ctx2, cancel2 := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
		defer cancel2()
		for try := 0; try < 2; try++ {
			now, gone, err := readAgySettings(ctx2, sess)
			if err != nil {
				return true, err
			}
			cur, has, err := settingsModel(now, gone)
			if err != nil {
				return true, err
			}
			if has == had && (!has || jsonSame(cur, was)) {
				return changed, nil
			}
			changed = true
			if gone {
				return true, errors.New("settings file is gone")
			}
			fixed, err := withModel(now, was, had)
			if err != nil {
				return true, err
			}
			_, werr := sshx.Exec(ctx2, sess.Client(), agySettingsWriteCommand(now, fixed), 4096, sshx.ExecOptions{Stdin: bytes.NewReader(fixed), Interactive: true, AwaitSlot: 2 * time.Second})
			var exitErr *sshx.ExitError
			if errors.As(werr, &exitErr) && exitErr.Status == 9 {
				continue
			}
			if werr != nil {
				return true, werr
			}
			return true, nil
		}
		return true, errors.New("settings kept changing")
	}

	// Send the slash command through the standard afree:<digest> gate.
	perfWhat := "effort"
	if resModel != "" {
		perfWhat = "model"
	}
	_, err = s.runGated(ctx, sess, gatedReq{
		Gate:      "afree:" + digest,
		Mode:      sess.Persistence,
		Session:   sess.CurrentTmuxSession(),
		Pane:      pane,
		Socket:    socket,
		PID:       pid,
		Start:     start,
		SID:       sid,
		Text:      slashCmd,
		MaxOut:    sendOut,
		AwaitSlot: pressSlotWait("model"),
		What:      perfWhat,
	})
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
	case errors.Is(err, gw.ErrUnknownResult):
		if ctx.Err() == nil {
			if _, rerr := restore(); rerr != nil {
				defaultChanged.Store(sess.ID, time.Now())
				*outcome, *reason = "default_changed", "default_changed"
				writeError(w, http.StatusInternalServerError, "default_changed", "Antigravity's default model in settings.json changed and could not be restored")
				return
			}
		}
		*outcome, *reason = "unknown", "corrald_unknown_result"
		writeCorraldUnknownResult(w)
		return
	case errors.Is(err, sshx.ErrBusy):
		*reason = "ssh_channels_busy"
		writeSSHChannelsBusy(w)
		return
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		*reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 9:
		*reason = "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		return
	default:
		*reason = "send_failed"
		writeError(w, http.StatusBadGateway, "send_failed", "could not send command to Antigravity")
		return
	}

	// Poll the live status bar / confirmation line for up to ~3.5s.
	confirmed := false
	var failMsg string
	deadline := time.Now().Add(3500 * time.Millisecond)
poll:
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			break poll
		case <-time.After(200 * time.Millisecond):
		}
		pctx, pcancel := context.WithTimeout(ctx, 5*time.Second)
		// Fast 200ms confirmation poll inside the 3.5s deadline: intentionally
		// AwaitSlot: 0 so a busy connection skips this tick and retries on the
		// next iteration instead of burning 2s of the confirmation window.
		scrRes, serr := sshx.Exec(pctx, sess.Client(), agyScreenCommand(sess.Persistence, pane, socket), 512<<10, sshx.ExecOptions{Interactive: true})
		scr := scrRes.Out
		pcancel()
		if serr != nil {
			continue
		}
		ok, bad := check(string(scr))
		if bad != "" {
			failMsg = bad
			break
		}
		if ok {
			confirmed = true
			break
		}
	}

	restored, rerr := restore()
	if rerr != nil {
		defaultChanged.Store(sess.ID, time.Now())
		*outcome, *reason = "default_changed", "default_changed"
		writeError(w, http.StatusInternalServerError, "default_changed", "Antigravity's default model in settings.json changed and could not be restored")
		return
	}
	if failMsg != "" {
		*outcome, *reason = "refused", "unsupported"
		writeError(w, http.StatusConflict, "unsupported", failMsg)
		return
	}
	*outcome = "switched"
	resp := map[string]any{"ok": true}
	if resModel != "" {
		resp["model"] = resModel
	}
	if resEffort != "" {
		resp["effort"] = resEffort
	}
	if restored {
		resp["restored"] = true
	}
	if !confirmed {
		resp["unconfirmed"] = true
	}
	writeJSON(w, http.StatusOK, resp)
}
