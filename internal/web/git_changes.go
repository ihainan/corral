package web

// What the agent's folder has changed, file by file with each file's diff,
// for the chat page's Changes view (reached from the session card's
// counts): the working tree against HEAD (staged and not), with the files
// git does not track listed; or against the remote's default branch, from
// where the branch left it (the card's "against base": the same merge-base,
// the same rename detection, tracked files only).
//
// As git_info.go: read only, the folder the pane's own (never the page's),
// the base found on the host as the card finds it. Of the request only
// which of the two ("work" or "base") and, to read one file's diff alone,
// a path in the repository: relative, no "..", no control character, taken
// literally (no pathspec magic), after "--". One git diff prints the list,
// the counts and the patch at once (a folder the agent edits meanwhile is
// not read as three different states); its output is cut on the host at
// gitChangesCap, said so, and every command is bounded in time. Where the
// host has no timeout(1) nothing runs; nor in a partial clone (the diff
// could fetch what it lacks).

import (
	"context"
	"errors"
	"net/http"
	"path"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ihainan/corral/internal/convo"
	"github.com/ihainan/corral/internal/sshx"
)

const (
	gitChangesCap      = 2 << 20  // the diff's output, cut on the host past this
	gitUntrackedCap    = 64 << 10 // the untracked list's
	gitChangesOut      = gitChangesCap + gitUntrackedCap + 16<<10
	gitUntrackedMax    = 500
	gitChangesPathMax  = 4096
	gitChangesFilesMax = 5000 // (files listed at most; past that, said)
)

// gitChangesCommand prints the changes as gitChangesCommand's parser reads
// them (parseGitChanges): a head of lines, then the diff's own output
// (--raw --numstat -z then the patch) and, where it ran to its end, its
// exit status; then the untracked list. Fixed script; the folder, "work" or
// "base", and a path or "" are its arguments.
func gitChangesCommand(dir, against, file string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`export GIT_OPTIONAL_LOCKS=0 GIT_TERMINAL_PROMPT=0 GIT_NO_LAZY_FETCH=1 GIT_LITERAL_PATHSPECS=1 LC_ALL=C; ` +
		`cd -- "$1" 2>/dev/null || { echo 'E nodir'; exit 0; }; ` +
		`command -v git >/dev/null 2>&1 || { echo 'E nogit'; exit 0; }; ` +
		`T=; if command -v timeout >/dev/null 2>&1; then T=timeout; elif command -v gtimeout >/dev/null 2>&1; then T=gtimeout; else echo 'E notimeout'; exit 0; fi; ` +
		`K=; $T -k 1 2 true 2>/dev/null && K='-k 2'; ` +
		`r() { s=$1; shift; $T $K "$s" "$@"; }; ` +
		`v=$(r 3 git version 2>/dev/null) || { case $? in 124|137|143) echo 'E timeout' ;; *) echo 'E nogit' ;; esac; exit 0; }; ` +
		`set -- "$1" "$2" "$3" $(printf '%s\n' "$v" | sed -n 's/^git version \([0-9]*\)\.\([0-9]*\).*/\1 \2/p'); ` +
		`[ -n "$5" ] && { [ "$4" -gt 2 ] || [ "$5" -ge 15 ]; } || { echo 'E oldgit'; exit 0; }; ` +
		`if [ "$4" -gt 2 ] || [ "$5" -ge 36 ]; then FS=false; else FS=; fi; ` +
		`q() { r 3 git -c core.fsmonitor=$FS "$@"; }; ` +
		`top=$(q rev-parse --show-toplevel 2>/dev/null) || { case $? in 124|137|143) echo 'E timeout' ;; *) echo 'E norepo' ;; esac; exit 0; }; ` +
		// (From the top: the paths are the repository's, wherever in it the pane is.)
		`cd -- "$top" 2>/dev/null || { echo 'E nodir'; exit 0; }; printf 'TOP %s\n' "$top"; ` +
		`partial=$(q config --get extensions.partialclone 2>/dev/null; q config --bool --get-regexp '^remote\..*\.promisor$' 2>/dev/null | grep ' true$'); ` +
		`[ -z "$partial" ] || { echo 'E partial'; exit 0; }; ` +
		`if [ "$2" = base ]; then ` +
		`base=$(q symbolic-ref -q --short refs/remotes/origin/HEAD 2>/dev/null); ` +
		`[ -n "$base" ] || for b in origin/main origin/master; do q rev-parse -q --verify "refs/remotes/$b" >/dev/null 2>&1 && { base=$b; break; }; done; ` +
		`[ -n "$base" ] && from=$(q merge-base HEAD "$base" 2>/dev/null) || { echo 'E nobase'; exit 0; }; printf 'BASE %s\n' "$base"; ` +
		// (No commit yet: against nothing, what is staged all added.)
		`else from=$(q rev-parse -q --verify HEAD 2>/dev/null) || from=$(q hash-object -t tree /dev/null 2>/dev/null) || { echo 'E norepo'; exit 0; }; fi; ` +
		`if [ -n "$3" ]; then set -- "$2" -- "$3"; else set -- "$2"; fi; a=$1; shift; ` +
		// (What a person's config could change of the output, set back to git's own. A diff given up
		// on (exit 124/137/143) or failed says so; one cut at the cap says nothing — its end not reached.)
		`printf '\001DIFF\n'; ` +
		`{ r 10 git -c core.fsmonitor=$FS -c core.quotepath=false -c diff.noprefix=false -c diff.mnemonicPrefix=false -c diff.relative=false -c diff.submodule=short -c diff.suppressBlankEmpty=false -c color.ui=false ` +
		`diff --no-color --no-ext-diff --no-textconv --src-prefix=a/ --dst-prefix=b/ --raw --numstat -z -p "$from" "$@" 2>/dev/null; printf '\n\001RC %s\n' "$?"; } | head -c ` + strconv.Itoa(gitChangesCap) + `; ` +
		`if [ "$a" = work ] && [ $# -eq 0 ]; then printf '\n\001UNTRACKED\n'; ` +
		`r 4 git -c core.fsmonitor=$FS ls-files --others --exclude-standard --directory --no-empty-directory -z 2>/dev/null | head -c ` + strconv.Itoa(gitUntrackedCap) + `; fi; ` +
		`printf '\n\001END\n'; exit 0`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(dir) + " " + sshx.QuoteShell(against) + " " + sshx.QuoteShell(file)
}

