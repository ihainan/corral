package web

import (
	"context"
	"encoding/json"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// Antigravity CLI (agy) does not write its conversation topic into the terminal
// pane title (herdr sees only "agy", blank, or the working folder). Without a
// backend title lookup, every Corral restart or page reload shows the working
// directory ("AI") until the user clicks into the conversation, and even then
// only the raw first prompt was shown.
//
// agyTitler mirrors codexTitler: it scans bare agy panes on the host, reads
// their active conversation_summaries.db entry and recent USER_INPUT prompts
// from transcript.jsonl, and produces a concise subtitle:
//   - When the user has thinking-translation LLM configured and enabled, it
//     summarizes the current topic into a short title in cfg.TargetLang and
//     caches it in SQLite (surviving Corral restarts with 0ms cache hits).
//   - Otherwise (or while the LLM call is in flight / on error), it falls back
//     immediately to the latest meaningful user prompt (skipping image paths
//     and trivial acks like "继续"/"确认"/"可以"), then AGY's native title.

const (
	agyTitleEvery       = 2 * time.Minute
	agyTitleSoon        = 2 * time.Second
	agyTitleNudge       = 4 * time.Second
	agyTitleRetryMin    = 3 * time.Second
	agyTitleRetryMax    = 30 * time.Second
	agyTitleScanMax     = 128 << 10
	agyTitleRunes       = 60
	agyTitleFailBackoff = 5 * time.Minute
	agyTitleLLMTimeout  = 15 * time.Second
)

var (
	agyLaunchTitle  = regexp.MustCompile(`^(?i)(?:(?:agy|antigravity)(?:\s+-\S+(?:\s+[^-\s]\S*)?)*|aswap\s+run\b.*)$`)
	reUserReqBlock  = regexp.MustCompile(`(?s)<USER_REQUEST>\s*(.*?)\s*</USER_REQUEST>`)
	reMetaBlock     = regexp.MustCompile(`(?s)<(?:ADDITIONAL_METADATA|CONTEXT_SUMMARY|SYSTEM_INSTRUCTION|USER_SETTINGS_CHANGE)>.*?</(?:ADDITIONAL_METADATA|CONTEXT_SUMMARY|SYSTEM_INSTRUCTION|USER_SETTINGS_CHANGE)>`)
	reUploadPath    = regexp.MustCompile(`(?:file://)?(?:/[^\s"'<>]+)*/\.cache/corral/uploads/[^\s"'<>]+`)
	reAgyImagePlace = regexp.MustCompile(`\[(?:Image #\d+|Attachment:[^\]]*)\]`)
	reTitlePrefix   = regexp.MustCompile(`^(?i)(?:title|标题|主题)\s*[:：]\s*`)
)

// agyTitleBare reports whether an agy pane's terminal title carries no useful
// session name (empty, launch command, "Antigravity", or just the folder name).
func agyTitleBare(agent, title, cwd string) bool {
	if !isAgyAgent(agent) {
		return false
	}
	t := strings.TrimLeftFunc(title, func(r rune) bool {
		return unicode.IsSpace(r) || r >= 0x2800 && r <= 0x28ff || strings.ContainsRune("·✢✳✶✻✽*◐◑◒◓○●◌", r)
	})
	t = strings.TrimSpace(t)
	if t == "" || strings.EqualFold(t, "antigravity") || agyLaunchTitle.MatchString(t) {
		return true
	}
	project := ""
	if p := strings.TrimRight(cwd, "/"); p != "" && !homeDir.MatchString(p) {
		project = path.Base(p)
	}
	if project != "" {
		if t == project {
			return true
		}
		for _, cut := range []string{"...", "…"} {
			if head, ok := strings.CutSuffix(t, cut); ok && head != "" && strings.HasPrefix(project, head) {
				return true
			}
		}
	}
	return false
}

type agyPaneTitleInfo struct {
	Pane         string   `json:"pane"`
	SID          string   `json:"sid"`
	SummaryTitle string   `json:"summary_title"`
	Preview      string   `json:"preview"`
	First        string   `json:"first"`
	Recent       []string `json:"recent"`
}

func agyTitlesCommand(socket string) string {
	return `command -v python3 >/dev/null 2>&1 || exit 0
exec python3 -I -c ` + sshx.QuoteShell(agyTitlesScript) + ` ` + sshx.QuoteShell(socket)
}

const agyTitlesScript = `import json, os, re, sqlite3, sys, urllib.parse

sock = sys.argv[1]
uid = os.geteuid()
pane_re = re.compile(r'^w[0-9A-Za-z]+:p\d+$')
lock_re = re.compile(r'^(.*)/presence/([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})\.lock$')
noninter_flag_arg = {
    '-i', '--prompt-interactive', '--add-dir', '--agent', '--conversation',
    '--effort', '--input-format', '--json-schema', '--log-file', '--mode',
    '--model', '--output-format', '--print-timeout', '--project', '--settings',
}
noninter_sub = {
    'acp', 'agent', 'agents', 'changelog', 'help', 'install', 'mcp',
    'mic-serve', 'models', 'plugin', 'plugins', 'remote-control', 'update',
}

def is_noninter(argv):
    skip = False
    for a in argv[1:]:
        if skip:
            skip = False
            continue
        if a == '--':
            return False
        if a in ('-p', '--print', '--prompt') or a.startswith('--print=') or a.startswith('--prompt='):
            return True
        if a in noninter_flag_arg:
            skip = True
            continue
        if a.startswith('-'):
            continue
        return a in noninter_sub
    return False

def is_fg(pid):
    try:
        s = open(f'/proc/{pid}/stat', 'rb').read().decode('utf-8', 'replace')
        rp = s.rfind(')')
        if rp < 0:
            return False
        parts = s[rp+2:].split()
        return len(parts) > 5 and parts[2] == parts[5]
    except Exception:
        return False

by_pane = {}
for name in os.listdir('/proc'):
    if not name.isdigit():
        continue
    pid = int(name)
    pdir = '/proc/' + name
    try:
        st = os.stat(pdir)
        if st.st_uid != uid:
            continue
        comm = open(pdir + '/comm', 'rb').read().decode('utf-8', 'replace').strip()
        if comm not in ('agy', 'antigravity'):
            continue
        raw_cmd = open(pdir + '/cmdline', 'rb').read()
        argv = [x.decode('utf-8', 'replace') for x in raw_cmd.split(b'\x00') if x]
        if not argv or is_noninter(argv):
            continue
        raw_env = open(pdir + '/environ', 'rb').read()
    except Exception:
        continue
    env_sock = ''
    env_pane = ''
    for item in raw_env.split(b'\x00'):
        if item.startswith(b'HERDR_SOCKET_PATH='):
            env_sock = item[18:].decode('utf-8', 'replace')
        elif item.startswith(b'HERDR_PANE_ID='):
            env_pane = item[14:].decode('utf-8', 'replace')
    if env_sock != sock or not pane_re.match(env_pane):
        continue
    sid = ''
    base = ''
    try:
        for fd in os.listdir(pdir + '/fd'):
            try:
                target = os.readlink(pdir + '/fd/' + fd)
            except Exception:
                continue
            m = lock_re.match(target)
            if m:
                base = m.group(1)
                sid = m.group(2).lower()
                break
    except Exception:
        continue
    if not sid:
        continue
    fg = is_fg(pid)
    prev = by_pane.get(env_pane)
    if prev is None or (fg and not prev[0]) or (fg == prev[0] and pid > prev[1]):
        by_pane[env_pane] = (fg, pid, sid, base)

db_cache = {}
def read_summary(base, sid):
    if base not in db_cache:
        db_path = os.path.join(base, 'conversation_summaries.db')
        con = None
        if os.path.isfile(db_path):
            try:
                con = sqlite3.connect('file:' + urllib.parse.quote(db_path) + '?mode=ro', uri=True, timeout=2)
            except Exception:
                con = None
        db_cache[base] = con
    con = db_cache[base]
    if con is None:
        return '', ''
    try:
        row = con.execute('SELECT title, preview FROM conversation_summaries WHERE conversation_id = ?', (sid,)).fetchone()
        if row:
            return (row[0] or '')[:200], (row[1] or '')[:200]
    except Exception:
        pass
    return '', ''

def extract_user_inputs(base, sid):
    logs_dir = os.path.join(base, 'brain', sid, '.system_generated', 'logs')
    path = os.path.join(logs_dir, 'transcript.jsonl')
    if not os.path.isfile(path):
        path = os.path.join(logs_dir, 'transcript_full.jsonl')
    if not os.path.isfile(path):
        return '', []
    first = ''
    recent = []
    max_tail = 8 * 1024 * 1024
    try:
        sz = os.path.getsize(path)
        with open(path, 'rb') as f:
            if sz > max_tail:
                head = f.read(32768)
                for line in head.splitlines():
                    if b'"USER_INPUT"' in line:
                        try:
                            o = json.loads(line)
                            if o.get('type') == 'USER_INPUT' and isinstance(o.get('content'), str):
                                first = o['content'][:400]
                                break
                        except Exception:
                            pass
                f.seek(sz - max_tail)
                f.readline()  # skip partial line
            for line in f:
                if b'"USER_INPUT"' not in line:
                    continue
                try:
                    o = json.loads(line)
                except Exception:
                    continue
                if o.get('type') == 'USER_INPUT' and isinstance(o.get('content'), str):
                    txt = o['content'][:400]
                    if not first:
                        first = txt
                    recent.append(txt)
                    if len(recent) > 16:
                        recent.pop(0)
    except Exception:
        pass
    return first, recent

out = []
for pane, (_, _, sid, base) in by_pane.items():
    stitle, preview = read_summary(base, sid)
    first, recent = extract_user_inputs(base, sid)
    out.append({
        'pane': pane,
        'sid': sid,
        'summary_title': stitle,
        'preview': preview,
        'first': first,
        'recent': recent,
    })

for con in db_cache.values():
    if con is not None:
        try:
            con.close()
        except Exception:
            pass

sys.stdout.write(json.dumps(out))
`

func parseAgyTitles(raw []byte) map[string]agyPaneTitleInfo {
	var rows []agyPaneTitleInfo
	if json.Unmarshal(raw, &rows) != nil {
		return nil
	}
	out := make(map[string]agyPaneTitleInfo, len(rows))
	for _, r := range rows {
		if !herdrPane.MatchString(r.Pane) || !sessionUUID.MatchString(r.SID) {
			continue
		}
		out[r.Pane] = r
	}
	return out
}

// cleanAgyPrompt extracts the human-written prompt from an AGY USER_INPUT step,
// stripping XML wrapper tags, Corral upload paths, and collapsing whitespace.
func cleanAgyPrompt(raw string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	if m := reUserReqBlock.FindStringSubmatch(s); len(m) == 2 {
		s = m[1]
	} else {
		s = reMetaBlock.ReplaceAllString(s, " ")
		s = strings.ReplaceAll(s, "<USER_REQUEST>", " ")
		s = strings.ReplaceAll(s, "</USER_REQUEST>", " ")
	}
	s = reUploadPath.ReplaceAllString(s, " ")
	s = reAgyImagePlace.ReplaceAllString(s, " ")
	s = strings.Join(strings.FieldsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	}), " ")
	return strings.TrimSpace(s)
}

