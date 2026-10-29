package web

// The conversation view: Claude Code's own log of the session running in the
// terminal's focused pane (herdr) or active pane (tmux), read over the
// session's SSH connection and shown as turns. Nothing is installed on the
// host: the pane is matched to its Claude Code process by the environment
// herdr and tmux give the processes they start, the process to its session
// by the file Claude Code keeps for each running process
// (~/.claude/sessions/<pid>.json), and the session to its log by name.
//
// Only the session's owner reads it. Someone watching a shared session sees
// the screen; the log holds the whole history, which is more than that.

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io/fs"
	"math"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

const (
	convoLocateTTL  = 10 * time.Second // a pane's binding to a log is checked again after this
	convoTitleSlack = 64 << 10         // the name is looked for this far into what is read too: a row of it the read's start cuts is whole there
	convoIdle       = 10 * time.Minute // a log nobody reads is forgotten after this
	convoTail       = 4 << 20          // bytes read from the end of a log when it is first opened
	convoStep       = 2 << 20          // bytes read at most by one request after that
	convoOlderStep  = 4 << 20          // bytes read from before what was read, when asked for (?older=1)
	convoLocateMax  = 256 << 10        // output of the locating script
	convoStatesMax  = 64               // conversation states kept at once
	convoReadLimit  = 15 * time.Second // a read the host has not answered by then is given up
	convoLongMax    = 1 << 20          // a line longer than a read, once its images' data is left out, kept up to this
	convoLongScan   = 32 << 20         // a line longer than an older part is read whole up to this (Codex's messages with images run to 11 MB)
)

// b64Strip leaves out of a line, written to it in parts, the base64 an
// image carries: a "data" string longer than b64Run that is base64 alone
// (Claude Code's image blocks and tool results), or the base64 of an
// "image_url" data address ("data:image/png;base64,…": Codex's), emptied of
// it. Any other string is kept as it was, so what is left stays the JSON it
// was: only base64 characters, which need no escaping, are ever left out.
type b64Strip struct {
	inData   bool   // in such a string, while it may be base64
	url      bool   // (an image_url: its "data:…;base64," first)
	held     []byte // what of it is held, until it is known whether it is
	dropping bool   // it is: left out, to its closing quote
}

const b64Run = 4096

var (
	dataKey = []byte(`"data":"`)
	urlKey  = []byte(`"image_url":"`)
)

func isB64(c byte) bool {
	return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '='
}

func (b *b64Strip) write(dst, src []byte) []byte {
	for len(src) > 0 {
		if b.inData && b.url {
			// (Its address's head, kept, to ";base64,"; not that, a string kept as it is.)
			c := src[0]
			if c == '"' || c == '\\' || len(b.held) > 100 {
				dst = append(dst, b.held...)
				b.inData, b.url, b.held = false, false, b.held[:0]
				continue
			}
			b.held = append(b.held, c)
			src = src[1:]
			if bytes.HasSuffix(b.held, []byte(";base64,")) && bytes.HasPrefix(b.held, []byte("data:")) {
				dst = append(dst, b.held...)
				b.url, b.held = false, b.held[:0]
			}
			continue
		}
		if b.inData {
			n := 0
			for n < len(src) && isB64(src[n]) {
				n++
			}
			if !b.dropping {
				b.held = append(b.held, src[:n]...)
				if len(b.held) > b64Run {
					b.held, b.dropping = b.held[:0], true
				}
			}
			src = src[n:]
			if len(src) == 0 {
				return dst
			}
			// (Another character: the string's end, or not base64 after all.)
			if !b.dropping {
				dst = append(dst, b.held...)
			}
			b.inData, b.dropping, b.held = false, false, b.held[:0]
			continue
		}
		i := bytes.IndexByte(src, '"')
		if i < 0 {
			return append(dst, src...)
		}
		dst = append(dst, src[:i+1]...)
		src = src[i+1:]
		for _, k := range [][]byte{dataKey, urlKey} {
			if bytes.HasSuffix(dst, k) && (len(dst) == len(k) || dst[len(dst)-len(k)-1] != '\\') {
				b.inData, b.url = true, bytes.Equal(k, urlKey)
			}
		}
	}
	return dst
}

// end: the line has ended.
func (b *b64Strip) end(dst []byte) []byte {
	if b.inData && !b.dropping {
		dst = append(dst, b.held...)
	}
	b.inData, b.url, b.dropping, b.held = false, false, false, b.held[:0]
	return dst
}

// convoLocateCommand prints, for each Claude Code process of the user that
// is still running, a NUL-terminated record of key=value lines: its pid, its
// start time (/proc/<pid>/stat field 22), whether it is in front on its
// terminal (its process group is the terminal's foreground one), the start
// time its session file
// records (a file left by a process that ended, whose pid was then reused,
// does not match), the session id, the log found for it and when that log
// was last written, and the herdr or
// tmux pane it runs in, the terminal its input is (/proc/<pid>/fd/0) and
// its ancestors (pid and name, nearest first). With mode "tmux" a first
// record gives the active pane of the named tmux session, its server's pid,
// the pane's terminal and its title (in one query: they go together); given
// a pane (tmuxPane, validated), a second that pane's terminal. Linux only (/proc):
// elsewhere it prints the record "noproc". Fixed script; the mode and the
// validated tmux session name are its only arguments.
func convoLocateCommand(mode, tmuxName, tmuxPane string) string {
	return convoLocateWith(mode, tmuxName, tmuxPane, "")
}

// convoLocateTimesCommand is convoLocateCommand that also says when each
// JSON log was last written by its records (logts): reading their ends,
// which the list's times ask for now and then, and nothing else does.
func convoLocateTimesCommand(mode, tmuxName string) string {
	return convoLocateWith(mode, tmuxName, "", "times")
}

func convoLocateWith(mode, tmuxName, tmuxPane, times string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
times=$4
if [ "$1" = tmux ]; then
  t=$(tmux -u display-message -p -t "=$2:" '#{pane_id} #{pid} #{pane_tty} #{pane_title}' 2>/dev/null | head -c 800 | tr -d '\n')
  printf 'tmux=%s\0' "$t"
  if [ -n "$3" ]; then
    t=$(tmux -u display-message -p -t "$3" '#{pane_tty}' 2>/dev/null | head -c 200 | tr -d '\n')
    printf 'panetty=%s\0' "$t"
  fi
fi
[ -r /proc/self/stat ] || { printf 'noproc\0'; exit 0; }
nl=$(printf '\nx'); nl=${nl%x}
btime=$(sed -n 's/^btime \([0-9]*\)$/\1/p' /proc/stat 2>/dev/null); hz=$(getconf CLK_TCK 2>/dev/null)
case $hz in ''|*[!0-9]*|0) hz=;; esac
# Codex's subcommand ($1 its pid): its first word after options and their
# values ("codex -p exec" is no exec; "codex -c x=1 exec" is), none after
# "--". Those that do not converse (exec, review, …) are no agent to type to.
codex_sub() {
  tr '\0' '\001' <"/proc/$1/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" } NR == 1 { next } skip { skip = 0; next }
    /^--$/ { exit } /^-/ { if ($0 ~ /^(-[cmpsaCi]|--(config|model|profile|sandbox|ask-for-approval|cd|image|add-dir|local-provider|remote|remote-auth-token-env|enable|disable))$/) skip = 1; next }
    { print; exit }'
}
# A process's record: what it is, where its log is, and where it runs.
rec() { # pid start fg procstart sid log agent
  env=$(tr '\0' '\n' <"/proc/$1/environ" 2>/dev/null | grep -E '^(HERDR_PANE_ID|HERDR_SOCKET_PATH|TMUX|TMUX_PANE)=' | head -c 4096)
  link_of "/proc/$1/fd/0" 200; tty=$l
  # (Read with the shell's own read: a process started for each step of
  # each of them, as many as there are, is most of what this takes.)
  anc= a=$1 i=0
  while [ "$i" -lt 40 ]; do
    ppid_of "$a"; a=$pp
    case $a in ''|0|1) break;; esac
    read_all "/proc/$a/comm" ''; c=$x
    case $c in *[!A-Za-z0-9._+-]*) c=$(printf '%s' "$c" | head -c 32 | tr -c 'A-Za-z0-9._+-' _);; esac
    anc="$anc$a:$c,"
    i=$((i + 1))
  done
  link_of "/proc/$1/cwd" 1024; cwd=$l
  # (When it was last written, asked for ("times": the list's, now and
  # then): a JSON log by its records' own times, as Claude Code touches its
  # log every hour it runs, nothing written. The latest of them near its
  # end that is not ahead of now: a record's own comes after the older ones
  # of what it holds, and is never ahead. Their fractions made nine digits
  # long, to be compared as text.)
  lw= lt=; [ -n "$6" ] && lw=$(stat -c %Y "$6" 2>/dev/null)
  if [ "$times" = times ] && [ -n "$6" ] && [ "${6%.jsonl}" != "$6" ]; then
    lt=$(tail -c 262144 "$6" 2>/dev/null | grep -ao '"timestamp":"20[0-9][0-9]-[0-9][0-9]-[0-9][0-9]T[0-9:.]*Z"' | cut -d'"' -f4 |
      awk -v now="$(date -u +%Y-%m-%dT%H:%M:%S).999999999" '{ t = $0; sub(/Z$/, "", t); if (t !~ /\./) t = t "."; i = index(t, "."); t = substr(t, 1, i) substr(substr(t, i + 1) "000000000", 1, 9) }
        t <= now && t > k { k = t; m = $0 } END { print m }')
  fi
  printf 'pid=%s\nstart=%s\nbtime=%s\nhz=%s\nfg=%s\nprocstart=%s\nsid=%s\nlog=%s\nlogmtime=%s\nlogts=%s\ntty=%s\ncwd=%s\nanc=%s\nagent=%s\n%s\0' "$1" "$2" "$btime" "$hz" "$3" "$4" "$5" "$6" "$lw" "$lt" "$tty" "$cwd" "$anc" "$7" "$env"
}
# (A file's lines joined by $2, as tr would put it in place of each
# newline, the last one's left out: x. The shell's own: a name of a
# process may have a newline in it.)
read_all() {
  x= y=
  { while IFS= read -r y; do x=$x$y$2; done; x=$x$y; } <"$1" 2>/dev/null
}
# (Its start time and whether it is in front on its terminal: st, fg. The
# fields after its name, which may have spaces and parentheses in it, read
# from after the last ") ".)
stat_of() {
  read_all "/proc/$1/stat" ' '; stat=$x
  set -f; set -- ${stat##*) }; set +f
  st=${20-}; pgrp=${3-}; tpgid=${6-}
  [ "${stat##*) }" != "$stat" ] || st=
  fg=0; [ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] && fg=1
  case $st in ''|*[!0-9]*) return 1;; esac
}
# (A process's parent, from the same file: pp.)
ppid_of() {
  pp=; read_all "/proc/$1/stat" ' '; s=$x
  case $s in *') '*) ;; *) return 0;; esac
  set -f; set -- ${s##*) }; set +f
  pp=${2-}
}
# (Where a link points, as one line of at most $2 bytes: l. The shell's
# own where it is one line and short enough to be within that however its
# characters are counted; else cut and joined as head and tr would.)
link_of() {
  l=$(readlink "$1" 2>/dev/null)
  case $l in
  *"$nl"*) l=$(printf '%s' "$l" | head -c "$2" | tr -d '\n');;
  *) [ "${#l}" -le $(($2 / 4)) ] || l=$(printf '%s' "$l" | head -c "$2");;
  esac
}
# (A JSON field of the session file's text c, as grep would find its first:
# "key" *: *, then the value up to its closing quote (str) or its digits
# after any quotes (num): v.)
json_after() {
  v=; r=${c#*\"$1\"}
  [ "$r" != "$c" ] || return 0
  while :; do case $r in ' '*) r=${r# };; *) break;; esac; done
  case $r in :*) r=${r#:};; *) return 0;; esac
  while :; do case $r in ' '*) r=${r# };; *) break;; esac; done
}
json_str() {
  json_after "$1"; [ -n "$r" ] || return 0
  case $r in '"'*) r=${r#\"}; case $r in *'"'*) v=${r%%\"*};; esac;; esac
}
json_num() {
  json_after "$1"; [ -n "$r" ] || return 0
  while :; do case $r in '"'*) r=${r#\"};; *) break;; esac; done
  v=${r%%[!0-9]*}
}
# Claude Code: each process's session file names its session.
for f in "$HOME"/.claude/sessions/*.json; do
  [ -f "$f" ] || continue
  pid=${f##*/}; pid=${pid%.json}
  case $pid in ''|*[!0-9]*) continue;; esac
  [ -r "/proc/$pid/environ" ] || continue
  stat_of "$pid" || continue
  c=$(head -c 65536 "$f" 2>/dev/null | tr -d '\n')
  json_str sessionId; sid=$v
  json_num procStart; ps=$v
  case $ps in ''|*[!0-9]*) continue;; esac
  case $sid in *[!0-9a-fA-F-]*) continue;; ????????-????-????-????-????????????) ;; *) continue;; esac
  log=
  for l in "$HOME"/.claude/projects/*/"$sid".jsonl; do
    case $l in *"$nl"*) continue;; esac
    [ -f "$l" ] && { log=$l; break; }
  done
  rec "$pid" "$st" "$fg" "$ps" "$sid" "$log" claude
done
# The user's codex and opencode processes, in one pass over /proc (by
# their names, read with the shell's own read; each kind then as before).
codexes= opencodes=
for d in /proc/[0-9]*; do
  [ -O "$d" ] || continue
  read_all "$d/comm" "$nl"
  while :; do case $x in *"$nl") x=${x%"$nl"};; *) break;; esac; done # (as $(…) would leave it)
  case $x in codex) codexes="$codexes $d";; opencode) opencodes="$opencodes $d";; esac
done
# What a log says of itself, from its head: where it was begun and when.
# (What it says of itself is in its first line's payload: the line's own
# timestamp is of when it was written, which a rewrite moves on.)
meta_of() { head -n 1 "$1" 2>/dev/null | head -c 8192 | awk 'match($0, /"payload"[ ]*:[ ]*\{/) { print substr($0, RSTART + RLENGTH) }' | grep -o "\"$2\" *: *\"[^\"]*\"" | head -n 1 | sed 's/.*"\([^"]*\)"$/\1/'; }
# Codex's app-server daemon keeps the logs of the conversations it runs (a
# Codex begun by the npm package talks to it and keeps none open of its
# own): each log it holds, with where that conversation was begun and when,
# so that the Codex in a pane can be told which of them is its own.
for d in $codexes; do
  case $(codex_sub "${d#/proc/}") in app-server) ;; *) continue;; esac
  for fd in "$d"/fd/*; do
    l=$(readlink "$fd" 2>/dev/null) || continue
    case $l in *"$nl"*) continue;; "$HOME"/.codex/sessions/*/rollout-*.jsonl) ;; *) continue;; esac
    printf 'cand=%s\ncandcwd=%s\ncandts=%s\ncandmtime=%s\0' "$l" "$(meta_of "$l" cwd)" "$(meta_of "$l" timestamp)" "$(stat -c %Y "$l" 2>/dev/null)"
  done