// gitChangedFile is one file of the changes: its path (and the one it had,
// renamed), what became of it (git's letter: A M D R C T U), its lines
// added and removed (Binary: none counted), and its diff where it was read
// — Cut where the cap fell in it; Omitted where it fell before it (to be
// asked for alone). No hunks, neither cut nor omitted nor binary: a change
// of mode or an empty file only.
type gitChangedFile struct {
	Path    string       `json:"path"`
	From    string       `json:"from,omitempty"`
	Status  string       `json:"status"`
	Add     int          `json:"add"`
	Del     int          `json:"del"`
	Binary  bool         `json:"binary,omitempty"`
	Hunks   []convo.Hunk `json:"hunks,omitempty"`
	Cut     bool         `json:"cut,omitempty"`
	Omitted bool         `json:"omitted,omitempty"`
}

type gitChanges struct {
	Repo   bool             `json:"repo"`
	Reason string           `json:"reason,omitempty"` // (none:) nodir, nogit, norepo, oldgit, notimeout, partial, nobase
	Root   string           `json:"root,omitempty"`
	Base   string           `json:"base,omitempty"`
	Files  []gitChangedFile `json:"files"`
	// More: files past those listed (the list itself cut). Untracked: what
	// git does not track and does not ignore, a folder of nothing tracked as
	// the folder ("dir/"), as the card counts them.
	More          bool     `json:"more,omitempty"`
	Untracked     []string `json:"untracked,omitempty"`
	UntrackedMore bool     `json:"untracked_more,omitempty"`
}

// errGitTimeout: the diff was given up on (too slow): not kept.
var errGitTimeout = errors.New("git took too long")

// parseGitChanges reads gitChangesCommand's output.
func parseGitChanges(out string) (gitChanges, error) {
	g := gitChanges{Files: []gitChangedFile{}}
	head, rest, _ := strings.Cut(out, "\x01DIFF\n")
	for _, line := range strings.Split(head, "\n") {
		key, v, _ := strings.Cut(strings.TrimRight(line, "\r"), " ")
		switch key {
		case "E":
			if v == "timeout" {
				return g, errGitTimeout
			}
			return gitChanges{Reason: v, Files: []gitChangedFile{}}, nil
		case "TOP":
			g.Repo, g.Root = true, v
		case "BASE":
			g.Base = v
		}
	}
	if !g.Repo {
		return gitChanges{Reason: "norepo", Files: []gitChangedFile{}}, nil
	}
	// The diff's part, and what follows it: its exit status where it ended
	// (none where the cap cut it), the untracked list.
	diff, tail, _ := strings.Cut(rest, "\n\x01RC ")
	cut := tail == ""
	if cut {
		diff, tail, _ = strings.Cut(rest, "\n\x01UNTRACKED\n")
		tail = "UNTRACKED\n" + tail
		if i := strings.Index(diff, "\n\x01END\n"); i >= 0 {
			diff = diff[:i]
		}
	} else {
		rc, after, _ := strings.Cut(tail, "\n")
		if rc != "0" && rc != "1" { // (1: some git's word for "there are differences")
			if rc == "124" || rc == "137" || rc == "143" {
				return g, errGitTimeout
			}
			return g, errors.New("git diff failed: " + rc)
		}
		tail = strings.TrimPrefix(after, "\n\x01")
	}
	g.Files, g.More = parseGitDiff(diff, cut)
	if u, ok := strings.CutPrefix(tail, "UNTRACKED\n"); ok {
		u, _, _ = strings.Cut(u, "\n\x01END")
		g.Untracked, g.UntrackedMore = splitNul(u, gitUntrackedMax, true)
	}
	return g, nil
}

