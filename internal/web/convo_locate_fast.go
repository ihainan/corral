package web

import (
	"bytes"
	"context"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/term"
)

// maxFastLocatePIDs caps how many foreground PIDs from herdr pane.process_info
// the single-pane fast path inspects directly before falling back to full scan.
const maxFastLocatePIDs = 16

// herdrFastMode describes how a single herdr pane should be located after
// consulting pane.process_info and herdr's cached pane snapshot.
type herdrFastMode int

const (
	herdrFastFull     herdrFastMode = iota // run the full host scan
	herdrFastPIDs                          // inspect only the pane's process subtree rooted at shellPID
	herdrFastOpenCode                      // run OpenCode-only scan (needs serve + sibling OpenCodes)
)

type herdrFastPlan struct {
	mode     herdrFastMode
	shellPID int
	pids     []int
}

// claudeConfigDirSh defines claude_cfg_dir <procdir>, setting clcfg to the
// process's CLAUDE_CONFIG_DIR (reading only that variable from <procdir>/environ
// with NULs turned into newlines and embedded newlines into \001 so another
// variable's value cannot forge it). Relative paths are resolved against
// <procdir>/cwd; unset, unreadable, or malformed values fall back to
// "$HOME/.claude". Existing directories are canonicalized via pwd -P.
const claudeConfigDirSh = `
claude_cfg_dir() {
  clcfg="$HOME/.claude"
  _clraw=$(tr '\0\n' '\n\001' <"$1/environ" 2>/dev/null | awk '/^CLAUDE_CONFIG_DIR=/{sub(/^CLAUDE_CONFIG_DIR=/,""); print; exit}')
  if [ -n "$_clraw" ]; then
    [ -n "${_clsoh:-}" ] || _clsoh=$(printf '\001')
    case $_clraw in
    *"$_clsoh"*) ;;
    /*) clcfg=$_clraw ;;
    *)
      _clcwd=$(readlink "$1/cwd" 2>/dev/null)
      case $_clcwd in
      /*) clcfg="$_clcwd/$_clraw" ;;
      esac ;;
    esac
    if [ "$clcfg" != "$HOME/.claude" ] && [ -d "$clcfg" ]; then
      _clabs=$(cd -- "$clcfg" 2>/dev/null && pwd -P) && [ -n "$_clabs" ] && clcfg=$_clabs
    fi
  fi
  while [ "$clcfg" != / ] && [ "${clcfg%/}" != "$clcfg" ]; do clcfg=${clcfg%/}; done
}
`

