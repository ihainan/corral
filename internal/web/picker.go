package web

// Choosing what to open on a host. A host has no fixed choice: opening it
// connects, looks for tmux and herdr there and lists their sessions, and the
// user picks one (or a new tmux session, or a plain shell). The connection
// is kept a short while for the pick, so the host is connected to (and its
// password asked for) once.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/sshx"
	"github.com/ihainan/corral/internal/store"
)

// What can be opened.
const (
	openShell   = "shell"    // a plain login shell
	openTmux    = "tmux"     // an existing tmux session (Name)
	openTmuxNew = "tmux_new" // a new tmux session, named by the gateway
	openHerdr   = "herdr"    // a herdr session (Name; "" is herdr's default)
	// A new herdr session, named by the gateway. herdr has no way to start
	// only a new one: a session of that name made meanwhile is attached.
	openHerdrNew = "herdr_new"
)

type openChoice struct {
	Kind string `json:"kind"`
	Name string `json:"name"`
}

func (c *openChoice) valid() bool {
	switch c.Kind {
	case openShell, openTmuxNew, openHerdrNew:
		return c.Name == ""
	case openTmux:
		return tmuxNameRE.MatchString(c.Name)
	case openHerdr:
		return c.Name == "" || tmuxNameRE.MatchString(c.Name)
	}
	return false
}

// What a host has to choose from.
type tmuxSession struct {
	Name     string `json:"name"`
	Windows  int    `json:"windows"`
	Attached bool   `json:"attached"` // a client is attached (elsewhere, or a Corral terminal)
}

type herdrChoice struct {
	Name    string `json:"name"`
	Default bool   `json:"default"`
	Running bool   `json:"running"` // a stopped one starts again when opened
}

type hostChoices struct {
	Tmux      bool          `json:"tmux"` // installed
	TmuxList  []tmuxSession `json:"tmux_sessions"`
	Herdr     bool          `json:"herdr"`
	HerdrList []herdrChoice `json:"herdr_sessions"`
	// The host could not be asked (it took too long, or failed): a new tmux
	// session and a plain shell are offered.
	Unknown bool `json:"unknown,omitempty"`
}

const probeTimeout = 6 * time.Second

// tmuxListCommand lists the tmux sessions of the user ("name|windows|
// attached"), after a line "tmux" if tmux is installed. No tmux server
// running is no sessions; any other failure fails the command. Fixed text.
const tmuxListCommand = `sh -c 'PATH="$HOME/.local/bin:$HOME/bin:/opt/homebrew/bin:/usr/local/bin:$PATH"; ` +
	`command -v tmux >/dev/null 2>&1 || exit 0; echo tmux; ` +
	`out=$(tmux -u list-sessions -F "#{session_name}|#{session_windows}|#{session_attached}" 2>&1) && { printf "%s\n" "$out"; exit 0; }; ` +
	`case "$out" in *"no server running"*|*"(No such file or directory)"*) exit 0;; esac; ` +
	`printf "%s\n" "$out" >&2; exit 1'`

func tmuxSessions(ctx context.Context, c *ssh.Client) (installed bool, list []tmuxSession, err error) {
	out, err := sshx.Output(ctx, c, tmuxListCommand, 256<<10)
	if err != nil {
		return false, nil, err
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	if len(lines) == 0 || lines[0] != "tmux" {
		return false, nil, nil
	}
	for _, l := range lines[1:] {
		f := strings.Split(l, "|")
		if len(f) != 3 || !tmuxNameRE.MatchString(f[0]) {
			continue // (a name Corral could not attach by: not offered)
		}
		n, _ := strconv.Atoi(f[1])
		a, _ := strconv.Atoi(f[2])
		list = append(list, tmuxSession{Name: f[0], Windows: n, Attached: a > 0})
	}
	return true, list, nil
}

// maxListed bounds the sessions offered (not those known when naming one).
const maxListed = 100

// probeHost asks the host what it has.
func probeHost(ctx context.Context, c *ssh.Client) hostChoices {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out := hostChoices{TmuxList: []tmuxSession{}, HerdrList: []herdrChoice{}}
	var err error
	if out.Tmux, out.TmuxList, err = tmuxSessions(ctx, c); err != nil {
		return hostChoices{TmuxList: []tmuxSession{}, HerdrList: []herdrChoice{}, Unknown: true}
	}
	if out.TmuxList == nil {
		out.TmuxList = []tmuxSession{}
	}
	if len(out.TmuxList) > maxListed {
		out.TmuxList = out.TmuxList[:maxListed]
	}
	list, err := herdr.Discover(ctx, c)
	switch {
	case err == nil:
		out.Herdr = true
		// Running ones first (herdr's own order within each).
		for _, running := range []bool{true, false} {
			for _, si := range list {
				if si.Running == running && tmuxNameRE.MatchString(si.Name) && len(out.HerdrList) < maxListed {
					out.HerdrList = append(out.HerdrList, herdrChoice{Name: si.Name, Default: si.Default, Running: si.Running})
				}
			}
		}
	case errors.Is(err, herdr.ErrNotInstalled):
	default:
		out.Unknown = true
	}
	return out
}

func (c hostChoices) any() bool { return c.Tmux || c.Herdr || c.Unknown }

// newTmuxName is a session name for a new tmux session: "corral-<user>",
// then with -2, -3… after it, the first not taken.
func newTmuxName(username string, taken []tmuxSession) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t.Name] = true
	}
	return newMuxName(username, used)
}

