package web

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

// GET  /api/sessions/{id}/pane-env?pane=<pane>
// POST /api/sessions/{id}/pane-env/reveal
//
// Reads environment variables of the session's foreground / agent process from
// Linux /proc/<pid>/environ on demand (never pre-fetched or polled). Only the
// session's owner may call these endpoints (via runningSession). Sensitive
// variables are never sent in the list response; revealing one requires a
// dedicated POST request and writes an audit entry without recording the value.

const (
	paneEnvReadMax    = 256 << 10 // cap on /proc/<pid>/environ bytes read from host
	paneEnvExecLimit  = 10 * time.Second
	paneEnvMaxNameLen = 256
)

type paneEnvVar struct {
	Name      string `json:"name"`
	Sensitive bool   `json:"sensitive"`
	Value     string `json:"value,omitempty"`   // omitted when sensitive
	Preview   string `json:"preview,omitempty"` // fixed mask placeholder when sensitive
}

type paneEnvResponse struct {
	Mode      string       `json:"mode"`
	Pane      string       `json:"pane,omitempty"`
	PID       int          `json:"pid,omitempty"`
	Process   string       `json:"process,omitempty"`
	Reason    string       `json:"reason,omitempty"`
	Truncated bool         `json:"truncated,omitempty"`
	Vars      []paneEnvVar `json:"vars"`
}

type paneEnvRevealRequest struct {
	Pane string `json:"pane"`
	Name string `json:"name"`
}

type paneEnvRevealResponse struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	PID   int    `json:"pid,omitempty"`
}

type rawEnvVar struct {
	Name  string
	Value string
}

// sensitiveNameTokens are case-insensitive substrings that mark an environment
// variable name as holding credentials or session secrets.
var sensitiveNameTokens = []string{
	"KEY",
	"TOKEN",
	"SECRET",
	"PASSWORD",
	"PASS",
	"AUTH",
	"CREDENTIAL",
	"COOKIE",
	"SESSION",
	"PRIVATE",
	"BEARER",
	"JWT",
	"SIGNING",
	"PGPASS",
	"NETRC",
	"CERT",
}

// sensitiveValuePrefixes identify common API keys, tokens, and PEM blocks by
// prefix (checked case-sensitively unless noted).
var sensitiveValuePrefixes = []string{
	"sk-",
	"sk_",
	"pk_live_",
	"pk_test_",
	"rk_live_",
	"rk_test_",
	"ghp_",
	"gho_",
	"ghu_",
	"ghs_",
	"ghr_",
	"github_pat_",
	"glpat-",
	"xoxb-",
	"xoxp-",
	"xoxa-",
	"xoxr-",
	"xoxs-",
	"npm_",
	"pypi-",
	"hf_",
	"-----BEGIN ",
}

var (
	// JWT: three base64url segments starting with eyJ.
	jwtValueRe = regexp.MustCompile(`^eyJ[A-Za-z0-9_-]{8,}\.eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]*$`)
	// AWS access key IDs (AKIA / ASIA + 16 uppercase alphanumeric chars).
	awsAccessKeyRe = regexp.MustCompile(`^(?:AKIA|ASIA)[0-9A-Z]{16}$`)
)

// sensitiveValueSubstrings are lowercase substrings that mark an environment
// variable value as sensitive anywhere they appear in the raw, percent-decoded,
// or JSON-unescaped value.
var sensitiveValueSubstrings = []string{
	"password",
	"passwd",
	"passphrase",
	"passkey",
	"pwd=",
	"secret",
	"token",
	"auth",
	"credential",
	"apikey",
	"api_key",
	"api-key",
	"access_key",
	"accesskey",
	"accountkey",
	"private_key",
	"privatekey",
	"bearer",
	"cookie",
	"signature",
}

