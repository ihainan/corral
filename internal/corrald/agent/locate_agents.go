//go:build linux

package agent

import (
	"bytes"
	"encoding/json"
	"io"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/corrald/proto"
)

const zeroSessionID = "00000000-0000-0000-0000-000000000000"

var (
	codexSubNameRe      = regexp.MustCompile(`^[a-z][a-z0-9-]*$`)
	codexSubagentRe     = regexp.MustCompile(`"thread_source": *"(subagent|guardian_review)"|"source": *\{ *"subagent"[ ]*:`)
	codexExecOrigRe     = regexp.MustCompile(`"originator": *"codex_exec"`)
	codexRolloutFileRe  = regexp.MustCompile(`^rollout-([^_/]+)-([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})(?:_[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})?$`)
	payloadStartRe      = regexp.MustCompile(`"payload"[ ]*:[ ]*\{`)
	urlSchemeRe         = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	settingEnvObjRe     = regexp.MustCompile(`"env"[ ]*:[ ]*\{([^}]*)\}`)
	nonCompactModelChar = regexp.MustCompile(`[^A-Za-z0-9._:-]`)
	codexMPArgRe        = regexp.MustCompile(`^model_provider[ \t]*=`)
	codexCfgMPArgRe     = regexp.MustCompile(`^--config=model_provider[ \t]*=`)
)

// --- Claude Code helpers (claudeConfigDirSh, emit_claude, claude_ep) ---

// claudeConfigDir mirrors claude_cfg_dir <procdir> in claudeConfigDirSh.
func (l *Locator) claudeConfigDir(pidDir string) string {
	defaultDir := l.home + "/.claude"
	clcfg := defaultDir
	kvs, err := l.rd.readProcEnv(pidDir, proto.ReadEnvClaudeConfigDir)
	if err == nil {
		raw := firstEnvVal(kvs, "CLAUDE_CONFIG_DIR")
		if raw != "" && !strings.ContainsRune(raw, '\x01') {
			if strings.HasPrefix(raw, "/") {
				clcfg = raw
			} else {
				cwd, err := l.rd.readLink(proto.ReadProc, filepath.Join(pidDir, "cwd"))
				if err == nil && strings.HasPrefix(cwd, "/") {
					clcfg = cwd + "/" + raw
				}
			}
			if clcfg != defaultDir && l.rd.isDir(proto.ReadAgentLogsClaude, clcfg) {
				if realPath, err := l.rd.evalSymlinks(proto.ReadAgentLogsClaude, clcfg); err == nil && realPath != "" {
					clcfg = realPath
				}
			}
		}
	}
	for clcfg != "/" && strings.HasSuffix(clcfg, "/") {
		clcfg = strings.TrimSuffix(clcfg, "/")
	}
	return clcfg
}

// jsonAfter mirrors json_after in convoLocateWith.
func jsonAfter(c, key string) (string, bool) {
	needle := `"` + key + `"`
	idx := strings.Index(c, needle)
	if idx < 0 {
		return "", false
	}
	r := c[idx+len(needle):]
	for strings.HasPrefix(r, " ") {
		r = r[1:]
	}
	if !strings.HasPrefix(r, ":") {
		return "", false
	}
	r = r[1:]
	for strings.HasPrefix(r, " ") {
		r = r[1:]
	}
	return r, true
}

func jsonStr(c, key string) string {
	r, ok := jsonAfter(c, key)
	if !ok || !strings.HasPrefix(r, `"`) {
		return ""
	}
	r = r[1:]
	end := strings.IndexByte(r, '"')
	if end < 0 {
		return ""
	}
	return r[:end]
}

func jsonNum(c, key string) string {
	r, ok := jsonAfter(c, key)
	if !ok {
		return ""
	}
	for strings.HasPrefix(r, `"`) {
		r = r[1:]
	}
	i := 0
	for i < len(r) && r[i] >= '0' && r[i] <= '9' {
		i++
	}
	return r[:i]
}

func (l *Locator) isSideCopy(pidDir string) bool {
	kvs, err := l.rd.readProcEnv(pidDir, proto.ReadEnvCorralSideCopy)
	if err != nil {
		return false
	}
	for _, kv := range kvs {
		if kv.key == "CORRAL_SIDE_COPY" && kv.val == "1" {
			return true
		}
	}
	return false
}

