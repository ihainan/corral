package web

// Files --------------------------------------------------------------------
//
// The file panel browses and changes files on a session's host over SFTP, on
// the terminal's own SSH connection and with the remote user's permissions
// (no more than the shell already has). Paths come from the browser; they are
// only ever SFTP arguments, never part of a command or a gateway-side path.
//
// Quick operations (listing, mkdir, rename, delete) share one SFTP channel per
// session, opened on first use and closed after a minute without use or when
// the session ends. A host that leaves one unanswered for too long loses the
// shared channel, and so does one whose channel broke; the next operation
// opens a new one. Transfers get a channel of their own, held by one of the
// session's transfer slots, so stopping one never disturbs anything else.
// Folders are deleted by the host's rm -rf, which never follows links and
// is not fooled by a folder being swapped for a link while it works.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"os"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/pkg/sftp"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sftplite"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/term"
)

var (
	fileOpTimeout     = 20 * time.Second
	fileDeleteTimeout = 2 * time.Minute
	sftpIdle          = time.Minute
	transferStall     = 2 * time.Minute // a transfer that makes no progress for this long is stopped
)

const (
	maxListEntries = 5000 // a listing beyond this is cut (and says so)
	maxLinkStats   = 256  // symbolic links whose target type is looked up per listing
	maxSharedUsers = 8    // operations waiting on one session's shared channel
	maxRemotePath  = 4096
	maxRemoteName  = 255
)

var (
	errFilesBusy       = errors.New("too many file operations at once")
	errFileTimeout     = errors.New("the host did not answer in time")
	errSFTPUnavailable = errors.New("the host does not allow file transfer")
	errNotDir          = errors.New("not a directory")
	errProtected       = errors.New("protected path")
)

// sftpPool keeps the shared SFTP channel of each session.
type sftpPool struct {
	mu sync.Mutex
	m  map[string]*sharedSFTP
}

type sharedSFTP struct {
	ready  chan struct{}    // closed once opening has finished
	gone   chan struct{}    // closed when the channel is given up
	client *sftplite.Client // set before ready closes; nil if opening failed
	close  func()
	err    error
	users  int
	idle   *time.Timer
	dead   bool
}

func newSFTPPool() *sftpPool { return &sftpPool{m: map[string]*sharedSFTP{}} }

// get returns the session's shared channel, opening it if needed. Every
// successful get is paired with one put.
func (p *sftpPool) get(ctx context.Context, sess *term.Session) (*sharedSFTP, error) {
	p.mu.Lock()
	c := p.m[sess.ID]
	if c == nil || c.dead {
		c = &sharedSFTP{ready: make(chan struct{}), gone: make(chan struct{})}
		p.m[sess.ID] = c
		go p.open(sess, c)
	}
	if c.users >= maxSharedUsers {
		p.mu.Unlock()
		return nil, errFilesBusy
	}
	c.users++
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	p.mu.Unlock()
	select {
	case <-c.ready:
	case <-ctx.Done():
		p.put(sess.ID, c, false)
		return nil, ctx.Err()
	}
	if c.err != nil {
		p.put(sess.ID, c, false)
		return nil, c.err
	}
	return c, nil
}

func (p *sftpPool) open(sess *term.Session, c *sharedSFTP) {
	client, closeFn, err := openSFTPLite(sess.Client())
	p.mu.Lock()
	if err != nil {
		c.err = fmt.Errorf("%w: %w", errSFTPUnavailable, err)
		p.killLocked(sess.ID, c)
	} else {
		c.client, c.close = client, closeFn
		if c.dead { // given up while opening (the session ended, or shutdown)
			closeFn()
		}
	}
	p.mu.Unlock()
	close(c.ready)
	if err == nil {
		go func() {
			select {
			case <-sess.Finished():
				p.kill(sess.ID, c)
			case <-c.gone:
			}
		}()
	}
}

// put ends one use; broken gives the channel up for everyone.
func (p *sftpPool) put(id string, c *sharedSFTP, broken bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	c.users--
	if broken {
		p.killLocked(id, c)
		return
	}
	if c.users == 0 && !c.dead {
		// A timer that fired just as the channel was taken again finds
		// another timer (or none) in c.idle, and leaves the channel alone.
		var t *time.Timer
		t = time.AfterFunc(sftpIdle, func() {
			p.mu.Lock()
			defer p.mu.Unlock()
			if c.idle == t && c.users == 0 {
				p.killLocked(id, c)
			}
		})
		c.idle = t
	}
}

func (p *sftpPool) kill(id string, c *sharedSFTP) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.killLocked(id, c)
}

func (p *sftpPool) killLocked(id string, c *sharedSFTP) {
	if c.dead {
		return
	}
	c.dead = true
	if p.m[id] == c {
		delete(p.m, id)
	}
	if c.idle != nil {
		c.idle.Stop()
		c.idle = nil
	}
	if c.close != nil {
		c.close()
	}
	close(c.gone)
}