done
# Codex: a process of the user's named codex, its log the session file it
# has open (rollout-<time>-<session id>.jsonl).
# (Not one run with exec, as its arguments or its log's head say: no
# conversation to type into. Of several logs open, the one written last.)
for d in $codexes; do
  pid=${d#/proc/}
  sub=$(codex_sub "$pid")
  case $sub in exec|e|exec-server|review|apply|a|login|logout|mcp|mcp-server|app-server|completion|sandbox|debug|cloud|features) continue;; esac
  log= any=
  for fd in "$d"/fd/*; do
    l=$(readlink "$fd" 2>/dev/null) || continue
    case $l in *"$nl"*) continue;; */rollout-*.jsonl) any=1;; esac
    case $l in "$HOME"/.codex/sessions/*/rollout-*.jsonl) { [ -z "$log" ] || [ "$l" -nt "$log" ]; } && log=$l;; esac
  done
  if [ -n "$log" ]; then
    # (No subcommand to go by: its log's head says how it was begun.)
    [ -z "$sub" ] && head -c 4096 "$log" 2>/dev/null | grep -q '"originator": *"codex_exec"' && continue
    sid=$(printf '%s' "${log%.jsonl}" | grep -o '[0-9a-fA-F]\{8\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{4\}-[0-9a-fA-F]\{12\}$')
    [ -n "$sid" ] || continue
  else
    # (None open: a new conversation, Codex beginning its log with the
    # first message. Its session is not known till then: the zero id
    # stands for it. Only the conversing one, run with no subcommand; not
    # one with a log elsewhere, CODEX_HOME say.)
    [ -z "$sub" ] && [ -z "$any" ] || continue
    sid=00000000-0000-0000-0000-000000000000
  fi
  stat_of "$pid" || continue
  rec "$pid" "$st" "$fg" "$st" "$sid" "$log" codex
done
# OpenCode: a process of the user's named opencode, its log the database it
# has open (…/opencode/opencode.db: all its sessions; which is this one's is
# told by herdr, or by where it runs). Not one that does not converse
# (opencode run, serve, …): its first word that is no option.
for d in $opencodes; do
  pid=${d#/proc/}
  sub=$(tr '\0' '\001' <"$d/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" } NR == 1 { next } skip { skip = 0; next } /^--$/ { exit }
      /^(-[msp]|--(model|session|prompt|agent|port|hostname|log-level|mdns-domain|cors))$/ { skip = 1; next }
      /^(run|serve|web|acp|mcp|auth|agent|upgrade|uninstall|models|stats|export|import|github|pr|debug|session|completion|generate|attach)$/ { print; exit }')
  case $sub in run|serve|web|acp|mcp|auth|agent|upgrade|uninstall|models|stats|export|import|github|pr|debug|session|completion|generate|attach) continue;; esac
  log=
  for fd in "$d"/fd/*; do
    l=$(readlink "$fd" 2>/dev/null) || continue
    case $l in *"$nl"*) continue;; */opencode/opencode.db) log=$l; break;; esac
  done
  [ -n "$log" ] || continue
  stat_of "$pid" || continue
  rec "$pid" "$st" "$fg" "$st" 00000000-0000-0000-0000-000000000000 "$log" opencode
done
exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(tmuxName) + " " + sshx.QuoteShell(tmuxPane) + " " + sshx.QuoteShell(times)
}

// convoScreenCommand prints the pane's screen as it is now. Fixed script;
// the mode, the validated pane and the herdr socket are its arguments.
func convoScreenCommand(mode, pane, socket string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
if [ "$1" = tmux ]; then tmux -u capture-pane -p -t "$2" 2>/dev/null 9>&-
else HERDR_SOCKET_PATH=$3 herdr pane read "$2" --source visible 2>/dev/null 9>&-; fi`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(pane) + " " + sshx.QuoteShell(socket)
}

// convoTailsCommand prints the end of each log, as "<path>\n<bytes>\n<the
// end of it>" NUL-terminated records. Fixed script; the paths, already
// matched against what a daemon keeps, are its arguments.
func convoTailsCommand(logs []string) string {
	script := `for l in "$@"; do
  [ -f "$l" ] || continue
  t=$(tail -c 16384 "$l" 2>/dev/null) || continue
  printf '%s\n%s\0' "$l" "$t"
done
exit 0`
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral"
	for _, l := range logs {
		cmd += " " + sshx.QuoteShell(l)
	}
	return cmd
}

type convoProc struct {
	pid, start    int64
	fg            bool  // in front on its terminal: what is typed there reaches it
	started       int64 // when it started, unix milliseconds (0 when not known; within the second the host's boot time is known to)
	wrote         int64 // when its log was last written, unix milliseconds (0: not known)
	wroteTold     bool  // wrote is its last record's time (not the file's)
	sid, log      string
	agent         string // "claude" (Claude Code) or "codex"
	herdrPane     string
	herdrSocket   string
	tmuxSocket    string // (TMUX is "<socket>,<server pid>,<session index>")
	tmuxServerPID string
	tmuxPane      string
	tty           string     // what its input is ("/dev/pts/3"; a pipe, or "", when it has no terminal)
	cwd           string     // where it runs
	anc           []convoAnc // its parent, the parent's parent… (to 40, not init)
}

type convoAnc struct {
	pid  int64
	comm string
}

// convoCand is a log Codex's app-server daemon holds: a conversation it
// runs for some Codex of the user's, which keeps none open itself.
type convoCand struct {
	log, cwd string
	began    int64 // (unix milliseconds; 0 when its head did not say)
	wrote    int64 // when it was last written to (unix milliseconds; 0 when not known)
}

// procWrote is when the process last wrote to its log, as the list of
// agents is told when each was last at work (see agentTimes.wrote).
//
// A Codex begun by the npm package keeps no log open: its app-server
// daemon holds the logs of the conversations it runs, so the process
// itself has none to go by. It is told instead by the last written of the
// conversations that daemon holds which were begun where this Codex runs
// and after it started — as its own conversation is picked out (see
// daemonLog). A daemon holds the logs of conversations long over as well,
// and one of those says nothing of what this Codex has been at. Where
// several Codex run in one place this is one time for all of them: enough
// to say what day they were last at, which is all the list asks of it.
func procWrote(p convoProc, cands []convoCand) int64 {
	if p.wrote > 0 || p.agent != "codex" || p.cwd == "" || p.started <= 0 {
		return p.wrote
	}
	var wrote int64
	for _, c := range cands {
		if c.cwd != p.cwd || c.began <= 0 || c.began+convoBeganSlack < p.started {
			continue
		}
		if c.wrote > wrote {
			wrote = c.wrote
		}
	}
	return wrote
}

// unescapeJSON reads a string as JSON wrote it (a path with a quote, a
// backslash or a newline in it); what is no JSON string is kept as it is.
func unescapeJSON(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var out string
	if json.Unmarshal([]byte(`"`+s+`"`), &out) == nil {
		return out
	}
	return s
}

// convoLocated is what convoLocateCommand found.
type convoLocated struct {
	procs                          []convoProc
	cands                          []convoCand
	tmuxActive, tmuxPID, tmuxTitle string
	tmuxTTY                        string // (the active pane's terminal)
	paneTTY                        string // (the terminal of the pane asked of)
	noProc                         bool   // the host has no /proc
}

var (
	convoUUID = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	herdrPane = regexp.MustCompile(`^[A-Za-z0-9:_.-]{1,64}$`)
	tmuxPane  = regexp.MustCompile(`^%[0-9]{1,9}$`)
)

// parseConvoLocate reads convoLocateCommand's output: the processes whose
// start time matches their session file, the tmux pane and server pid if
// asked, and whether the host has /proc at all.
func parseConvoLocate(out string) (l convoLocated) {
	for _, rec := range strings.Split(out, "\x00") {
		if rec == "noproc" {
			l.noProc = true
			continue
		}
		if strings.HasPrefix(rec, "cand=") {
			var c convoCand
			var ts string
			for _, line := range strings.Split(rec, "\n") {
				k, v, ok := strings.Cut(line, "=")
				if !ok {
					continue
				}
				switch k {
				case "cand":
					c.log = v
				case "candcwd":
					c.cwd = v
				case "candts":
					ts = v
				case "candmtime":
					if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
						c.wrote = n * 1000
					}
				}
			}
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				c.began = t.UnixMilli()
			}
			c.cwd = unescapeJSON(c.cwd)
			if _, ok := cleanRemotePath(c.log); ok && c.cwd != "" {
				l.cands = append(l.cands, c)
			}
			continue
		}
		if v, ok := strings.CutPrefix(rec, "panetty="); ok {
			if strings.HasPrefix(v, "/dev/") && !strings.ContainsAny(v, " \n") {
				l.paneTTY = v
			}
			continue
		}
		if v, ok := strings.CutPrefix(rec, "tmux="); ok {
			// One query, so the pane, its server, its terminal and its title go together.
			f := strings.SplitN(v, " ", 4)
			if len(f) >= 3 && tmuxPane.MatchString(f[0]) && isDigits(f[1]) {
				l.tmuxActive, l.tmuxPID = f[0], f[1]
				if strings.HasPrefix(f[2], "/dev/") {
					l.tmuxTTY = f[2]
				}
				if len(f) == 4 {
					l.tmuxTitle = truncate(strings.Map(func(r rune) rune {
						if r < 0x20 || r == 0x7f || (r >= 0x202a && r <= 0x202e) || (r >= 0x2066 && r <= 0x2069) {
							return -1
						}
						return r
					}, strings.ToValidUTF8(f[3], "")), 120)
				}
			}
			continue
		}
		var p convoProc
		var procStart int64 = -1
		var btime, hz int64 // (when the host booted, unix seconds; its clock's ticks a second)
		seen := map[string]bool{}
		for _, line := range strings.Split(rec, "\n") {
			k, v, ok := strings.Cut(line, "=")
			if !ok || seen[k] {
				continue // (the first value of a key counts: a later line cannot replace it)
			}
			seen[k] = true
			switch k {
			case "pid":
				p.pid, _ = strconv.ParseInt(v, 10, 64)
			case "start":
				p.start, _ = strconv.ParseInt(v, 10, 64)
			case "fg":
				p.fg = v == "1"
			case "btime":
				btime, _ = strconv.ParseInt(v, 10, 64)
			case "hz":
				hz, _ = strconv.ParseInt(v, 10, 64)
			case "procstart":
				if n, err := strconv.ParseInt(v, 10, 64); err == nil {
					procStart = n
				}
			case "sid":
				p.sid = v
			case "log":
				p.log = v
			case "logmtime":
				// When its log was last written: what the agent last said
				// or was told, for a pane the gateway never saw change.
				// (Its last record's time, where it has one: see logts.)
				if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 && !p.wroteTold {
					p.wrote = n * 1000
				}
			case "logts":
				// The time its last record says (a JSON log's): the file's
				// own is moved on with nothing written (Claude Code touches
				// its log every hour it runs).
				if t, err := time.Parse(time.RFC3339Nano, v); err == nil && t.UnixMilli() > 0 {
					p.wrote, p.wroteTold = t.UnixMilli(), true
				}
			case "agent":
				p.agent = v
			case "HERDR_PANE_ID":
				p.herdrPane = v
			case "HERDR_SOCKET_PATH":
				p.herdrSocket = v
			case "TMUX":
				parts := strings.Split(v, ",")
				if len(parts) == 3 {
					p.tmuxSocket, p.tmuxServerPID = parts[0], parts[1]
				}
			case "TMUX_PANE":
				p.tmuxPane = v
			case "tty":
				p.tty = v
			case "cwd":
				p.cwd = v
			case "anc":
				for _, a := range strings.Split(v, ",") {
					pid, comm, ok := strings.Cut(a, ":")
					if n, err := strconv.ParseInt(pid, 10, 64); ok && err == nil && n > 1 {
						p.anc = append(p.anc, convoAnc{n, comm})
					}
				}
			}
		}
		if p.pid <= 0 || p.start <= 0 || procStart != p.start || !convoUUID.MatchString(p.sid) {
			continue
		}
		// (Here, not in the host's shell, whose numbers may be 32 bits.)
		if btime > 0 && hz > 0 && hz <= 100000 && p.start < math.MaxInt64/1000 {
			p.started = btime*1000 + p.start*1000/hz
		}
		switch p.agent {
		case "codex":
		case "opencode":
			// (Its session is told after: by herdr, or by where it runs.)
			if p.sid != convoNoSession {
				continue
			}
		default:
			p.agent = "claude"
			if p.sid == convoNoSession {
				continue // (only a Codex goes without its session known)
			}
		}
		if !convoLogOf(p.agent, p.log, p.sid) {
			p.log = ""
		}
		l.procs = append(l.procs, p)
	}
	return l
}

// convoLogOf: log is where that agent keeps session sid's (Claude Code:
// …/.claude/projects/<dir>/<sid>.jsonl; Codex: …/.codex/sessions/…/rollout-…-<sid>.jsonl).
func convoLogOf(agent, log, sid string) bool {
	l, ok := cleanRemotePath(log)
	if !ok {
		return false
	}
	if agent == "opencode" {
		return strings.HasSuffix(l, "/opencode/opencode.db")
	}
	if agent == "codex" {
		i := strings.LastIndexByte(l, '/')
		return strings.Contains(l, "/.codex/sessions/") && strings.HasPrefix(l[i+1:], "rollout-") && strings.HasSuffix(l, "-"+sid+".jsonl")
	}
	return strings.HasSuffix(l, "/"+sid+".jsonl")
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// convoBound: the log and session this gateway found for that process when
// it last read the pane's conversation, if it is still the one it reads.
func (s *Server) convoBound(sess *term.Session, pane string, pid, start int64) (string, string) {
	key := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		key += pane
	}
	st := s.convos.get(key)
	defer st.done()
	// (Not waited for: a read holds it while it asks the host, and what is
	// to be typed is not to wait on that. Unheld, it is read anew.)
	if !st.mu.TryLock() {
		return "", ""
	}
	defer st.mu.Unlock()
	if st.bind == nil || st.bind.PID != pid || st.bind.Start != start || st.log == "" || st.sid == "" {
		return "", ""
	}
	return st.log, st.sid
}

// convoScreenTTL: how long what the pane shows is taken to still hold.
const convoScreenTTL = 20 * time.Second

// screenSays: which of the conversations a daemon keeps the Codex in the
// pane is at, by what its screen shows of them — the one whose words are
// on it. "" when its screen shows none of them (it may have scrolled, or
// be showing only what is being typed): then what was told by where and
// when the conversations began stands.
//
// This is what tells a Codex that has begun another conversation (/new):
// its screen then shows that one's words, and the view follows it.
func (s *Server) screenSays(ctx context.Context, sess *term.Session, pane string, p *convoProc, l convoLocated) (log, sid string) {
	// (The ones written last, where there are many: what a screen shows is
	// of one being written, or lately was.)
	var cands []*convoCand
	for i := range l.cands {
		if l.cands[i].cwd == p.cwd {
			cands = append(cands, &l.cands[i])
		}
	}
	if len(cands) == 0 {
		return "", ""
	}
	slices.SortFunc(cands, func(a, b *convoCand) int { return int(min(max(b.wrote-a.wrote, -1), 1)) })
	if len(cands) > maxScreenLogs {
		cands = cands[:maxScreenLogs]
	}
	wrote := map[string]int64{}
	logs := make([]string, 0, len(cands))
	for _, c := range cands {
		logs = append(logs, c.log)
		wrote[c.log] = c.wrote
	}
	target := pane
	if sess.Persistence == "tmux" {
		target = p.tmuxPane
	}
	out, err := sshx.Output(ctx, sess.Client(), convoScreenCommand(sess.Persistence, target, s.herdrSocks.get(sess.ID)), 256<<10)
	if err != nil {
		return "", "" // (no screen to go by)
	}
	screen := flatten(string(out))
	if screen == "" {
		return "", ""
	}
	tails, err := sshx.Output(ctx, sess.Client(), convoTailsCommand(logs), maxScreenLogs*(32<<10))
	if err != nil {
		return "", ""
	}
	best, bestOn := "", 0
	for _, rec := range strings.Split(string(tails), "\x00") {
		path, tail, ok := strings.Cut(rec, "\n")
		if !ok || !slices.Contains(logs, path) {
			continue
		}
		// (Its end: what came before the first line read whole is a line's
		// middle, no JSON of its own, and is left out.)
		if len(tail) >= convoTailBytes {
			if i := strings.IndexByte(tail, '\n'); i >= 0 {
				tail = tail[i+1:]
			}
		}
		c := convo.NewCodex()
		c.Feed([]byte(tail))
		on := 0
		seen := map[string]bool{}
		for _, t := range lastSaid(c.Since(0), 8) {
			if seen[t] { // (the same words again: one thing on the screen)
				continue
			}
			seen[t] = true
			if strings.Contains(screen, t) {
				on++
			}
		}
		// (As many of its words on the screen as another's: the one it is
		// at now stays it, else the one written last.)
		if on > bestOn || (on == bestOn && on > 0 && (path == p.log || (best != p.log && wrote[path] > wrote[best]))) {
			best, bestOn = path, on
		}
	}
	if bestOn == 0 {
		return "", ""
	}
	return best, convoSessionOf(best)
}

// maxScreenLogs: how many of a daemon's conversations are held against one
// screen; convoTailBytes: how much of the end of each is read (as the
// script reads it).
const (
	maxScreenLogs  = 6
	convoTailBytes = 16384
)

// lastSaid: up to n pieces of what was said lately in the conversation, as
// a screen would hold them (what is long enough to tell one from another).
func lastSaid(entries []convo.Entry, n int) []string {
	var out []string
	for i := len(entries) - 1; i >= 0 && len(out) < n; i-- {
		e := entries[i]
		if e.Role != convo.RoleUser && e.Role != convo.RoleAssistant {
			continue
		}
		for _, p := range e.Parts {
			if p.Kind != convo.KindText || p.Tool != nil {
				continue
			}
			if t := flatten(p.Text); len([]rune(t)) >= 10 {
				out = append(out, middle(t, 30))
				break
			}
		}
	}
	return out
}

// middle: n runes from the middle of s (its start and end may be drawn
// with other words around them).
func middle(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	from := (len(r) - n) / 2
	return string(r[from : from+n])
}

// flatten: a text as a screen holds it, without what the screen may have
// put in it — colours, and the breaks and spaces of wrapping, anywhere in
// it (a line wrapped in the middle of a word, or of a word in Chinese).
func flatten(s string) string {
	s = ansi.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) || r == 0xa0 || r == 0x200b {
			return -1
		}
		return r
	}, s)
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;:?]*[A-Za-z]|\x1b\][^\x07\x1b]*(\x07|\x1b\\)?|\x1b[()][B0]`)