// claudeConfigDirsSh defines claude_cfg_dirs, setting cldirs to a newline-
// separated list of unique existing Claude Code config directories canonicalized
// by physical path (pwd -P, so symlinks such as $HOME/.claude -> /data/claude do
// not duplicate entries): the shell's CLAUDE_CONFIG_DIR (if set), "$HOME/.claude",
// and any CLAUDE_CONFIG_DIR set on running Claude Code processes of the current user.
// Sets cldirs_incomplete=1 when an existing config/projects directory cannot be
// entered/read or a candidate Claude process's environment/cwd cannot be read,
// so callers do not mistake a remaining copy for a sole unambiguous log.
const claudeConfigDirsSh = `claude_cfg_dirs() {
  _cdnl=$(printf '\nx'); _cdnl=${_cdnl%x}
  _cdsoh=$(printf '\001')
  cldirs=
  cldirs_incomplete=0
  _add_cldir() {
    _cd=$1
    [ -n "$_cd" ] || return 0
    case $_cd in *"$_cdnl"*|*"$_cdsoh"*) cldirs_incomplete=1; return 0;; esac
    if [ -e "$_cd" ] || [ -L "$_cd" ]; then
      [ -d "$_cd" ] || return 0
      if ! _cd=$(cd -- "$_cd" 2>/dev/null && pwd -P); then
        cldirs_incomplete=1
        return 0
      fi
      if [ -e "$_cd/projects" ]; then
        if [ ! -r "$_cd/projects" ] || [ ! -x "$_cd/projects" ]; then
          cldirs_incomplete=1
          return 0
        fi
        for _pd in "$_cd"/projects/*; do
          [ -d "$_pd" ] || continue
          if [ ! -r "$_pd" ] || [ ! -x "$_pd" ]; then
            cldirs_incomplete=1
            return 0
          fi
        done
      fi
    else
      return 0
    fi
    while [ "$_cd" != / ] && [ "${_cd%/}" != "$_cd" ]; do _cd=${_cd%/}; done
    case "$_cdnl$cldirs$_cdnl" in *"$_cdnl$_cd$_cdnl"*) ;; *) cldirs="${cldirs:+$cldirs$_cdnl}$_cd";; esac
  }
  [ -n "${CLAUDE_CONFIG_DIR:-}" ] && _add_cldir "$CLAUDE_CONFIG_DIR"
  _add_cldir "$HOME/.claude"
  for _p in /proc/[0-9]*; do
    [ -O "$_p" ] || continue
    _cm=; { IFS= read -r _cm <"$_p/comm"; } 2>/dev/null || continue
    case $_cm in
    claude) ;;
    node|bun)
      if [ ! -r "$_p/cmdline" ]; then
        [ -d "$_p" ] && cldirs_incomplete=1
        continue
      fi
      grep -q 'claude' "$_p/cmdline" 2>/dev/null || continue;;
    [0-9]*.[0-9]*)
      _exe=$(readlink "$_p/exe" 2>/dev/null)
      if [ -z "$_exe" ]; then
        [ -d "$_p" ] && cldirs_incomplete=1
        continue
      fi
      case $_exe in */claude/*) ;; *) continue;; esac;;
    *) continue;;
    esac
    if [ ! -r "$_p/environ" ]; then
      [ -d "$_p" ] && cldirs_incomplete=1
      continue
    fi
    _r=$(tr '\0\n' '\n\001' <"$_p/environ" 2>/dev/null | sed -n 's/^CLAUDE_CONFIG_DIR=//p' | head -n 1)
    case $_r in
    '') ;;
    *"$_cdsoh"*) cldirs_incomplete=1 ;;
    /*) _add_cldir "$_r" ;;
    *)
      _cw=$(readlink "$_p/cwd" 2>/dev/null)
      case $_cw in
      /*) _add_cldir "$_cw/$_r" ;;
      *) [ -d "$_p" ] && cldirs_incomplete=1 ;;
      esac ;;
    esac
  done
}
`

// parseClaudeConfigDir extracts only the CLAUDE_CONFIG_DIR variable from raw
// NUL-separated /proc/<pid>/environ bytes. Values containing control newlines
// or \001 are rejected (returning "").
func parseClaudeConfigDir(environ []byte) string {
	for _, entry := range bytes.Split(environ, []byte{0}) {
		v, ok := bytes.CutPrefix(entry, []byte("CLAUDE_CONFIG_DIR="))
		if !ok {
			continue
		}
		if bytes.ContainsAny(v, "\n\r\x01\x00") {
			return ""
		}
		return string(v)
	}
	return ""
}

// resolveClaudeConfigDir resolves a raw CLAUDE_CONFIG_DIR value into an
// absolute cleaned directory path. Unset or malformed values fall back to
// "<home>/.claude". Relative paths are resolved against cwd when cwd is
// absolute; otherwise they fall back to "<home>/.claude".
func resolveClaudeConfigDir(raw, cwd, home string) string {
	fallback := ""
	if strings.HasPrefix(home, "/") && !strings.ContainsAny(home, "\x00\n\r\x01") {
		fallback = path.Clean(home + "/.claude")
	}
	if raw == "" || strings.ContainsAny(raw, "\x00\n\r\x01") {
		return fallback
	}
	if strings.HasPrefix(raw, "/") {
		return path.Clean(raw)
	}
	if strings.HasPrefix(cwd, "/") && !strings.ContainsAny(cwd, "\x00\n\r\x01") {
		return path.Clean(cwd + "/" + raw)
	}
	return fallback
}