func (p *sftpPool) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, c := range p.m {
		p.killLocked(id, c)
	}
}

// withSFTP runs f on the session's shared channel. If the host has not
// answered within limit, the channel is closed (which ends f at once) and
// errFileTimeout is returned. A browser that gives up does not stop f: the
// operation is short and others may share the channel.
func (s *Server) withSFTP(ctx context.Context, sess *term.Session, limit time.Duration, f func(*sftplite.Client) error) error {
	c, err := s.files.get(ctx, sess)
	if err != nil {
		return err
	}
	t := time.AfterFunc(limit, func() { s.files.kill(sess.ID, c) })
	err = f(c.client)
	timedOut := !t.Stop()
	s.files.put(sess.ID, c, timedOut || channelLost(err))
	if timedOut {
		return errFileTimeout
	}
	return err
}

// channelLost tells errors of the SFTP channel itself from the host's
// answers about files: after one, the channel is of no further use.
func channelLost(err error) bool {
	var st *sftp.StatusError
	if errors.As(err, &st) {
		return st.FxCode() == sftp.ErrSSHFxConnectionLost || st.FxCode() == sftp.ErrSSHFxNoConnection
	}
	return errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, sftp.ErrSSHFxConnectionLost) || errors.Is(err, sftp.ErrSSHFxNoConnection)
}

// runningSession resolves the {id} of a file request to one of the user's
// running sessions, or writes the error.
func (s *Server) runningSession(w http.ResponseWriter, r *http.Request) (*term.Session, bool) {
	sess, err := s.terms.Get(currentUser(r).ID, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "not_found", "no such session")
		return nil, false
	}
	if sess.State() != term.StateRunning {
		writeError(w, http.StatusConflict, "session_ended", "this session has ended")
		return nil, false
	}
	return sess, true
}

// cleanRemotePath accepts an absolute remote path.
func cleanRemotePath(p string) (string, bool) {
	if !strings.HasPrefix(p, "/") || len(p) > maxRemotePath || strings.ContainsRune(p, 0) || !utf8.ValidString(p) {
		return "", false
	}
	return path.Clean(p), true
}

// validName accepts a single path element.
func validName(n string) bool {
	return n != "" && n != "." && n != ".." && len(n) <= maxRemoteName && !strings.ContainsAny(n, "/\x00") && utf8.ValidString(n)
}

func writeFileError(w http.ResponseWriter, err error) {
	var status *sftp.StatusError
	var lstatus *sftplite.StatusError
	switch {
	case errors.Is(err, errFilesBusy), errors.Is(err, sshx.ErrBusy):
		writeError(w, http.StatusTooManyRequests, "busy", "too many file operations at once")
	case errors.Is(err, errFileTimeout):
		writeError(w, http.StatusGatewayTimeout, "sftp_timeout", "the host did not answer in time")
	case errors.Is(err, errSFTPUnavailable):
		writeError(w, http.StatusBadGateway, "sftp_unavailable", "the host does not allow file transfer")
	case errors.Is(err, errNotFile):
		writeError(w, http.StatusConflict, "not_a_file", "only a plain file can be replaced")
	case errors.Is(err, errNotDir):
		writeError(w, http.StatusBadRequest, "not_a_folder", "this is not a folder")
	case errors.Is(err, fs.ErrNotExist):
		writeError(w, http.StatusNotFound, "file_not_found", "no such file or folder")
	case errors.Is(err, fs.ErrPermission):
		writeError(w, http.StatusForbidden, "permission_denied", "permission denied on the host")
	case errors.Is(err, fs.ErrExist):
		writeError(w, http.StatusConflict, "file_exists", "a file or folder with this name already exists")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		writeError(w, http.StatusGatewayTimeout, "sftp_timeout", "the host did not answer in time")
	case errors.As(err, &status), errors.As(err, &lstatus):
		writeError(w, http.StatusBadGateway, "sftp_failed", "the host refused the operation")
	default:
		writeError(w, http.StatusBadGateway, "sftp_failed", "the file operation failed")
	}
}

// Listing --------------------------------------------------------------------

type fileEntry struct {
	Name  string `json:"name"`
	Type  string `json:"type"` // "dir", "file" or "other"; for a link, what it points to
	Link  bool   `json:"link"` // a symbolic link ("other" when its target is missing or was not looked up)
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
	Mode  string `json:"mode"`
}

func entryType(m fs.FileMode) string {
	switch {
	case m.IsDir():
		return "dir"
	case m.IsRegular():
		return "file"
	}
	return "other"
}