// daemonLog gives a Codex that keeps no log of its own the one its
// app-server daemon keeps for it (the npm package's Codex talks to such a
// daemon, and keeps nothing open itself).
//
// A daemon keeps the logs of conversations long over as well, so which is
// whose is told by where and when: a conversation is of the Codex that
// started last before it began, where it began. Of several so told (a
// Codex beginning another conversation, /new), the one written last. One
// whose Codex is no longer there is of none; nor is one begun before every
// Codex there started. Then nothing is shown rather than another's.
func (s *Server) daemonLog(p *convoProc, l convoLocated) (took, refused bool) {
	if p == nil || p.agent != "codex" || p.log != "" || p.cwd == "" || p.started <= 0 {
		return false, false
	}
	alive := aliveCodex(l)
	var best *convoCand
	var bestSID string
	for i := range l.cands {
		c := &l.cands[i]
		// Begun where this Codex runs, and after it started: one begun
		// before it is another's.
		if c.cwd != p.cwd || c.began <= 0 || c.began+convoBeganSlack < p.started {
			continue
		}
		sid := convoSessionOf(c.log)
		if sid == "" {
			continue
		}
		// Another Codex there it could be of as well (one started before
		// it began too): which is whose cannot be told. Neither takes it,
		// then or later: once that other has ended, the one left is no
		// more its owner than it was.
		rival := false
		for j := range l.procs {
			q := &l.procs[j]
			if q.pid != p.pid && q.agent == "codex" && q.cwd == c.cwd && q.started > 0 && c.began+convoBeganSlack >= q.started {
				s.convoClaimed.contest(sid)
				rival = true
				break
			}
		}
		if rival {
			refused = true
			continue
		}
		if best == nil || c.wrote > best.wrote || (c.wrote == best.wrote && c.began > best.began) {
			best, bestSID = c, sid
		}
	}
	if best == nil {
		return false, refused
	}
	if !s.convoClaimed.take(bestSID, p.pid, p.started, alive) {
		return false, true // (there is one, but it is another Codex's)
	}
	p.log, p.sid = best.log, bestSID
	return true, refused
}

// convoClaims remembers which Codex each conversation was found to be of,
// so that one left behind by a Codex that has ended is not taken for
// another's, and that one which could be either of two is of neither ever
// after. Bounded: when it is full, the conversations of Codexes long gone
// are forgotten first, and what is left is forgotten rather than any of it
// being opened up again — nothing more is taken then.
type convoClaims struct {
	mu   sync.Mutex
	by   map[string]convoClaim
	in   []string // (in the order they were claimed)
	full bool     // it has been full: nothing is taken any more
}

// convoClaim: the Codex a conversation was found to be of (pid 0: of none,
// it could be either of two), and when that Codex was last seen running.
type convoClaim struct {
	pid  int64
	seen int64 // unix milliseconds
}

const maxConvoClaims = 4096

// aliveCodex: the Codexes running now.
func aliveCodex(l convoLocated) map[int64]bool {
	alive := map[int64]bool{}
	for i := range l.procs {
		if l.procs[i].agent == "codex" {
			alive[l.procs[i].pid] = true
		}
	}
	return alive
}

// take: the conversation is taken as that process's own, if it may be — it
// is of no other Codex, or of one that was already gone when this one
// started (as a conversation resumed in a new Codex is). alive: the Codexes
// running now, of which the one it is of, if there, is seen again. Looking
// and taking are one, so that two readings cannot take the same one.
func (c *convoClaims) take(sid string, pid, started int64, alive map[int64]bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	was, ok := c.by[sid]
	if ok && was.pid != 0 && alive[was.pid] {
		was.seen = time.Now().UnixMilli()
		c.by[sid] = was
	}
	switch {
	case c.full:
		return false
	case ok && was.pid == pid:
		was.seen = time.Now().UnixMilli()
		c.by[sid] = was
		return true
	case ok && was.pid == 0:
		return false // (it could be either of two: of neither, ever after)
	case ok && alive[was.pid]:
		return false // (the Codex it is of is still at it)
	case ok && started <= was.seen:
		return false // (it was there beside the one it is of)
	}
	return c.set(sid, convoClaim{pid: pid, seen: time.Now().UnixMilli()})
}

// contest: it could be either of two Codexes, so it is of neither, ever
// after (one of them ending makes the other no more its owner).
func (c *convoClaims) contest(sid string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if was, ok := c.by[sid]; ok {
		was.pid = 0
		c.by[sid] = was
		return
	}
	c.set(sid, convoClaim{})
}

// set keeps it, within what it holds; false when there is no room for
// another and it was not kept. (Held: c.mu.)
func (c *convoClaims) set(sid string, cl convoClaim) bool {
	if c.by == nil {
		c.by = map[string]convoClaim{}
	}
	if _, ok := c.by[sid]; !ok {
		if len(c.in) >= maxConvoClaims {
			c.full = true // (what was said of which is whose is no longer whole)
			return false
		}
		c.in = append(c.in, sid)
	}
	c.by[sid] = cl
	return true
}

// convoBeganSlack: how much before a Codex started its conversation may
// seem to have begun. Both times come from the host's own clock, but when
// a process started is known only to the second (the host's boot time is).
const convoBeganSlack = 1_500

// convoSessionOf: the session a Codex log is of, by its name.
func convoSessionOf(log string) string {
	name := strings.TrimSuffix(log[strings.LastIndexByte(log, '/')+1:], ".jsonl")
	const n = len("00000000-0000-0000-0000-000000000000")
	if len(name) < n+len("rollout-") || !strings.HasPrefix(name, "rollout-") {
		return ""
	}
	if sid := name[len(name)-n:]; convoUUID.MatchString(sid) {
		return sid
	}
	return ""
}

// pickConvoProc chooses the process running in the pane: for herdr, the
// pane (pane) of the herdr server the session is attached to; for tmux,
// the pane (pane: the active one, or the one asked of) of the session's
// tmux server. A Claude Code started from inside the pane (by the agent, as
// a command or in a terminal of its own) has the pane's variables too, so
// only one that is the pane's own counts: under herdr (a herdr process
// among its ancestors), or (tmux) with the pane's terminal (paneTTY) as its
// input, where that is known. Of several, by this order:
//   - the one whose session is want (what herdr says the pane runs; "" when
//     it says none);
//   - one not started under another of them (the agent, not what it ran);
//   - the one started last.
func pickConvoProc(procs []convoProc, persistence, pane, herdrSocket, tmuxPID, paneTTY, want string) *convoProc {
	own := func(p *convoProc) bool {
		if persistence == "tmux" {
			return paneTTY == "" || p.tty == paneTTY
		}
		for _, a := range p.anc {
			if a.comm == "herdr" {
				return true
			}
		}
		return false
	}
	var in []*convoProc
	for i := range procs {
		p := &procs[i]
		switch persistence {
		case "herdr":
			if pane == "" || herdrSocket == "" || p.herdrPane != pane || p.herdrSocket != herdrSocket {
				continue
			}
		case "tmux":
			if pane == "" || p.tmuxPane != pane || p.tmuxServerPID != tmuxPID {
				continue
			}
		default:
			continue
		}
		if own(p) {
			in = append(in, p)
		}
	}
	under := func(p *convoProc) bool {
		for _, q := range in {
			for _, a := range p.anc {
				if q != p && a.pid == q.pid {
					return true
				}
			}
		}
		return false
	}
	// (Higher first, in that order.)
	rank := func(p *convoProc) [2]bool {
		return [2]bool{want != "" && p.sid == want, !under(p)}
	}
	ahead := func(a, b [2]bool) bool {
		for k := range a {
			if a[k] != b[k] {
				return a[k]
			}
		}
		return false
	}
	var best *convoProc
	var bestRank [2]bool
	for _, p := range in {
		if r := rank(p); best == nil || ahead(r, bestRank) || r == bestRank && p.start > best.start {
			best, bestRank = p, r
		}
	}
	return best
}

// convoGens numbers generations across all states, so a state made anew
// (the old one forgotten while a browser still held its numbers) never
// repeats one. Nor across restarts: counted from a random start (as
// instance tells runs apart), not from zero. From zero, a browser that had
// followed a log before a restart met the same generation again for another
// reading, and asked it only for what changed since a revision the new
// reading had not reached: nothing more of the conversation showed, nor
// its own message sent just before. (Random rather than the clock's time:
// a clock put back would repeat that.)
var convoGens = func() *atomic.Int64 {
	var g atomic.Int64
	g.Store(genStart())
	return &g
}()

// genStart: where a run's generations begin, at random in [2^50, 2^52):
// two runs meet only by a chance of their counts over 2^51, and the numbers
// stay well within those a browser holds exactly (2^53).
func genStart() int64 {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return 1<<50 + int64(binary.BigEndian.Uint64(b)%(3<<50))
}

// convoState is what the gateway knows of one session's conversation.
// earlyTitle: the name the person gave a session further back in its log
// than was read, looked for beside reading (see lookEarlierTitle).
type earlyTitle struct {
	mu   sync.Mutex
	gen  int64  // the reading it was looked for in
	line []byte // its row, once found; taken in once
	done bool
}