func fromHexByte(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

// percentDecodeOnce performs best-effort %XX decoding, leaving malformed
// percent sequences intact so arbitrary input never fails.
func percentDecodeOnce(s string) (string, bool) {
	if !strings.ContainsRune(s, '%') {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) {
			hi, ok1 := fromHexByte(s[i+1])
			lo, ok2 := fromHexByte(s[i+2])
			if ok1 && ok2 {
				b.WriteByte(hi<<4 | lo)
				i += 2
				changed = true
				continue
			}
		}
		b.WriteByte(s[i])
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

// jsonUnescapeOnce decodes JSON \uXXXX, \/, \", \\, \t, \n, \r, \b, and \f
// escapes in a single linear pass, leaving malformed escapes intact.
func jsonUnescapeOnce(s string) (string, bool) {
	if !strings.ContainsRune(s, '\\') {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s))
	changed := false
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) {
			switch s[i+1] {
			case '/', '"', '\\':
				b.WriteByte(s[i+1])
				i++
				changed = true
				continue
			case 't':
				b.WriteByte('\t')
				i++
				changed = true
				continue
			case 'n':
				b.WriteByte('\n')
				i++
				changed = true
				continue
			case 'r':
				b.WriteByte('\r')
				i++
				changed = true
				continue
			case 'b':
				b.WriteByte('\b')
				i++
				changed = true
				continue
			case 'f':
				b.WriteByte('\f')
				i++
				changed = true
				continue
			case 'u':
				if i+5 < len(s) {
					h0, ok0 := fromHexByte(s[i+2])
					h1, ok1 := fromHexByte(s[i+3])
					h2, ok2 := fromHexByte(s[i+4])
					h3, ok3 := fromHexByte(s[i+5])
					if ok0 && ok1 && ok2 && ok3 {
						r := rune(h0)<<12 | rune(h1)<<8 | rune(h2)<<4 | rune(h3)
						b.WriteRune(r)
						i += 5
						changed = true
						continue
					}
				}
			}
		}
		b.WriteByte(s[i])
	}
	if !changed {
		return s, false
	}
	return b.String(), true
}

// hasColonBeforeAt reports whether any whitespace-free run in s contains ':'
// followed later by '@' (user:pass@host URLs, Go MySQL DSNs, scp-like URLs
// with embedded passwords, while leaving git@host:org/repo visible).
func hasColonBeforeAt(s string) bool {
	seenColon := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case ' ', '\t', '\r', '\n', '\v', '\f':
			seenColon = false
		case ':':
			seenColon = true
		case '@':
			if seenColon {
				return true
			}
		}
	}
	return false
}

func isASCIIAlphaNum(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}

// envScanStats counts the exact scanning work performed during value
// classification so linearity tests can assert O(n) scaling deterministically
// without wall-clock jitter.
type envScanStats struct {
	forms int
	steps int
}

// hasSensitiveFieldToken scans lower in a single forward pass (no backtracking)
// for standalone field names (key, sig, accountkey, sharedaccesskey, pwd)
// followed by optional ASCII whitespace and one of '=', ':', '"', or '\”.
func hasSensitiveFieldToken(lower string, stats *envScanStats) bool {
	n := len(lower)
	for i := 0; i < n; {
		if stats != nil {
			stats.steps++
		}
		if !isASCIIAlphaNum(lower[i]) {
			i++
			continue
		}
		start := i
		for i < n && isASCIIAlphaNum(lower[i]) {
			if stats != nil {
				stats.steps++
			}
			i++
		}
		switch lower[start:i] {
		case "key", "sig", "accountkey", "sharedaccesskey", "pwd":
			for i < n && (lower[i] == ' ' || lower[i] == '\t' || lower[i] == '\r' || lower[i] == '\n') {
				if stats != nil {
					stats.steps++
				}
				i++
			}
			if i < n && (lower[i] == '=' || lower[i] == ':' || lower[i] == '"' || lower[i] == '\'') {
				return true
			}
		}
	}
	return false
}

// isBasicNonSecretEnvName reports whether an uppercase env var name belongs to
// the small allowlist of standard shell/path/locale variables whose values skip
// keyword-substring matching (so e.g. HOME=/home/author or
// PATH=/usr/local/sbin:/opt/auth:/usr/bin stay visible). Structural checks
// (field tokens like ?key=/sig=, colon-before-at credentials, key prefixes,
// JWT, AWS keys, high entropy) still apply.
func isBasicNonSecretEnvName(upper string) bool {
	switch upper {
	case "HOME", "PATH", "PWD", "OLDPWD", "SHELL", "USER", "LOGNAME",
		"LANG", "LANGUAGE", "TERM", "TMPDIR", "MANPATH", "INFOPATH",
		"TERMINFO", "TERMINFO_DIRS":
		return true
	}
	if strings.HasPrefix(upper, "LC_") {
		return true
	}
	if strings.HasPrefix(upper, "XDG_") &&
		(strings.HasSuffix(upper, "_DIR") || strings.HasSuffix(upper, "_DIRS") ||
			strings.HasSuffix(upper, "_HOME") || strings.HasSuffix(upper, "_PATH")) {
		return true
	}
	return false
}

