package web

// Notifications: herdr status changes and terminal bells become Web Push
// messages, unless the user is looking at that session right now.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ihainan/corral/internal/herdr"
	"github.com/ihainan/corral/internal/notify"
	"github.com/ihainan/corral/internal/store"
	"github.com/ihainan/corral/internal/term"
)

// visibility tracks, per terminal session, how many open sockets report that
// their page is visible.
type visibility struct {
	mu   sync.Mutex
	seen map[string]map[*liveSocket]bool
}

func (v *visibility) set(sessionID string, ls *liveSocket, visible bool) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.seen[sessionID] == nil {
		v.seen[sessionID] = map[*liveSocket]bool{}
	}
	v.seen[sessionID][ls] = visible
}

func (v *visibility) drop(sessionID string, ls *liveSocket) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.seen[sessionID], ls)
	if len(v.seen[sessionID]) == 0 {
		delete(v.seen, sessionID)
	}
}

func (v *visibility) watched(sessionID string) bool {
	v.mu.Lock()
	defer v.mu.Unlock()
	for _, on := range v.seen[sessionID] {
		if on {
			return true
		}
	}
	return false
}

// waitAlerts follows agents waiting for the user, by alert tag (a pane of a
// session): which wait is under way, the "needs you" alert sent for it, and
// the single-use tokens its buttons answer with.
type waitAlerts struct {
	mu      sync.Mutex
	seq     uint64
	waiting map[string]uint64     // tag → the wait under way
	sent    map[string]*waitAlert // tag → the alert shown for it
	tokens  map[string]*waitAlert // a button's token → its alert
}

// current says whether a wait is still the one under way for its tag.
func (a *waitAlerts) current(tag string, wait uint64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.waiting[tag] == wait
}

// waitAlert is a "needs you" alert sent for one wait.
type waitAlert struct {
	tag       string
	wait      uint64
	user      int64
	session   string
	pane      string
	digest    string          // of the screen the alert was made from
	keys      map[string]bool // the choices its buttons press
	token     string
	at        time.Time
}

// tokenTTL is how long an alert's buttons answer.
const tokenTTL = 30 * time.Minute

// begin notes that an agent started to wait, and names this wait.
func (a *waitAlerts) begin(tag string) uint64 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waiting == nil {
		a.waiting, a.sent, a.tokens = map[string]uint64{}, map[string]*waitAlert{}, map[string]*waitAlert{}
	}
	if len(a.waiting) > 5000 { // (waits of sessions gone without ending)
		a.waiting = map[string]uint64{}
	}
	for k, al := range a.tokens {
		if time.Since(al.at) > tokenTTL {
			delete(a.tokens, k)
		}
	}
	a.seq++
	a.waiting[tag] = a.seq
	return a.seq
}

// send sends an alert for a wait if that wait still lasts and the alert is
// not held back, and keeps it; both under the lock, so the wait's end sees
// either no alert or this one. (Devices show the alerts of a tag in the
// order they were sent, see Note.Seq.)
func (a *waitAlerts) send(al *waitAlert, sendNote func() bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.waiting[al.tag] != al.wait || !sendNote() {
		return false
	}
	return a.keep(al)
}

// keep records an alert sent (the lock held).
func (a *waitAlerts) keep(al *waitAlert) bool {
	al.at = time.Now()
	if old := a.sent[al.tag]; old != nil {
		delete(a.tokens, old.token)
	}
	a.sent[al.tag] = al
	if al.token != "" {
		a.tokens[al.token] = al
	}
	return true
}

// sendReplacement sends what replaces an alert, unless the agent waits
// again (that wait's alert replaces it).
func (a *waitAlerts) sendReplacement(tag string, sendNote func() bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, again := a.waiting[tag]; !again {
		sendNote()
	}
}

// end notes that the agent of a tag no longer waits, and returns the alert
// shown for its wait, if any (to be replaced). Its buttons stop answering.
func (a *waitAlerts) end(tag string) *waitAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.waiting, tag)
	al := a.sent[tag]
	delete(a.sent, tag)
	if al != nil {
		delete(a.tokens, al.token)
	}
	return al
}