type convoState struct {
	used        atomic.Int64 // unix nanoseconds of the last request
	inUse       atomic.Int32 // requests holding it now: it is not let go meanwhile
	relocating  atomic.Bool  // a check of what the pane runs is under way beside (see relocateLater)
	relocFailed atomic.Bool  // the last such check failed: the next is made before answering
	mu          sync.Mutex
	located     time.Time
	// guessed: whose conversation this is was told by where and when it
	// began (a Codex whose log its daemon keeps), and by what the pane's
	// screen showed of it: screenSID, of the process screenPID/screenStart,
	// as it was at screenAt.
	guessed     bool
	screenAt    time.Time
	screenPID   int64
	screenStart int64
	screenSID   string
	screenLog   string
	screenFrom  string // (the conversation it was taken for when the screen was asked)
	pane        string // (herdr) the pane it was located for
	reason      string // why there is nothing to read, when there is not
	sid, log    string
	gen         int64      // changes when another log is read: what was shown does not apply
	bind        *convoBind // the process the log is of, while there is one
	parser      convo.Parser
	early       earlyTitle // (see lookEarlierTitle)
	agent       string     // whose log: "claude", "codex" or "opencode"
	offset      int64      // read up to here
	size        int64      // the log's size when last looked at
	mtime       time.Time
	grewAt      time.Time // when the log last grew, as this gateway saw it (at first, its own time of writing)
	older       bool      // the log has more before what was read
	from        int64     // where in the log the first line read begins (-1 until a first line is whole)
	more        bool      // the last read stopped before the end
	status      string    // (tmux) the process's own word: "busy", "idle", or "" when not known
	skipping    bool      // in a line too long to keep, until its end
	long        []byte    // a line longer than a read, so far, its images' data left out (nil when not in one)
	strip       b64Strip
	ocCursor    int64 // (OpenCode) rows written since this are read next (unix ms)
	ocCursorK   int   // (OpenCode) and, stopped within its millisecond, the last row read in it
	ocCursorID  string
	noReaderAt  time.Time // (OpenCode) when reading found no python3
	sub         bool      // a subagent's own log (see apiConversationSubagent)
}

// newParser: a parser for the log read, a subagent's own as such (its
// side-chain rows its own); every reader made anew is made by it.
func (st *convoState) newParser() convo.Parser {
	if st.sub {
		return convo.NewClaudeSub()
	}
	return convo.New(st.agent)
}

type convoStates struct {
	mu sync.Mutex
	m  map[string]*convoState
}

func (c *convoStates) get(id string) *convoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil {
		c.m = map[string]*convoState{}
	}
	for k, st := range c.m {
		if k != id && st.idle() && st.inUse.Load() == 0 {
			delete(c.m, k)
		}
	}
	st, ok := c.m[id]
	// At most convoStatesMax, across sessions and users: the one used
	// longest ago gives way (its reader starts over if it asks again). One a
	// request holds stays (dropping it would make a second state read the
	// same log beside it); past the limit while all were in use, every later
	// request brings the count back down.
	room := convoStatesMax
	if ok {
		room++ // (this one stays)
	}
	for len(c.m) >= room {
		old, at := "", int64(0)
		for k, s := range c.m {
			if u := s.used.Load(); k != id && s.inUse.Load() == 0 && (old == "" || u < at) {
				old, at = k, u
			}
		}
		if old == "" {
			break // (all in use: as many as requests under way, for a moment)
		}
		delete(c.m, old)
	}
	if !ok {
		st = &convoState{gen: convoGens.Add(1)}
		c.m[id] = st
	}
	st.used.Store(time.Now().UnixNano())
	st.inUse.Add(1) // (the caller lets go with done)
	return st
}

func (st *convoState) done() { st.inUse.Add(-1) }

// peek: the state of id, if there is one; nil else. Nothing is made, nor
// let go to make room (see get).
func (c *convoStates) peek(id string) *convoState {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[id]
}

func (st *convoState) idle() bool {
	return time.Since(time.Unix(0, st.used.Load())) > convoIdle
}

// convoNoSession stands for the session of a Codex that has none open yet
// (it begins its log with the first message).
const convoNoSession = "00000000-0000-0000-0000-000000000000"

// Reasons there is nothing to show.
const (
	convoNoAgent = "no_agent" // no Claude Code in the pane
	convoNoLog   = "no_log"   // it has not written its log yet (a new conversation)
	// convoNotTold: which conversation the Codex in the pane is at cannot
	// be told (its log is its daemon's, and what was taken for it is not
	// what the pane shows).
	convoNotTold     = "not_told"
	convoUnsupported = "unsupported" // the host has no /proc, or the session is a plain shell
	// (and convoNoReader: see convo_opencode.go)
)

// convoBind says what the entries are of: the pane, the Claude Code process
// in it (its pid and start time) and its session, so the browser can tell
// whether what it shows, and what it would act on, is still that.
type convoBind struct {
	Pane    string `json:"pane"`
	PID     int64  `json:"pid"`
	Start   int64  `json:"start"`
	Session string `json:"session"`
	Agent   string `json:"agent"`           // "claude" or "codex"
	Title   string `json:"title,omitempty"` // (tmux: the pane's title)
	// started is when the process started (unix milliseconds; 0 when not
	// known): what was queued before was another process's.
	started int64
}

type convoAnswer struct {
	// QuietMs: how long since the log last grew (what was read of it; for
	// OpenCode, its rows), in milliseconds.
	QuietMs int64 `json:"quiet_ms,omitempty"`
	// WaitingFor: with AgentStatus "waiting" (tmux), what Claude Code waits
	// for (see convoProcStatus).
	WaitingFor string `json:"waiting_for,omitempty"`
	Available  bool   `json:"available"`
	Reason     string `json:"reason,omitempty"`
	Gen        int64  `json:"gen"`
	Rev        int    `json:"rev"`
	Reset      bool   `json:"reset,omitempty"` // drop what was shown: these are all the entries
	Older      bool   `json:"older,omitempty"` // the conversation began before the first entry
	// OlderCut: what is before cannot be read in as well (as many entries
	// are kept as there may be); older=1 brings nothing more.
	OlderCut bool       `json:"older_cut,omitempty"`
	More     bool       `json:"more,omitempty"` // more of the log is to be read: ask again soon
	Bind     *convoBind `json:"bind,omitempty"`
	// AgentStatus (tmux, where herdr does not say): "busy" or "idle" as
	// Claude Code's file for the process says; absent when not known.
	AgentStatus string `json:"agent_status,omitempty"`
	// Title is the session's own (Claude Code's, or the person's);
	// Tasks the agent's task list as the log has it.
	Title string       `json:"title,omitempty"`
	Tasks []convo.Task `json:"tasks,omitempty"`
	Meta  *convo.Meta  `json:"meta,omitempty"` // what it runs with, as the log says
	// Queued is what the person sent that the agent has not taken in yet.
	Queued []convo.Queued `json:"queued,omitempty"`
	// Running is what the agent set going and left to itself: a command in
	// the background, a subagent on its own.
	Running []convo.Running `json:"running,omitempty"`
	// DefaultChanged: a model switch here left the default model of new
	// sessions changed (not put back), lately.
	DefaultChanged bool          `json:"default_changed,omitempty"`
	Entries        []convo.Entry `json:"entries"`
}

// GET /api/sessions/{id}/conversation?pane=<herdr pane>&gen=<n>&rev=<n>[&older=1]
//
// The entries changed since revision rev of generation gen; all of them when
// gen is not the current one (then reset is set). older=1 reads in the part
// of the log before what was read (convoOlderStep of it): its entries come
// as changed ones, before the others (lower seqs).
func (s *Server) apiConversation(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane := q.Get("pane")
	if pane != "" && !herdrPane.MatchString(pane) {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane")
		return
	}
	gen, _ := strconv.ParseInt(q.Get("gen"), 10, 64)
	rev, _ := strconv.Atoi(q.Get("rev"))
	older := q.Get("older") == "1"
	w.Header().Set("Cache-Control", "no-store")
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeJSON(w, http.StatusOK, convoAnswer{Reason: convoUnsupported, Entries: []convo.Entry{}})
		return
	}
	// One state per pane read (herdr's pane asked for; tmux has its active
	// one): a tab and another page reading other panes do not take turns
	// rebuilding a single one.
	key := sess.ID + "\x00" // (tmux reads its active pane: the pane asked for does not matter)
	if sess.Persistence == "herdr" {
		key += pane
	}
	st := s.convos.get(key)
	defer st.done()
	st.mu.Lock()
	defer st.mu.Unlock()
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	// (A Codex not yet begun on its log is looked for again each time: its
	// first message begins it, and what is sent next goes to that session.)
	fresh := st.bind != nil && st.bind.Session == convoNoSession
	// What the pane runs, checked again now and then: where a log is read
	// already, beside this request rather than before it (the scan of the
	// host's processes takes a while; what is read meanwhile is of the
	// process it was, and a change shows on the next request). Sending and
	// keys check it themselves, on the host, before they type.
	// (Not once one beside has failed: checks that keep failing would leave
	// what was found shown on and on, as if current. Before answering, a
	// failure is said.)
	due := time.Since(st.located) > convoLocateTTL
	if due && !st.relocFailed.Load() && !fresh && st.bind != nil && st.reason == "" && !(sess.Persistence == "herdr" && pane != st.pane) {
		s.relocateLater(sess, key, st, pane)
		due = false
	}
	if fresh || due || (sess.Persistence == "herdr" && pane != st.pane) {
		err := s.locateConvo(ctx, sess, st, pane)
		st.relocFailed.Store(err != nil) // (made before answering till one succeeds)
		if err != nil {
			if errors.Is(err, sshx.ErrBusy) {
				writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
			} else {
				writeError(w, http.StatusBadGateway, "host_error", "the host did not answer")
			}
			return
		}
	}
	ans := convoAnswer{Gen: st.gen, Bind: st.bind, Entries: []convo.Entry{}, DefaultChanged: defaultChangedOf(sess.ID)}
	if st.reason != "" {
		ans.Reason = st.reason
		writeJSON(w, http.StatusOK, ans)
		return
	}
	more, err := s.readConvo(sess, st)
	if err == nil && older && gen == st.gen {
		err = s.readOlder(sess, st)
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	if st.reason != "" { // (found reading: OpenCode's, with no python3)
		ans.Reason = st.reason
		writeJSON(w, http.StatusOK, ans)
		return
	}
	ans.Available, ans.Gen, ans.Rev, ans.Older, ans.More, ans.AgentStatus = true, st.gen, st.parser.Rev(), st.older, more, st.status
	if w, ok := strings.CutPrefix(st.status, "waiting:"); ok {
		ans.AgentStatus, ans.WaitingFor = "waiting", w
	}
	ans.OlderCut = st.older && (st.parser.Trimmed() || st.from < 0)
	if gen != st.gen {
		ans.Reset = true
		rev = 0
	}
	ans.Entries = st.parser.Since(rev)
	st.takeEarlierTitle()
	ans.Title, ans.Tasks = st.parser.Title(), st.parser.Tasks()
	if m := st.parser.Meta(); m != (convo.Meta{}) {
		ans.Meta = &m
	}
	ans.Queued = st.parser.Queued()
	ans.Running = st.parser.Running()
	if !st.grewAt.IsZero() {
		ans.QuietMs = max(0, time.Since(st.grewAt).Milliseconds())
	}
	writeJSONCompressed(w, r, http.StatusOK, ans)
}

// convoScans: the scans of a host's processes (the locate script) that
// reading conversations asks for, shared by the panes of one connection for
// a few seconds (each the same scan of the whole host), and one under way
// shared by all who ask meanwhile. By connection and the command whole (its
// mode and tmux session in it). Keys are not pressed by one: sending
// scans again, on its own.
type convoScans struct {
	mu sync.Mutex
	m  map[string]*convoScan
}

type convoScan struct {
	done  chan struct{} // closed once made
	out   []byte
	err   error
	at    time.Time // when it was made
	began time.Time // when it began: what it found is of then or after
}

// convoScanShared: how long a scan stands for the other panes.
const convoScanShared = 5 * time.Second

// run gives the output of the scan cmd on sess's host, when it was made
// and when it began: one made within convoScanShared, or the one under
// way, else a new one (not bound to the asking request: others may be
// waiting on it).
func (c *convoScans) run(ctx context.Context, sess *term.Session, cmd string) ([]byte, time.Time, time.Time, error) {
	key := sess.ID + "\x00" + cmd
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]*convoScan{}
	}
	for k, e := range c.m {
		select {
		case <-e.done:
			if time.Since(e.at) > convoScanShared {
				delete(c.m, k)
			}
		default:
		}
	}
	e, ok := c.m[key]
	if ok {
		select {
		case <-e.done:
			if e.err != nil {
				ok = false // (failed: made again)
			}
		default:
		}
	}
	if !ok {
		// (Made beside: the one who asked first, gone, does not wait on it
		// either; it is there for the next.)
		e = &convoScan{done: make(chan struct{}), began: time.Now()}
		c.m[key] = e
		go func() {
			sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			e.out, e.err = sshx.Output(sctx, sess.Client(), cmd, convoLocateMax)
			e.at = time.Now()
			close(e.done)
		}()
	}
	c.mu.Unlock()
	select {
	case <-e.done:
		return e.out, e.at, e.began, e.err
	case <-ctx.Done():
		return nil, time.Time{}, time.Time{}, ctx.Err()
	}
}

// relocateLater checks again what the pane runs (locateConvo), beside the
// request that found it due: one at a time, with the state held (taken
// again by key: one let go meanwhile is not checked).
func (s *Server) relocateLater(sess *term.Session, key string, st *convoState, pane string) {
	if !st.relocating.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer st.relocating.Store(false)
		held := s.convos.get(key)
		defer held.done()
		if held != st {
			return
		}
		st.mu.Lock()
		defer st.mu.Unlock()
		if time.Since(st.located) <= convoLocateTTL || (sess.Persistence == "herdr" && pane != st.pane) {
			return // (checked meanwhile)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		// (Failed: what was found stands, and the next check is made before answering.)
		st.relocFailed.Store(s.locateConvo(ctx, sess, st, pane) != nil)
	}()
}