func matchesSensitiveForm(s string, skipKeywords bool, stats *envScanStats) bool {
	if stats != nil {
		stats.forms++
		stats.steps += len(s) // ToLower + colon-before-at scan
	}
	lower := strings.ToLower(s)
	if !skipKeywords {
		for _, sub := range sensitiveValueSubstrings {
			if stats != nil {
				stats.steps += len(lower)
			}
			if strings.Contains(lower, sub) {
				return true
			}
		}
	}
	if hasSensitiveFieldToken(lower, stats) {
		return true
	}
	return hasColonBeforeAt(lower)
}

const (
	maxDecodeRounds = 3
	maxDecodeForms  = 8
)

// hasSensitiveValuePattern expands v through up to maxDecodeRounds BFS rounds
// of single-step percent-decoding and JSON-unescaping (deduplicated, capped at
// maxDecodeForms distinct forms) and checks each form in linear time.
func hasSensitiveValuePattern(v string, skipKeywords bool) bool {
	return hasSensitiveValuePatternWithStats(v, skipKeywords, nil)
}

func hasSensitiveValuePatternWithStats(v string, skipKeywords bool, stats *envScanStats) bool {
	if matchesSensitiveForm(v, skipKeywords, stats) {
		return true
	}
	forms := make([]string, 0, maxDecodeForms)
	seen := make(map[string]struct{}, maxDecodeForms)
	forms = append(forms, v)
	seen[v] = struct{}{}

	frontierStart := 0
	for round := 0; round < maxDecodeRounds && len(forms) < maxDecodeForms; round++ {
		frontierEnd := len(forms)
		if frontierStart >= frontierEnd {
			break
		}
		for i := frontierStart; i < frontierEnd && len(forms) < maxDecodeForms; i++ {
			cur := forms[i]
			if stats != nil {
				stats.steps += len(cur)
			}
			if d, ok := percentDecodeOnce(cur); ok {
				if _, dup := seen[d]; !dup {
					if matchesSensitiveForm(d, skipKeywords, stats) {
						return true
					}
					seen[d] = struct{}{}
					forms = append(forms, d)
					if len(forms) >= maxDecodeForms {
						break
					}
				}
			}
			if stats != nil {
				stats.steps += len(cur)
			}
			if u, ok := jsonUnescapeOnce(cur); ok {
				if _, dup := seen[u]; !dup {
					if matchesSensitiveForm(u, skipKeywords, stats) {
						return true
					}
					seen[u] = struct{}{}
					forms = append(forms, u)
				}
			}
		}
		frontierStart = frontierEnd
	}
	return false
}

// isSensitiveEnv reports whether an environment variable should be masked by
// default based on either its name or the shape of its value.
func isSensitiveEnv(name, value string) bool {
	upper := strings.ToUpper(name)
	for _, tok := range sensitiveNameTokens {
		if strings.Contains(upper, tok) {
			return true
		}
	}
	return isSensitiveEnvValue(value, isBasicNonSecretEnvName(upper))
}

func isSensitiveEnvValue(value string, skipKeywords bool) bool {
	v := strings.TrimSpace(value)
	if len(v) == 0 {
		return false
	}
	if hasSensitiveValuePattern(v, skipKeywords) {
		return true
	}
	if len(v) < 6 {
		return false
	}
	for _, p := range sensitiveValuePrefixes {
		if strings.HasPrefix(v, p) {
			return true
		}
	}
	if strings.HasPrefix(strings.ToLower(v), "basic ") {
		return true
	}
	if strings.HasPrefix(v, "AIza") && len(v) >= 30 {
		return true
	}
	if awsAccessKeyRe.MatchString(v) {
		return true
	}
	if jwtValueRe.MatchString(v) {
		return true
	}
	return looksLikeRandomSecret(v)
}