var trivialAgyPrompts = map[string]bool{
	"继续": true, "请继续": true, "继续吧": true,
	"确认": true, "确认执行": true, "同意": true,
	"可以": true, "好的": true, "好": true, "行": true, "是的": true, "对": true, "嗯": true,
	"谢谢": true, "没问题": true, "开始": true, "执行": true, "下一步": true, "好的继续": true,
	"ok": true, "okay": true, "yes": true, "y": true, "no": true, "n": true,
	"sure": true, "go": true, "go ahead": true, "continue": true, "proceed": true,
	"confirm": true, "confirmed": true, "approved": true, "lgtm": true,
	"thanks": true, "thank you": true, "done": true, "next": true,
}

func isTrivialAgyPrompt(s string) bool {
	t := strings.TrimSpace(s)
	if t == "" {
		return true
	}
	t = strings.TrimRightFunc(t, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) || unicode.IsSymbol(r)
	})
	t = strings.TrimSpace(t)
	if t == "" {
		return true
	}
	if trivialAgyPrompts[strings.ToLower(t)] {
		return true
	}
	// Pure digits/symbols (e.g. selecting option "1" or "2") are not topic titles.
	hasLetter := false
	for _, r := range t {
		if unicode.IsLetter(r) {
			hasLetter = true
			break
		}
	}
	return !hasLetter
}