// claudeSessionFilePath returns "<configDir>/sessions/<pid>.json" for a valid
// absolute configDir and positive pid.
func claudeSessionFilePath(configDir string, pid int64) (string, bool) {
	if pid <= 0 {
		return "", false
	}
	clean, ok := cleanRemotePath(configDir)
	if !ok {
		return "", false
	}
	return path.Join(clean, "sessions", strconv.FormatInt(pid, 10)+".json"), true
}

// claudeProjectLogGlob returns "<configDir>/projects/*/<sid>.jsonl" for a valid
// absolute configDir and Claude session UUID.
func claudeProjectLogGlob(configDir, sid string) (string, bool) {
	if !convoUUID.MatchString(sid) {
		return "", false
	}
	clean, ok := cleanRemotePath(configDir)
	if !ok {
		return "", false
	}
	return path.Join(clean, "projects", "*", sid+".jsonl"), true
}

// claudeConfigDirFromLog extracts "<configDir>" from a Claude Code session log
// path of the form "<configDir>/projects/<project>/<sid>.jsonl". Works for both
// default "$HOME/.claude" and custom CLAUDE_CONFIG_DIR (including paths with
// spaces).
func claudeConfigDirFromLog(log string) (string, bool) {
	clean, ok := cleanRemotePath(log)
	if !ok || !strings.HasSuffix(clean, ".jsonl") {
		return "", false
	}
	projDir := path.Dir(clean)
	projectsDir := path.Dir(projDir)
	if path.Base(projectsDir) != "projects" || projDir == projectsDir {
		return "", false
	}
	cfgDir := path.Dir(projectsDir)
	if cfgDir == "" || cfgDir == "." || cfgDir == "/" {
		return "", false
	}
	return cfgDir, true
}

// claudeForkConfigDir returns the CLAUDE_CONFIG_DIR to pass explicitly when
// forking a Claude Code session whose source log is at log. Always returns the
// source directory (preferring its resolved physical path realCfg when valid),
// including for the default "~/.claude", so an inherited CLAUDE_CONFIG_DIR in
// the target tmux or herdr environment cannot redirect resume elsewhere.
func claudeForkConfigDir(log, realCfg string) string {
	cfgDir, ok := claudeConfigDirFromLog(log)
	if !ok {
		return ""
	}
	if clean, ok := cleanRemotePath(realCfg); ok {
		return clean
	}
	return cfgDir
}

// isOpenCodeProc reports whether a pane.process_info entry looks like OpenCode.
func isOpenCodeProc(e herdr.ProcessEntry) bool {
	if strings.EqualFold(e.Name, "opencode") {
		return true
	}
	if len(e.Argv) > 0 && strings.EqualFold(path.Base(e.Argv[0]), "opencode") {
		return true
	}
	return false
}

// isCodexProc reports whether a pane.process_info entry looks like Codex.
func isCodexProc(e herdr.ProcessEntry) bool {
	if strings.EqualFold(e.Name, "codex") {
		return true
	}
	if len(e.Argv) > 0 && strings.EqualFold(path.Base(e.Argv[0]), "codex") {
		return true
	}
	return false
}