// parseClaudeSession reads and validates a Claude session JSON file against
// /proc/<pid>/stat. Returns:
//   - ok=1: valid interactive/normal Claude Code session
//   - ok=2: Corral side-question copy (sdk-cli + CORRAL_SIDE_COPY=1) to ignore
//   - ok=0: invalid or mismatched process
func (l *Locator) parseClaudeSession(pid, sessionFile string) (st procStat, sid, ps string, ok int) {
	pidDir := filepath.Join(l.procRoot, pid)
	if !l.rd.isRegularFile(proto.ReadAgentLogsClaude, sessionFile) ||
		!l.rd.isReadableFile(proto.ReadProc, filepath.Join(pidDir, "environ")) {
		return procStat{}, "", "", 0
	}
	st, statOK := l.readProcStat(pid)
	if !statOK {
		return procStat{}, "", "", 0
	}
	raw, err := l.rd.readFileLimit(proto.ReadAgentLogsClaude, sessionFile, 65536)
	if err != nil {
		return procStat{}, "", "", 0
	}
	c := strings.ReplaceAll(string(raw), "\n", "")
	sid = jsonStr(c, "sessionId")
	ps = jsonNum(c, "procStart")
	if !isAllDigits(ps) || ps != st.start || !isUUID(sid) {
		return procStat{}, "", "", 0
	}
	if jsonStr(c, "entrypoint") == "sdk-cli" && l.isSideCopy(pidDir) {
		return st, sid, ps, 2
	}
	return st, sid, ps, 1
}

// resolveClaudeLog finds the unambiguous log file for a Claude session, using
// /proc/<pid>/fd/* to disambiguate when multiple projects contain <sid>.jsonl.
func (l *Locator) resolveClaudeLog(pid, cfgDir, sid string) string {
	matches, _ := l.rd.glob(proto.ReadAgentLogsClaude, cfgDir+"/projects/*/"+sid+".jsonl")
	logPath := ""
	count := 0
	for _, m := range matches {
		if strings.ContainsRune(m, '\n') {
			continue
		}
		if !l.rd.isRegularFile(proto.ReadAgentLogsClaude, m) {
			continue
		}
		count++
		if count == 1 {
			logPath = m
		} else {
			logPath = ""
		}
	}
	if count <= 1 {
		return logPath
	}

	realCfg, _ := l.rd.evalSymlinks(proto.ReadAgentLogsClaude, cfgDir)
	fdMatches := 0
	pidDir := filepath.Join(l.procRoot, pid)
	for _, target := range l.listFDLinks(pidDir) {
		if strings.ContainsRune(target, '\n') {
			continue
		}
		matched := matchesClaudeProjectLog(target, cfgDir, sid)
		if !matched && realCfg != "" {
			matched = matchesClaudeProjectLog(target, realCfg, sid)
		}
		if !matched || !l.rd.isRegularFile(proto.ReadAgentLogsClaude, target) {
			continue
		}
		if fdMatches == 0 {
			logPath = target
			fdMatches = 1
		} else if logPath != target {
			logPath = ""
			fdMatches = 2
			break
		}
	}
	return logPath
}

// matchesClaudeProjectLog mimics shell case `"$base"/projects/*/"$sid".jsonl`.
func matchesClaudeProjectLog(target, base, sid string) bool {
	prefix := base + "/projects/"
	suffix := "/" + sid + ".jsonl"
	if !strings.HasPrefix(target, prefix) || !strings.HasSuffix(target, suffix) {
		return false
	}
	return len(target) >= len(prefix)+len(suffix)
}

// hostOf mirrors host_of in convoEndpointSh.
func hostOf(rawURL string) string {
	loc := urlSchemeRe.FindStringIndex(rawURL)
	if loc == nil {
		return ""
	}
	s := rawURL[loc[1]:]
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if at := strings.IndexByte(s, '@'); at >= 0 {
		s = s[at+1:]
	}
	if len(s) > 120 {
		s = s[:120]
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'A' && c <= 'Z') || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '.' || c == ':' || c == '_' || c == '-') {
			return ""
		}
	}
	return s
}

func extractLastJSONStringField(body, key string) string {
	needle := `"` + key + `"`
	lastVal := ""
	found := false
	for start := 0; start < len(body); {
		idx := strings.Index(body[start:], needle)
		if idx < 0 {
			break
		}
		pos := start + idx + len(needle)
		for pos < len(body) && body[pos] == ' ' {
			pos++
		}
		if pos >= len(body) || body[pos] != ':' {
			start = start + idx + len(needle)
			continue
		}
		pos++
		for pos < len(body) && body[pos] == ' ' {
			pos++
		}
		if pos >= len(body) || body[pos] != '"' {
			start = pos
			continue
		}
		pos++
		end := strings.IndexByte(body[pos:], '"')
		if end < 0 {
			break
		}
		lastVal = body[pos : pos+end]
		found = true
		start = pos + end + 1
	}
	if !found {
		return ""
	}
	return lastVal
}

func (l *Locator) settingEnv(path, key string) string {
	raw, err := l.rd.readFileLimit(proto.ReadAgentLogsClaude, path, 65536)
	if err != nil || len(raw) == 0 {
		return ""
	}
	flat := strings.ReplaceAll(string(raw), "\n", " ")
	allObjs := settingEnvObjRe.FindAllStringSubmatch(flat, -1)
	if len(allObjs) == 0 {
		return ""
	}
	envBody := allObjs[len(allObjs)-1][1]
	val := extractLastJSONStringField(envBody, key)
	if len(val) > 400 {
		val = val[:400]
	}
	return val
}