// meaningfulAgyPrompts returns up to the last 3 non-trivial cleaned prompts
// (chronological order), falling back to the cleaned first prompt if needed.
func meaningfulAgyPrompts(recent []string, first string) []string {
	var all []string
	for _, r := range recent {
		c := cleanAgyPrompt(r)
		if isTrivialAgyPrompt(c) {
			continue
		}
		if len(all) > 0 && all[len(all)-1] == c {
			continue
		}
		all = append(all, c)
	}
	if len(all) > 3 {
		all = all[len(all)-3:]
	}
	if len(all) == 0 {
		if c := cleanAgyPrompt(first); c != "" && !isTrivialAgyPrompt(c) {
			all = append(all, c)
		}
	}
	return all
}

func clipAgyRunes(s string, maxR int) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) <= maxR {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:maxR-1])) + "…"
}

// agyFallbackTitle picks a readable subtitle without an LLM: the latest
// meaningful user prompt first, then AGY's native summary title, then preview/first.
func agyFallbackTitle(info agyPaneTitleInfo) string {
	if prompts := meaningfulAgyPrompts(info.Recent, info.First); len(prompts) > 0 {
		return clipAgyRunes(prompts[len(prompts)-1], agyTitleRunes)
	}
	for _, cand := range []string{info.SummaryTitle, info.Preview, info.First} {
		if c := cleanAgyPrompt(cand); c != "" {
			return clipAgyRunes(c, agyTitleRunes)
		}
	}
	return ""
}