// endSession ends every wait of a session (it finished).
func (a *waitAlerts) endSession(sessionID string) []*waitAlert {
	a.mu.Lock()
	var tags []string
	for tag, al := range a.sent {
		if al.session == sessionID {
			tags = append(tags, tag)
		}
	}
	for tag := range a.waiting {
		if strings.HasPrefix(tag, waitTagPrefix(sessionID)) {
			delete(a.waiting, tag)
		}
	}
	a.mu.Unlock()
	var out []*waitAlert
	for _, tag := range tags {
		if al := a.end(tag); al != nil {
			out = append(out, al)
		}
	}
	return out
}

// use takes a button's token, once: the alert it belongs to, if the token is
// the user's, its wait still lasts and the key is one of its choices.
func (a *waitAlerts) use(token string, user int64, key string) *waitAlert {
	a.mu.Lock()
	defer a.mu.Unlock()
	al := a.tokens[token]
	if al == nil || al.user != user || time.Since(al.at) > tokenTTL || a.waiting[al.tag] != al.wait || !al.keys[key] {
		return nil
	}
	delete(a.tokens, token)
	return al
}

func waitTagPrefix(sessionID string) string { return "herdr-" + sessionID[:8] + "-" }

// randomToken names an alert's answer: 128 random bits.
func randomToken() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// How long after an agent starts waiting its screen is read for the alert
// (the question drawn), and how long the reading may take.
var (
	waitScreenDelay   = 700 * time.Millisecond
	waitScreenTimeout = 5 * time.Second
)

// onHerdrTransition alerts on "needs you" and on finishing work, and
// replaces a "needs you" alert once the agent no longer waits.
func (s *Server) onHerdrTransition(sess *term.Session, h *store.Host, tr herdr.Transition) {
	if s.notifier == nil {
		return
	}
	agent, agentZh := tr.Pane.Agent, tr.Pane.Agent
	if agent == "" {
		agent, agentZh = "terminal", "终端"
	}
	where := h.Name
	if tr.Workspace != "" {
		where += " · " + tr.Workspace
	}
	tag := waitTagPrefix(sess.ID) + strings.ReplaceAll(tr.Pane.ID, ":", "-")
	// Opened, the alert brings the session to this pane (herdr focuses it).
	note := notify.Note{Body: where, BodyZh: where, URL: "/t/" + sess.ID + "?pane=" + url.QueryEscape(tr.Pane.ID), Tag: tag}
	// Waiting no more: the alert on the devices is replaced, also while the
	// session is looked at (that is often where it was answered).
	if tr.From == herdr.StatusBlocked && tr.To != herdr.StatusBlocked {
		if al := s.waitAlerts.end(tag); al != nil {
			note.Replace, note.Quiet = true, true
			if tr.To == herdr.StatusDone {
				note.Title, note.TitleZh = agent+" finished", agentZh+" 已完成"
			} else {
				note.Title, note.TitleZh = agent+" no longer waits for you", agentZh+" 不再等你了"
			}
			s.replaceAlert(sess.OwnerID, al, note)
			return
		}
	}
	if tr.To == herdr.StatusBlocked {
		wait := s.waitAlerts.begin(tag) // (also while looked at: a later alert must not be for this wait)
		if !s.vis.watched(sess.ID) {
			go s.alertWaiting(sess, tr, wait, note, agent, agentZh)
		}
		return
	}
	if s.vis.watched(sess.ID) {
		return
	}
	if tr.To == herdr.StatusDone && tr.From == herdr.StatusWorking {
		note.Title, note.TitleZh, note.Quiet = agent+" finished", agentZh+" 已完成", true
		if tr.Pane.Title != "" {
			note.Body += "\n" + truncate(tr.Pane.Title, 120)
			note.BodyZh = note.Body
		}
		s.notifier.Send(context.Background(), sess.OwnerID, note)
	}
}

// replaceAlert replaces an alert, unless the agent waits again meanwhile.
func (s *Server) replaceAlert(user int64, al *waitAlert, note notify.Note) {
	s.waitAlerts.sendReplacement(al.tag, func() bool { return s.notifier.Send(context.Background(), user, note) })
}

