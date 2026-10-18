package web

// Notifications: herdr status changes and terminal bells become Web Push
// messages, unless the user is looking at that session right now.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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

func (s *Server) lang(zh, en string) string {
	if strings.HasPrefix(s.cfg.DefaultLanguage, "zh") {
		return zh
	}
	return en
}

// onHerdrTransition alerts on "needs you" and on finishing work.
func (s *Server) onHerdrTransition(sess *term.Session, h *store.Host, tr herdr.Transition) {
	if s.notifier == nil || s.vis.watched(sess.ID) {
		return
	}
	agent := tr.Pane.Agent
	if agent == "" {
		agent = s.lang("终端", "terminal")
	}
	where := h.Name
	if tr.Workspace != "" {
		where += " · " + tr.Workspace
	}
	var note notify.Note
	switch {
	case tr.To == herdr.StatusBlocked:
		note = notify.Note{Title: fmt.Sprintf(s.lang("%s 在等你处理", "%s needs you"), agent), Body: where}
	case tr.To == herdr.StatusDone && tr.From == herdr.StatusWorking:
		note = notify.Note{Title: fmt.Sprintf(s.lang("%s 已完成", "%s finished"), agent), Body: where, Quiet: true}
	default:
		return
	}
	if tr.Pane.Title != "" {
		note.Body += "\n" + truncate(tr.Pane.Title, 120)
	}
	note.URL = "/t/" + sess.ID
	note.Tag = "herdr-" + sess.ID[:8] + "-" + strings.ReplaceAll(tr.Pane.ID, ":", "-")
	s.notifier.Send(context.Background(), sess.OwnerID, note)
}

// onTerminalAlert handles bells and OSC 9/777 notifications from programs.
func (s *Server) onTerminalAlert(sess *term.Session, a term.Alert) {
	if s.notifier == nil || s.vis.watched(sess.ID) {
		return
	}
	title := sess.HostName
	body := a.Message
	if body == "" {
		body = s.lang("终端发出了提醒。", "The terminal rang the bell.")
	}
	s.notifier.Send(context.Background(), sess.OwnerID, notify.Note{
		Title: title, Body: body, URL: "/t/" + sess.ID, Tag: "term-" + sess.ID[:8],
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
	err := s.store.SavePushSubscription(r.Context(), &store.PushSubscription{
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
		Title: "Corral", Body: s.lang("这是一条测试通知。", "This is a test notification."), URL: "/settings",
		Tag: fmt.Sprintf("test-%d", time.Now().Unix()),
	})
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}