// agyTitleDigest uniquely identifies the topic inputs of an AGY conversation.
// Trivial replies ("继续"/"确认"/"可以") do not alter the digest, preventing
// redundant LLM calls and keeping the cached title stable.
func agyTitleDigest(info agyPaneTitleInfo) string {
	prompts := meaningfulAgyPrompts(info.Recent, info.First)
	var b strings.Builder
	b.WriteString(info.SID)
	b.WriteByte('\x00')
	b.WriteString(cleanAgyPrompt(info.SummaryTitle))
	for _, p := range prompts {
		b.WriteByte('\x00')
		b.WriteString(clipAgyRunes(p, 240))
	}
	if len(prompts) == 0 {
		b.WriteByte('\x00')
		b.WriteString(cleanAgyPrompt(info.Preview))
	}
	return translateHash(b.String())
}

func agyTitleSystemPrompt(targetLang string) string {
	name, ok := translateLangNames[targetLang]
	if !ok {
		name = "Simplified Chinese"
	}
	return "Generate a very concise coding session subtitle in " + name + ".\n" +
		"Rules:\n" +
		"- Focus primarily on the most recent user request(s); use the native session title only as background context.\n" +
		"- Keep it very short and scannable (6 to 16 Chinese characters, or 3 to 6 words for alphabetic languages).\n" +
		"- Common technical terms (k8s, nginx, API, UI, SSH, AGY, Corral, Codex, SQLite, etc.) may stay in English.\n" +
		"- Output ONLY the plain title text: no quotes, no prefixes, no markdown, no bullet points, no trailing punctuation."
}