// newHerdrName is the same for herdr, after all its sessions, stopped ones
// included (a stopped one of the name would be started instead).
func newHerdrName(username string, taken []herdr.SessionInfo) string {
	used := map[string]bool{}
	for _, t := range taken {
		used[t.Name] = true
	}
	return newMuxName(username, used)
}

func newMuxName(username string, used map[string]bool) string {
	base := defaultTmuxSession(username)
	if !used[base] {
		return base
	}
	for i := 2; ; i++ {
		suffix := "-" + strconv.Itoa(i)
		name := base
		if len(name)+len(suffix) > 64 {
			name = name[:64-len(suffix)]
		}
		if !used[name+suffix] {
			return name + suffix
		}
	}
}

// Connections kept for a pick ---------------------------------------------

const (
	pendingTTL      = time.Minute
	maxPendingConns = 3 // per user; the oldest goes
)

type pendingConn struct {
	owner, host int64
	cfg         string // the host configuration it was made with
	login       string // the login it was made for
	client      *ssh.Client
	at          time.Time
	timer       *time.Timer
}

type pendingConns struct {
	mu     sync.Mutex
	m      map[string]*pendingConn
	closed bool // the server is shutting down: nothing is kept
}

// put keeps a connection for the owner's pick and returns its token.
func (p *pendingConns) put(owner, host int64, cfg, login string, c *ssh.Client) string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	token := hex.EncodeToString(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		go c.Close()
		return token // (never found)
	}
	if p.m == nil {
		p.m = map[string]*pendingConn{}
	}
	var mine []*pendingConn
	for _, pc := range p.m {
		if pc.owner == owner {
			mine = append(mine, pc)
		}
	}
	for len(mine) >= maxPendingConns {
		oldest := 0
		for i, pc := range mine {
			if pc.at.Before(mine[oldest].at) {
				oldest = i
			}
		}
		p.dropLocked(mine[oldest])
		mine = append(mine[:oldest], mine[oldest+1:]...)
	}
	pc := &pendingConn{owner: owner, host: host, cfg: cfg, login: login, client: c, at: time.Now()}
	pc.timer = time.AfterFunc(pendingTTL, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		if p.m[token] == pc {
			p.dropLocked(pc)
		}
	})
	p.m[token] = pc
	return token
}

// dropLocked closes a kept connection; p.mu is held.
func (p *pendingConns) dropLocked(pc *pendingConn) {
	for t, x := range p.m {
		if x == pc {
			delete(p.m, t)
		}
	}
	pc.timer.Stop()
	go pc.client.Close()
}

// take returns the connection kept under token, if it is the owner's, for
// that host, made for the same login with the host's current configuration;
// nil otherwise. A connection that does not match is closed.
func (p *pendingConns) take(token string, owner, host int64, cfg, login string) *ssh.Client {
	if token == "" {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	pc := p.m[token]
	if pc == nil || pc.owner != owner {
		return nil
	}
	if pc.host != host || pc.cfg != cfg || pc.login != login {
		p.dropLocked(pc)
		return nil
	}
	delete(p.m, token)
	pc.timer.Stop()
	return pc.client
}

// dropHost closes the connections kept for a host (edited, deleted).
func (p *pendingConns) dropHost(owner, host int64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pc := range p.m {
		if pc.owner == owner && pc.host == host {
			p.dropLocked(pc)
		}
	}
}

// dropLogins closes the connections kept for a user's logins that match
// (signed out, disabled, password changed).
func (p *pendingConns) dropLogins(owner int64, match func(login string) bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, pc := range p.m {
		if pc.owner == owner && match(pc.login) {
			p.dropLocked(pc)
		}
	}
}

// shutdown closes every kept connection and keeps no more.
func (p *pendingConns) shutdown() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, pc := range p.m {
		p.dropLocked(pc)
	}
}

// Opens being connected or asked at once, per user: each holds an SSH
// handshake and a probe (a terminal slot is only taken to start one).
const maxConnectingOpens = 4

type openSlots struct {
	mu sync.Mutex
	n  map[int64]int
}

func (o *openSlots) acquire(user int64) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n == nil {
		o.n = map[int64]int{}
	}
	if o.n[user] >= maxConnectingOpens {
		return false
	}
	o.n[user]++
	return true
}

func (o *openSlots) release(user int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.n[user]--; o.n[user] <= 0 {
		delete(o.n, user)
	}
}

// runningChoice is a Corral terminal of the host that is running now.
type runningChoice struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"` // what it runs in: shell, tmux or herdr
	Name      string `json:"name"`
	Title     string `json:"title"`
	CreatedAt int64  `json:"created_at"`
}

// sameMux is true for two names of one tmux or herdr session: for herdr, ""
// is its default session, which is named "default".
func sameMux(kind, a, b string) bool {
	if kind == openHerdr {
		if a == "" {
			a = "default"
		}
		if b == "" {
			b = "default"
		}
	}
	return a == b
}

// sessionChoice is what a running session was opened as.
func sessionKind(persistence string) string {
	switch persistence {
	case store.PersistTmux:
		return openTmux
	case store.PersistHerdr:
		return openHerdr
	}
	return openShell
}
