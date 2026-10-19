package web

// Notifications: herdr status changes and terminal bells become Web Push
// messages, unless the user is looking at that session right now.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
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

// onHerdrTransition alerts on "needs you" and on finishing work.
func (s *Server) onHerdrTransition(sess *term.Session, h *store.Host, tr herdr.Transition) {
	if s.notifier == nil || s.vis.watched(sess.ID) {
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
	var note notify.Note
	switch {
	case tr.To == herdr.StatusBlocked:
		note = notify.Note{Title: agent + " needs you", TitleZh: agentZh + " 在等你处理", Body: where}
	case tr.To == herdr.StatusDone && tr.From == herdr.StatusWorking:
		note = notify.Note{Title: agent + " finished", TitleZh: agentZh + " 已完成", Body: where, Quiet: true}
	default:
		return
	}
	if tr.Pane.Title != "" {
		note.Body += "\n" + truncate(tr.Pane.Title, 120)
	}
	note.BodyZh = note.Body
	note.URL = "/t/" + sess.ID
	if tr.To == herdr.StatusBlocked {
		note.URL += "?reply=" + url.QueryEscape(tr.Pane.ID) // opens the answer to it
	}
	note.Tag = "herdr-" + sess.ID[:8] + "-" + strings.ReplaceAll(tr.Pane.ID, ":", "-")
	s.notifier.Send(context.Background(), sess.OwnerID, note)
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
	var b struct {
		Endpoint string `json:"endpoint"`
		Keys     struct {
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