func agyTitleUserPrompt(info agyPaneTitleInfo) string {
	var b strings.Builder
	if st := cleanAgyPrompt(info.SummaryTitle); st != "" {
		b.WriteString("Session native title: ")
		b.WriteString(clipAgyRunes(st, 120))
		b.WriteByte('\n')
	}
	prompts := meaningfulAgyPrompts(info.Recent, info.First)
	if len(prompts) > 0 {
		b.WriteString("Recent user requests (oldest to newest):\n")
		for _, p := range prompts {
			b.WriteString("- ")
			b.WriteString(clipAgyRunes(p, 240))
			b.WriteByte('\n')
		}
	} else if pv := cleanAgyPrompt(info.Preview); pv != "" {
		b.WriteString("Preview: ")
		b.WriteString(clipAgyRunes(pv, 240))
		b.WriteByte('\n')
	}
	return strings.TrimSpace(b.String())
}

func cleanAgyLLMTitle(raw string) string {
	for _, line := range strings.Split(raw, "\n") {
		s := strings.TrimSpace(line)
		if s == "" {
			continue
		}
		s = reTitlePrefix.ReplaceAllString(s, "")
		s = strings.Trim(s, "\"'“”‘’`《》「」【】")
		s = strings.TrimRight(s, "。.,!！?？;；:：")
		s = strings.Join(strings.Fields(s), " ")
		if s != "" {
			return clipAgyRunes(s, agyTitleRunes)
		}
	}
	return ""
}

// agyTitler periodically scans bare AGY panes on a herdr server and decorates
// their herdrAgent.ThreadTitle with a concise cached title.
type agyTitler struct {
	mu        sync.Mutex
	want      map[string]bool
	found     map[string]string // pane -> resolved subtitle
	paneSID   map[string]string // pane -> last seen conversation UUID
	digest    map[string]string // pane -> last resolved topic digest
	inFlight  map[string]bool   // pane -> async LLM summarization running for digest
	failAt    map[string]time.Time
	soon      bool
	nudged    bool
	retryWait time.Duration
	wake      chan struct{}
	poke      func()
	scan      func(ctx context.Context) (map[string]agyPaneTitleInfo, error)
	resolve   func(ctx context.Context, pane string, info agyPaneTitleInfo) (title string, needLLM bool)
	genLLM    func(ctx context.Context, pane string, info agyPaneTitleInfo) (string, error)
	seedBySID func(ctx context.Context, sids []string) map[string]string
	saveBySID func(ctx context.Context, sid, title string)
	every     time.Duration
}

func newAgyTitler(
	scan func(context.Context) (map[string]agyPaneTitleInfo, error),
	resolve func(context.Context, string, agyPaneTitleInfo) (string, bool),
	genLLM func(context.Context, string, agyPaneTitleInfo) (string, error),
) *agyTitler {
	return &agyTitler{
		want:     map[string]bool{},
		found:    map[string]string{},
		paneSID:  map[string]string{},
		digest:   map[string]string{},
		inFlight: map[string]bool{},
		failAt:   map[string]time.Time{},
		wake:     make(chan struct{}, 1),
		scan:     scan,
		resolve:  resolve,
		genLLM:   genLLM,
		every:    agyTitleEvery,
	}
}