// splitNul: the NUL-ended names of a list, at most max of them (more: there
// were others, or the last was cut — a name without its NUL is not whole).
func splitNul(s string, max int, ends bool) ([]string, bool) {
	var out []string
	more := false
	parts := strings.Split(s, "\x00")
	if ends {
		last := parts[len(parts)-1]
		parts = parts[:len(parts)-1]
		more = last != ""
	}
	for _, p := range parts {
		if !cleanName(p) {
			continue
		}
		if len(out) == max {
			return out, true
		}
		out = append(out, p)
	}
	return out, more
}

// cleanName: a path git printed that the page can be given as one (no
// control character, valid UTF-8: one that is not is left out, not shown
// mangled).
func cleanName(p string) bool {
	return p != "" && utf8.ValidString(p) && !strings.ContainsFunc(p, unicode.IsControl)
}

// parseGitDiff reads "git diff --raw --numstat -z -p": the raw records
// (":modes shas X\0path\0", a rename's or copy's two paths), the numstat
// ones in the same order ("add\tdel\tpath\0", a rename's "add\tdel\t\0from\0to\0";
// a binary file's counts "-"), a NUL, then the patch, a "diff --git" chunk
// a file in the same order again. cut: the output stopped at the cap.
func parseGitDiff(s string, cut bool) ([]gitChangedFile, bool) {
	files := []gitChangedFile{}
	more := false
	at := 0
	// next: the field up to the next NUL (ok false: none whole is left).
	next := func() (string, bool) {
		i := strings.IndexByte(s[at:], 0)
		if i < 0 {
			return "", false
		}
		f := s[at : at+i]
		at += i + 1
		return f, true
	}
	// (The list cut before its counts: none of it whole.)
	for at < len(s) && s[at] == ':' {
		rec, ok := next()
		if !ok {
			return keepWhole(files, 0), true
		}
		st := rec[strings.LastIndexByte(rec, ' ')+1:]
		if st == "" {
			return keepWhole(files, 0), true
		}
		f := gitChangedFile{Status: st[:1]}
		p, ok := next()
		if !ok {
			return keepWhole(files, 0), true
		}
		if f.Status == "R" || f.Status == "C" {
			to, ok := next()
			if !ok {
				return keepWhole(files, 0), true
			}
			f.From, p = p, to
		}
		f.Path = p
		files = append(files, f)
	}
	for i := range files {
		rec, ok := next()
		if !ok {
			return keepWhole(files, i), true
		}
		a, rest, _ := strings.Cut(rec, "\t")
		d, p, _ := strings.Cut(rest, "\t")
		if p == "" && files[i].From != "" { // (a rename's two paths follow)
			if _, ok := next(); !ok {
				return keepWhole(files, i), true
			}
			if p, ok = next(); !ok {
				return keepWhole(files, i), true
			}
		}
		if p != files[i].Path { // (not the file's counts: none of the rest taken as theirs)
			return keepWhole(files, i), true
		}
		if a == "-" && d == "-" {
			files[i].Binary = true
		} else {
			files[i].Add, _ = strconv.Atoi(a)
			files[i].Del, _ = strconv.Atoi(d)
		}
	}
	if at < len(s) && s[at] == 0 {
		at++
	}
	patch := s[at:]
	// The patch, a chunk a file: in order, each checked to be of its file's name where that is written plain.
	chunks := strings.Split("\n"+patch, "\ndiff --git ")[1:]
	for i := range files {
		if i >= len(chunks) {
			files[i].Omitted = true
			continue
		}
		c := chunks[i]
		if want := "a/" + nameOr(files[i].From, files[i].Path) + " b/" + files[i].Path + "\n"; !strings.HasPrefix(c, `"`) && !strings.HasPrefix(c, want) {
			files[i].Omitted = true // (not the file's: nothing shown rather than another's lines)
			continue
		}
		last := i == len(chunks)-1
		files[i].Hunks, files[i].Cut = parseHunks(c)
		if cut && last {
			files[i].Cut = true
		}
		if files[i].Binary {
			files[i].Hunks, files[i].Cut = nil, false
		}
	}
	// (Bad names out; the list bounded.)
	kept := files[:0]
	for _, f := range files {
		if cleanName(f.Path) && (f.From == "" || cleanName(f.From)) {
			kept = append(kept, f)
		}
	}
	files = kept
	if len(files) > gitChangesFilesMax {
		files, more = files[:gitChangesFilesMax], true
	}
	return files, more
}

func nameOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// keepWhole: the files whose counts were read; the list cut there.
func keepWhole(files []gitChangedFile, n int) []gitChangedFile {
	files = files[:n]
	for i := range files {
		files[i].Omitted = true
	}
	return files
}

// parseHunks reads a file's chunk of a patch: its "@@ -a,b +c,d @@" hunks
// and their lines (" ", "+", "-"); "\ No newline at end of file" and
// anything else not a line of a hunk left out. cut: a hunk with fewer lines
// than its head says (the output stopped in it).
func parseHunks(c string) ([]convo.Hunk, bool) {
	var hs []convo.Hunk
	left := 0 // (lines the hunk's head says are still to come: old and new counted as git does)
	oldLeft, newLeft := 0, 0
	for _, l := range strings.Split(c, "\n") {
		if strings.HasPrefix(l, "@@ -") {
			o, n, oc, nc, ok := hunkHead(l)
			if !ok {
				continue
			}
			hs = append(hs, convo.Hunk{OldStart: o, NewStart: n, Lines: []string{}})
			oldLeft, newLeft = oc, nc
			left = oc + nc
			continue
		}
		if len(hs) == 0 || left == 0 || l == "" {
			continue
		}
		h := &hs[len(hs)-1]
		switch l[0] {
		case ' ':
			oldLeft--
			newLeft--
		case '-':
			oldLeft--
		case '+':
			newLeft--
		default:
			continue
		}
		h.Lines = append(h.Lines, l)
		left = max(oldLeft, 0) + max(newLeft, 0)
	}
	return hs, left > 0
}

// hunkHead reads "@@ -o[,oc] +n[,nc] @@…" (a count left out is 1).
func hunkHead(l string) (o, n, oc, nc int, ok bool) {
	f := strings.Fields(l)
	if len(f) < 3 || !strings.HasPrefix(f[1], "-") || !strings.HasPrefix(f[2], "+") {
		return 0, 0, 0, 0, false
	}
	read := func(s string) (int, int, bool) {
		a, b, has := strings.Cut(s[1:], ",")
		x, err := strconv.Atoi(a)
		if err != nil {
			return 0, 0, false
		}
		y := 1
		if has {
			if y, err = strconv.Atoi(b); err != nil {
				return 0, 0, false
			}
		}
		return x, y, true
	}
	o, oc, ok1 := read(f[1])
	n, nc, ok2 := read(f[2])
	return o, n, oc, nc, ok1 && ok2
}

// cleanRepoPath checks a path in the repository the page asks one file's
// diff of: relative, as git printed it, nothing that would leave it.
func cleanRepoPath(p string) bool {
	if p == "" || len(p) > gitChangesPathMax || !cleanName(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "-") {
		return false
	}
	return path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

// GET /api/sessions/{id}/git/changes?pane=&against=work|base[&path=]: the
// changes of the pane's folder (gitChanges); with path, that file's alone.
func (s *Server) apiGitChanges(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pane, against, file := q.Get("pane"), q.Get("against"), q.Get("path")
	if len(pane) > gitPaneMax || (against != "work" && against != "base") || (file != "" && !cleanRepoPath(file)) {
		writeError(w, http.StatusBadRequest, "bad_request", "no such change")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gitWaitTime)
	defer cancel()
	w.Header().Set("Cache-Control", "no-store")
	key := "changes\x00" + sess.ID + "\x00" + pane + "\x00" + against + "\x00" + file
	v, err := s.gitCalls.do(ctx, key, gitInfoTTL, func(ctx context.Context) (any, error) {
		cwd, err := s.paneCwd(ctx, sess, pane)
		if err != nil {
			return nil, err
		}
		if cwd == "" {
			return nil, errGitNoDir
		}
		out, err := sshx.Output(ctx, sess.Client(), gitChangesCommand(cwd, against, file), gitChangesOut)
		if err != nil {
			return nil, err
		}
		return parseGitChanges(string(out))
	})
	switch {
	case errors.Is(err, errGitNoDir):
		writeJSON(w, http.StatusOK, gitChanges{Reason: "nodir", Files: []gitChangedFile{}})
	case errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
	case errors.Is(err, errGitTimeout), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "timeout", "git took too long")
	case err != nil:
		writeError(w, http.StatusBadGateway, "host_error", "the host did not say")
	default:
		writeJSONCompressed(w, r, http.StatusOK, v)
	}
}