// looksLikeRandomSecret detects high-entropy token/key strings while avoiding
// false positives on file paths, PATH lists, URLs without credentials, and
// human-readable flags.
func looksLikeRandomSecret(v string) bool {
	n := len(v)
	if n < 24 || n > 512 {
		return false
	}
	if v[0] == '/' || v[0] == '~' || v[0] == '.' {
		return false
	}
	var hasLower, hasUpper, hasDigit, hasSym bool
	hexOnly := true
	var freq [128]int
	for i := 0; i < n; i++ {
		c := v[i]
		if c >= 128 {
			return false
		}
		switch {
		case c >= 'a' && c <= 'z':
			hasLower = true
			if c > 'f' {
				hexOnly = false
			}
		case c >= 'A' && c <= 'Z':
			hasUpper = true
			if c > 'F' {
				hexOnly = false
			}
		case c >= '0' && c <= '9':
			hasDigit = true
		case c == '+' || c == '/' || c == '=' || c == '_' || c == '-':
			hasSym = true
			hexOnly = false
		default:
			return false
		}
		freq[c]++
	}
	// Reject path-like values containing multiple slashes.
	if strings.Count(v, "/") >= 2 && !hasSym {
		return false
	}
	var entropy float64
	fn := float64(n)
	for _, count := range freq {
		if count == 0 {
			continue
		}
		p := float64(count) / fn
		entropy -= p * math.Log2(p)
	}
	if hexOnly {
		return n >= 32 && hasDigit && (hasLower || hasUpper) && entropy >= 3.0
	}
	classes := 0
	if hasLower {
		classes++
	}
	if hasUpper {
		classes++
	}
	if hasDigit {
		classes++
	}
	if hasSym {
		classes++
	}
	if classes >= 3 && entropy >= 3.5 {
		return true
	}
	if n >= 32 && hasDigit && (hasLower || hasUpper) && entropy >= 3.5 {
		return true
	}
	return false
}

// maskEnvPreview returns a fixed bullet placeholder for non-empty sensitive
// values without revealing any prefix characters (which could expose short
// credentials such as "a:b@" at the start of a DSN).
func maskEnvPreview(value string) string {
	if value == "" {
		return ""
	}
	return "••••••••"
}

// parseProcEnviron parses NUL-delimited KEY=VALUE records from Linux
// /proc/<pid>/environ, deduplicating by name (last wins) and sorting by name.
func parseProcEnviron(raw []byte, truncated bool) []rawEnvVar {
	if len(raw) == 0 {
		return nil
	}
	// When the read hit our byte cap without a trailing NUL, drop the partial
	// last record so we never emit a half-cut value.
	if truncated && raw[len(raw)-1] != 0 {
		if idx := bytes.LastIndexByte(raw, 0); idx >= 0 {
			raw = raw[:idx]
		} else {
			return nil
		}
	}
	byName := make(map[string]string)
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		eq := bytes.IndexByte(entry, '=')
		if eq <= 0 {
			continue
		}
		name := string(entry[:eq])
		if !utf8.ValidString(name) {
			name = strings.ToValidUTF8(name, "\uFFFD")
		}
		val := string(entry[eq+1:])
		if !utf8.ValidString(val) {
			val = strings.ToValidUTF8(val, "\uFFFD")
		}
		byName[name] = val
	}
	out := make([]rawEnvVar, 0, len(byName))
	for k, v := range byName {
		out = append(out, rawEnvVar{Name: k, Value: v})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out
}

func isKnownAgentProc(e herdr.ProcessEntry) bool {
	check := func(s string) bool {
		switch strings.ToLower(path.Base(strings.TrimSpace(s))) {
		case "claude", "codex", "agy", "opencode", "devin":
			return true
		}
		return false
	}
	if check(e.Name) {
		return true
	}
	if len(e.Argv) > 0 && check(e.Argv[0]) {
		return true
	}
	return false
}

// pickHerdrEnvPID chooses the best foreground process PID from herdr's
// pane.process_info: prefers a known agent process, then a non-shell foreground
// process, then the first foreground process, and finally the pane shell PID.
func pickHerdrEnvPID(pi *herdr.PaneProcessInfoResult) (int, string) {
	if pi == nil {
		return 0, ""
	}
	for _, pr := range pi.ForegroundProcesses {
		if pr.PID > 0 && isKnownAgentProc(pr) {
			return pr.PID, pr.Name
		}
	}
	for _, pr := range pi.ForegroundProcesses {
		if pr.PID > 0 && pr.PID != pi.ShellPID && !ocShells[strings.ToLower(pr.Name)] {
			return pr.PID, pr.Name
		}
	}
	for _, pr := range pi.ForegroundProcesses {
		if pr.PID > 0 {
			return pr.PID, pr.Name
		}
	}
	if pi.ShellPID > 0 {
		return pi.ShellPID, ""
	}
	return 0, ""
}