func (s *Server) newServerAgyTitler(sess *term.Session, socket string) *agyTitler {
	userID := sess.OwnerID
	at := newAgyTitler(
		func(ctx context.Context) (map[string]agyPaneTitleInfo, error) {
			res, err := sshx.Exec(ctx, sess.Client(), agyTitlesCommand(socket), agyTitleScanMax, sshx.ExecOptions{AwaitSlot: 12 * time.Second})
			if err != nil {
				return nil, err
			}
			return parseAgyTitles(res.Out), nil
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, bool) {
			return s.resolveAgyTitleSync(ctx, userID, info)
		},
		func(ctx context.Context, _ string, info agyPaneTitleInfo) (string, error) {
			return s.generateAgyTitleLLM(ctx, userID, info)
		},
	)
	if userID > 0 && s.store != nil {
		at.seedBySID = func(ctx context.Context, sids []string) map[string]string {
			keys := make([]string, 0, len(sids))
			for _, sid := range sids {
				keys = append(keys, "agy-sid:"+strings.ToLower(sid))
			}
			cached, err := s.store.CachedThoughtTranslations(ctx, userID, "_", keys)
			if err != nil || len(cached) == 0 {
				return nil
			}
			out := make(map[string]string, len(cached))
			for k, v := range cached {
				if sid, ok := strings.CutPrefix(k, "agy-sid:"); ok && strings.TrimSpace(v) != "" {
					out[sid] = strings.TrimSpace(v)
				}
			}
			return out
		}
		at.saveBySID = func(ctx context.Context, sid, title string) {
			sid = strings.ToLower(strings.TrimSpace(sid))
			title = strings.TrimSpace(title)
			if sid == "" || title == "" {
				return
			}
			_ = s.store.PutThoughtTranslation(ctx, userID, "agy-sid:"+sid, "_", title)
		}
	}
	return at
}

// resolveAgyTitleSync checks the SQLite cache if the user's LLM config is
// enabled; on hit it returns (cachedTitle, false). On miss with LLM enabled it
// returns (fallback, true) so the caller can trigger async LLM generation.
func (s *Server) resolveAgyTitleSync(ctx context.Context, userID int64, info agyPaneTitleInfo) (string, bool) {
	fb := agyFallbackTitle(info)
	if fb == "" || userID <= 0 || s.store == nil || s.vault == nil {
		return fb, false
	}
	cfg, err := s.store.TranslateConfigOf(ctx, userID)
	if err != nil || !cfg.Enabled || cfg.BaseURL == "" || cfg.Model == "" || len(cfg.Ciphertext) == 0 {
		return fb, false
	}
	cacheKey := "agy-title:" + agyTitleDigest(info)
	if cached, err := s.store.CachedThoughtTranslations(ctx, userID, cfg.TargetLang, []string{cacheKey}); err == nil {
		if t := strings.TrimSpace(cached[cacheKey]); t != "" {
			return t, false
		}
	}
	return fb, true
}

// generateAgyTitleLLM calls the user's configured translation LLM to summarize
// the AGY conversation topic and stores the result in SQLite.
func (s *Server) generateAgyTitleLLM(ctx context.Context, userID int64, info agyPaneTitleInfo) (string, error) {
	cfg, err := s.store.TranslateConfigOf(ctx, userID)
	if err != nil || !cfg.Enabled || cfg.BaseURL == "" || cfg.Model == "" || len(cfg.Ciphertext) == 0 {
		return "", err
	}
	prompt := agyTitleUserPrompt(info)
	if prompt == "" {
		return "", nil
	}
	digest := agyTitleDigest(info)
	cacheKey := "agy-title:" + digest
	if cached, err := s.store.CachedThoughtTranslations(ctx, userID, cfg.TargetLang, []string{cacheKey}); err == nil {
		if t := strings.TrimSpace(cached[cacheKey]); t != "" {
			return t, nil
		}
	}
	apiKey, err := s.openTranslateKey(cfg)
	if err != nil {
		return "", err
	}
	flightKey := strconv.FormatInt(userID, 10) + ":" + cfg.TargetLang + ":" + cacheKey
	out, err := s.translateFlight.do(flightKey, func() (string, error) {
		sem := s.translateFlight.sem(userID)
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		case <-ctx.Done():
			return "", ctx.Err()
		}
		lctx, cancel := context.WithTimeout(ctx, agyTitleLLMTimeout)
		defer cancel()
		raw, err := s.callChatLLM(lctx, cfg.BaseURL, cfg.Model, apiKey, agyTitleSystemPrompt(cfg.TargetLang), prompt, 0.2)
		if err != nil {
			return "", err
		}
		title := cleanAgyLLMTitle(raw)
		if title == "" {
			return "", nil
		}
		_ = s.store.PutThoughtTranslation(lctx, userID, cacheKey, cfg.TargetLang, title)
		return title, nil
	})
	return out, err
}