// apiFilesList lists a folder; without a path, the remote home folder.
func (s *Server) apiFilesList(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	dir := r.URL.Query().Get("path")
	if dir != "" {
		if dir, ok = cleanRemotePath(dir); !ok {
			writeError(w, http.StatusBadRequest, "bad_path", "the path must be absolute")
			return
		}
	}
	var entries []fileEntry
	truncated := false
	err := s.withSFTP(r.Context(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		if dir == "" {
			home, err := c.Getwd()
			if err != nil {
				return err
			}
			if dir, ok = cleanRemotePath(home); !ok {
				return errors.New("unusable home folder")
			}
		}
		// At most maxListEntries are read: a huge folder is never read whole
		// (the entries shown are the first ones the host lists, sorted).
		infos, more, err := c.ReadDirLimit(dir, maxListEntries)
		if err != nil {
			if fi, serr := c.Stat(dir); serr == nil && !fi.IsDir() {
				return errNotDir
			}
			return err
		}
		for _, fi := range infos {
			name := fi.Name()
			if name == "." || name == ".." || !validName(name) {
				continue // a name we could not address again
			}
			e := fileEntry{Name: name, Type: entryType(fi.Mode()), Size: fi.Size(), Mtime: fi.ModTime().Unix(), Mode: fi.Mode().String()}
			if fi.Mode()&fs.ModeSymlink != 0 {
				e.Link, e.Type = true, "other"
			}
			entries = append(entries, e)
		}
		sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
		// Links are looked up after sorting so the first ones shown get a type.
		var links []int
		for i := range entries {
			if entries[i].Link {
				links = append(links, i)
			}
		}
		if len(links) > maxLinkStats {
			links = links[:maxLinkStats]
		}
		statLinks(c, dir, entries, links)
		truncated = more
		return nil
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	if entries == nil {
		entries = []fileEntry{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": dir, "entries": entries, "truncated": truncated})
}

// statLinks resolves what the listed links point to, a few at a time.
func statLinks(c *sftplite.Client, dir string, entries []fileEntry, idx []int) {
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(8, len(idx)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range work {
				if fi, err := c.Stat(path.Join(dir, entries[i].Name)); err == nil {
					entries[i].Type, entries[i].Size = entryType(fi.Mode()), fi.Size()
				}
			}
		}()
	}
	for _, i := range idx {
		work <- i
	}
	close(work)
	wg.Wait()
}

// Changes ---------------------------------------------------------------------

func (s *Server) apiFilesMkdir(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Dir  string `json:"dir"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "dir and name are required")
		return
	}
	dir, ok := cleanRemotePath(b.Dir)
	if !ok || !validName(b.Name) {
		writeError(w, http.StatusBadRequest, "bad_name", "this name cannot be used")
		return
	}
	full := path.Join(dir, b.Name)
	err := s.withSFTP(r.Context(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		if _, err := c.Lstat(full); err == nil {
			return fs.ErrExist
		}
		return c.Mkdir(full)
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	s.audit(r, "file.mkdir", sess.HostName, map[string]any{"session": sess.ID, "path": full})
	writeJSON(w, http.StatusCreated, map[string]any{"path": full})
}

// apiFilesRename gives a file or folder a new name in the same folder.
func (s *Server) apiFilesRename(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Path string `json:"path"`
		Name string `json:"name"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "path and name are required")
		return
	}
	from, ok := cleanRemotePath(b.Path)
	if !ok || from == "/" || !validName(b.Name) {
		writeError(w, http.StatusBadRequest, "bad_name", "this name cannot be used")
		return
	}
	to := path.Join(path.Dir(from), b.Name)
	if to == from {
		writeJSON(w, http.StatusOK, map[string]any{"path": to})
		return
	}
	err := s.withSFTP(r.Context(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		if _, err := c.Lstat(from); err != nil {
			return err
		}
		// SFTP's rename does not replace on OpenSSH; the check covers servers
		// that would (a race with another writer remains).
		if _, err := c.Lstat(to); err == nil {
			return fs.ErrExist
		}
		return c.Rename(from, to)
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	s.audit(r, "file.rename", sess.HostName, map[string]any{"session": sess.ID, "from": from, "to": to})
	writeJSON(w, http.StatusOK, map[string]any{"path": to})
}

// apiFilesDelete deletes a file, a link (not what it points to), or a folder
// with everything in it. Neither the root nor the home folder (nor a folder
// holding it) can be deleted, also not through an alias: the SFTP server's
// resolved path is checked first, and for a folder the host checks again
// with its own resolution (some servers do not resolve links in realpath)
// before its rm -rf, which handles links and concurrent changes safely.
func (s *Server) apiFilesDelete(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	var b struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "path is required")
		return
	}
	p, ok := cleanRemotePath(b.Path)
	if !ok || p == "/" {
		writeError(w, http.StatusBadRequest, "bad_path", "this cannot be deleted")
		return
	}
	var target string
	folder := false
	err := s.withSFTP(r.Context(), sess, fileOpTimeout, func(c *sftplite.Client) error {
		home, err := c.Getwd()
		if err != nil {
			return err
		}
		if home, err = c.RealPath(home); err != nil {
			return err
		}
		dir, err := c.RealPath(path.Dir(p))
		if err != nil {
			return err
		}
		var ok bool
		if target, ok = cleanRemotePath(path.Join(dir, path.Base(p))); !ok {
			return errProtected
		}
		home = path.Clean(home)
		if target == "/" || target == home || strings.HasPrefix(home, target+"/") {
			return errProtected
		}
		fi, err := c.Lstat(target)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			folder = true
			return nil
		}
		return c.Remove(target)
	})
	if errors.Is(err, errProtected) {
		writeError(w, http.StatusBadRequest, "bad_path", "the root or home folder cannot be deleted here")
		return
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	if folder {
		ctx, cancel := context.WithTimeout(r.Context(), fileDeleteTimeout)
		defer cancel()
		_, err := sshx.Output(ctx, sess.Client(), removeTreeCommand(target), 64<<10)
		var exit *sshx.ExitError
		switch {
		case errors.As(err, &exit) && exit.Status == 4:
			writeError(w, http.StatusBadRequest, "bad_path", "the root or home folder cannot be deleted here")
			return
		case errors.As(err, &exit) && exit.Status == 3:
			writeFileError(w, fs.ErrNotExist)
			return
		case errors.Is(err, sshx.ErrBusy):
			writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
			return
		case errors.As(err, &exit):
			s.audit(r, "file.delete", sess.HostName, map[string]any{"session": sess.ID, "path": target, "folder": true, "complete": false})
			writeError(w, http.StatusBadGateway, "delete_incomplete", "some items could not be deleted")
			return
		case err != nil:
			// The command may still be running on the host.
			s.audit(r, "file.delete", sess.HostName, map[string]any{"session": sess.ID, "path": target, "folder": true, "complete": false})
			writeFileError(w, errFileTimeout)
			return
		}
	}
	s.audit(r, "file.delete", sess.HostName, map[string]any{"session": sess.ID, "path": target, "folder": folder})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// removeTreeCommand deletes a folder and its contents, unless the folder,
// with links resolved, is the root, the home folder or holds it (status 4);
// status 3: it cannot be resolved. Fixed script; the validated absolute path
// is its only argument.
//
// The shell first enters the folder's parent and checks there; rm then works
// relative to that directory, so swapping a folder above it for a link after
// the check changes nothing. The folder itself must be a real folder, not a
// link (and rm never follows one it meets inside).
func removeTreeCommand(p string) string {
	// pwd's output gets a "." after its line break, so that a folder name
	// ending in line breaks survives the command substitution.
	script := `d=${1%/*}; b=${1##*/}; [ -n "$b" ] || exit 4; cd -P -- "${d:-/}" 2>/dev/null || exit 3; ` +
		`p=$(pwd -P && echo .) || exit 3; p=${p%??}; t=${p%/}/$b; ` +
		`h=$(cd -P -- "$HOME" && pwd -P && echo .) || exit 4; h=${h%??}; ` +
		`[ -d "$b" ] && [ ! -L "$b" ] || exit 3; case "$h/" in "$t"/*) exit 4;; esac; exec rm -rf -- "./$b"`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(p)
}

// Terminal folder ----------------------------------------------------------------

// apiFilesCwd reports the folder the terminal is in, where the host can tell
// (see sessionCwd). Otherwise the path is empty.
func (s *Server) apiFilesCwd(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	cwd, err := s.sessionCwd(ctx, sess)
	if errors.Is(err, sshx.ErrBusy) {
		writeError(w, http.StatusTooManyRequests, "busy", "the host is still answering earlier requests")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": cwd})
}

// sessionCwd is the folder the terminal is in, where the host can tell:
// tmux's active pane, or herdr's focused pane; else "". The error is
// sshx.ErrBusy when the host is still answering earlier requests.
func (s *Server) sessionCwd(ctx context.Context, sess *term.Session) (string, error) {
	cwd := ""
	switch sess.Persistence {
	case "tmux":
		out, err := sshx.Output(ctx, sess.Client(), tmuxCwdCommand(sess.TmuxSession), 8192)
		if errors.Is(err, sshx.ErrBusy) {
			return "", err
		}
		if err == nil {
			cwd = strings.TrimRight(string(out), "\r\n")
		}
	case "herdr":
		if socket := s.herdrSocks.get(sess.ID); socket != "" {
			if c, err := herdr.Dial(ctx, sess.Client(), socket); err == nil {
				if snap, err := c.Snapshot(ctx); err == nil {
					for _, p := range snap.Panes {
						if p.ID == snap.FocusedPane {
							cwd = p.Cwd
						}
					}
				}
				c.Close()
			}
		}
	}
	if p, ok := cleanRemotePath(cwd); ok && !strings.ContainsAny(cwd, "\r\n") {
		return p, nil
	}
	return "", nil
}

// tmuxCwdCommand prints the current folder of the active pane of a tmux
// session. Fixed script; the validated session name is its only argument.
func tmuxCwdCommand(name string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
		`exec tmux -u display-message -p -t "=$1:" '#{pane_current_path}'`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(name)
}

// Transfers --------------------------------------------------------------------

// transfer is a file transfer bound to a session slot and to the login, as a
// terminal socket is: sign-out, password change, disable or the login's
// expiry stop it, and so does no progress for transferStall.
type transfer struct {
	ctx     context.Context
	cancel  context.CancelFunc
	stall   *time.Timer
	cleanup []func()
}

// progress postpones the stall watchdog.
func (t *transfer) progress() { t.stall.Reset(transferStall) }

func (t *transfer) end() {
	if t.stall != nil {
		t.stall.Stop()
	}
	for i := len(t.cleanup) - 1; i >= 0; i-- {
		t.cleanup[i]()
	}
}

// beginTransfer reserves a transfer slot and binds the request to the login;
// on failure it has written the response.
func (s *Server) beginTransfer(w http.ResponseWriter, r *http.Request, sess *term.Session) (*transfer, bool) {
	u := currentUser(r)
	releaseSlot, ok := sess.UploadSlot()
	if !ok {
		writeError(w, http.StatusTooManyRequests, "busy", "another transfer is in progress")
		return nil, false
	}
	t := &transfer{cleanup: []func(){releaseSlot}}
	expiry, err := s.auth.LoginExpiry(r.Context(), loginHash(r))
	if err != nil {
		t.end()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return nil, false
	}
	t.ctx, t.cancel = context.WithDeadline(r.Context(), expiry)
	t.cleanup = append(t.cleanup, t.cancel)
	t.cleanup = append(t.cleanup, s.trackPending(u.ID, &liveSocket{loginHash: loginHash(r), hostID: sess.HostID, cancel: t.cancel}))
	t.stall = time.AfterFunc(transferStall, t.cancel)
	if _, err := s.auth.CheckLogin(t.ctx, loginHash(r)); err != nil {
		t.end()
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return nil, false
	}
	// The login is checked again while the transfer runs, as for terminals.
	hash := loginHash(r)
	recheckDone := make(chan struct{})
	go func() {
		defer close(recheckDone)
		tick := time.NewTicker(loginRecheck)
		defer tick.Stop()
		for {
			select {
			case <-t.ctx.Done():
				return
			case <-tick.C:
				if _, err := s.auth.CheckLogin(t.ctx, hash); err != nil {
					t.cancel()
					return
				}
			}
		}
	}()
	t.cleanup = append(t.cleanup, func() { t.cancel(); <-recheckDone })
	// Reads and writes end with the transfer, also ones blocked on a client
	// that stopped sending or reading.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(expiry)
	_ = rc.SetWriteDeadline(expiry)
	// The callback never runs past the handler: it could otherwise touch the
	// next request on the same connection.
	stop := afterFuncWait(t.ctx, func() {
		_ = rc.SetReadDeadline(time.Now())
		_ = rc.SetWriteDeadline(time.Now())
		r.Body.Close()
	})
	t.cleanup = append(t.cleanup, stop)
	return t, true
}

type progressWriter struct {
	w io.Writer
	t *transfer
	n int64
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if n > 0 {
		p.t.progress()
	}
	return n, err
}

type progressReader struct {
	r io.Reader
	t *transfer
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.t.progress()
	}
	return n, err
}

// openTransferSFTP opens a channel for one transfer; it closes when the
// transfer ends, which also unblocks anything waiting on the host.
func (s *Server) openTransferSFTP(t *transfer, sess *term.Session) (*sftp.Client, error) {
	client, closeSFTP, err := openSFTP(sess.Client())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", errSFTPUnavailable, err)
	}
	stop := afterFuncWait(t.ctx, closeSFTP)
	t.cleanup = append(t.cleanup, closeSFTP, stop)
	return client, nil
}

// apiFilesDownload sends a remote file as an attachment.
func (s *Server) apiFilesDownload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	p, ok := cleanRemotePath(r.URL.Query().Get("path"))
	if !ok || p == "/" {
		writeError(w, http.StatusBadRequest, "bad_path", "the path must be absolute")
		return
	}
	t, ok := s.beginTransfer(w, r, sess)
	if !ok {
		return
	}
	defer t.end()
	client, err := s.openTransferSFTP(t, sess)
	if err != nil {
		writeFileError(w, err)
		return
	}
	f, err := client.Open(p)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		writeFileError(w, err)
		return
	}
	if !fi.Mode().IsRegular() {
		writeError(w, http.StatusBadRequest, "not_a_file", "only files can be downloaded")
		return
	}
	s.audit(r, "file.download", sess.HostName, map[string]any{"session": sess.ID, "path": p, "bytes": fi.Size()})
	disp := mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(p)})
	if disp == "" {
		disp = "attachment"
	}
	h := w.Header()
	h.Set("Content-Type", "application/octet-stream")
	h.Set("Content-Disposition", disp)
	h.Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	w.WriteHeader(http.StatusOK)
	// A file that changes while it is sent may come out short or long; the
	// declared length then makes the browser report a failed download.
	_, _ = io.Copy(&progressWriter{w: w, t: t}, io.LimitReader(f, fi.Size()))
}