// claudeEndpoint mirrors claude_ep in convoEndpointSh (including the Python
// autoCompactWindow/pct extraction, implemented natively in Go).
func (l *Locator) claudeEndpoint(pid, cwd string) (ep, auth, ac string) {
	pidDir := filepath.Join(l.procRoot, pid)
	kvs, _ := l.rd.readProcEnv(
		pidDir,
		proto.ReadEnvAnthropicBaseURL,
		proto.ReadEnvAnthropicAuthToken,
		proto.ReadEnvAnthropicAPIKey,
		proto.ReadEnvClaudeCodeUseBedrock,
		proto.ReadEnvClaudeCodeUseVertex,
		proto.ReadEnvClaudeConfigDir,
		proto.ReadEnvClaudeAutoCompactPctOverride,
	)

	// Cap filtered env lines at 4096 bytes like claude_ep's `head -c 4096`.
	kvs = capEnvKVs(kvs, 4096)

	cfgd := firstEnvVal(kvs, "CLAUDE_CONFIG_DIR")
	if cfgd == "" || strings.ContainsRune(cfgd, '\x01') {
		cfgd = ""
	} else if !strings.HasPrefix(cfgd, "/") {
		if strings.HasPrefix(cwd, "/") {
			cfgd = cwd + "/" + cfgd
		} else {
			cfgd = ""
		}
	}
	if cfgd == "" {
		cfgd = l.home + "/.claude"
	}

	settingsFiles := []string{
		"/etc/claude-code/managed-settings.json",
		cwd + "/.claude/settings.local.json",
		cwd + "/.claude/settings.json",
		cfgd + "/settings.local.json",
		cfgd + "/settings.json",
	}

	var bu, au string
	for _, sf := range settingsFiles {
		if !l.rd.isReadableFile(proto.ReadAgentLogsClaude, sf) {
			continue
		}
		if bu == "" {
			bu = l.settingEnv(sf, "ANTHROPIC_BASE_URL")
		}
		if au == "" {
			if l.settingEnv(sf, "ANTHROPIC_AUTH_TOKEN") != "" {
				au = "token"
			}
			if au == "" && l.settingEnv(sf, "ANTHROPIC_API_KEY") != "" {
				au = "key"
			}
		}
	}
	if bu == "" {
		bu = firstEnvVal(kvs, "ANTHROPIC_BASE_URL")
	}
	if au == "" {
		if firstEnvVal(kvs, "ANTHROPIC_AUTH_TOKEN") != "" {
			au = "token"
		} else if firstEnvVal(kvs, "ANTHROPIC_API_KEY") != "" {
			au = "key"
		}
	}
	switch firstEnvVal(kvs, "CLAUDE_CODE_USE_BEDROCK") {
	case "", "0", "false":
	default:
		au = "bedrock"
		bu = ""
	}
	switch firstEnvVal(kvs, "CLAUDE_CODE_USE_VERTEX") {
	case "", "0", "false":
	default:
		au = "vertex"
		bu = ""
	}
	if au == "" {
		au = "login"
	}
	auth = au
	if bu != "" {
		ep = hostOf(bu)
	}

	acp := firstEnvVal(kvs, "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE")
	ac = l.computeClaudeAutoCompact(acp, settingsFiles)
	return ep, auth, ac
}

func capEnvKVs(kvs []envKV, limit int) []envKV {
	if len(kvs) == 0 {
		return nil
	}
	var b strings.Builder
	for _, kv := range kvs {
		b.WriteString(kv.key)
		b.WriteByte('=')
		b.WriteString(kv.val)
		b.WriteByte('\n')
		if b.Len() >= limit {
			break
		}
	}
	s := b.String()
	if len(s) > limit {
		s = s[:limit]
	}
	var out []envKV
	for _, line := range strings.Split(strings.TrimRight(s, "\n"), "\n") {
		if line == "" {
			continue
		}
		if k, v, ok := strings.Cut(line, "="); ok {
			out = append(out, envKV{key: k, val: v})
		}
	}
	return out
}