// locateConvo finds the log of the Claude Code session in the pane; a
// different one from before starts a new generation.
func (s *Server) locateConvo(ctx context.Context, sess *term.Session, st *convoState, pane string) error {
	// (looked: when the scan read began, shared or not: what it found is of
	// then or after, and a look begun later says more.)
	out, at, looked, err := s.convoScans.run(ctx, sess, convoLocateCommand(sess.Persistence, sess.TmuxSession, ""))
	// (The script ends with exit 0: an error is a scan cut short, by its size
	// or a failure, which may leave out the agent started last. What was
	// found before stays, rather than a part of it.)
	if err != nil {
		return err
	}
	l := parseConvoLocate(string(out))
	st.located, st.pane = at, pane // (when the scan was made: one shared may be a little older)
	sid, log, reason, agent := "", "", "", ""
	guessed := false
	switch {
	case l.noProc:
		reason = convoUnsupported
	default:
		in := pane // (herdr: the pane asked of; tmux: the active one)
		if sess.Persistence == "tmux" {
			in = l.tmuxActive
		}
		p := pickConvoProc(l.procs, sess.Persistence, in, s.herdrSocks.get(sess.ID), l.tmuxPID, l.tmuxTTY, s.herdrSocks.sid(sess.ID, pane))
		// (OpenCode: which of its database's sessions it is at.)
		ocWhy := ""
		if p != nil && p.agent == "opencode" {
			if p.sid, ocWhy = s.ocSessionOf(ctx, sess, p, pane, l); p.sid == convoNoSession {
				p.log = ""
			}
		}
		var refused bool
		guessed, refused = s.daemonLog(p, l)
		// What the pane shows has the last word on which conversation it
		// is at: a Codex that has begun another (/new) shows that one's
		// words. Asked of the host only now and then (what a screen shows
		// changes slowly), and what it said stands until then — else what
		// was told by where and when they began would take it back.
		if guessed || refused {
			// (Another process, or another conversation told by where and
			// when they began — it has begun one since: what the screen
			// said was of how things were, and is asked again.)
			if st.screenPID != p.pid || st.screenStart != p.start || st.screenFrom != p.sid {
				st.screenPID, st.screenStart, st.screenSID, st.screenLog, st.screenAt = p.pid, p.start, "", "", time.Time{}
			}
			if time.Since(st.screenAt) > convoScreenTTL {
				st.screenAt, st.screenFrom = time.Now(), p.sid
				st.screenLog, st.screenSID = s.screenSays(ctx, sess, pane, p, l)
			}
			if st.screenSID != "" && st.screenSID != p.sid && s.convoClaimed.take(st.screenSID, p.pid, p.started, aliveCodex(l)) {
				p.log, p.sid, guessed = st.screenLog, st.screenSID, true
			}
		}
		st.bind = nil
		if p != nil {
			b := &convoBind{PID: p.pid, Start: p.start, Session: p.sid, Agent: p.agent, Pane: p.herdrPane}
			if sess.Persistence == "tmux" {
				b.Pane, b.Title = p.tmuxPane, l.tmuxTitle
			}
			// (Another process of the same session, resumed: what the one
			// before had queued is not this one's.)
			b.started = p.started
			st.bind = b
		}
		switch {
		case p == nil:
			reason = convoNoAgent
		case ocWhy != "":
			reason, sid, agent = ocWhy, p.sid, p.agent
		case p.log == "" && refused:
			// (It has one: which of the ones its daemon keeps cannot be told.)
			reason, sid, agent = convoNotTold, p.sid, p.agent
		case p.log == "":
			reason, sid, agent = convoNoLog, p.sid, p.agent
		default:
			sid, log, agent = p.sid, p.log, p.agent
		}
	}
	st.guessed = guessed && log != ""
	// (A herdr pane's Claude Code session, as found: what herdr does not say
	// of it, in some versions, for what its turns left running, see bgSession.)
	if sess.Persistence == "herdr" && pane != "" {
		found := ""
		if agent == "claude" {
			found = sid // (else none there now, or another: not that one's)
		}
		s.locatedEverywhere(sess, pane, found, looked)
	}
	// (No python3 to read OpenCode's with, found reading: said a minute,
	// then tried again.)
	if reason == "" && agent == "opencode" && st.reason == convoNoReader && sid == st.sid && log == st.log && time.Since(st.noReaderAt) < time.Minute {
		reason = convoNoReader
	}
	if sid != st.sid || log != st.log || agent != st.agent {
		st.gen = convoGens.Add(1)
		st.sid, st.log, st.agent = sid, log, agent
		st.parser = convo.New(agent)
		st.offset, st.size, st.mtime, st.older, st.skipping, st.from = 0, -1, time.Time{}, false, false, 0
		st.grewAt = time.Time{} // (another log's: its own, from its first look)
		st.long, st.strip, st.ocCursor, st.ocCursorK, st.ocCursorID = nil, b64Strip{}, 0, 0, ""
	}
	st.reason = reason
	// (What was queued to a process of the same session before this one: not this one's.)
	if st.parser != nil && st.bind != nil {
		st.parser.QueueSince(st.bind.started)
	}
	return nil
}

// readConvo reads what the log gained since the last read: from the end at
// first, then forward. A log that got shorter was replaced and is read
// again. more says one request did not take all there is.
func (s *Server) readConvo(sess *term.Session, st *convoState) (more bool, err error) {
	if st.agent == "opencode" {
		return false, s.readOpenCode(sess, st)
	}
	err = s.withSFTP(context.Background(), sess, convoReadLimit, func(c *sftplite.Client) error {
		if sess.Persistence == "tmux" && st.agent != "codex" && !st.sub {
			st.status = convoProcStatus(c, st)
		}
		// (Codex's log says whether it works.)
		defer func() {
			if cx, ok := st.parser.(*convo.Codex); ok && sess.Persistence == "tmux" {
				st.status = cx.Status()
			}
		}()
		fi, err := c.Stat(st.log)
		if err != nil {
			return err
		}
		size := fi.Size()
		if size == st.size && fi.ModTime().Equal(st.mtime) && !st.more {
			return nil
		}
		if size < st.offset || size < st.size {
			st.gen = convoGens.Add(1)
			st.parser = st.newParser()
			st.offset, st.older, st.skipping, st.from = 0, false, false, 0
			st.long, st.strip = nil, b64Strip{}
			if st.bind != nil {
				st.parser.QueueSince(st.bind.started) // (as locateConvo says it)
			}
		}
		limit := int64(convoStep)
		if st.size < 0 { // first read: the end of a long log
			limit = convoTail
			if size > convoTail {
				st.offset, st.older, st.skipping, st.from = size-convoTail, true, true, -1
				// The name the person gave it may be further back than what
				// is read: its latest there, looked for beside (a long log
				// takes the host a while to go through, and what is read is
				// shown meanwhile), taken in by a later request.
				if st.agent != "codex" && !st.sub {
					st.lookEarlierTitle(sess, st.offset+convoTitleSlack)
				}
			}
		}
		n := min(size-st.offset, limit)
		data, err := c.ReadRange(st.log, st.offset, int(n))
		if err != nil {
			return err
		}
		if st.size < 0 || st.grewAt.IsZero() {
			// (The first look: when it was last written, by the host's clock,
			// taken for this one's: a host whose clock is behind seems quiet
			// longer. One ahead, not later than now.)
			st.grewAt = fi.ModTime()
			if st.grewAt.After(time.Now()) {
				st.grewAt = time.Now()
			}
		} else if size > st.size {
			st.grewAt = time.Now()
		}
		st.size, st.mtime = size, fi.ModTime()
		st.more = st.offset+int64(len(data)) < size
		pos := 0
		if st.skipping { // (the rest of a line begun before what was read)
			i := bytes.IndexByte(data, '\n')
			if i < 0 {
				st.offset += int64(len(data))
				return nil
			}
			pos, st.skipping = i+1, false
			if st.from < 0 {
				st.from = st.offset + int64(pos) // (the first whole line read)
			}
		}
		// A line longer than a whole read (a message with images, whose data
		// is in it): read on with the images' data left out, and taken in
		// whole if what is left is not too long (else left out).
		if st.long != nil {
			i := bytes.IndexByte(data[pos:], '\n')
			if i < 0 {
				st.long = st.strip.write(st.long, data[pos:])
				st.offset += int64(len(data))
				if len(st.long) > convoLongMax {
					st.long, st.strip, st.skipping = nil, b64Strip{}, true
				}
				return nil
			}
			line := append(st.strip.end(st.strip.write(st.long, data[pos:pos+i])), '\n')
			st.long, st.strip = nil, b64Strip{}
			if len(line) <= convoLongMax {
				st.parser.Feed(line)
			}
			pos += i + 1
		}
		end := bytes.LastIndexByte(data[pos:], '\n')
		switch {
		case end >= 0:
			st.parser.Feed(data[pos : pos+end+1])
			st.offset += int64(pos + end + 1)
		case int64(len(data)-pos) >= limit:
			st.long = st.strip.write(make([]byte, 0, 64<<10), data[pos:])
			st.offset += int64(len(data))
		default:
			st.offset += int64(pos) // (the last line is still being written)
		}
		return nil
	})
	return st.more, err
}

// lookEarlierTitle looks for the name the person gave the session in the
// log's first upto bytes (the part before what is read), beside the
// reading: its answer is not waited for. takeEarlierTitle gives it to the
// parser in a later request (st.mu held), if the reading is still that one.
func (st *convoState) lookEarlierTitle(sess *term.Session, upto int64) {
	gen, log, sid := st.gen, st.log, st.sid
	st.early.mu.Lock()
	st.early.gen, st.early.line, st.early.done = gen, nil, false
	st.early.mu.Unlock()
	go func() {
		line := convoCustomTitle(sess, log, sid, upto)
		st.early.mu.Lock()
		defer st.early.mu.Unlock()
		if st.early.gen == gen {
			st.early.line, st.early.done = line, true
		}
	}()
}

func (st *convoState) takeEarlierTitle() {
	st.early.mu.Lock()
	line := st.early.line
	ok := st.early.done && st.early.gen == st.gen && line != nil
	if ok {
		st.early.line = nil
	}
	st.early.mu.Unlock()
	if c, isClaude := st.parser.(*convo.Claude); ok && isClaude {
		c.EarlierTitle(line)
	}
}

// convoCustomTitle finds the last "custom-title" row of a log (the name
// given with /rename) of session sid, as a line to feed the parser; nil
// when there is none or it cannot be told. grep goes through a log of
// hundreds of MB in well under a second.
func convoCustomTitle(sess *term.Session, log, sid string, upto int64) []byte {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if !convoUUID.MatchString(sid) {
		return nil
	}
	out, err := sshx.Output(ctx, sess.Client(), convoTitleCommand(log, sid, upto), 65536)
	if err != nil {
		return nil
	}
	return customTitleLine(out, sid)
}

// customTitleLine: the row, if it is one (a row of its own, not the words
// inside another's), of that session.
func customTitleLine(out []byte, sid string) []byte {
	var r struct {
		Type        string `json:"type"`
		CustomTitle string `json:"customTitle"`
		SessionID   string `json:"sessionId"`
	}
	if json.Unmarshal(bytes.TrimSpace(out), &r) != nil || r.Type != "custom-title" || (r.SessionID != "" && r.SessionID != sid) {
		return nil
	}
	line, err := json.Marshal(map[string]string{"type": r.Type, "customTitle": r.CustomTitle})
	if err != nil {
		return nil
	}
	return append(line, '\n')
}

// convoTitleCommand: fixed, its first upto bytes only, the log's path (one locateConvo found, under
// .claude/projects/) and the session id (validated) its arguments. Rows
// Claude Code writes for a name begin so; a tool's input that holds the
// words does not. Of this session's only (a log may hold another's).
func convoTitleCommand(log, sid string, upto int64) string {
	return "head -c " + strconv.FormatInt(max(upto, 0), 10) + " " + sshx.QuoteShell(log) + " 2>/dev/null | LC_ALL=C grep -a '^{\"type\":\"custom-title\"' | grep -F " + sshx.QuoteShell(`"sessionId":"`+sid+`"`) + " | tail -n 1 | head -c 65536"
}

// readOlder reads in the part of the log before what was read: up to
// convoOlderStep of it, from its first whole line (all of it, at the log's
// start), parsed on its own and put before (convo.Claude.Prepend). A part
// that is all one line (a message with images, their data in it) is read
// whole from where it begins, its images' data left out; one longer than
// convoLongScan, or still too long without it, is passed over, and the
// next request goes on before it. Nothing once entries were dropped as too
// many.
func (s *Server) readOlder(sess *term.Session, st *convoState) error {
	if st.agent == "opencode" {
		return s.readOlderOpenCode(sess, st)
	}
	if !st.older || st.from <= 0 || st.parser.Trimmed() {
		return nil
	}
	return s.withSFTP(context.Background(), sess, convoReadLimit, func(c *sftplite.Client) error {
		start := max(0, st.from-convoOlderStep)
		data, err := c.ReadRange(st.log, start, int(st.from-start))
		if err != nil {
			return err
		}
		if int64(len(data)) != st.from-start {
			return errors.New("the log changed under the read") // (shortened: read again from the end next)
		}
		pos := 0
		if start > 0 {
			i := bytes.IndexByte(data, '\n')
			if i < 0 || i+1 == len(data) {
				// (All one line, longer than the part. Its end, where what was
				// read begins: read whole. Not that, the rest of one passed
				// over as too long before: on before it.)
				line, from := []byte(nil), start
				if data[len(data)-1] == '\n' {
					line, from, err = longLineBefore(c, st.log, start, data)
					switch {
					case errors.Is(err, errLogChanged):
						return err
					case err != nil:
						// (Not read in time, say over a slow link: passed over as
						// before, rather than the conversation failing with it.)
						line, from = nil, start
					default:
						// (Shortened while it was read: what was read is not it.)
						if fi, err := c.Stat(st.log); err != nil || fi.Size() < st.from {
							return errLogChanged
						}
					}
				}
				if line != nil {
					o := st.newParser()
					o.Feed(line)
					st.parser.Prepend(o)
				}
				st.from = from
				st.older = st.from > 0
				return nil
			}
			pos = i + 1
		}
		o := st.newParser()
		o.Feed(data[pos:])
		st.parser.Prepend(o)
		st.from = start + int64(pos)
		st.older = st.from > 0
		return nil
	})
}

var errLogChanged = errors.New("the log changed under the read")

// rangeReader reads part of a file (sftplite.Client's).
type rangeReader interface {
	ReadRange(p string, off int64, n int) ([]byte, error)
}

// longLineBefore reads in whole a line longer than an older part: tail is
// its end, read from start, to its newline. Where it begins is looked for
// before start (up to convoLongScan of the line), and it is put through
// b64Strip from there. line nil where it is too long either way: from is
// then where the look gave up, inside it (what is before that is read as
// any older part is, the rest of this line not taken for one).
func longLineBefore(c rangeReader, log string, start int64, tail []byte) (line []byte, from int64, err error) {
	var parts [][]byte // (from where it begins, backwards: read once)
	pos, size := start, int64(len(tail))
	for pos > 0 {
		if size >= convoLongScan {
			return nil, pos, nil
		}
		n := min(pos, convoOlderStep, convoLongScan-size)
		b, err := c.ReadRange(log, pos-n, int(n))
		if err != nil {
			return nil, 0, err
		}
		if int64(len(b)) != n {
			return nil, 0, errLogChanged
		}
		pos -= n
		size += n
		if j := bytes.LastIndexByte(b, '\n'); j >= 0 {
			parts = append(parts, b[j+1:])
			pos += int64(j + 1)
			break
		}
		parts = append(parts, b)
	}
	var strip b64Strip
	out := make([]byte, 0, 64<<10)
	for k := len(parts) - 1; k >= 0; k-- {
		out = strip.write(out, parts[k])
		parts[k] = nil
		if len(out) > convoLongMax {
			return nil, pos, nil
		}
	}
	out = strip.end(strip.write(out, tail[:len(tail)-1]))
	if len(out) > convoLongMax {
		return nil, pos, nil
	}
	return append(out, '\n'), pos, nil
}