// endSessionAlerts replaces the "needs you" alerts of a session that ended.
func (s *Server) endSessionAlerts(sess *term.Session) {
	if s.notifier == nil {
		return
	}
	for _, al := range s.waitAlerts.endSession(sess.ID) {
		note := notify.Note{Title: sess.HostName + ": session ended", TitleZh: sess.HostName + "：会话已结束", URL: "/", Tag: al.tag, Quiet: true, Replace: true}
		s.replaceAlert(sess.OwnerID, al, note)
	}
}

// alertWaiting sends "needs you" with what the agent asks, read from its
// screen. When the agent asks with a menu and all it shows with it fits in
// the alert, the first and last choices become buttons, answering with a
// single-use token while this wait lasts and the screen is unchanged.
// Without the screen (herdr slow or gone) the alert goes as it is.
func (s *Server) alertWaiting(sess *term.Session, tr herdr.Transition, wait uint64, note notify.Note, agent, agentZh string) {
	note.Title, note.TitleZh = agent+" needs you", agentZh+" 在等你处理"
	note.URL += "&reply=" + url.QueryEscape(tr.Pane.ID) // opens the answer to it
	note.Distinct = true                                // (another wait than the last alert of this pane)
	al := &waitAlert{tag: note.Tag, wait: wait, user: sess.OwnerID, session: sess.ID, pane: tr.Pane.ID}
	if text, digest, ok := s.waitingScreen(sess, tr.Pane.ID); ok {
		w := waitQuestion(text)
		if w.question != "" {
			note.Body += "\n" + w.question
		}
		if w.complete && len(w.menu) >= 2 {
			first, last := w.menu[0], w.menu[len(w.menu)-1]
			// A button shows its whole choice, or there is none.
			if len([]rune(first.label)) <= waitLabelMax && len([]rune(last.label)) <= waitLabelMax {
				al.digest, al.token, al.keys = digest, randomToken(), map[string]bool{}
				for _, c := range []waitChoice{first, last} {
					note.Actions = append(note.Actions, notify.Action{Action: c.key, Title: c.label})
					al.keys[c.key] = true
				}
				note.Answer = al.token
			}
		}
	} else if tr.Pane.Title != "" {
		note.Body += "\n" + truncate(tr.Pane.Title, 120)
	}
	note.BodyZh = note.Body
	// Looked at meanwhile (the screen was being read): no alert.
	if s.vis.watched(sess.ID) || sess.State() != term.StateRunning {
		return
	}
	s.waitAlerts.send(al, func() bool { return s.notifier.Send(context.Background(), sess.OwnerID, note) })
}

// waitingScreen reads a waiting pane's screen, shortly after it started to
// wait.
func (s *Server) waitingScreen(sess *term.Session, pane string) (text, digest string, ok bool) {
	select {
	case <-time.After(waitScreenDelay):
	case <-sess.Finished():
		return "", "", false
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		return "", "", false
	}
	ctx, cancel := context.WithTimeout(context.Background(), waitScreenTimeout)
	defer cancel()
	c, err := herdr.Dial(ctx, sess.Client(), socket)
	if err != nil {
		return "", "", false
	}
	defer c.Close()
	text, digest, err = paneScreen(ctx, c, pane)
	return text, digest, err == nil
}

// alertAnswerHook runs between taking a token and typing (tests: the wait
// changing meanwhile).
var alertAnswerHook func()

// POST /api/alerts/answer {"token": "…", "key": "1"}
// A button of a "needs you" alert: the choice is pressed in the pane, once,
// while the agent still waits as the alert said and its screen is the one
// the alert was made from (as a reply from the dialog is checked).
func (s *Server) apiAlertAnswer(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Token string `json:"token"`
		Key   string `json:"key"`
	}
	if err := readJSON(r, &b); err != nil || len(b.Token) == 0 || len(b.Token) > 64 {
		writeError(w, http.StatusBadRequest, "bad_request", "token and key are required")
		return
	}
	al := s.waitAlerts.use(b.Token, currentUser(r).ID, b.Key)
	if al == nil {
		writeError(w, http.StatusGone, "alert_gone", "this alert no longer answers; open it to look")
		return
	}
	if alertAnswerHook != nil {
		alertAnswerHook()
	}
	sess, err := s.terms.Get(currentUser(r).ID, al.session)
	if err != nil || sess.State() != term.StateRunning {
		writeError(w, http.StatusGone, "alert_gone", "the session has ended")
		return
	}
	socket := s.herdrSocks.get(sess.ID)
	if socket == "" {
		writeError(w, http.StatusConflict, "herdr_unavailable", "herdr is not available in this session")
		return
	}
	s.replyToPane(w, r, sess, socket, al.pane, al.digest, []string{b.Key}, "", func() bool { return s.waitAlerts.current(al.tag, al.wait) })
}

