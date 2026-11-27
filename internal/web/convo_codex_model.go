package web

import (
	"bytes"
	"context"
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

// Switching a Codex's model and reasoning effort from the agents page,
// through its own /model picker, for the session only
// (plans/5d3c-codex-model.md). Codex 0.164's picker has two steps: "Select
// Model and Effort" (a numbered list, Enter goes on to the next step), then
// "Select Reasoning Level for <model>", where Enter saves the choice as the
// default of every new session ("enter default") and "s" takes it for this
// session only — so Enter is never pressed there, only "s". No digit is
// pressed (a digit picks at once). Each step is read before the next;
// anything not as expected is left with Escape.

// codexPicker is one step of Codex's picker as its screen shows it.
type codexPicker struct {
	ok           bool
	effort       bool   // (the second step: "Select Reasoning Level for …")
	of           string // (the second step's model, as its head names it)
	options      []modelOpt
	above, below bool // rows outside the visible window
}

var (
	codexPickerHead   = regexp.MustCompile(`^\s*Select (?:Model and Effort|Reasoning Level for (.+?))\s*$`)
	codexPickerOption = regexp.MustCompile(`^\s*(›)?\s*(\d{1,2})\.\s+(\S.*?)\s*$`)
	codexPickerFoot   = regexp.MustCompile(`^\s*enter (?:select|default)\b.*\besc back\b`)
)

// parseCodexPicker reads the picker at the end of the screen: its head, its
// numbered choices (each label apart from its description: two spaces or
// more between them), one with the cursor (›), then its footer.
func parseCodexPicker(screen string) codexPicker {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	foot := -1
	for i := len(lines) - 1; i >= 0 && len(lines)-i <= 4; i-- {
		if codexPickerFoot.MatchString(lines[i]) {
			foot = i
			break
		}
	}
	if foot < 0 {
		return codexPicker{}
	}
	head := -1
	var p codexPicker
	for i := foot - 1; i >= 0 && foot-i <= 30; i-- {
		if m := codexPickerHead.FindStringSubmatch(lines[i]); m != nil {
			head, p.effort, p.of = i, strings.Contains(lines[i], "Reasoning Level"), m[1]
			break
		}
	}
	if head < 0 {
		return codexPicker{}
	}
	cursors := 0
	for _, l := range lines[head+1 : foot] {
		if strings.TrimSpace(l) == "" {
			continue
		}
		if arrow := strings.TrimSpace(l); arrow == "↑" || arrow == "↓" || arrow == "↕" {
			p.above = p.above || arrow == "↑" || arrow == "↕"
			p.below = p.below || arrow == "↓" || arrow == "↕"
			continue
		}
		m := codexPickerOption.FindStringSubmatch(l)
		if m == nil {
			// (A description wrapped in a narrow pane: indented under it, its words go on.)
			if n := len(p.options); n > 0 && strings.HasPrefix(l, "    ") {
				p.options[n-1].Desc = strings.TrimSpace(p.options[n-1].Desc + " " + strings.TrimSpace(l))
				continue
			}
			return codexPicker{} // (another line among its choices: not the picker as known)
		}
		o := codexOptions([]string{m[3]})[0]
		n := 0
		for _, c := range m[2] {
			n = n*10 + int(c-'0')
		}
		if n < 1 || (len(p.options) > 0 && n != p.options[len(p.options)-1].N+1) {
			return codexPicker{}
		}
		if !p.effort {
			o.label = strings.TrimSuffix(o.label, " (current)")
		}
		opt := modelOpt{N: n, Label: o.label, Desc: o.desc, at: m[1] != ""}
		if opt.at {
			cursors++
		}
		p.options = append(p.options, opt)
	}
	p.ok = len(p.options) > 0 && cursors == 1
	return p
}

// codexOptions splits a picker line's words into its label and description
// (as the chat page's card does: see web/src/terminal/menu.ts).
func codexOptions(labels []string) []struct{ label, desc string } {
	out := make([]struct{ label, desc string }, len(labels))
	for i, s := range labels {
		if j := strings.Index(s, "  "); j > 0 {
			out[i].label, out[i].desc = strings.TrimSpace(s[:j]), strings.TrimSpace(s[j:])
		} else {
			out[i].label = s
		}
	}
	return out
}

func (p codexPicker) cursor() (modelOpt, bool) {
	for _, o := range p.options {
		if o.at {
			return o, true
		}
	}
	return modelOpt{}, false
}

// codexEffortName: an effort choice's label without what it says of itself
// ("Medium (default)" is Medium).
func codexEffortName(label string) string {
	if i := strings.Index(label, " ("); i > 0 {
		return label[:i]
	}
	return label
}

// codexModelSet: how many times the screen says model with effort was
// taken for this session ("Model changed to gpt-6-sol high for this session
// only": the model's label in lower case, its effort's id).
func codexModelSet(screen, model, effort string) int {
	n := 0
	// (A narrow pane wraps it: its words read as one line.)
	for _, m := range codexModelSaid.FindAllStringSubmatch(strings.Join(strings.Fields(screen), " "), -1) {
		if strings.EqualFold(m[1], strings.ReplaceAll(model, " ", "-")) && strings.EqualFold(m[2], codexEffortID(effort)) {
			n++
		}
	}
	return n
}

var codexModelSaid = regexp.MustCompile(`Model changed to (\S+) (\S+) for this session only`)

// codexEffortID: an effort as Codex says it ("Extra high" is xhigh).
func codexEffortID(effort string) string {
	if strings.EqualFold(effort, "Extra high") {
		return "xhigh"
	}
	return strings.ToLower(effort)
}

// codexPickerOpen: a picker of Codex's is open — its footer at the end of
// the screen — whether the rest of it reads as known or not.
func codexPickerOpen(screen string) bool {
	lines := strings.Split(strings.TrimRight(screen, "\n"), "\n")
	for i := len(lines) - 1; i >= 0 && len(lines)-i <= 4; i-- {
		if codexPickerFoot.MatchString(lines[i]) {
			return true
		}
	}
	return false
}

const maxCodexPickerMove = 12

// codexIO: the screen read whole, with its digest (look); keys or a text
// pressed only while the screen is still the one of that digest (press:
// the host checks, exit 10 where it is not — a key typed in the terminal
// meanwhile, or anything drawn since).
type codexIO struct {
	look  func() (string, string, error)
	press func(keys []string, text, digest string) error
}

// settle: the screen read again, a few times a moment apart, till ok says
// it shows what a press was to bring (Codex drawing it a little after):
// the last read, as it is, if never.
func (io codexIO) settle(ok func(string) bool) (string, string, error) {
	var scr, d string
	var err error
	for i := 0; i < codexSettleTries; i++ {
		if i > 0 {
			time.Sleep(codexSettleGap)
		}
		if scr, d, err = io.look(); err != nil || ok(scr) {
			return scr, d, err
		}
	}
	return scr, d, nil
}

var (
	codexSettleTries = 4
	codexSettleGap   = 400 * time.Millisecond
)

// (Left as is — review501: the host checks the screen, then presses, a few
// milliseconds after; a picker closed in the terminal in between gets the
// Escape. One Escape on Codex's input only shows a hint ("esc again to
// edit previous message"); the flow never presses a second on a screen
// not read again.)

// codexModelFlow opens the picker; with model "" it lists the models and
// closes it, else it takes model with effort for this session. Each press
// is of the screen it was decided on (codexIO), its Escapes too: one only
// on its picker, read open. As modelFlow, it acts only on Codex at rest,
// its input empty (a picker of its left open is closed first); on a
// failure after the picker opened, it is left (undo: the same, in a while
// of its own, the request's own may be over).
func codexModelFlow(do, undo codexIO, model, effort string) (opts []modelOpt, err error) {
	screen, digest, err := do.look()
	if err != nil {
		return nil, err
	}
	for i := 0; i < 2 && codexPickerOpen(screen); i++ {
		if err := do.press([]string{"esc"}, "", digest); err != nil {
			return nil, err
		}
		if screen, digest, err = do.settle(func(s string) bool { return !codexPickerOpen(s) }); err != nil {
			return nil, err
		}
	}
	if codexPickerOpen(screen) {
		return nil, errScreen
	}
	if err := codexIdleInput(screen); err != nil {
		return nil, err
	}
	before := codexModelSet(screen, model, effort)
	// (Leaving: Escape on its picker read open, then read again till Codex
	// has drawn what it did — not a second Escape on a picker it was still
	// closing, which would land after it — review501.)
	out := func(e error) ([]modelOpt, error) {
		if errors.Is(e, gw.ErrUnknownResult) {
			return nil, e
		}
		scr, d, err := undo.look()
		for i := 0; i < 2 && err == nil && codexPickerOpen(scr); i++ {
			if undo.press([]string{"esc"}, "", d) != nil {
				break
			}
			scr, d, err = undo.settle(func(s string) bool { return !codexPickerOpen(s) })
		}
		return nil, e
	}
	err = do.press(nil, "/model", digest)
	if gateRefused(err) || screenRefused(err) {
		return nil, err // (nothing typed)
	}
	if err != nil {
		return out(err)
	}
	// (The picker as now read — once it shows what the press brought — and
	// its digest: what the next press is of.)
	read := func(ok func(codexPicker) bool) (codexPicker, error) {
		scr, d, err := do.settle(func(s string) bool { return ok(parseCodexPicker(s)) })
		if err != nil {
			return codexPicker{}, err
		}
		digest = d
		return parseCodexPicker(scr), nil
	}
	isModels := func(p codexPicker) bool { return p.ok && !p.effort }
	pk, err := read(isModels)
	if err != nil {
		return out(err)
	}
	if !isModels(pk) {
		return out(errNoPicker)
	}
	// A current CLI shows only a window of the list. Read every window
	// before offering models, verifying the cursor after each guarded move.
	// Returning a visible prefix would silently hide the remaining models.
	const maxCodexModels = 64
	moveCursor := func(p codexPicker, number int, same func(codexPicker) bool) (codexPicker, error) {
		for moved := 0; moved < maxCodexModels; {
			at, ok := p.cursor()
			if !ok || number < 1 || number > maxCodexModels {
				return p, errNoModel
			}
			d := number - at.N
			if d == 0 {
				return p, nil
			}
			key := "down"
			if d < 0 {
				key, d = "up", -d
			}
			d = min(d, maxCodexPickerMove)
			keys := make([]string, d)
			for i := range keys {
				keys[i] = key
			}
			if err := do.press(keys, "", digest); err != nil {
				return p, err
			}
			want := at.N + d
			if key == "up" {
				want = at.N - d
			}
			there := func(p codexPicker) bool { c, ok := p.cursor(); return p.ok && ok && c.N == want && same(p) }
			var err error
			if p, err = read(there); err != nil {
				return p, err
			}
			if !there(p) {
				return p, errNotThere
			}
			moved += d
		}
		return p, errNoModel
	}
	if pk.above || pk.options[0].N != 1 {
		if pk, err = moveCursor(pk, 1, isModels); err != nil {
			return out(err)
		}
	}
	var models []modelOpt
	for windows := 0; ; windows++ {
		foundTarget := false
		for _, opt := range pk.options {
			if opt.N > maxCodexModels || opt.N > len(models)+1 {
				return out(errNoPicker)
			}
			if opt.N <= len(models) {
				if models[opt.N-1].Label != opt.Label {
					return out(errScreen)
				}
			} else {
				models = append(models, opt)
			}
			if model != "" && strings.EqualFold(opt.Label, model) {
				foundTarget = true
			}
		}
		if foundTarget || !pk.below {
			break
		}
		if windows >= maxCodexModels || len(models) >= maxCodexModels {
			return out(errNoPicker)
		}
		if pk, err = moveCursor(pk, len(models)+1, isModels); err != nil {
			return out(err)
		}
	}
	if model == "" {
		if err := do.press([]string{"esc"}, "", digest); err != nil {
			return out(err)
		}
		if scr, _, err := do.settle(func(s string) bool { return !codexPickerOpen(s) }); err != nil || codexPickerOpen(scr) {
			return out(errScreen)
		}
		return models, nil
	}
	// (Moved to the choice match finds on picker p, read again: the cursor
	// there, on the step of the same model.)
	moveTo := func(p codexPicker, choices []modelOpt, match func(modelOpt) bool, same func(codexPicker) bool) (codexPicker, error) {
		var to modelOpt
		found := false
		for _, o := range choices {
			if match(o) {
				to, found = o, true
			}
		}
		_, known := p.cursor()
		if !found || !known {
			return p, errNoModel
		}
		p, err := moveCursor(p, to.N, same)
		if err != nil {
			return p, err
		}
		at, ok := p.cursor()
		if !ok || !match(at) {
			return p, errNotThere
		}
		return p, nil
	}
	if _, err := moveTo(pk, models, func(o modelOpt) bool { return strings.EqualFold(o.Label, model) }, isModels); err != nil {
		return out(err)
	}
	if err := do.press([]string{"yes"}, "", digest); err != nil {
		return out(err)
	}
	ofModel := func(p codexPicker) bool { return p.ok && p.effort && strings.EqualFold(p.of, model) }
	ef, err := read(ofModel)
	if err != nil {
		return out(err)
	}
	if !ofModel(ef) {
		return out(errNotTaken)
	}
	if _, err := moveTo(ef, ef.options, func(o modelOpt) bool { return strings.EqualFold(codexEffortName(o.Label), effort) }, ofModel); err != nil {
		return out(err)
	}
	if err := do.press([]string{"this"}, "", digest); err != nil { // ("s": this session only)
		return out(err)
	}
	scr, _, err := do.settle(func(s string) bool { return codexModelSet(s, model, effort) > before })
	switch {
	case err != nil:
		return out(err)
	case codexModelSet(scr, model, effort) > before:
		return []modelOpt{{Label: model, Desc: effort}}, nil
	case codexPickerOpen(scr):
		return out(errNotTaken)
	}
	return nil, errUnsure
}

// screenRefused: the press not made, the screen not the one it was of
// (exit 10), or not to be checked there (exit 11).
func screenRefused(err error) bool {
	var e *sshx.ExitError
	return errors.As(err, &e) && (e.Status == 10 || e.Status == 11)
}

type codexModelFront struct {
	hostID    int64
	via, auth string
	at        time.Time
}

type codexModelCacheEntry struct {
	opts []modelOpt
	at   time.Time
}

var (
	codexModelFronts    sync.Map // modelFrontKey → codexModelFront
	codexModelsCacheMu  sync.Mutex
	codexModelsCache    = map[string]codexModelCacheEntry{}
	codexRejectedDiskAt = map[string]time.Time{} // key → time of last switch failure; disk caches with fetched_at <= this are skipped
)

func pruneCodexCachesLocked(now time.Time) {
	for k, ent := range codexModelsCache {
		if now.Sub(ent.at) >= lineListFor {
			delete(codexModelsCache, k)
		}
	}
	for k, rej := range codexRejectedDiskAt {
		if now.Sub(rej) > codexDiskCacheMaxAge {
			delete(codexRejectedDiskAt, k)
		}
	}
}

func codexModelStoreKey(hostID int64, via, auth string) string {
	return "codex:" + strconv.FormatInt(hostID, 10) + "\x00" + via + "\x00" + auth
}

func noteCodexModelFront(key string, f codexModelFront) {
	codexModelFronts.Range(func(k, v any) bool {
		if f.at.Sub(v.(codexModelFront).at) > lineListFor {
			codexModelFronts.Delete(k)
		}
		return true
	})
	codexModelFronts.Store(key, f)
}

func (s *Server) noteCodexModels(ctx context.Context, hostID int64, via, auth string, opts []modelOpt) {
	s.noteCodexModelsAt(ctx, hostID, via, auth, opts, time.Now())
}

func (s *Server) noteCodexDiskModels(ctx context.Context, hostID int64, via, auth string, opts []modelOpt, fetchedAt time.Time) {
	s.noteCodexModelsAt(ctx, hostID, via, auth, opts, fetchedAt)
}

func (s *Server) noteCodexModelsAt(ctx context.Context, hostID int64, via, auth string, opts []modelOpt, at time.Time) {
	if len(opts) == 0 {
		return
	}
	clean := make([]modelOpt, len(opts))
	for i, o := range opts {
		o.Current = false
		clean[i] = o
	}
	key := codexModelStoreKey(hostID, via, auth)
	now := time.Now()
	codexModelsCacheMu.Lock()
	pruneCodexCachesLocked(now)
	codexModelsCache[key] = codexModelCacheEntry{opts: clean, at: at}
	codexModelsCacheMu.Unlock()
	if s.store != nil {
		if raw, err := json.Marshal(clean); err == nil {
			_ = s.store.PutModelList(context.WithoutCancel(ctx), key, at.Unix(), raw)
		}
	}
}

func (s *Server) forgetCodexModels(ctx context.Context, hostID int64, via, auth string) {
	key := codexModelStoreKey(hostID, via, auth)
	now := time.Now()
	codexModelsCacheMu.Lock()
	pruneCodexCachesLocked(now)
	delete(codexModelsCache, key)
	codexRejectedDiskAt[key] = now
	codexModelsCacheMu.Unlock()
	if s.store != nil {
		_ = s.store.DeleteModelList(context.WithoutCancel(ctx), key)
	}
}

func isCodexDiskCacheRejected(hostID int64, via, auth string, fetchedAt time.Time) bool {
	key := codexModelStoreKey(hostID, via, auth)
	codexModelsCacheMu.Lock()
	defer codexModelsCacheMu.Unlock()
	rej, ok := codexRejectedDiskAt[key]
	if !ok {
		return false
	}
	if time.Since(rej) > codexDiskCacheMaxAge {
		delete(codexRejectedDiskAt, key)
		return false
	}
	return !fetchedAt.After(rej)
}

const (
	codexModelsCacheMax  = 4<<20 + 64
	codexDiskCacheMaxAge = lineListFor // 12h: matches server model-list TTL; older disk caches fall back to live TUI
)

// codexModelsCacheCommand reads Codex's own models_cache.json from the
// process's CODEX_HOME (falling back to $CODEX_HOME or $HOME/.codex). When
// python3 is present, it verifies the file size first and strips the large
// per-model instructions_template fields on the host so only a few kilobytes
// cross SSH. Exits 3 when absent, oversized, or CODEX_HOME contains a newline.
func codexModelsCacheCommand(pid int64) string {
	script := `h="${CODEX_HOME:-$HOME/.codex}"
if [ -n "$1" ] && [ "$1" -gt 0 ] 2>/dev/null && [ -r "/proc/$1/environ" ]; then
  eh=$(tr '\0\n' '\n\001' <"/proc/$1/environ" 2>/dev/null | sed -n 's/^CODEX_HOME=//p' | head -n 1)
  s1=$(printf '\001')
  case "$eh" in *"$s1"*) exit 3 ;; esac
  [ -n "$eh" ] && h=$eh
fi
f="$h/models_cache.json"
[ -f "$f" ] || exit 3
printf 'now=%s\n' "$(date +%s)"
if command -v python3 >/dev/null 2>&1; then
  sz=$(wc -c <"$f" 2>/dev/null | tr -d '[:space:]') || exit 3
  [ -n "$sz" ] && [ "$sz" -gt 0 ] && [ "$sz" -le 4194304 ] 2>/dev/null || exit 3
  python3 - "$f" <<'PY'
import json, sys
with open(sys.argv[1], "r", encoding="utf-8") as fh:
    raw = fh.read(4194305)
if len(raw) > 4194304:
    sys.exit(3)
data = json.loads(raw)
models = []
for m in data.get("models") or []:
    if not isinstance(m, dict):
        continue
    models.append({
        "slug": m.get("slug") or "",
        "display_name": m.get("display_name") or "",
        "description": m.get("description") or "",
        "visibility": m.get("visibility") or "",
        "priority": m.get("priority", 0),
        "supported_in_api": m.get("supported_in_api"),
    })
json.dump({
    "fetched_at": data.get("fetched_at") or "",
    "client_version": data.get("client_version") or "",
    "models": models,
}, sys.stdout, separators=(",", ":"))
PY
else
  head -c 4194305 "$f"
fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(strconv.FormatInt(pid, 10))
}

// parseCodexModelsCache extracts visible Codex picker models from
// models_cache.json, ordered stably by ascending priority (matching Codex CLI).
// Rejects caches that lack a fresh fetched_at timestamp or contain models with
// supported_in_api=false (which Codex filters differently under API-key login).
func parseCodexModelsCache(raw []byte, now time.Time) ([]modelOpt, time.Time, bool) {
	var doc struct {
		FetchedAt     string `json:"fetched_at"`
		ClientVersion string `json:"client_version"`
		Models        []struct {
			Slug           string `json:"slug"`
			DisplayName    string `json:"display_name"`
			Description    string `json:"description"`
			Visibility     string `json:"visibility"`
			Priority       int    `json:"priority"`
			SupportedInAPI *bool  `json:"supported_in_api"`
		} `json:"models"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &doc) != nil || len(doc.Models) == 0 {
		return nil, time.Time{}, false
	}
	fetchedStr := strings.TrimSpace(doc.FetchedAt)
	if fetchedStr == "" {
		return nil, time.Time{}, false
	}
	fetchedAt, err := time.Parse(time.RFC3339Nano, fetchedStr)
	if err != nil {
		fetchedAt, err = time.Parse(time.RFC3339, fetchedStr)
		if err != nil {
			return nil, time.Time{}, false
		}
	}
	if now.Sub(fetchedAt) > codexDiskCacheMaxAge || fetchedAt.Sub(now) > 5*time.Minute {
		return nil, time.Time{}, false
	}
	type item struct {
		opt      modelOpt
		priority int
	}
	var items []item
	for _, m := range doc.Models {
		if m.SupportedInAPI != nil && !*m.SupportedInAPI {
			// API-key login filters out supported_in_api=false models; without
			// reading auth.json we cannot know the login mode, so fall back to TUI.
			return nil, time.Time{}, false
		}
		vis := strings.TrimSpace(m.Visibility)
		if vis != "" && !strings.EqualFold(vis, "list") {
			continue
		}
		slug := strings.TrimSpace(m.Slug)
		label := strings.TrimSpace(m.DisplayName)
		if label == "" {
			label = slug
		}
		if label == "" {
			continue
		}
		items = append(items, item{
			opt:      modelOpt{Label: label, Slug: slug, Desc: strings.TrimSpace(m.Description)},
			priority: m.Priority,
		})
	}
	if len(items) == 0 {
		return nil, time.Time{}, false
	}
	slices.SortStableFunc(items, func(a, b item) int {
		switch {
		case a.priority < b.priority:
			return -1
		case a.priority > b.priority:
			return 1
		default:
			return 0
		}
	})
	seen := make(map[string]struct{}, len(items))
	out := make([]modelOpt, 0, len(items))
	for _, it := range items {
		k := strings.ToLower(it.opt.Label)
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		out = append(out, it.opt)
	}
	return out, fetchedAt, true
}