const procEnvironByPIDScript = `pid=$1
if [ ! -d /proc/self ]; then
  printf 'ERR\tno_proc\n'
  exit 0
fi
case "$pid" in ''|*[!0-9]*) printf 'ERR\tno_process\n'; exit 0;; esac
if [ ! -e "/proc/$pid/environ" ]; then
  printf 'ERR\tno_process\n'
  exit 0
fi
if ! head -c 1 "/proc/$pid/environ" >/dev/null 2>&1; then
  if [ -e "/proc/$pid/environ" ]; then
    printf 'ERR\tpermission_denied\n'
  else
    printf 'ERR\tno_process\n'
  fi
  exit 0
fi
comm=$(tr '\n\r\t' '   ' <"/proc/$pid/comm" 2>/dev/null | head -c 64)
printf 'OK\t%s\t%s\n' "$pid" "$comm"
head -c 262144 "/proc/$pid/environ" 2>/dev/null`

func procEnvironByPIDCommand(pid int) string {
	return "sh -c " + sshx.QuoteShell(procEnvironByPIDScript) + " corral " + sshx.QuoteShell(strconv.Itoa(pid))
}

const tmuxPaneEnvironScript = `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
sess=$1 pane=$2 sess_id=$3
if [ ! -d /proc/self ]; then
  printf 'ERR\tno_proc\n'
  exit 0
fi
if [ -z "$sess" ]; then
  printf 'ERR\tpane_not_in_session\n'
  exit 0
fi
cur_id=$(tmux -u display-message -p -t "=$sess:" '#{session_id}' 2>/dev/null)
case "$cur_id" in
  '$'[0-9]*)
    case "${cur_id#\$}" in
      ''|*[!0-9]*) cur_id="" ;;
    esac
    ;;
  *) cur_id="" ;;
esac
if [ -n "$sess_id" ]; then
  if [ "$cur_id" != "$sess_id" ]; then
    if [ -n "$cur_id" ] || tmux -u has-session -t "$sess_id" >/dev/null 2>&1; then
      printf 'ERR\tsession_mismatch\n'
    else
      printf 'ERR\tno_process\n'
    fi
    exit 0
  fi
  target="$sess_id:"
else
  if [ -z "$cur_id" ]; then
    printf 'ERR\tno_process\n'
    exit 0
  fi
  printf 'ID\t%s\n' "$cur_id"
  target="$cur_id:"
fi
if [ -n "$pane" ]; then
  if ! panes=$(tmux -u list-panes -s -t "$target" -F '#{pane_id}' 2>/dev/null); then
    printf 'ERR\tno_process\n'
    exit 0
  fi
  in_sess=0
  while IFS= read -r p; do
    if [ "$p" = "$pane" ]; then
      in_sess=1
      break
    fi
  done <<EOF
$panes
EOF
  if [ "$in_sess" != 1 ]; then
    printf 'ERR\tpane_not_in_session\n'
    exit 0
  fi
  pane_pid=$(tmux -u display-message -p -t "$pane" '#{pane_pid}' 2>/dev/null)
else
  pane_pid=$(tmux -u display-message -p -t "$target" '#{pane_pid}' 2>/dev/null)
fi
case "$pane_pid" in ''|*[!0-9]*) printf 'ERR\tno_process\n'; exit 0;; esac
fg=$(sed 's/.*) //' "/proc/$pane_pid/stat" 2>/dev/null | cut -d' ' -f6)
case "$fg" in ''|0|*[!0-9]*) fg=$(ps -o tpgid= -p "$pane_pid" 2>/dev/null | tr -d ' ');; esac
pid=$pane_pid
case "$fg" in
  ''|0|*[!0-9]*) ;;
  *)
    if [ -e "/proc/$fg/environ" ]; then
      pid=$fg
    else
      for st in /proc/[0-9]*/stat; do
        pg=$(sed 's/.*) //' "$st" 2>/dev/null | cut -d' ' -f3)
        if [ "$pg" = "$fg" ]; then
          cand=${st#/proc/}; cand=${cand%/stat}
          if [ -e "/proc/$cand/environ" ]; then
            pid=$cand
            break
          fi
        fi
      done
    fi
    ;;
esac
if [ ! -e "/proc/$pid/environ" ]; then
  printf 'ERR\tno_process\n'
  exit 0
fi
if ! head -c 1 "/proc/$pid/environ" >/dev/null 2>&1; then
  if [ -e "/proc/$pid/environ" ]; then
    printf 'ERR\tpermission_denied\n'
  else
    printf 'ERR\tno_process\n'
  fi
  exit 0
fi
comm=$(tr '\n\r\t' '   ' <"/proc/$pid/comm" 2>/dev/null | head -c 64)
printf 'OK\t%s\t%s\n' "$pid" "$comm"
head -c 262144 "/proc/$pid/environ" 2>/dev/null`