// convoProcStatus reads what Claude Code's file for the bound process says it
// is doing ("busy", "idle"); "" when it cannot tell (no file, another
// process's file under a reused pid, another word). It never fails a read.
func convoProcStatus(c *sftplite.Client, st *convoState) string {
	if st.bind == nil {
		return ""
	}
	i := strings.LastIndex(st.log, "/.claude/projects/")
	if i < 0 {
		return ""
	}
	base := st.log[:i]
	b, err := c.ReadRange(base+"/.claude/sessions/"+strconv.FormatInt(st.bind.PID, 10)+".json", 0, 64<<10)
	if err != nil {
		return ""
	}
	var f struct {
		Status     string          `json:"status"`
		WaitingFor string          `json:"waitingFor"`
		SessionID  string          `json:"sessionId"`
		ProcStart  json.RawMessage `json:"procStart"`
	}
	if json.Unmarshal(b, &f) != nil || f.SessionID != st.bind.Session || strings.Trim(string(f.ProcStart), `"`) != strconv.FormatInt(st.bind.Start, 10) {
		return ""
	}
	switch f.Status {
	case "busy", "idle":
		return f.Status
	case "shell":
		return "busy" // (a command typed with "!" runs)
	case "waiting":
		// What for, as Claude Code 2.1.312 says it: "permission prompt",
		// "input needed" (a question), "sandbox request", "worker request",
		// "dialog open" (a picker, a dialog of its own); another, "other".
		switch f.WaitingFor {
		case "permission prompt", "input needed", "sandbox request", "worker request", "dialog open":
			return "waiting:" + f.WaitingFor
		}
		return "waiting:other"
	}
	return ""
}

// convoSendCommand types a message into a pane, or presses keys there
// (keys: herdr's names or tmux's, by mode; then no text is read), checking
// right before that the process asked for (pid, its start in clock ticks) is
// still the one in front on its terminal, in that pane (its HERDR_PANE_ID or
// TMUX_PANE; in tmux also on that pane's terminal: an environment inherited
// by a program on another terminal is not enough), and still in the Claude
// Code session asked for (its session file: /clear or /resume start another
// in the same process). The gateway's own check came a moment before, and
// text and Enter reaching a shell would run as a command. The text is read
// whole first (its length in bytes must be as said: a cut short input types
// nothing) and goes in as one bracketed paste, so its newlines and tabs are
// text, not keys: herdr types it as given (the markers included, for the
// program in front, which asked for them), tmux wraps it when the program
// asked (paste-buffer -p, its newlines kept with -r).
//
// Enter goes a moment after the paste: one right on it was let go by Claude
// Code at times (a pasted path to a picture, being looked at), the words left
// in its input and the next message typed on after them, the two sent as one.
//
// One at a time to a pane on the host too (messages and keys alike), where
// flock and the user's own XDG_RUNTIME_DIR are there (a lock the kernel lets
// go of with the script: the gateway may give up on a send whose script goes
// on; not in a folder others can write to, as /tmp), tried for 10 seconds
// (-n: BusyBox's has no -w). Without them, the gateway's turn is what there is.
//
// Exits 3: not that agent in front in that pane now (or, tmux, the pane is
// not in this session); 4: nothing typed; 5: the text typed, not Enter; 6:
// typing failed part way, or may have (so does a signal once typing began);
// 7: the pane's lock was not had in time (nothing typed); 8: the screen
// asked for could not be read. With read (lines, to 60), the last lines of
// the pane's screen are printed a moment after the keys or the text (what
// they changed); keys none (not nil): the screen only. Fixed script; the
// mode, the validated tmux session name, pane id, pid, start, Claude session
// id, length, the herdr socket (from discovery) and keys of a fixed set
// (convoKeys) are its arguments, the text its input.
func convoSendCommand(mode, session, pane, socket string, pid, start int64, sid string, length int, keys []string, read int) string {
	return convoGatedCommand("", mode, session, pane, socket, pid, start, sid, length, keys, read)
}

// convoGatedCommand is convoSendCommand with a gate (see the script): ""
// none; "free", not while Claude Code waits; "idle", only while it is idle
// (exit 12 where it is not); "reply:<sha256 of the screen answered>", only
// while it waits for an answer, on that screen.
func convoGatedCommand(gate, mode, session, pane, socket string, pid, start int64, sid string, length int, keys []string, read int) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:$HOME/.cargo/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
mode=$1 sess=$2 p=$3 pid=$4 start=$5 len=$6 sock=$7 sid=$8 rd=$9
shift 9
gate=$1
shift
case $rd in ''|*[!0-9]*) rd=0;; esac
# (What the pane shows after, its last rd lines but blank ones at the end.)
# (Not read, it exits 8: no screen is not an empty one.)
screen() {
  [ "$rd" -gt 0 ] || return 0
  sleep 0.7
  if [ "$mode" = tmux ]; then s=$(tmux -u capture-pane -p -t "$p" 2>/dev/null 9>&-) || exit 8
  else s=$(HERDR_SOCKET_PATH=$sock herdr pane read "$p" --source visible 2>/dev/null 9>&-) || exit 8; fi
  printf '%s\n' "$s" | awk -v n="$rd" '{ a[NR] = $0 } NF { last = NR } END { for (i = last - n + 1; i <= last; i++) if (i > 0) print a[i] }'
}
b=corral-send-$$ typing=0
f=$(mktemp) || exit 4
trap 'rm -f "$f"; [ "$mode" = tmux ] && tmux delete-buffer -b "$b" 2>/dev/null 9>&-' EXIT
trap '[ "$typing" = 1 ] && exit 6; exit 4' HUP INT TERM
if [ "$len" != keys ]; then cat >"$f" || exit 4; fi
l=${XDG_RUNTIME_DIR:-}/corral-send-$(printf '%s' "$mode-$p" | tr -c 'A-Za-z0-9-' _)
if [ -n "${XDG_RUNTIME_DIR:-}" ] && [ -d "$XDG_RUNTIME_DIR" ] && [ ! -L "$l" ] && command -v flock >/dev/null 2>&1 && (exec 9>>"$l") 2>/dev/null; then
  exec 9>>"$l"
  n=0
  until flock -n 9; do
    [ "$n" -ge 100 ] && exit 7
    if sleep 0.1 2>/dev/null; then n=$((n + 1)); else sleep 1; n=$((n + 10)); fi
  done
fi
[ "$len" = keys ] || [ "$(wc -c <"$f" | tr -d ' ')" = "$len" ] || exit 4
front() {
  stat=$(tr '\n' ' ' <"/proc/$pid/stat" 2>/dev/null) || return 1
  rest=${stat##*) }
  [ "$(printf '%s' "$rest" | cut -d' ' -f20)" = "$start" ] || return 1
  pgrp=$(printf '%s' "$rest" | cut -d' ' -f3); tpgid=$(printf '%s' "$rest" | cut -d' ' -f6)
  [ -n "$pgrp" ] && [ "$pgrp" = "$tpgid" ] || return 1
  if [ "$mode" = tmux ]; then k=TMUX_PANE; else k=HERDR_PANE_ID; fi
  tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null | grep -qxF "$k=$p" || return 1
  if [ "$(head -c 16 "/proc/$pid/comm" 2>/dev/null)" = codex ]; then
    # (Codex, not run with exec (its arguments, its log's head): the
    # session's log is the one it has open and wrote last, as locating chose it.)
    cs=$(tr '\0' '\001' <"/proc/$pid/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" } NR == 1 { next } skip { skip = 0; next }
      /^--$/ { exit } /^-/ { if ($0 ~ /^(-[cmpsaCi]|--(config|model|profile|sandbox|ask-for-approval|cd|image|add-dir|local-provider|remote|remote-auth-token-env|enable|disable))$/) skip = 1; next }
      { print; exit }')
    case $cs in exec|e|exec-server|review|apply|a|login|logout|mcp|mcp-server|app-server|completion|sandbox|debug|cloud|features) return 1;; esac
    lg= any=
    for fd in /proc/$pid/fd/*; do
      l=$(readlink "$fd" 2>/dev/null) || continue
      case $l in */rollout-*.jsonl) any=1;; esac
      case $l in "$HOME"/.codex/sessions/*/rollout-*.jsonl) { [ -z "$lg" ] || [ "$l" -nt "$lg" ]; } && lg=$l;; esac
    done
    # (The zero id: one with no log open yet, anywhere, run with no
    # subcommand, as it was located.)
    if [ "$sid" = 00000000-0000-0000-0000-000000000000 ]; then
      [ -z "$cs" ] && [ -z "$any" ] || return 1
    elif [ -z "$lg" ]; then
      # (None of its own: its app-server daemon keeps it. That log is of
      # this session and was begun where this process runs, as locating
      # found it.)
      # (Whose conversation it is was told when it was read, by where and
      # when it began; here only that it is still one a Codex of ours runs.)
      [ -z "$cs" ] || return 1
      held=
      for dd in /proc/[0-9]*; do
        [ -O "$dd" ] && [ "$(head -c 16 "$dd/comm" 2>/dev/null)" = codex ] || continue
        for fd in "$dd"/fd/*; do
          l=$(readlink "$fd" 2>/dev/null) || continue
          case $l in "$HOME"/.codex/sessions/*/rollout-*-"$sid".jsonl) held=1; break;; esac
        done
        [ -n "$held" ] && break
      done
      [ -n "$held" ] || return 1
    else
      case $lg in *-"$sid".jsonl) ;; *) return 1;; esac
      [ -z "$cs" ] && head -c 4096 "$lg" 2>/dev/null | grep -q '"originator": *"codex_exec"' && return 1
    fi
  elif [ "$(head -c 16 "/proc/$pid/comm" 2>/dev/null)" = opencode ]; then
    # (OpenCode, not one that does not converse. Which of its sessions it
    # is at was told when it was read: it keeps no file to ask here.)
    cs=$(tr '\0' '\001' <"/proc/$pid/cmdline" 2>/dev/null | awk 'BEGIN { RS = "\001" } NR == 1 { next } skip { skip = 0; next } /^--$/ { exit }
      /^(-[msp]|--(model|session|prompt|agent|port|hostname|log-level|mdns-domain|cors))$/ { skip = 1; next }
      /^(run|serve|web|acp|mcp|auth|agent|upgrade|uninstall|models|stats|export|import|github|pr|debug|session|completion|generate|attach)$/ { print; exit }')
    case $cs in run|serve|web|acp|mcp|auth|agent|upgrade|uninstall|models|stats|export|import|github|pr|debug|session|completion|generate|attach) return 1;; esac
  else
    c=$(head -c 65536 "$HOME/.claude/sessions/$pid.json" 2>/dev/null | tr -d '\n')
    [ "$(printf '%s' "$c" | grep -o '"sessionId" *: *"[0-9a-fA-F-]*"' | head -n 1 | sed 's/.*"\([0-9a-fA-F-]*\)"$/\1/')" = "$sid" ] || return 1
  fi
  if [ "$mode" = tmux ]; then
    tty=$(tmux -u display-message -p -t "$p" '#{pane_tty}' 2>/dev/null 9>&-)
    [ -n "$tty" ] && [ "$(readlink "/proc/$pid/fd/0" 2>/dev/null)" = "$tty" ] || return 1
  fi
}
front || exit 3
# The gate, by what Claude Code's session file says: a message not while it
# waits (for an answer, or with a dialog open: the message's Enter would go
# there), exit 9; an answer only while it waits for one (exit 9) and the
# pane's screen is still the one answered (its SHA-256 as the gateway read
# it; 10 where it is not, 11 where it cannot be told).
# (waiting: what the file says now, its status and what for.)
waiting() {
  c=$(head -c 65536 "$HOME/.claude/sessions/$pid.json" 2>/dev/null | tr -d '\n')
  st=$(printf '%s' "$c" | grep -o '"status" *: *"[a-z]*"' | head -n 1 | sed 's/.*"\([a-z]*\)"$/\1/')
  wf=$(printf '%s' "$c" | grep -o '"waitingFor" *: *"[a-z ]*"' | head -n 1 | sed 's/.*"\([a-z ]*\)"$/\1/')
}
# (Right before the Enter that sends a text: it may have come to wait
# meanwhile, the text typed. Exit 5: typed, not sent.)
gate_enter() {
  [ -n "$gate" ] || return 0
  waiting
  case $gate in
  free) [ "$st" = waiting ] && exit 5 ;;
  idle) [ "$st" = idle ] || exit 5 ;;
  reply:*) [ "$st" = waiting ] || exit 5 ;;
  esac
  return 0
}
if [ -n "$gate" ]; then
  waiting
  case $gate in
  free) [ "$st" = waiting ] && exit 9 ;;
  idle) [ "$st" = idle ] || exit 12 ;;
  reply:*)
    [ "$st" = waiting ] || exit 9
    case $wf in "permission prompt"|"input needed"|"sandbox request"|"worker request") ;; *) exit 9;; esac
    command -v sha256sum >/dev/null 2>&1 || exit 11
    if [ "$mode" = tmux ]; then d=$(tmux -u capture-pane -p -t "$p" 2>/dev/null 9>&- | sha256sum)
    else d=$(HERDR_SOCKET_PATH=$sock herdr pane read "$p" --source visible 2>/dev/null 9>&- | sha256sum); fi
    [ "${d%% *}" = "${gate#reply:}" ] || exit 10 ;;
  *) exit 4 ;;
  esac