// apiFilesUpload writes a file into a folder under the given name. An
// existing file is only replaced when asked to; the data goes to a temporary
// name first and is moved into place once complete.
func (s *Server) apiFilesUpload(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.runningSession(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	dir, ok := cleanRemotePath(q.Get("dir"))
	name := q.Get("name")
	if !ok || !validName(name) {
		writeError(w, http.StatusBadRequest, "bad_name", "this name cannot be used")
		return
	}
	overwrite := q.Get("overwrite") == "1"
	target := path.Join(dir, name)
	t, ok := s.beginTransfer(w, r, sess)
	if !ok {
		return
	}
	defer t.end()
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "expected a multipart upload")
		return
	}
	var part io.Reader
	for {
		p, err := mr.NextPart()
		if err != nil {
			writeError(w, http.StatusBadRequest, "bad_request", "no file in upload")
			return
		}
		if p.FormName() == "file" {
			part = p
			break
		}
	}
	client, err := s.openTransferSFTP(t, sess)
	if err != nil {
		writeFileError(w, err)
		return
	}
	var keepMode fs.FileMode
	replacing := false
	if fi, err := client.Lstat(target); err == nil {
		if !overwrite {
			writeFileError(w, fs.ErrExist)
			return
		}
		if fi.IsDir() {
			writeError(w, http.StatusConflict, "is_folder", "a folder with this name already exists")
			return
		}
		// Only a plain file is replaced: a link's own permissions say nothing
		// about what it points to, and replacing it would not write there.
		if !fi.Mode().IsRegular() {
			writeError(w, http.StatusConflict, "not_a_file", "only a plain file can be replaced")
			return
		}
		replacing, keepMode = true, fi.Mode().Perm()
	} else if !errors.Is(err, fs.ErrNotExist) {
		writeFileError(w, err)
		return
	}
	// The data goes into a staging folder that only the remote user can
	// enter, made private before anything is written to it, so no one else
	// can open the file while it fills; the replaced file is set aside there
	// too. Both move by rename, within the same folder's file system.
	//
	// Known limit: SFTP works by path, so in a folder where other users may
	// rename entries (world-writable without the sticky bit) they can still
	// swap the staging folder between steps. Such a folder lets them tamper
	// with anything put there anyway, by any tool; the checks below only
	// narrow that window.
	stage := path.Join(dir, ".corral-upload-"+randHex(6))
	if err := client.Mkdir(stage); err != nil {
		writeFileError(w, err)
		return
	}
	// Until the upload's file exists, only the (empty) folder is removed on
	// failure: rmdir neither empties a folder nor follows a link.
	if fi, err := client.Lstat(stage); err != nil || !fi.IsDir() {
		_ = client.RemoveDirectory(stage)
		writeFileError(w, fs.ErrExist)
		return
	}
	if err := client.Chmod(stage, 0o700); err != nil {
		_ = client.RemoveDirectory(stage)
		writeFileError(w, err)
		return
	}
	// The files inside get random names too: no one else can list the
	// folder, so even if it were swapped for a link, the names would not
	// match anything where the link points.
	st := staged{dir: stage, data: path.Join(stage, "u-"+randHex(8)), prev: path.Join(stage, "o-"+randHex(8))}
	tmp := st.data
	f, err := client.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL)
	if err != nil {
		_ = client.RemoveDirectory(stage)
		writeFileError(w, err)
		return
	}
	// The replaced file's permissions, or the ones the host gave the new
	// file; private (set on the open file, not by path) while it fills.
	mode := keepMode
	if !replacing {
		if fi, err := f.Stat(); err == nil {
			mode = fi.Mode().Perm()
		} else {
			mode = 0o644
		}
	}
	limit := s.cfg.UploadMaxBytes
	var written int64
	err = f.Chmod(0o600)
	if err == nil {
		written, err = io.Copy(f, io.LimitReader(&progressReader{r: part, t: t}, limit+1))
	}
	if err == nil && written > limit {
		err = errTooLarge
	}
	if err == nil {
		err = f.Chmod(mode)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	keep := false
	if err == nil {
		if replacing {
			keep, err = replaceFile(client, st, target)
		} else {
			err = client.Rename(tmp, target)
		}
	}
	if !keep {
		s.removeLater(client, sess, st)
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.Is(err, errTooLarge) || errors.As(err, &tooBig):
			writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("files are limited to %d MB", limit>>20))
		case errors.Is(err, errReplaceFailed):
			writeError(w, http.StatusBadGateway, "replace_failed", err.Error())
		case t.ctx.Err() != nil:
			writeError(w, http.StatusRequestTimeout, "upload_stopped", "the upload stopped")
		default:
			writeFileError(w, err)
		}
		return
	}
	s.audit(r, "file.upload", sess.HostName, map[string]any{"session": sess.ID, "path": target, "bytes": written, "replaced": replacing})
	writeJSON(w, http.StatusCreated, map[string]any{"path": target, "bytes": written})
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