func tmuxPaneEnvironCommand(tmuxSession, tmuxSessionID, pane string) string {
	if !tmuxSessionIDRE.MatchString(tmuxSessionID) {
		tmuxSessionID = ""
	}
	return "sh -c " + sshx.QuoteShell(tmuxPaneEnvironScript) + " corral " + sshx.QuoteShell(tmuxSession) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(tmuxSessionID)
}

func parseEnvExecOutput(out []byte) (sessionID string, pid int, procName string, vars []rawEnvVar, reason string, truncated bool) {
	if bytes.HasPrefix(out, []byte("ID\t")) {
		if nl := bytes.IndexByte(out, '\n'); nl >= 0 {
			cand := strings.TrimSpace(string(out[3:nl]))
			if tmuxSessionIDRE.MatchString(cand) {
				sessionID = cand
			}
			out = out[nl+1:]
		}
	}
	nl := bytes.IndexByte(out, '\n')
	if nl < 0 {
		return sessionID, 0, "", nil, "read_failed", false
	}
	header := strings.TrimRight(string(out[:nl]), "\r")
	payload := out[nl+1:]
	parts := strings.Split(header, "\t")
	if len(parts) < 2 {
		return sessionID, 0, "", nil, "read_failed", false
	}
	switch parts[0] {
	case "ERR":
		return sessionID, 0, "", nil, strings.TrimSpace(parts[1]), false
	case "OK":
		pid, _ = strconv.Atoi(strings.TrimSpace(parts[1]))
		if len(parts) >= 3 {
			procName = truncate(strings.TrimSpace(parts[2]), 64)
		}
		truncated = len(payload) >= paneEnvReadMax
		vars = parseProcEnviron(payload, truncated)
		return sessionID, pid, procName, vars, "", truncated
	default:
		return sessionID, 0, "", nil, "read_failed", false
	}
}