fi
if [ "$mode" = tmux ]; then
  [ "$(tmux -u display-message -p -t "$p" '#{session_name}' 2>/dev/null 9>&-)" = "$sess" ] || exit 3
  if [ "$len" = keys ]; then
    front || exit 3
    [ $# -gt 0 ] && typing=1
    for k in "$@"; do
      if [ "$k" = pause ]; then sleep 0.3; else tmux send-keys -t "$p" "$k" 9>&- || exit 6; fi
    done
    screen
    exit 0
  fi
  tmux load-buffer -b "$b" "$f" 9>&- || exit 4
  front || exit 3
  typing=1
  tmux paste-buffer -p -r -d -b "$b" -t "$p" 9>&- || exit 6
  sleep 0.3
  gate_enter
  tmux send-keys -t "$p" Enter 9>&- || exit 5
  screen
else
  e=$(printf '\033')
  if [ "$len" = keys ]; then
    front || exit 3
    [ $# -gt 0 ] && typing=1
    for k in "$@"; do
      # Shift+Tab as the bytes themselves: herdr before 0.9.5 sends a
      # plain Tab for that name, which Claude Code does nothing with, so
      # the permission mode never changed.
      if [ "$k" = pause ]; then
        sleep 0.3
      elif [ "$k" = shift+tab ]; then
        HERDR_SOCKET_PATH=$sock herdr pane send-text "$p" "$e[Z" >/dev/null 2>&1 9>&- || exit 6
      else
        HERDR_SOCKET_PATH=$sock herdr pane send-keys "$p" "$k" >/dev/null 2>&1 9>&- || exit 6
      fi
    done
    screen
    exit 0
  fi
  t=$(cat "$f" && printf x) || exit 4
  [ "${t%x}" != "$t" ] || exit 4
  t=${t%x}
  front || exit 3
  typing=1
  HERDR_SOCKET_PATH=$sock herdr pane send-text "$p" "$e[200~$t$e[201~" >/dev/null 2>&1 9>&- || exit 6
  sleep 0.3
  gate_enter
  HERDR_SOCKET_PATH=$sock herdr pane send-keys "$p" enter >/dev/null 2>&1 9>&- || exit 5
  screen
fi`
	n := strconv.Itoa(length)
	if keys != nil {
		n = "keys"
	}
	cmd := "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(mode) + " " + sshx.QuoteShell(session) + " " + sshx.QuoteShell(pane) + " " +
		strconv.FormatInt(pid, 10) + " " + strconv.FormatInt(start, 10) + " " + n + " " + sshx.QuoteShell(socket) + " " + sshx.QuoteShell(sid) + " " + strconv.Itoa(max(0, min(read, 60))) + " " + sshx.QuoteShell(gate)
	for _, k := range keys {
		cmd += " " + sshx.QuoteShell(k)
	}
	return cmd
}

// convoSending lets one message at a time go to a pane: two typed at once
// would interleave (one's text, the other's, then both Enters: one message).
// A turn is given back once the host's command has really ended, not when
// the request gives up; a pane no one is sending to is forgotten.
var convoSending = paneTurns{m: map[string]*paneTurn{}}

// convoTurnKey names a pane's turn: its session's, and the pane.
func convoTurnKey(session, pane string) string { return session + "\x00" + pane }

type paneTurns struct {
	mu sync.Mutex
	m  map[string]*paneTurn
}

type paneTurn struct {
	ch chan struct{} // (of one: whose turn it is)
	n  int           // having it or waiting for it
}

func (p *paneTurns) take(ctx context.Context, key string) (release func(), ok bool) {
	p.mu.Lock()
	t := p.m[key]
	if t == nil {
		t = &paneTurn{ch: make(chan struct{}, 1)}
		p.m[key] = t
	}
	t.n++
	p.mu.Unlock()
	leave := func() {
		p.mu.Lock()
		if t.n--; t.n == 0 {
			delete(p.m, key)
		}
		p.mu.Unlock()
	}
	select {
	case t.ch <- struct{}{}:
		var once sync.Once
		return func() { once.Do(func() { <-t.ch; leave() }) }, true
	case <-ctx.Done():
		leave()
		return nil, false
	}
}

// What the conversation presses in the agent's pane, in herdr's names and
// tmux's: stop (Escape), send a queued message now (Claude Code's Ctrl+X
// Ctrl+S, which stops what it was doing to take the message in), the next
// permission mode (Shift+Tab), and send a message (its text, then Enter:
// see sendText).
var convoKeys = map[string]struct {
	audit string
	herdr []string
	tmux  []string
}{
	"interrupt": {"convo.interrupt", []string{"esc"}, []string{"Escape"}},
	"send_now":  {"convo.send_now", []string{"ctrl+x", "ctrl+s"}, []string{"C-x", "C-s"}},
	"mode":      {"convo.mode", []string{"shift+tab"}, []string{"BTab"}}, // (Claude Code's next permission mode)
	"models":    {"convo.models", nil, nil},                              // (its picker read: see convo_model.go)
	"model":     {"convo.model", nil, nil},
	"effort":    {"convo.effort", nil, nil}, // (its slider: see convo_effort.go)
	// Looking for a message that was typed and has not turned up in the
	// log: nothing is pressed, the screen is read (see convo_sent.go).
	"sent_missing": {"convo.sent_missing", nil, nil},
	// Enter for such a message, seen still in the input (see convo_sent.go).
	"sent_enter": {"convo.sent_enter", nil, nil},
	// Claude Code's suggestion of the next message: nothing is pressed, the
	// screen is read with its colours (see convo_suggest.go).
	"suggest": {"convo.suggest", nil, nil},
	"send":    {"convo.send", nil, nil}, // (typed by convoSendCommand)
	// An answer to what Claude Code asks: its screen read, then keys or a
	// line (see convo_reply.go).
	"screen": {"convo.screen", nil, nil},
	"reply":  {"convo.reply", nil, nil},
	// One of Claude Code's own commands on the session (see commandText),
	// only while it is idle.
	"command": {"convo.command", nil, nil},
}

// commandText: a command on the session the page may send, one line:
// /compact (with what to keep, if said), /rename with the name, /clear.
func commandText(s string) (string, bool) {
	t, ok := sendText(s)
	if !ok || strings.ContainsAny(t, "\n\t") || len(t) > 2000 {
		return "", false
	}
	switch {
	case t == "/compact" || t == "/clear" || strings.HasPrefix(t, "/compact "):
	case strings.HasPrefix(t, "/rename ") && strings.TrimSpace(t[len("/rename "):]) != "":
	default:
		return "", false
	}
	return t, true
}

// screenMode reads the permission mode off the bottom of Claude Code's
// screen ("⏵⏵ accept edits on (shift+tab to cycle)", "⏸ plan mode on"…),
// in the log's names; "" when it does not say.
func screenMode(screen string) string {
	lines := strings.Split(screen, "\n")
	// (Under the input's box, below its last rule: not the thread above.)
	for i := len(lines) - 1; i >= 0; i-- {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "────") {
			lines = lines[i+1:]
			break
		}
	}
	for i := len(lines) - 1; i >= 0; i-- {
		// (Its own line begins with its mark.)
		t := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(t, "⏵") && !strings.HasPrefix(t, "⏸") {
			continue
		}
		l := strings.ToLower(t)
		for _, m := range screenModes {
			if strings.Contains(l, m.on) {
				return m.mode
			}
		}
	}
	return ""
}

var screenModes = []struct{ on, mode string }{
	{"accept edits on", "acceptEdits"},
	{"plan mode on", "plan"},
	{"auto mode on", "auto"},
	{"bypass permissions on", "bypassPermissions"},
	{"don't ask on", "dontAsk"},
	{"manual mode on", "default"},
}

// maxSendText is the most a message sent from the agents page may hold (in
// bytes): herdr takes far more in one request (120 KB tried).
const maxSendText = 100 << 10

// sendText is a message as it may be typed into a pane: valid UTF-8, its
// line ends as "\n", and no other control character than a tab (an Escape
// or a Ctrl+B in it would be pressed, not typed). ok false: not so.
func sendText(s string) (string, bool) {
	if s == "" || len(s) > maxSendText || !utf8.ValidString(s) {
		return "", false
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	if strings.TrimSpace(s) == "" {
		return "", false
	}
	for _, r := range s {
		if (r < 0x20 && r != '\n' && r != '\t') || (r >= 0x7f && r <= 0x9f) || r == 0x2028 || r == 0x2029 {
			return "", false
		}
	}
	return s, true
}

// POST /api/sessions/{id}/conversation/interrupt {"pane","pid","start","session"}
// POST /api/sessions/{id}/conversation/send-now (the same)
// POST /api/sessions/{id}/conversation/send (the same, and "text")
//
// Presses Escape in the pane (what Claude Code takes as "stop"), or Ctrl+X
// Ctrl+S (send the messages queued now), for the Claude Code process the
// conversation shows. The process is looked for again first: if that pane
// no longer runs it (another program, a restart), nothing is pressed and
// the answer is 409. Not with the terminal's own input: that goes to
// whatever pane is in front by the time it arrives.
func (s *Server) apiConversationInterrupt(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "interrupt")
}

func (s *Server) apiConversationSendNow(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "send_now")
}

// POST /api/sessions/{id}/conversation/mode (the same as interrupt, and "mode")
//
// Presses Shift+Tab: Claude Code goes on to its next permission mode. It
// writes the mode to its log only with the next message, so the answer
// says it as the screen shows it ({"ok":true,"mode":"plan"}; "" when the
// screen does not say). With "mode", Shift+Tab until the screen shows that
// one (see convoModeTo).
func (s *Server) apiConversationMode(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "mode")
}

// POST /api/sessions/{id}/conversation/models (the same as interrupt)
// POST /api/sessions/{id}/conversation/model (the same, and "model")
//
// The models Claude Code's picker offers ({"models":[{"label","desc",
// "current"}]}), or the session switched to one of them for this session
// only ({"ok":true,"model":…}): see modelFlow. Its log then says so.
func (s *Server) apiConversationModels(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "models")
}

func (s *Server) apiConversationModel(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "model")
}

// modeLines: the screen read after a key, for what (the mode's line).
func modeLines(what string) int {
	switch what {
	case "mode":
		return 8
	case "send":
		return sendLines
	}
	return 0
}

// sendLines: of the screen after a send, how much is read to see whether the
// words are still in Claude Code's input box (its end: the box and what
// follows it, a long message wrapped over many lines in it).
const sendLines = 60

// sendOut: what a send's command may print (its screen, wide lines and
// all): more than that fails the command, and a message typed was then
// said not known to have gone.
const sendOut = 256 << 10

// Send types a message into the agent's pane and presses Enter (Claude Code
// queues it while it works), as the terminal's composer would, but into that
// pane only: what herdr has in front, or the terminal shows, is not changed.
// POST /api/sessions/{id}/conversation/command (as a send, the text one of
// commandText's) → {"ok": true}; 409 agent_busy where it is not idle.
func (s *Server) apiConversationCommand(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "command")
}

func (s *Server) apiConversationSend(w http.ResponseWriter, r *http.Request) {
	s.convoPress(w, r, "send")
}

func (s *Server) convoPress(w http.ResponseWriter, r *http.Request, what string) {
	keys := convoKeys[what]
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Pane    string `json:"pane"`
		PID     int64  `json:"pid"`
		Start   int64  `json:"start"`
		Session string `json:"session"`
		Text    string `json:"text"`   // (send)
		Model   string `json:"model"`  // (model: as the picker names it)
		Effort  string `json:"effort"` // (effort: a level of effortLevels)
		Mode    string `json:"mode"`   // (mode: the one to go to, of convoModes; none: the next)
		// (reply: the screen answered, as screen gave it, and the keys
		// pressed (of replyKeys, up to 4) or the text typed and Enter.)
		Digest string   `json:"digest"`
		Keys   []string `json:"keys"`
	}
	err := readJSON(r, &b)
	text, textOK := sendText(b.Text)
	if what == "command" {
		text, textOK = commandText(b.Text)
	} else if what != "send" && what != "sent_missing" && what != "sent_enter" && what != "reply" {
		textOK = b.Text == ""
	}
	if what == "reply" {
		textOK = replyOK(b.Digest, b.Keys, b.Text) // (keys, or a line: its own rules)
		text = b.Text
	}
	if what != "reply" && (b.Digest != "" || len(b.Keys) > 0) {
		textOK = false
	}
	if what == "model" && (b.Model == "" || len(b.Model) > 64 || strings.ContainsFunc(b.Model, func(r rune) bool { return r < 0x20 || r == 0x7f })) {
		textOK = false
	}
	if what == "effort" && effortLevel(b.Effort) < 0 {
		textOK = false
	}
	if b.Mode != "" && (what != "mode" || !convoModes[b.Mode]) {
		textOK = false
	}
	if err != nil || !textOK {
		writeError(w, http.StatusBadRequest, "bad_request", "the pane, the process (and, to send, a message) are required")
		return
	}
	if b.PID <= 0 || b.Start <= 0 || !(convoUUID.MatchString(b.Session) || ocSessionID.MatchString(b.Session)) ||
		(sess.Persistence == "herdr" && !herdrPane.MatchString(b.Pane)) || (sess.Persistence == "tmux" && !tmuxPane.MatchString(b.Pane)) {
		writeError(w, http.StatusBadRequest, "bad_request", "the pane and the process are required")
		return
	}
	if sess.Persistence != "herdr" && sess.Persistence != "tmux" {
		writeError(w, http.StatusConflict, "unsupported", "only in tmux and herdr sessions")
		return
	}
	// Every attempt is audited, with what it was for and what became of it
	// ("typed", "refused" or "unknown"; why, when nothing was pressed).
	outcome, reason := "refused", ""
	defer func() {
		// (The suggestion is looked for by the page itself, at each turn's end:
		// only a screen that could not be read is noted, not each look, nor
		// one not made, the agent gone from the pane say.)
		if what == "suggest" && reason != "no_screen" {
			return
		}
		actx, acancel := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
		defer acancel()
		detail := map[string]any{"session": sess.ID, "pane": b.Pane, "pid": b.PID, "start": b.Start, "claude_session": b.Session, "outcome": outcome}
		if what == "send" {
			detail["length"] = len(text) // (what was said is not kept)
		}
		if what == "command" {
			detail["command"] = strings.Fields(text)[0] // (its words, a name say, are not kept)
		}
		if reason != "" {
			detail["reason"] = reason
		}
		s.audit(r.WithContext(actx), keys.audit, sess.HostName, detail)
	}()
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		reason = "signed_out"
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	ctx, cancel := context.WithDeadline(r.Context(), expiry)
	defer cancel()
	step := 15 * time.Second
	if what == "models" || what == "model" || what == "effort" || (what == "mode" && b.Mode != "") {
		step = 45 * time.Second // (several steps on the host: see modelFlow, convoModeTo)
	}
	ctx, cancelStep := context.WithTimeout(ctx, step)
	defer cancelStep()
	untrack := s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
	defer untrack()
	// A login ended meanwhile cancels the request: say that, not "the host".
	signedOut := func() bool {
		if _, err := s.auth.CheckLogin(context.WithoutCancel(r.Context()), loginHash(r)); err != nil {
			reason = "signed_out"
			writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
			return true
		}
		return false
	}
	// (One thing at a time to a pane, a message or keys (and an answer, see
	// replyToPane), from the check to its last key; once its command is on
	// its way, given back when that has ended.)
	// (An answer's lock first, then the pane's turn: as herdr's own answer takes them, see replyToPane.)
	var answer *paneReply
	// (Given back once the host's command has ended, not when this request
	// gives up on it: an answer still on its way there holds the pane.)
	unlockAnswer, answerHeld := func() {}, false
	defer func() {
		if !answerHeld {
			unlockAnswer()
		}
	}()
	if what == "reply" {
		lock, err := s.replies.lock(ctx, s.convoReplyKey(sess, b.Pane))
		if err != nil {
			reason = "busy"
			writeError(w, http.StatusTooManyRequests, "busy", "an earlier answer to this pane is still under way")
			return
		}
		var once sync.Once
		unlockAnswer = func() { once.Do(func() { s.replies.unlock(lock) }) }
		if lock.digest == b.Digest && time.Since(lock.at) < answeredFor {
			reason = "already_answered"
			writeError(w, http.StatusConflict, "already_answered", "this screen was just answered; look again before answering")
			return
		}
		answer = lock
	}
	turn, ok := convoSending.take(ctx, convoTurnKey(sess.ID, b.Pane))
	if !ok {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "another message to this agent is still being sent")
		return
	}
	defer func() { turn() }()

	tmuxTarget := "" // (the pane's own terminal, for whether a process is the pane's)
	if sess.Persistence == "tmux" {
		tmuxTarget = b.Pane
	}
	out, err := sshx.Output(ctx, sess.Client(), convoLocateCommand(sess.Persistence, sess.TmuxSession, tmuxTarget), convoLocateMax)
	if errors.Is(err, sshx.ErrBusy) {
		reason = "busy"
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	}
	// (A scan cut short, by its size or a failure, may leave out the agent
	// started last: what it found does not decide where keys go.)
	if err != nil {
		if signedOut() {
			return
		}
		reason = "host_error"
		writeError(w, http.StatusBadGateway, "host_error", "the host did not answer")
		return
	}
	// The process must be the one the pane's conversation shows now (chosen
	// as reading chooses it): a Claude Code started inside it since, or a
	// restart, is not the one the press was asked for.
	l := parseConvoLocate(string(out))
	socket := s.herdrSocks.get(sess.ID)
	tty := l.paneTTY // (tmux: the pane's terminal, active or not)
	front := pickConvoProc(l.procs, sess.Persistence, b.Pane, socket, l.tmuxPID, tty, s.herdrSocks.sid(sess.ID, b.Pane))
	// A Codex whose log its daemon keeps: the session this gateway gave
	// that very process when it read its conversation (not the one asked
	// of it, which is no word on what the pane runs).
	if front != nil && front.agent == "codex" && front.log == "" {
		if log, sid := s.convoBound(sess, b.Pane, front.pid, front.start); sid != "" {
			front.log, front.sid = log, sid
		} else {
			s.daemonLog(front, l)
		}
	}
	// An OpenCode: the session it is at now, asked anew (it may have gone to
	// another since it was read; one that cannot be told is not typed to).
	if front != nil && front.agent == "opencode" {
		var why string
		if front.sid, why = s.ocSessionOf(ctx, sess, front, b.Pane, l); why != "" {
			front.sid = ""
		}
	}
	if front == nil || front.pid != b.PID || front.start != b.Start || front.sid != b.Session {
		reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	}
	// (Claude Code's own keys and picker: not for Codex or OpenCode.)
	if (front.agent == "codex" || front.agent == "opencode") && what != "send" && what != "interrupt" {
		reason = "unsupported"
		writeError(w, http.StatusConflict, "unsupported", "not for this agent")
		return
	}
	// (Stopped with Ctrl+Z, or another program in front of it: keys typed
	// there would go to that.)
	if !front.fg {
		reason = "not_in_front"
		writeError(w, http.StatusConflict, "not_in_front", "the agent is not in front in its pane")
		return
	}
	if signedOut() {
		return
	}
	if what == "models" || what == "model" {
		release := turn
		turn = func() {} // (convoModel gives it back, once its steps have ended on the host)
		s.convoModel(w, ctx, sess, b.Pane, socket, b.PID, b.Start, b.Session, b.Model, &outcome, &reason, release)
		return
	}
	if what == "effort" {
		release := turn
		turn = func() {} // (convoEffort gives it back, once its steps have ended on the host)
		s.convoEffort(w, ctx, sess, b.Pane, socket, b.PID, b.Start, b.Session, b.Effort, &outcome, &reason, release)
		return
	}
	if what == "suggest" {
		release := turn
		turn = func() {} // (the screen read gives it back, once it has ended on the host)
		s.suggestion(w, ctx, sess, b.Pane, socket, &outcome, &reason, release)
		return
	}
	if what == "sent_missing" {
		release := turn
		turn = func() {} // (the screen read gives it back, once it has ended on the host)
		s.sentMissing(w, ctx, sess, b.Pane, socket, text, &outcome, &reason, release)
		return
	}
	if what == "sent_enter" {
		if front.agent != "claude" {
			outcome, reason = "refused", "not_claude"
			writeError(w, http.StatusConflict, "not_claude", "only Claude Code's input box can be told apart")
			return
		}
		release := turn
		turn = func() {} // (given back once its commands have ended on the host)
		s.sentEnter(w, ctx, sess, b.Pane, socket, b.PID, b.Start, b.Session, text, &outcome, &reason, release)
		return
	}
	if what == "screen" {
		release := turn
		turn = func() {} // (the screen read gives it back, once it has ended on the host)
		s.convoScreen(w, ctx, sess, b.Pane, socket, &outcome, &reason, release)
		return
	}
	// (An answer goes one at a time to the pane, whichever connection of
	// the person's it comes from; the same screen answered moments ago is
	// not answered again: a double tap.)
	// The check again and the typing or pressing, on the host in one go (see convoSendCommand).
	var press []string
	if what == "reply" && len(b.Keys) > 0 {
		press = replyPress(sess.Persistence, b.Keys)
	} else if what != "send" && what != "reply" && what != "command" {
		press = keys.tmux
		if sess.Persistence == "herdr" {
			press = keys.herdr
		}
		// (OpenCode stops at a second Esc, "esc again to interrupt": not one
		// right on the other, which reaches it as one key.)
		if what == "interrupt" && front.agent == "opencode" {
			press = append(append(append([]string(nil), press...), "pause"), press...)
		}
	}
	if what == "mode" && b.Mode != "" {
		release := turn
		turn = func() {} // (convoModeTo gives it back, once its last command has ended on the host)
		s.convoModeTo(w, ctx, sess, b.Pane, socket, b.PID, b.Start, b.Session, press, b.Mode, &outcome, &reason, release)
		return
	}
	release := turn
	turn = func() {} // (given back once its commands have ended on the host: an Enter again too, see enterAgain)
	var ended sync.WaitGroup
	answerHeld = true
	defer func() {
		go func() {
			ended.Wait()
			release()
			unlockAnswer()
		}()
	}()
	ended.Add(1)
	gate := ""
	switch {
	case what == "reply":
		gate = "reply:" + b.Digest
	case front.agent == "claude" && (what == "send" || what == "send_now" || what == "mode"):
		gate = "free" // (not while it waits: what is typed would go to its question)
	case what == "command":
		gate = "idle" // (not while it works either: it would wait in its queue, and run when no one looks)
	}
	res, err := sshx.Exec(ctx, sess.Client(), convoGatedCommand(gate, sess.Persistence, sess.TmuxSession, b.Pane, socket, b.PID, b.Start, b.Session, len(text), press, modeLines(what)), sendOut,
		sshx.ExecOptions{Stdin: strings.NewReader(text), Release: ended.Done})
	var exitErr *sshx.ExitError
	switch {
	case err == nil:
		outcome = "typed"
	case errors.Is(err, sshx.ErrBusy):
		reason = "busy" // (not started: nothing was typed)
	case errors.As(err, &exitErr) && exitErr.Status == 3:
		reason = "agent_changed"
		writeError(w, http.StatusConflict, "agent_changed", "that pane no longer runs this agent")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 7:
		reason = "busy" // (another to the pane on the host still: nothing typed)
	case errors.As(err, &exitErr) && exitErr.Status == 4:
		reason = "send_failed" // (nothing typed)
	case errors.As(err, &exitErr) && exitErr.Status == 9 && what == "reply":
		reason = "not_waiting"
		writeError(w, http.StatusConflict, "not_waiting", "it no longer waits for an answer")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 9:
		reason = "agent_waiting"
		writeError(w, http.StatusConflict, "agent_waiting", "it waits for an answer, or has a dialog open: answer it first")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 5 && gate != "":
		reason = "typed_not_sent"
		writeError(w, http.StatusConflict, "typed_not_sent", "the text is in its input, not sent: it came to wait for an answer first")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 10:
		reason = "screen_changed"
		writeError(w, http.StatusConflict, "screen_changed", "the screen changed; look again before answering")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 12:
		reason = "agent_busy"
		writeError(w, http.StatusConflict, "agent_busy", "it is not idle: a command waits until it is")
		return
	case errors.As(err, &exitErr) && exitErr.Status == 11:
		reason = "cannot_verify"
		writeError(w, http.StatusConflict, "cannot_verify", "the host cannot check the screen (no sha256sum): answer in the terminal")
		return
	default:
		outcome = "unknown" // (typed in part, or all of it but Enter, or it is not known)
	}
	if answer != nil && outcome != "refused" {
		answer.digest, answer.at = b.Digest, time.Now()
	}
	if outcome == "typed" && what == "send" && front.agent == "claude" {
		s.enterAgain(ctx, sess, b.Pane, socket, b.PID, b.Start, b.Session, text, string(res.Out), &ended, func(cancel func()) func() {
			return s.trackPending(currentUser(r).ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: cancel})
		})
	}
	switch {
	case outcome == "typed" && what == "mode":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "mode": screenMode(string(res.Out))})
	case outcome == "typed":
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	case reason == "busy":
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case outcome == "refused":
		writeError(w, http.StatusConflict, "refused", "the pane did not take the key")
	default:
		if signedOut() {
			return
		}
		writeError(w, http.StatusBadGateway, "unknown", "it is not known whether the key arrived")
	}
}

// GET /api/sessions/{id}/conversation/subagent?pane=&agent=<id>&gen=&rev=&older=
// → as /conversation (its entries, gen, rev, more, older), of a subagent
// the conversation's agent (Claude Code) sent off: its own log, beside the
// session's (<session>/subagents/agent-<id>.jsonl), read as the session's
// is, a part at a time. Only of the log the conversation reads now, by the
// id its calls gave: no other file, and not through a link.
func (s *Server) apiConversationSubagent(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane, agent := q.Get("pane"), q.Get("agent")
	if (pane != "" && !herdrPane.MatchString(pane)) || !convo.SubagentID(agent) {
		writeError(w, http.StatusBadRequest, "bad_request", "bad pane or subagent")
		return
	}
	gen, _ := strconv.ParseInt(q.Get("gen"), 10, 64)
	rev, _ := strconv.Atoi(q.Get("rev"))
	older := q.Get("older") == "1"
	w.Header().Set("Cache-Control", "no-store")
	// (The conversation's own reading, as it stands: whose log it is.)
	key := sess.ID + "\x00"
	if sess.Persistence == "herdr" {
		key += pane
	}
	main := s.convos.peek(key)
	if main == nil {
		writeError(w, http.StatusConflict, "not_located", "the conversation is not read yet")
		return
	}
	main.mu.Lock()
	root, agentKind, reason := main.log, main.agent, main.reason
	main.mu.Unlock()
	if root == "" || reason != "" || agentKind != "claude" || main.sub || !strings.HasSuffix(root, ".jsonl") {
		writeError(w, http.StatusConflict, "not_located", "the conversation is not read yet")
		return
	}
	home := strings.TrimSuffix(root, ".jsonl") // (the session's own folder, beside its log)
	dir := home + "/subagents"
	file := dir + "/agent-" + agent + ".jsonl"
	st := s.subConvos.get(key + "\x00" + root + "\x00" + agent)
	defer st.done()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.log != file {
		st.sub, st.agent, st.log, st.sid = true, "claude", file, agent
		st.gen = convoGens.Add(1)
		st.parser = st.newParser()
		st.offset, st.size, st.mtime, st.older, st.skipping, st.from = 0, -1, time.Time{}, false, false, 0
		st.long, st.strip = nil, b64Strip{}
	}
	// (The session's folder, its subagents' and the log: none a link (to
	// another session's, or out of them), the log a plain file (not a pipe
	// a read would wait on). Looked at, then opened: between the two, only
	// the account itself could change them, which can read all it reaches
	// in its terminal anyway; this keeps a log from being taken for
	// another, not a boundary.)
	err := s.withSFTP(r.Context(), sess, convoReadLimit, func(c *sftplite.Client) error {
		for _, p := range []string{home, dir, file} {
			fi, err := c.Lstat(p)
			if err != nil {
				return err
			}
			if fi.Mode()&fs.ModeSymlink != 0 || (p == file && !fi.Mode().IsRegular()) || (p != file && !fi.IsDir()) {
				return errSubagentFile
			}
		}
		return nil
	})
	if errors.Is(err, errSubagentFile) {
		writeError(w, http.StatusForbidden, "not_allowed", "not a subagent's log")
		return
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	more, err := s.readConvo(sess, st)
	if err == nil && older && gen == st.gen {
		err = s.readOlder(sess, st)
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	ans := convoAnswer{Available: true, Gen: st.gen, Rev: st.parser.Rev(), Older: st.older, More: more}
	ans.OlderCut = st.older && (st.parser.Trimmed() || st.from < 0)
	if gen != st.gen {
		ans.Reset = true
		rev = 0
	}
	ans.Entries = st.parser.Since(rev)
	ans.Tasks = st.parser.Tasks()
	ans.Running = st.parser.Running() // (what it set going in turn)
	if m := st.parser.Meta(); m != (convo.Meta{}) {
		ans.Meta = &m
	}
	writeJSONCompressed(w, r, http.StatusOK, ans)
}

var errSubagentFile = errors.New("not a subagent's log")