var errReplaceFailed = errors.New("replace failed")

// replaceFile moves tmp over target. Where the host cannot do that in one
// step, the old file is first moved into the staging folder and put back if
// the new one cannot take its place, so a failure never loses both. keep
// reports that the staging folder holds the only copy of something (the old
// file, the upload, or possibly both) and must stay.
func replaceFile(c *sftp.Client, st staged, target string) (keep bool, err error) {
	stage, tmp := st.dir, st.data
	err = c.PosixRename(tmp, target)
	var status *sftp.StatusError
	if !errors.As(err, &status) || status.FxCode() != sftp.ErrSSHFxOpUnsupported {
		return false, err
	}
	kept := func() error {
		return fmt.Errorf("%w: the previous file and the upload are kept in %s", errReplaceFailed, path.Base(stage))
	}
	old := st.prev
	// Only a plain file is set aside, also if the name changed hands since
	// the upload began; anything else found there afterwards goes back.
	if fi, err := c.Lstat(target); err != nil {
		return false, err
	} else if !fi.Mode().IsRegular() {
		return false, errNotFile
	}
	if err := c.Rename(target, old); err != nil {
		// The answer may be lost although the move happened: only a host
		// that says the old file is still in place proves it was not moved.
		if _, serr := c.Lstat(old); errors.Is(serr, fs.ErrNotExist) && !channelLost(err) {
			return false, err
		} else if serr != nil {
			return true, kept()
		}
	}
	if fi, err := c.Lstat(old); err != nil || !fi.Mode().IsRegular() {
		if c.Rename(old, target) == nil {
			return false, errNotFile
		}
		return true, kept()
	}
	if err := c.Rename(tmp, target); err != nil {
		if c.Rename(old, target) == nil {
			return false, err
		}
		return true, kept()
	}
	return false, nil
}