// readPaneEnviron locates the foreground process of sess/pane and reads its
// /proc/<pid>/environ on demand.
func (s *Server) readPaneEnviron(ctx context.Context, sess *term.Session, pane string) (pid int, procName string, vars []rawEnvVar, reason string, truncated bool, err error) {
	switch sess.Persistence {
	case "herdr":
		if !herdrPaneID.MatchString(pane) {
			return 0, "", nil, "bad_pane", false, nil
		}
		socket := s.herdrSocks.get(sess.ID)
		if socket == "" {
			return 0, "", nil, "herdr_unavailable", false, nil
		}
		pi := s.herdrPaneProcessInfo(ctx, sess, socket, pane)
		targetPID, fallbackName := pickHerdrEnvPID(pi)
		if targetPID <= 0 {
			return 0, "", nil, "no_process", false, nil
		}
		res, execErr := sshx.Exec(ctx, sess.Client(), procEnvironByPIDCommand(targetPID), paneEnvReadMax+512, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
		if execErr != nil {
			return 0, "", nil, "", false, execErr
		}
		_, pid, procName, vars, reason, truncated = parseEnvExecOutput(res.Out)
		if procName == "" {
			procName = truncate(fallbackName, 64)
		}
		return pid, procName, vars, reason, truncated, nil

	case "tmux":
		if pane != "" && !tmuxPaneID.MatchString(pane) {
			return 0, "", nil, "bad_pane", false, nil
		}
		boundID := sess.CurrentTmuxSessionID()
		res, execErr := sshx.Exec(ctx, sess.Client(), tmuxPaneEnvironCommand(sess.CurrentTmuxSession(), boundID, pane), paneEnvReadMax+512, sshx.ExecOptions{Interactive: true, AwaitSlot: 2 * time.Second})
		if execErr != nil {
			return 0, "", nil, "", false, execErr
		}
		var discoveredID string
		discoveredID, pid, procName, vars, reason, truncated = parseEnvExecOutput(res.Out)
		if boundID == "" {
			if discoveredID == "" {
				if reason == "" {
					reason = "no_process"
				}
				return 0, "", nil, reason, false, nil
			}
			if !sess.SetTmuxSessionID(discoveredID) {
				return 0, "", nil, "session_mismatch", false, nil
			}
		} else if discoveredID != "" && !sess.SetTmuxSessionID(discoveredID) {
			return 0, "", nil, "session_mismatch", false, nil
		}
		return pid, procName, vars, reason, truncated, nil

	default:
		return 0, "", nil, "unsupported_mode", false, nil
	}
}

func (s *Server) apiPaneEnv(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	pane := strings.TrimSpace(r.URL.Query().Get("pane"))
	if sess.Persistence == "herdr" && !herdrPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid pane id is required")
		return
	}
	if sess.Persistence == "tmux" && pane != "" && !tmuxPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid tmux pane id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), paneEnvExecLimit)
	defer cancel()

	pid, procName, rawVars, reason, truncated, err := s.readPaneEnviron(ctx, sess, pane)
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			writeSSHChannelsBusy(w)
			return
		}
		writeError(w, http.StatusBadGateway, "env_failed", "could not read process environment")
		return
	}

	resp := paneEnvResponse{
		Mode:      sess.Persistence,
		Pane:      pane,
		PID:       pid,
		Process:   procName,
		Reason:    reason,
		Truncated: truncated,
		Vars:      make([]paneEnvVar, 0, len(rawVars)),
	}
	for _, v := range rawVars {
		if isSensitiveEnv(v.Name, v.Value) {
			resp.Vars = append(resp.Vars, paneEnvVar{
				Name:      v.Name,
				Sensitive: true,
				Preview:   maskEnvPreview(v.Value),
			})
		} else {
			resp.Vars = append(resp.Vars, paneEnvVar{
				Name:      v.Name,
				Sensitive: false,
				Value:     v.Value,
			})
		}
	}
	writeJSON(w, http.StatusOK, resp)
}

func validEnvName(name string) bool {
	if name == "" || len(name) > paneEnvMaxNameLen || !utf8.ValidString(name) {
		return false
	}
	return !strings.ContainsAny(name, "=\x00\r\n")
}

func (s *Server) apiPaneEnvReveal(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var req paneEnvRevealRequest
	if err := readJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid request body")
		return
	}
	pane := strings.TrimSpace(req.Pane)
	name := req.Name
	if !validEnvName(name) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid variable name")
		return
	}
	if sess.Persistence == "herdr" && !herdrPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "valid pane id is required")
		return
	}
	if sess.Persistence == "tmux" && pane != "" && !tmuxPaneID.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid tmux pane id")
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), paneEnvExecLimit)
	defer cancel()

	pid, _, rawVars, reason, _, err := s.readPaneEnviron(ctx, sess, pane)
	if err != nil {
		if errors.Is(err, sshx.ErrBusy) {
			writeSSHChannelsBusy(w)
			return
		}
		writeError(w, http.StatusBadGateway, "env_failed", "could not read process environment")
		return
	}
	if reason != "" {
		writeError(w, http.StatusConflict, reason, "process environment is unavailable")
		return
	}
	for _, v := range rawVars {
		if v.Name == name {
			// Audit every reveal of an environment variable without recording its value.
			detail := map[string]any{
				"session": sess.ID,
				"name":    name,
				"pid":     pid,
			}
			if pane != "" {
				detail["pane"] = pane
			}
			s.audit(r, "session.env_reveal", sess.HostName, detail)
			writeJSON(w, http.StatusOK, paneEnvRevealResponse{
				Name:  v.Name,
				Value: v.Value,
				PID:   pid,
			})
			return
		}
	}
	writeError(w, http.StatusNotFound, "not_found", "environment variable not found")
}