type waitChoice struct{ key, label string }

// As the reply dialog reads a menu (ReplyDialog.tsx, choices): numbered
// lines in a row, one marked as selected, followed by at most a few lines
// of blank space, borders or key hints.
var (
	waitMenuLine = regexp.MustCompile(`^[\s│|]*([❯›>▶→●◉])?\s*([1-9])[.)]\s+(\S.*?)\s*[│|]?\s*$`)
	waitBorder   = regexp.MustCompile(`^[\s│|╭╮╰╯─━┌┐└┘]*$`)
	waitKeyHint  = regexp.MustCompile(`(?i)\b(esc|cancel|confirm)\b`)
	// A line that is a border alone: the top of a prompt's box or rule.
	waitRuleLine = regexp.MustCompile(`^\s*[╭┌]?[─━]{3,}[╮┐]?\s*$`)
)

// isHint: blank space, a border, or a short line of key hints ("Esc to
// cancel"); a longer line may say more (a warning) and is no hint.
func isHint(l string) bool {
	return waitBorder.MatchString(l) || (len([]rune(strings.TrimSpace(l))) <= waitHintMax && waitKeyHint.MatchString(l))
}

const (
	waitQuestionMax = 300 // characters of the question in an alert
	waitLabelMax    = 32  // characters of a button (a longer choice gets none)
	waitHintMax     = 40  // characters of a line of key hints
	waitAboveMax    = 12  // lines above a menu looked at for its box
)

type waitAsk struct {
	question string       // what the agent shows with its question, one line
	menu     []waitChoice // its choices, when it asks with a menu
	complete bool         // the question is all of the menu's box: buttons may answer it
}

// waitQuestion finds what a waiting agent asks at the end of its screen: the
// text above its menu of choices, up to the top of the box or rule the
// menu is drawn in (complete when found within waitAboveMax lines and short
// enough to show whole); without a menu, its last lines.
func waitQuestion(text string) waitAsk {
	lines := strings.Split(text, "\n")
	end := len(lines)
	for end > 0 && !waitMenuLine.MatchString(lines[end-1]) && isHint(lines[end-1]) && len(lines)-end < 3 {
		end--
	}
	var menu []waitChoice
	marked, start := 0, end
	for i := end - 1; i >= 0; i-- {
		m := waitMenuLine.FindStringSubmatch(lines[i])
		if m == nil {
			break
		}
		menu = append([]waitChoice{{key: m[2], label: m[3]}}, menu...)
		if m[1] != "" {
			marked++
		}
		start = i
	}
	inOrder := true
	for i, c := range menu {
		inOrder = inOrder && c.key == strconv.Itoa(i+1)
	}
	trimmed := func(i int) string { return strings.Trim(lines[i], " \t│|╭╮╰╯─━┌┐└┘") }
	if len(menu) < 2 || !inOrder || marked != 1 {
		// No menu: the last three lines of text.
		var q []string
		for i := len(lines) - 1; i >= 0 && len(q) < 3 && len(lines)-i <= 40; i-- {
			if l := trimmed(i); l != "" && !isHint(l) {
				q = append([]string{l}, q...)
			} else if len(q) > 0 {
				break
			}
		}
		return waitAsk{question: clip(strings.Join(q, " "), waitQuestionMax)}
	}
	var q []string
	top := false
	for i := start - 1; i >= 0 && start-i <= waitAboveMax; i-- {
		if waitRuleLine.MatchString(lines[i]) {
			top = true
			break
		}
		if l := trimmed(i); l != "" {
			q = append([]string{l}, q...)
		}
	}
	joined := strings.Join(q, " ")
	complete := top && len(q) > 0 && len([]rune(joined)) <= waitQuestionMax
	if !complete && !top {
		// Without its box, the lines right above the menu.
		if len(q) > 3 {
			q = q[len(q)-3:]
		}
		joined = strings.Join(q, " ")
	}
	return waitAsk{question: clip(joined, waitQuestionMax), menu: menu, complete: complete}
}