var errNotFile = errors.New("not a plain file")

// staged is an upload's staging folder and the two files it may hold.
type staged struct{ dir, data, prev string }

// removeLater deletes a leftover staging folder: with the transfer's own
// channel while it works, otherwise (the transfer was stopped, which closes
// it) with the session's shared one.
func (s *Server) removeLater(c *sftp.Client, sess *term.Session, st staged) {
	if removeStage(c, st) == nil {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), fileOpTimeout)
		defer cancel()
		_ = s.withSFTP(ctx, sess, fileOpTimeout, func(c *sftplite.Client) error { return removeStage(c, st) })
	}()
}

// removeStage deletes a staging folder and the files the upload put there,
// by their random names. One that is no longer a real folder (someone able
// to rename in the parent folder swapped it for a link) is left alone.
// stageOps is what removing a staging folder takes, from either client.
type stageOps interface {
	Lstat(p string) (os.FileInfo, error)
	Remove(p string) error
	RemoveDirectory(p string) error
}

func removeStage(c stageOps, st staged) error {
	fi, err := c.Lstat(st.dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return nil
	}
	for _, p := range []string{st.data, st.prev} {
		if err := c.Remove(p); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	if err := c.RemoveDirectory(st.dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Coding agents ----------------------------------------------------------------

// geminiTitle matches the terminal titles Gemini CLI sets: "Gemini CLI
// (<folder>)", or a status symbol and two spaces ("◇  Ready (<folder>)").
var geminiTitle = regexp.MustCompile(`^(Gemini CLI |[◇✋⏲✦]  )`)

// geminiCommandLine is true for the command line of Gemini CLI: its own
// program ("gemini"); a JavaScript runtime (node, bun, deno) given a path to
// its script (".../gemini", ".../gemini.js", or a file of the gemini-cli
// package) anywhere in its arguments, or, after "x" or "run", its package
// or program;
// or a package runner running the package or its program (npx
// @google/gemini-cli, npm exec gemini, pnpm dlx...). A runtime's options are not parsed (they differ
// between runtimes and versions): a path to Gemini's script among them is
// taken as running it. Other mentions do not count (vim gemini.js, node
// server.js gemini, npm install @google/gemini-cli).
func geminiCommandLine(args string) bool {
	f := strings.Fields(strings.ToLower(args))
	if len(f) == 0 {
		return false
	}
	isPackage := func(a string) bool {
		a = strings.TrimPrefix(a, "npm:")
		rest, ok := strings.CutPrefix(a, "@google/gemini-cli")
		return ok && (rest == "" || strings.HasPrefix(rest, "@"))
	}
	isScript := func(a string) bool {
		b := path.Base(a)
		return strings.Contains(a, "/") && (b == "gemini" || b == "gemini.js") || strings.Contains(a, "/gemini-cli/")
	}
	// runsGemini: what a package runner runs, the package or its program
	// (npm exec gemini; npm also shows "npm exec --package=... -- gemini" so).
	runsGemini := func(a string) bool { return isPackage(a) || a == "gemini" }
	// operand is the first argument after skip words that is no option.
	operand := func(skip int) string {
		for _, a := range f[min(1+skip, len(f)):] {
			if !strings.HasPrefix(a, "-") {
				return a
			}
		}
		return ""
	}
	sub := ""
	if len(f) > 1 {
		sub = f[1]
	}
	switch path.Base(strings.TrimPrefix(f[0], "-")) {
	case "gemini":
		return true
	case "node", "nodejs", "bun", "deno":
		runs := false
		for _, a := range f[1:] {
			switch {
			case strings.HasPrefix(a, "-"):
			case isScript(a):
				return true
			case runs: // what "x" or "run" runs; later arguments are its own
				return runsGemini(a)
			case a == "x" || a == "run":
				runs = true
			}
		}
	case "npx", "pnpx", "bunx":
		return runsGemini(operand(0))
	case "npm":
		return (sub == "exec" || sub == "x") && runsGemini(operand(1))
	case "pnpm":
		return (sub == "dlx" || sub == "exec") && runsGemini(operand(1))
	case "yarn":
		return sub == "dlx" && runsGemini(operand(1))
	}
	return false
}

// pathPrefix is what goes before an uploaded file's path in the terminal:
// "@" when the program in front is Gemini CLI, which takes files as @path;
// otherwise nothing. It asks herdr (the agent of the focused pane), or tmux
// (the active pane's title, which a program that has ended may have left
// behind, so only together with the command line of the program in front).
// A session without either cannot tell what runs in front, nor can a host
// that does not answer quickly: no prefix.
func (s *Server) pathPrefix(ctx context.Context, sess *term.Session) string {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	switch sess.Persistence {
	case "herdr":
		socket := s.herdrSocks.get(sess.ID)
		if socket == "" {
			return ""
		}
		c, err := herdr.Dial(ctx, sess.Client(), socket)
		if err != nil {
			return ""
		}
		defer c.Close()
		snap, err := c.Snapshot(ctx)
		if err != nil {
			return ""
		}
		for _, p := range snap.Panes {
			if p.ID == snap.FocusedPane && strings.Contains(strings.ToLower(p.Agent), "gemini") {
				return "@"
			}
		}
	case "tmux":
		out, err := sshx.Output(ctx, sess.Client(), tmuxForegroundCommand(sess.TmuxSession), 4096)
		if err != nil {
			return ""
		}
		title, args, _ := strings.Cut(string(out), "\n")
		if geminiTitle.MatchString(strings.TrimSpace(title)) && geminiCommandLine(args) {
			return "@"
		}
	}
	return ""
}

// tmuxForegroundCommand prints the title of the active pane of a tmux
// session and, on the next line, the command line of the program in front
// there (the terminal's foreground process group, found with ps, or /proc
// where ps cannot tell).
func tmuxForegroundCommand(name string) string {
	script := `PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"
out=$(tmux -u display-message -p -t "=$1:" '#{pane_pid} #{pane_title}') || exit 1
pid=${out%% *}; title=${out#* }
case $pid in ''|*[!0-9]*) exit 1;; esac
fg=$(ps -o tpgid= -p "$pid" 2>/dev/null | tr -d ' ')
case $fg in ''|*[!0-9]*) fg=$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f6);; esac
case $fg in ''|0|*[!0-9]*) exit 1;; esac
args=$(ps -ww -o args= -p "$fg" 2>/dev/null)
[ -n "$args" ] || args=$(tr '\0' ' ' <"/proc/$fg/cmdline" 2>/dev/null)
printf '%s\n%s\n' "$title" "$args" | head -c 3000`
	return "sh -c " + sshx.QuoteShell(script) + " corral " + sshx.QuoteShell(name)
}