func (a *agyTitler) setPoke(f func()) {
	a.mu.Lock()
	a.poke = f
	a.mu.Unlock()
}

func (a *agyTitler) track(sum *herdrSummary) {
	a.mu.Lock()
	want := map[string]bool{}
	var needSeed []struct{ pane, sid string }
	for _, w := range sum.Workspaces {
		for _, ag := range w.Agents {
			if ag.BareTitle && isAgyAgent(ag.Agent) {
				want[ag.PaneID] = true
				if !a.want[ag.PaneID] {
					a.soon = true
				}
				if a.found[ag.PaneID] == "" && ag.Session != "" && a.seedBySID != nil {
					needSeed = append(needSeed, struct{ pane, sid string }{ag.PaneID, strings.ToLower(ag.Session)})
				}
			}
		}
	}
	for p := range a.found {
		if !want[p] {
			delete(a.found, p)
			delete(a.paneSID, p)
			delete(a.digest, p)
			delete(a.inFlight, p)
		}
	}
	a.want = want
	seedFn := a.seedBySID
	soon := a.soon
	a.mu.Unlock()

	// Seed titles immediately from SQLite by Herdr's reported session UUID so
	// AGY panes show their saved topic on the very first frame after restart.
	if len(needSeed) > 0 && seedFn != nil {
		sids := make([]string, 0, len(needSeed))
		for _, ns := range needSeed {
			sids = append(sids, ns.sid)
		}
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		seeded := seedFn(sctx, sids)
		cancel()
		if len(seeded) > 0 {
			a.mu.Lock()
			for _, ns := range needSeed {
				if a.want[ns.pane] && a.found[ns.pane] == "" {
					if title := seeded[ns.sid]; title != "" {
						a.found[ns.pane] = title
						a.paneSID[ns.pane] = ns.sid
					}
				}
			}
			a.mu.Unlock()
		}
	}

	if soon {
		select {
		case a.wake <- struct{}{}:
		default:
		}
	}
}

// nudge wakes the titler after a short throttle when a tracked AGY pane
// transitions state (e.g., user sent a new prompt or a turn completed).
func (a *agyTitler) nudge(paneID string) {
	a.mu.Lock()
	if !a.want[paneID] {
		a.mu.Unlock()
		return
	}
	a.nudged = true
	a.mu.Unlock()
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

func (a *agyTitler) decorate(sum *herdrSummary) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range sum.Workspaces {
		for j := range sum.Workspaces[i].Agents {
			ag := &sum.Workspaces[i].Agents[j]
			if ag.BareTitle && isAgyAgent(ag.Agent) && ag.ThreadTitle == "" {
				ag.ThreadTitle = a.found[ag.PaneID]
			}
		}
	}
}