// readCodexModelsCache reads Codex's on-disk models_cache.json over SSH in one
// fast call (avoiding driving the interactive /model TUI page-by-page).
func (s *Server) readCodexModelsCache(ctx context.Context, sess *term.Session, pid int64) ([]modelOpt, time.Time, bool) {
	cctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	before := time.Now()
	res, err := sshx.Exec(cctx, sess.Client(), codexModelsCacheCommand(pid), codexModelsCacheMax, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
	out := res.Out
	if err != nil || len(out) > 4<<20 {
		return nil, time.Time{}, false
	}
	return parseCodexModelsCacheAt(out, before)
}

// parseCodexModelsCacheAt reads the command's output: its host's clock first
// ("now=<unix seconds>"), then the cache. fetched_at is written by that host's
// clock, so its age is judged by that clock, and it is told in the gateway's
// to be held against a switch's failure time, which is the gateway's (review
// E, 11-24). gatewayBefore is the gateway's clock taken before the command
// ran. The host's line is whole seconds and the command takes a while, so the
// two clocks' difference is known only to a range; the shift taken is the
// range's earliest (the host's second just ending as it printed, the command
// ending at once), so a list read before a failure is never moved after it —
// at worst one read just after is refused, and the live list read instead
// (review F, G). Without that line, fetched_at as written.
func parseCodexModelsCacheAt(out []byte, gatewayBefore time.Time) ([]modelOpt, time.Time, bool) {
	rest, ok := bytes.CutPrefix(out, []byte("now="))
	if !ok {
		return parseCodexModelsCache(out, gatewayBefore)
	}
	line, body, found := bytes.Cut(rest, []byte("\n"))
	if !found {
		return nil, time.Time{}, false
	}
	sec, err := strconv.ParseInt(strings.TrimSpace(string(line)), 10, 64)
	if err != nil || sec <= 0 {
		return nil, time.Time{}, false
	}
	hostNow := time.Unix(sec, 0)
	opts, fetchedAt, ok := parseCodexModelsCache(body, hostNow)
	if !ok {
		return nil, time.Time{}, false
	}
	return opts, fetchedAt.Add(gatewayBefore.Sub(hostNow) - time.Second), true
}

// keptCodexModels serves a previously read Codex model list immediately
// without driving Codex's interactive /model picker or running locateScan.
func (s *Server) keptCodexModels(w http.ResponseWriter, ctx context.Context, sess *term.Session, pane string, pid, start int64, sid string, outcome, reason *string) bool {
	fk := modelFrontKey(sess.ID, pane, pid, start, sid)
	v, ok := codexModelFronts.Load(fk)
	if !ok {
		return false
	}
	f := v.(codexModelFront)
	if time.Since(f.at) > lineListFor {
		codexModelFronts.Delete(fk)
		return false
	}
	return s.keptCodexModelsFor(w, ctx, f.hostID, f.via, f.auth, outcome, reason)
}

// keptCodexModelsFor serves a cached Codex model list for (hostID, via, auth)
// from memory or SQLite.
func (s *Server) keptCodexModelsFor(w http.ResponseWriter, ctx context.Context, hostID int64, via, auth string, outcome, reason *string) bool {
	key := codexModelStoreKey(hostID, via, auth)
	codexModelsCacheMu.Lock()
	ent, hit := codexModelsCache[key]
	codexModelsCacheMu.Unlock()
	var opts []modelOpt
	if hit && time.Since(ent.at) < lineListFor && len(ent.opts) > 0 {
		opts = slices.Clone(ent.opts)
	} else if s.store != nil {
		atUnix, raw, found, err := s.store.ModelList(ctx, key)
		if err == nil && found && time.Since(time.Unix(atUnix, 0)) < lineListFor {
			var parsed []modelOpt
			if json.Unmarshal(raw, &parsed) == nil && len(parsed) > 0 {
				opts = parsed
				codexModelsCacheMu.Lock()
				codexModelsCache[key] = codexModelCacheEntry{opts: slices.Clone(parsed), at: time.Unix(atUnix, 0)}
				codexModelsCacheMu.Unlock()
			}
		}
	}
	if len(opts) == 0 {
		return false
	}
	for i := range opts {
		opts[i].Current = false
	}
	*outcome, *reason = "kept", "list_kept"
	writeJSON(w, http.StatusOK, map[string]any{"models": opts, "kept": true})
	return true
}