// clip shortens s to n characters, the last one an ellipsis.
func clip(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}
	return s
}

// onTerminalAlert handles bells and OSC 9/777 notifications from programs.
func (s *Server) onTerminalAlert(sess *term.Session, a term.Alert) {
	if s.notifier == nil || s.vis.watched(sess.ID) {
		return
	}
	body, bodyZh := a.Message, a.Message
	if body == "" {
		body, bodyZh = "The terminal rang the bell.", "终端发出了提醒。"
	}
	s.notifier.Send(context.Background(), sess.OwnerID, notify.Note{
		Title: sess.HostName, TitleZh: sess.HostName, Body: body, BodyZh: bodyZh, URL: "/t/" + sess.ID, Tag: "term-" + sess.ID[:8],
	})
}

// Push API ----------------------------------------------------------------

// maxPushSubscriptionsPerUser caps stored devices; the oldest make way.
var maxPushSubscriptionsPerUser = 10

func (s *Server) apiPushInfo(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false})
		return
	}
	subs, err := s.store.PushSubscriptions(r.Context(), currentUser(r).ID)
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": true, "public_key": s.notifier.PublicKey(), "devices": subs, "webhooks": s.notifier.WebhooksEnabled(),
	})
}

func (s *Server) apiPushSubscribe(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeError(w, http.StatusNotFound, "not_found", "notifications are not available")
		return
	}
	// A PushSubscription's toJSON(); its expirationTime (usually null) is
	// accepted and not used.
	var b struct {
		Endpoint       string   `json:"endpoint"`
		ExpirationTime *float64 `json:"expirationTime"`
		Keys           struct {
			P256dh string `json:"p256dh"`
			Auth   string `json:"auth"`
		} `json:"keys"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	if err := s.notifier.CheckEndpoint(b.Endpoint); err != nil || len(b.Endpoint) > 2048 ||
		b.Keys.P256dh == "" || b.Keys.Auth == "" || len(b.Keys.P256dh) > 200 || len(b.Keys.Auth) > 100 {
		writeError(w, http.StatusUnprocessableEntity, "push_endpoint", "this browser's push service is not supported")
		return
	}
	ua := r.UserAgent()
	if len(ua) > 200 {
		ua = ua[:200]
	}
	// Bound to this browser login: signing out (or any end of the login)
	// removes it, so a shared or handed-on device stops receiving.
	version, err := s.store.SavePushSubscription(r.Context(), &store.PushSubscription{
		UserID: currentUser(r).ID, LoginHash: loginHash(r), Endpoint: b.Endpoint, P256dh: b.Keys.P256dh, Auth: b.Keys.Auth, UserAgent: ua,
	}, maxPushSubscriptionsPerUser)
	if errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusUnauthorized, "unauthorized", "please sign in")
		return
	}
	if err != nil {
		s.internalError(w, r, err)
		return
	}
	// Notes already on their way to this device for its previous owner stop.
	s.notifier.Rebound(b.Endpoint, version)
	s.audit(r, "push.subscribe", "", nil)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiPushUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var b struct {
		Endpoint string `json:"endpoint"`
		ID       int64  `json:"id"`
	}
	if err := readJSON(r, &b); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", err.Error())
		return
	}
	u := currentUser(r)
	if b.ID > 0 {
		_ = s.store.DeletePushSubscriptionByID(r.Context(), u.ID, b.ID)
	} else {
		_ = s.store.DeletePushSubscription(r.Context(), u.ID, b.Endpoint)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) apiPushTest(w http.ResponseWriter, r *http.Request) {
	if s.notifier == nil {
		writeError(w, http.StatusNotFound, "not_found", "notifications are not available")
		return
	}
	s.notifier.Send(r.Context(), currentUser(r).ID, notify.Note{
		Title: "Corral", Body: "This is a test notification.", TitleZh: "Corral", BodyZh: "这是一条测试通知。", URL: "/settings",
		Tag: fmt.Sprintf("test-%d", time.Now().Unix()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