func (a *agyTitler) run(ctx context.Context) {
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	var last time.Time
	for {
		a.mu.Lock()
		wait := time.Hour
		if len(a.want) > 0 {
			wait = max(0, time.Until(last.Add(a.every)))
			if a.retryWait > 0 {
				wait = min(wait, max(0, time.Until(last.Add(a.retryWait))))
			}
			if a.soon {
				wait = min(wait, max(0, time.Until(last.Add(agyTitleSoon))))
			} else if a.nudged {
				wait = min(wait, max(0, time.Until(last.Add(agyTitleNudge))))
			}
		}
		a.mu.Unlock()
		timer.Reset(wait)
		select {
		case <-ctx.Done():
			return
		case <-a.wake:
			continue
		case <-timer.C:
		}
		a.mu.Lock()
		due := len(a.want) > 0
		a.soon = false
		a.nudged = false
		a.mu.Unlock()
		if !due {
			continue
		}
		last = time.Now()
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		got, err := a.scan(sctx)
		cancel()
		if err != nil {
			a.mu.Lock()
			if a.retryWait <= 0 {
				a.retryWait = agyTitleRetryMin
			} else {
				a.retryWait = min(a.retryWait*2, agyTitleRetryMax)
			}
			a.mu.Unlock()
			continue
		}
		a.mu.Lock()
		a.retryWait = 0
		a.mu.Unlock()
		type llmJob struct {
			pane   string
			digest string
			info   agyPaneTitleInfo
		}
		var jobs []llmJob
		var panes []string
		a.mu.Lock()
		for p := range a.want {
			panes = append(panes, p)
		}
		a.mu.Unlock()

		changed := false
		now := time.Now()
		for _, p := range panes {
			info, ok := got[p]
			if !ok {
				a.mu.Lock()
				if a.found[p] != "" {
					delete(a.found, p)
					delete(a.paneSID, p)
					delete(a.digest, p)
					changed = true
				}
				a.mu.Unlock()
				continue
			}
			dig := agyTitleDigest(info)
			a.mu.Lock()
			prevDig := a.digest[p]
			prevSID := a.paneSID[p]
			prevTitle := a.found[p]
			running := a.inFlight[p]
			failedRecently := !a.failAt[dig].IsZero() && now.Sub(a.failAt[dig]) < agyTitleFailBackoff
			a.mu.Unlock()

			// Skip re-resolving if topic digest didn't change and we already have a title
			// (unless an earlier LLM attempt wasn't cached and backoff expired).
			if prevDig == dig && prevTitle != "" && (running || failedRecently) {
				continue
			}

			rctx, rcancel := context.WithTimeout(ctx, 3*time.Second)
			title, needLLM := a.resolve(rctx, p, info)
			rcancel()

			a.mu.Lock()
			if !a.want[p] {
				a.mu.Unlock()
				continue
			}
			sameSID := prevSID == info.SID
			saveSID, saveTitle := "", ""
			if !needLLM {
				// Exact cache hit or LLM disabled: apply immediately.
				if title != a.found[p] {
					if title == "" {
						delete(a.found, p)
					} else {
						a.found[p] = title
					}
					changed = true
				}
				if title != "" {
					saveSID, saveTitle = info.SID, title
				}
				a.paneSID[p] = info.SID
				a.digest[p] = dig
			} else {
				// Cache miss with LLM enabled: keep existing title for the same
				// conversation while regenerating to avoid flickering, or show
				// fallback immediately if this pane/session has no title yet.
				if (!sameSID || a.found[p] == "") && title != "" && title != a.found[p] {
					a.found[p] = title
					changed = true
					saveSID, saveTitle = info.SID, title
				}
				a.paneSID[p] = info.SID
				if !a.inFlight[p] && !failedRecently && a.genLLM != nil {
					a.inFlight[p] = true
					jobs = append(jobs, llmJob{pane: p, digest: dig, info: info})
				}
			}
			saveFn := a.saveBySID
			a.mu.Unlock()
			if saveFn != nil && saveSID != "" && saveTitle != "" {
				sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
				saveFn(sctx, saveSID, saveTitle)
				scancel()
			}
		}
		if changed {
			a.mu.Lock()
			poke := a.poke
			a.mu.Unlock()
			if poke != nil {
				poke()
			}
		}
		for _, job := range jobs {
			go func(j llmJob) {
				title, err := a.genLLM(ctx, j.pane, j.info)
				a.mu.Lock()
				delete(a.inFlight, j.pane)
				poke := a.poke
				saveFn := a.saveBySID
				updated := false
				if err != nil {
					a.failAt[j.digest] = time.Now()
					if len(a.failAt) > 200 {
						a.failAt = map[string]time.Time{j.digest: time.Now()}
					}
				} else if title != "" && a.want[j.pane] && a.paneSID[j.pane] == j.info.SID {
					if a.found[j.pane] != title {
						a.found[j.pane] = title
						updated = true
					}
					a.digest[j.pane] = j.digest
				}
				a.mu.Unlock()
				if err == nil && title != "" && saveFn != nil {
					sctx, scancel := context.WithTimeout(ctx, 2*time.Second)
					saveFn(sctx, j.info.SID, title)
					scancel()
				}
				if updated && poke != nil {
					poke()
				}
			}(job)
		}
	}
}