// planHerdrFastLocate decides whether locating pane can use the pane's process
// subtree rooted at pi.ShellPID, should use the OpenCode-only scan, or must
// fall back to the full host scan.
//
// Codex panes never use the subtree fast path (they have locateCodexFast /
// codexFront and rely on host-wide convoClaimed.seeAlive protection).
func planHerdrFastLocate(pi *herdr.PaneProcessInfoResult, pane string, hp herdr.Pane, hasHP bool) herdrFastPlan {
	fallback := herdrFastPlan{mode: herdrFastFull}
	if hasHP && isOpenCodeAgent(hp.Agent) {
		fallback.mode = herdrFastOpenCode
	}
	if hasHP && isCodexAgent(hp.Agent) {
		return herdrFastPlan{mode: herdrFastFull}
	}
	if pi == nil || pane == "" || (pi.PaneID != "" && pi.PaneID != pane) || pi.ShellPID <= 0 || len(pi.ForegroundProcesses) == 0 {
		return fallback
	}
	for _, pr := range pi.ForegroundProcesses {
		if isCodexProc(pr) {
			return herdrFastPlan{mode: herdrFastFull}
		}
		if isOpenCodeProc(pr) {
			return herdrFastPlan{mode: herdrFastOpenCode}
		}
	}
	// If only the pane's shell is in the foreground, an agent may be suspended
	// or backgrounded in this pane; fall back to full scan to preserve exact
	// behavior with the full process scan.
	if len(pi.ForegroundProcesses) == 1 {
		only := pi.ForegroundProcesses[0]
		if only.PID == pi.ShellPID && ocShells[only.Name] {
			return fallback
		}
	}
	var pids []int
	for _, pr := range pi.ForegroundProcesses {
		if pr.PID <= 0 {
			continue
		}
		if !slices.Contains(pids, pr.PID) {
			pids = append(pids, pr.PID)
		}
	}
	if len(pids) == 0 || len(pids) > maxFastLocatePIDs {
		return fallback
	}
	slices.Sort(pids)
	return herdrFastPlan{mode: herdrFastPIDs, shellPID: pi.ShellPID, pids: pids}
}

// convoLocatePidsCommand is convoLocateCommand scoped to the pane's process
// subtree rooted at shellPID (verifying all fg pids are inside it and that the
// subtree contains strictly one candidate agent process).
func convoLocatePidsCommand(shellPID int, pids []int) string {
	var parts []string
	for _, p := range pids {
		if p > 0 {
			parts = append(parts, strconv.Itoa(p))
		}
	}
	return convoLocateWith("herdr", "", "", "", "tree:"+strconv.Itoa(shellPID)+":"+strings.Join(parts, ","))
}

// ancOwnedByHerdrShell reports whether anc reaches a herdr ancestor and every
// process between the agent and herdr is a known shell (no outer agent or
// wrapper that could own the pane instead in a full scan).
func ancOwnedByHerdrShell(anc []convoAnc) bool {
	for _, a := range anc {
		if a.comm == "herdr" {
			return true
		}
		if !ocShells[a.comm] {
			return false
		}
	}
	return false
}

// fastLocateAccept validates the output of convoLocatePidsCommand for a single
// herdr pane. It returns (proc, true) only when the pane's process subtree was
// verified completely and contains strictly one candidate Claude/Agy agent
// whose result is guaranteed to match a full host scan; otherwise (_, false)
// tells the caller to fall back to the full scan.
func fastLocateAccept(l convoLocated, pane, socket, want string) (*convoProc, bool) {
	if l.noProc {
		return nil, true
	}
	if l.fastIncomplete || len(l.procs) != 1 {
		return nil, false
	}
	p := pickConvoProc(l.procs, "herdr", pane, socket, "", "", want)
	if p == nil {
		return nil, false
	}
	// Only Claude Code and Agy use the subtree fast path; Codex, OpenCode,
	// Devin, and any unknown agent fall back to their dedicated/full paths.
	if p.agent != "claude" && p.agent != "agy" {
		return nil, false
	}
	if !ancOwnedByHerdrShell(p.anc) {
		return nil, false
	}
	// If herdr reports a specific session for this pane and the process holds a
	// different one, fall back to full scan.
	if want != "" && p.sid != want {
		return nil, false
	}
	return p, true
}

// herdrPaneProcessInfo queries herdr's pane.process_info for pane over the
// existing SSH-forwarded Unix socket. Any error (unsupported method on older
// herdr, timeout, dial failure) returns nil so the caller falls back cleanly.
func (s *Server) herdrPaneProcessInfo(ctx context.Context, sess *term.Session, socket, pane string) *herdr.PaneProcessInfoResult {
	if sess == nil || socket == "" || !herdrPane.MatchString(pane) {
		return nil
	}
	pctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	cl, err := herdr.DialWait(pctx, sess.Client(), socket, 2*time.Second)
	if err != nil {
		return nil
	}
	defer cl.Close()
	pi, err := cl.PaneProcessInfo(pctx, pane)
	if err != nil {
		return nil
	}
	return pi
}