func (l *Locator) computeClaudeAutoCompact(envPct string, settingsFiles []string) string {
	type modelEntry struct {
		key string
		val int
	}
	seenModels := map[string]bool{}
	var models []modelEntry
	globalWindow := 0
	pct := 0

	for _, sf := range settingsFiles {
		raw, err := l.rd.readFile(proto.ReadAgentLogsClaude, sf)
		if err != nil {
			continue
		}
		// Parse top-level JSON object preserving modelSettings key insertion order.
		var top map[string]json.RawMessage
		if err := json.Unmarshal(raw, &top); err != nil || top == nil {
			continue
		}
		if msRaw, ok := top["modelSettings"]; ok {
			for _, kv := range parseOrderedJSONObject(msRaw) {
				var entry map[string]any
				dec := json.NewDecoder(bytes.NewReader(kv.val))
				dec.UseNumber()
				if err := dec.Decode(&entry); err != nil || entry == nil {
					continue
				}
				if w, ok := jsonPosInt(entry["autoCompactWindow"]); ok {
					lk := nonCompactModelChar.ReplaceAllString(strings.ToLower(kv.key), "")
					if len(lk) > 64 {
						lk = lk[:64]
					}
					if lk != "" && !seenModels[lk] {
						seenModels[lk] = true
						models = append(models, modelEntry{key: lk, val: w})
					}
				}
			}
		}
		if globalWindow == 0 {
			if gwRaw, ok := top["autoCompactWindow"]; ok {
				var v any
				dec := json.NewDecoder(bytes.NewReader(gwRaw))
				dec.UseNumber()
				if err := dec.Decode(&v); err == nil {
					if w, ok := jsonPosInt(v); ok {
						globalWindow = w
					}
				}
			}
		}
		if pct == 0 {
			if evRaw, ok := top["env"]; ok {
				var ev map[string]any
				dec := json.NewDecoder(bytes.NewReader(evRaw))
				dec.UseNumber()
				if err := dec.Decode(&ev); err == nil && ev != nil {
					if rawPct, exists := ev["CLAUDE_AUTOCOMPACT_PCT_OVERRIDE"]; exists {
						if n, ok := parseCompactPct(rawPct); ok {
							pct = n
						}
					}
				}
			}
		}
	}

	if pct == 0 && envPct != "" {
		if n, err := strconv.Atoi(strings.TrimSpace(envPct)); err == nil && n >= 1 && n <= 100 {
			pct = n
		}
	}

	parts := make([]string, 0, len(models)+2)
	for _, m := range models {
		parts = append(parts, m.key+"="+strconv.Itoa(m.val))
	}
	if globalWindow > 0 {
		parts = append(parts, "global="+strconv.Itoa(globalWindow))
	}
	if pct > 0 {
		parts = append(parts, "pct="+strconv.Itoa(pct))
	}
	res := strings.Join(parts, ",")
	if len(res) > 512 {
		res = res[:512]
	}
	return res
}

type orderedRawKV struct {
	key string
	val json.RawMessage
}

func parseOrderedJSONObject(raw json.RawMessage) []orderedRawKV {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}
	var out []orderedRawKV
	for dec.More() {
		ktok, err := dec.Token()
		if err != nil {
			break
		}
		key, ok := ktok.(string)
		if !ok {
			break
		}
		var val json.RawMessage
		if err := dec.Decode(&val); err != nil {
			break
		}
		out = append(out, orderedRawKV{key: key, val: val})
	}
	return out
}

func jsonPosInt(v any) (int, bool) {
	switch n := v.(type) {
	case json.Number:
		if i, err := n.Int64(); err == nil {
			if i > 0 {
				return int(i), true
			}
			return 0, false
		}
		if f, err := n.Float64(); err == nil {
			if int(f) > 0 {
				return int(f), true
			}
		}
	case float64:
		if int(n) > 0 {
			return int(n), true
		}
	}
	return 0, false
}

func parseCompactPct(v any) (int, bool) {
	var s string
	switch x := v.(type) {
	case string:
		s = x
	case json.Number:
		s = x.String()
	default:
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n < 1 || n > 100 {
		return 0, false
	}
	return n, true
}

// --- Codex helpers (codexArgsAwk, codexFdsSh, codex_ep, meta_of) ---

var codexSkipNextArg = map[string]bool{
	"-c": true, "-m": true, "-p": true, "-s": true, "-a": true, "-C": true, "-i": true,
	"--config": true, "--model": true, "--profile": true, "--sandbox": true,
	"--ask-for-approval": true, "--cd": true, "--image": true, "--add-dir": true,
	"--local-provider": true, "--remote": true, "--remote-auth-token-env": true,
	"--enable": true, "--disable": true,
}

var codexNonInteractiveSubs = map[string]bool{
	"exec": true, "e": true, "exec-server": true, "review": true,
	"apply": true, "a": true, "login": true, "logout": true,
	"mcp": true, "mcp-server": true, "app-server": true,
	"completion": true, "sandbox": true, "debug": true, "cloud": true, "features": true,
}

// parseCodexArgs mirrors codexArgsAwk for both want=sub and want=resume/fork.
func parseCodexArgs(args []string) (sub string, second string) {
	skip := false
	var pos [2]string
	n := 0
	for i := 1; i < len(args); i++ {
		a := args[i]
		if skip {
			skip = false
			continue
		}
		if a == "--" {
			break
		}
		if strings.HasPrefix(a, "-") {
			if codexSkipNextArg[a] {
				skip = true
			}
			continue
		}
		pos[n] = a
		n++
		if n == 2 {
			break
		}
	}
	s := pos[0]
	if s != "" && !codexSubNameRe.MatchString(s) {
		s = ""
	}
	return s, pos[1]
}

func (l *Locator) codexSub(pid string) string {
	sub, _ := parseCodexArgs(l.readCmdlineArgs(pid))
	return sub
}

func (l *Locator) codexResumeOf(pid string) string {
	sub, arg2 := parseCodexArgs(l.readCmdlineArgs(pid))
	if sub == "resume" && isUUIDOrHyphenChars(arg2) {
		return strings.ToLower(arg2)
	}
	return ""
}

func (l *Locator) codexHome(pidDir string) string {
	kvs, err := l.rd.readProcEnv(pidDir, proto.ReadEnvCodexHome)
	cxhome := ""
	if err == nil {
		cxhome = firstEnvVal(kvs, "CODEX_HOME")
	}
	if cxhome == "" {
		cxhome = l.home + "/.codex"
	}
	return strings.TrimSuffix(cxhome, "/")
}

func (l *Locator) codexIsSubagent(logPath string) bool {
	head, err := l.rd.readFileLimit(proto.ReadAgentLogsCodex, logPath, 4096)
	if err != nil {
		return false
	}
	return codexSubagentRe.Match(head)
}

func codexLogSID(logPath string) string {
	base := path.Base(logPath)
	if !strings.HasPrefix(base, "rollout-") || !strings.HasSuffix(base, ".jsonl") {
		return ""
	}
	stem := strings.TrimSuffix(base, ".jsonl")
	m := codexRolloutFileRe.FindStringSubmatch(stem)
	if m == nil {
		return ""
	}
	return m[2]
}

func matchesCodexSessionRollout(link, cxhome string) bool {
	prefix := cxhome + "/sessions/"
	rest, ok := strings.CutPrefix(link, prefix)
	if !ok {
		return false
	}
	slash := strings.LastIndexByte(rest, '/')
	if slash < 0 {
		return false
	}
	base := rest[slash+1:]
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

func matchesCodexWriterLock(link, cxhome string) (string, bool) {
	prefix := cxhome + "/thread-writer-locks/"
	if !strings.HasPrefix(link, prefix) || !strings.HasSuffix(link, ".lock") {
		return "", false
	}
	base := path.Base(link)
	id := strings.TrimSuffix(base, ".lock")
	if !isUUID(id) {
		return "", false
	}
	return id, true
}

func isAnyRolloutLink(link string) bool {
	if !strings.ContainsRune(link, '/') {
		return false
	}
	base := path.Base(link)
	return strings.HasPrefix(base, "rollout-") && strings.HasSuffix(base, ".jsonl")
}

type codexFdsResult struct {
	cxlog  string
	cxany  bool
	cxpend string
	cxnew  bool
}

// codexFds mirrors codex_fds in codexFdsSh.
func (l *Locator) codexFds(pidDir string, nowSec int64) codexFdsResult {
	var res codexFdsResult
	cxhome := l.codexHome(pidDir)

	type lockInfo struct {
		id string
		t  int64
	}
	var locks []lockInfo
	var openLogs []string
	var bestLogMtime time.Time

	for _, link := range l.listFDLinks(pidDir) {
		if link == "" || strings.ContainsRune(link, '\n') {
			continue
		}
		if isAnyRolloutLink(link) {
			res.cxany = true
		}
		if matchesCodexSessionRollout(link, cxhome) {
			openLogs = append(openLogs, strings.TrimSuffix(link, ".jsonl"))
			if l.codexIsSubagent(link) {
				continue
			}
			info, err := l.rd.stat(proto.ReadAgentLogsCodex, link)
			if res.cxlog == "" {
				res.cxlog = link
				if err == nil {
					bestLogMtime = info.ModTime()
				}
			} else if err == nil && (bestLogMtime.IsZero() || info.ModTime().After(bestLogMtime)) {
				res.cxlog = link
				bestLogMtime = info.ModTime()
			}
		} else if id, ok := matchesCodexWriterLock(link, cxhome); ok {
			if info, err := l.rd.stat(proto.ReadAgentLogsCodex, link); err == nil {
				locks = append(locks, lockInfo{id: id, t: info.ModTime().Unix()})
			}
		}
	}

	var hasRef bool
	var refT int64
	if res.cxlog != "" {
		stem := strings.TrimSuffix(res.cxlog, ".jsonl")
		for _, lk := range locks {
			if strings.HasSuffix(stem, "-"+lk.id) || strings.HasSuffix(stem, "_"+lk.id) {
				refT = lk.t
				hasRef = true
			}
		}
	}

	var pend string
	var hasPend bool
	var pt int64
	var tie bool
	for _, lk := range locks {
		if logListHasID(openLogs, lk.id) {
			continue
		}
		if res.cxlog != "" {
			if !hasRef {
				tie = true
				continue
			}
			if lk.t < refT {
				continue
			}
			if lk.t == refT {
				tie = true
				continue
			}
		}
		if nowSec-lk.t < 3 {
			res.cxnew = true
			continue
		}
		if !hasPend || lk.t > pt {
			pend = lk.id
			pt = lk.t
			hasPend = true
			tie = false
		} else if lk.t == pt {
			tie = true
		}
	}
	if tie {
		res.cxnew = true
	} else {
		res.cxpend = pend
	}
	return res
}

func logListHasID(stems []string, id string) bool {
	dash := "-" + id
	under := "_" + id
	for _, s := range stems {
		if strings.HasSuffix(s, dash) || strings.HasSuffix(s, under) {
			return true
		}
	}
	return false
}

// metaOf mirrors meta_of in convoLocateWith (first line, max 8192 bytes,
// substring after "payload":{, first "key":"value").
func (l *Locator) metaOf(logPath, key string) string {
	raw, err := l.rd.readFileLimit(proto.ReadAgentLogsCodex, logPath, 8192)
	if err != nil || len(raw) == 0 {
		return ""
	}
	if nl := bytes.IndexByte(raw, '\n'); nl >= 0 {
		raw = raw[:nl]
	}
	loc := payloadStartRe.FindIndex(raw)
	if loc == nil {
		return ""
	}
	return jsonStr(string(raw[loc[1]:]), key)
}

func (l *Locator) tomlOf(path, section, key string) string {
	raw, err := l.rd.readFile(proto.ReadAgentLogsCodex, path)
	if err != nil {
		return ""
	}
	curSec := ""
	for _, line := range strings.Split(string(raw), "\n") {
		trimLeft := strings.TrimLeft(line, " \t")
		if strings.HasPrefix(trimLeft, "[") {
			sec := strings.TrimPrefix(trimLeft, "[")
			sec = strings.TrimRight(sec, " \t\r")
			sec = strings.TrimSuffix(sec, "]")
			curSec = sec
			continue
		}
		trimmed := strings.Trim(line, " \t\r")
		if curSec != section {
			continue
		}
		if !strings.HasPrefix(trimmed, key) {
			continue
		}
		afterKey := strings.TrimLeft(trimmed[len(key):], " \t")
		if !strings.HasPrefix(afterKey, "=") {
			continue
		}
		rhs := strings.TrimLeft(afterKey[1:], " \t")
		rhs = strings.TrimPrefix(rhs, `"`)
		rhs = strings.TrimSuffix(rhs, `"`)
		if len(rhs) > 200 {
			rhs = rhs[:200]
		}
		return rhs
	}
	return ""
}

// codexEndpoint mirrors codex_ep in convoEndpointSh.
func (l *Locator) codexEndpoint(pid string) (ep, auth string) {
	pidDir := filepath.Join(l.procRoot, pid)
	kvs, _ := l.rd.readProcEnv(pidDir, proto.ReadEnvCodexHome, proto.ReadEnvOpenAIBaseURL)
	kvs = capEnvKVs(kvs, 2048)

	cfgd := firstEnvVal(kvs, "CODEX_HOME")
	if cfgd == "" {
		cfgd = l.home + "/.codex"
	}
	sf := cfgd + "/config.toml"

	args := l.readCmdlineArgs(pid)
	want := ""
	p := ""
	for _, arg := range args {
		if want == "c" {
			if codexMPArgRe.MatchString(arg) {
				_, rhs, _ := strings.Cut(arg, "=")
				rhs = strings.TrimLeft(rhs, " \t")
				rhs = strings.TrimPrefix(rhs, `"`)
				rhs = strings.TrimSuffix(rhs, `"`)
				p = "p=" + rhs
			}
			want = ""
			continue
		}
		if want == "p" {
			p = "f=" + arg
			want = ""
			continue
		}
		switch {
		case arg == "-c" || arg == "--config":
			want = "c"
		case arg == "-p" || arg == "--profile":
			want = "p"
		case codexCfgMPArgRe.MatchString(arg):
			rest := strings.TrimPrefix(arg, "--config=")
			_, rhs, _ := strings.Cut(rest, "=")
			rhs = strings.TrimLeft(rhs, " \t")
			rhs = strings.TrimPrefix(rhs, `"`)
			rhs = strings.TrimSuffix(rhs, `"`)
			p = "p=" + rhs
		case strings.HasPrefix(arg, "--profile="):
			p = "f=" + strings.TrimPrefix(arg, "--profile=")
		}
	}

	var au string
	if v, ok := strings.CutPrefix(p, "p="); ok {
		au = v
	} else if v, ok := strings.CutPrefix(p, "f="); ok {
		au = l.tomlOf(sf, "profiles."+v, "model_provider")
	}
	if au == "" {
		if prof := l.tomlOf(sf, "", "profile"); prof != "" {
			au = l.tomlOf(sf, "profiles."+prof, "model_provider")
		}
	}
	if au == "" {
		au = l.tomlOf(sf, "", "model_provider")
	}

	var bu string
	switch au {
	case "", "openai", "oss":
		bu = firstEnvVal(kvs, "OPENAI_BASE_URL")
	default:
		bu = l.tomlOf(sf, "model_providers."+au, "base_url")
	}
	if bu != "" {
		ep = hostOf(bu)
	}
	return ep, ""
}

// --- OpenCode helpers (oc_sub, oc_db, svdb, ocports) ---

var ocSkipNextArg = map[string]bool{
	"-m": true, "-s": true, "-p": true,
	"--model": true, "--session": true, "--prompt": true, "--agent": true,
	"--port": true, "--hostname": true, "--log-level": true, "--mdns-domain": true,
	"--cors": true, "--server": true, "--completions": true,
}

var ocNonInteractiveSubs = map[string]bool{
	"run": true, "serve": true, "web": true, "acp": true, "mcp": true,
	"auth": true, "agent": true, "upgrade": true, "update": true, "uninstall": true,
	"models": true, "stats": true, "export": true, "import": true, "github": true,
	"pr": true, "debug": true, "session": true, "completion": true, "generate": true,
	"attach": true, "api": true, "plugin": true, "service": true, "reload": true, "pair": true,
}

func (l *Locator) ocSub(pid string) string {
	args := l.readCmdlineArgs(pid)
	skip := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if skip {
			skip = false
			continue
		}
		if a == "--" {
			return ""
		}
		if ocSkipNextArg[a] {
			skip = true
			continue
		}
		if ocNonInteractiveSubs[a] {
			return a
		}
	}
	return ""
}

func (l *Locator) ocDB(pidDir string) string {
	for _, link := range l.listFDLinks(pidDir) {
		if strings.ContainsRune(link, '\n') {
			continue
		}
		if strings.HasSuffix(link, "/opencode/opencode.db") {
			return link
		}
	}
	return ""
}

type ocServerInfo struct {
	pid  string
	ppid string
	db   string
}

type ocPortEntry struct {
	pid  string
	kind byte // 'L' or 'C'
	port uint64
}

func (l *Locator) readOCPorts(inodeOwner map[string]string) []ocPortEntry {
	var out []ocPortEntry
	for _, name := range []string{"tcp", "tcp6"} {
		raw, err := l.rd.readFile(proto.ReadProc, filepath.Join(l.procRoot, "net", name))
		if err != nil {
			continue
		}
		lines := strings.Split(string(raw), "\n")
		for i := 1; i < len(lines); i++ {
			fields := strings.Fields(lines[i])
			if len(fields) < 10 {
				continue
			}
			owner, ok := inodeOwner[fields[9]]
			if !ok {
				continue
			}
			state := fields[3]
			if state == "0A" {
				if _, hexPort, ok := strings.Cut(fields[1], ":"); ok {
					if port, err := strconv.ParseUint(hexPort, 16, 64); err == nil {
						out = append(out, ocPortEntry{pid: owner, kind: 'L', port: port})
					}
				}
			} else if state == "01" {
				if remAddr, hexPort, ok := strings.Cut(fields[2], ":"); ok && isLoopbackProcHex(remAddr) {
					if port, err := strconv.ParseUint(hexPort, 16, 64); err == nil {
						out = append(out, ocPortEntry{pid: owner, kind: 'C', port: port})
					}
				}
			}
		}
	}
	return out
}

func isLoopbackProcHex(r string) bool {
	if len(r) == 8 && r[6:8] == "7F" {
		return true
	}
	if r == "00000000000000000000000001000000" {
		return true
	}
	if len(r) == 32 && r[:24] == "0000000000000000FFFF0000" && r[30:32] == "7F" {
		return true
	}
	return false
}

func matchOCServiceDB(pid string, ports []ocPortEntry, svdb []ocServerInfo) string {
	dbByPID := make(map[string]string, len(svdb))
	for _, sv := range svdb {
		dbByPID[sv.pid] = sv.db
	}
	want := map[uint64]bool{}
	listenAt := map[uint64]string{}
	listenTwo := map[uint64]bool{}
	for _, e := range ports {
		if e.pid == pid && e.kind == 'C' {
			want[e.port] = true
			continue
		}
		if e.kind == 'L' {
			if prev, exists := listenAt[e.port]; exists && prev != e.pid {
				listenTwo[e.port] = true
			}
			listenAt[e.port] = e.pid
		}
	}
	got := ""
	for q := range want {
		if listenTwo[q] {
			continue
		}
		lp, ok := listenAt[q]
		if !ok {
			continue
		}
		d, ok := dbByPID[lp]
		if !ok {
			continue
		}
		if got != "" && got != d {
			got = ""
			break
		}
		got = d
	}
	if got != "" {
		return got
	}
	for _, sv := range svdb {
		if sv.ppid == pid {
			return sv.db
		}
	}
	return ""
}

// --- Agy helpers (agyFdsSh) ---

var agySkipNextArg = map[string]bool{
	"-i": true, "--prompt-interactive": true, "--add-dir": true, "--agent": true,
	"--conversation": true, "--effort": true, "--input-format": true, "--json-schema": true,
	"--log-file": true, "--mode": true, "--model": true, "--output-format": true,
	"--print-timeout": true, "--project": true, "--settings": true,
}

var agyNonInteractiveSubs = map[string]bool{
	"acp": true, "agent": true, "agents": true, "changelog": true, "help": true,
	"install": true, "mcp": true, "mic-serve": true, "models": true, "plugin": true,
	"plugins": true, "remote-control": true, "update": true,
}

func (l *Locator) agyNonInter(pid string) bool {
	args := l.readCmdlineArgs(pid)
	skip := false
	for i := 1; i < len(args); i++ {
		a := args[i]
		if skip {
			skip = false
			continue
		}
		if a == "--" {
			return false
		}
		if a == "-p" || a == "--print" || a == "--prompt" ||
			strings.HasPrefix(a, "--print=") || strings.HasPrefix(a, "--prompt=") {
			return true
		}
		if agySkipNextArg[a] {
			skip = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return agyNonInteractiveSubs[a]
	}
	return false
}

func (l *Locator) agyFds(pidDir string) (sid, logPath string) {
	for _, link := range l.listFDLinks(pidDir) {
		if strings.ContainsRune(link, '\n') {
			continue
		}
		idx := strings.LastIndex(link, "/presence/")
		if idx < 0 || !strings.HasSuffix(link, ".lock") {
			continue
		}
		name := link[idx+len("/presence/"):]
		if strings.ContainsRune(name, '/') {
			continue
		}
		id := strings.TrimSuffix(name, ".lock")
		if !isUUID(id) {
			continue
		}
		sid = strings.ToLower(id)
		agyBase := link[:idx]
		cand := agyBase + "/brain/" + sid + "/.system_generated/logs/transcript_full.jsonl"
		if l.rd.isRegularFile(proto.ReadAgentLogsAgy, cand) {
			logPath = cand
		}
		break
	}
	return sid, logPath
}

// --- Devin helpers (devinFdsSh, devinLocateSh) ---

var dvSkipValArg = map[string]bool{
	"--prompt-file": true, "--config": true, "--permission-mode": true,
	"--process-model": true, "--model": true, "--verbosity": true,
}

var dvSkipOptValArg = map[string]bool{
	"-r": true, "--resume": true, "--export": true, "--respect-workspace-trust": true,
}

func (l *Locator) dvInter(pid string) bool {
	args := l.readCmdlineArgs(pid)
	skip := 0
	for i := 1; i < len(args); i++ {
		a := args[i]
		if skip == 1 {
			skip = 0
			continue
		}
		if skip == 2 {
			skip = 0
			if !strings.HasPrefix(a, "-") {
				continue
			}
		}
		if a == "--" {
			return true
		}
		if a == "-p" || a == "--print" || strings.HasPrefix(a, "--print=") {
			return false
		}
		if dvSkipValArg[a] {
			skip = 1
			continue
		}
		if dvSkipOptValArg[a] {
			skip = 2
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		return false
	}
	return true
}

func (l *Locator) dvFds(pidDir string) (dvsid, dvdb string) {
	dvn := 0
	for _, link := range l.listFDLinks(pidDir) {
		if strings.ContainsRune(link, '\n') {
			continue
		}
		if idx := strings.LastIndex(link, "/devin/cli/session_locks/"); idx >= 0 && strings.HasSuffix(link, ".lock") {
			name := link[idx+len("/devin/cli/session_locks/"):]
			if strings.ContainsRune(name, '/') {
				continue
			}
			id := strings.TrimSuffix(name, ".lock")
			if !isDevinLockID(id) {
				continue
			}
			if id != dvsid {
				dvsid = id
				dvn++
			}
		} else if strings.HasSuffix(link, "/devin/cli/sessions.db") {
			dvdb = link
		}
	}
	if dvn > 1 {
		dvsid = ""
	}
	return dvsid, dvdb
}

func isDevinLockID(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return false
		}
	}
	return true
}

func (l *Locator) xdgDataHome() string {
	if l.xdgDataHomeOverride != nil {
		return *l.xdgDataHomeOverride
	}
	return l.rd.readSelfEnv(proto.ReadEnvXDGDataHome)
}

func (l *Locator) selfClaudeConfigDir() string {
	if l.claudeConfigDirOverride != nil {
		return *l.claudeConfigDirOverride
	}
	return l.rd.readSelfEnv(proto.ReadEnvClaudeConfigDir)
}

// Keep unused io import guard unnecessary; confirm no unused imports.
var _ = io.EOF
