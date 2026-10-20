// Package notify delivers "an agent needs you" style alerts to a user's devices.
//
// Detection happens on the server (a phone suspends background tabs, so the
// browser cannot be relied on to notice anything). Delivery is Web Push to every
// browser the user enabled, plus an optional webhook the administrator allowed.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"

	"github.com/ihainan/corral/internal/store"
)

// Note is one alert.
type Note struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	// The same in Chinese: each device shows the language it uses.
	TitleZh string `json:"title_zh,omitempty"`
	BodyZh  string `json:"body_zh,omitempty"`
	URL     string `json:"url"`   // path inside Corral to open on tap
	Tag     string `json:"tag"`   // same tag replaces the previous alert on the device
	Quiet   bool   `json:"quiet"` // informational: no banner on platforms that support it
	// Buttons, on the platforms that show them (not iOS): each answers with
	// its Action, using Answer.
	Actions []Action `json:"actions,omitempty"`
	Answer  string   `json:"answer,omitempty"` // a single-use token the buttons answer with
	// Replace takes the place of an earlier alert with the same tag that is
	// no longer true (an agent that stopped waiting): devices only, never
	// collapsed or limited.
	Replace bool `json:"-"`
	// Distinct is not collapsed with the last alert of its tag (another
	// wait of the same agent); it still counts against the limit.
	Distinct bool `json:"-"`
	// Seq orders the alerts of a tag (set by Send): a device that gets an
	// older one after a newer one (pushes can overtake each other) keeps
	// showing the newer.
	Seq int64 `json:"seq"`
}

// Action is a button on an alert.
type Action struct {
	Action  string `json:"action"` // the key it presses (see Reply)
	Title   string `json:"title"`
	TitleZh string `json:"title_zh,omitempty"`
}

// Push services browsers hand out endpoints for. The gateway only ever POSTs to
// these, so a crafted subscription cannot point it at an internal address.
var defaultPushHosts = []string{".push.apple.com", "fcm.googleapis.com", ".push.services.mozilla.com", ".notify.windows.com"}

type Notifier struct {
	store         *store.Store
	log           *slog.Logger
	vapidPublic   string
	vapidPrivate  string
	subscriber    string
	pushHosts     []string
	webhookPrefix []webhookPrefix
	webhookZh     bool // webhooks in Chinese (Options.WebhookChinese)
	http          *http.Client
	mu            sync.Mutex
	recent        map[string]time.Time // per user+tag, to collapse bursts
	minGap        time.Duration
	sent          map[int64][]time.Time // per user, the alerts of the last rateWindow
	rateMax       int                   // at most this many per user in rateWindow
	lastSeq       int64                 // the last Note.Seq given
	// Per user, the notes waiting to be delivered, in the order sent: one
	// at a time, so a push service that keeps only the last note of a
	// topic for an offline device keeps the newest. Webhooks have a line of
	// their own, so a slow one does not hold up the devices.
	pushes, hooks lines
	revoked       map[int64]uint64      // per user, bumped when access ends; older queued notes are dropped
	sending       map[*sending]struct{}
	loginValid    func(ctx context.Context, loginHash string) (bool, error)
}

// sending is one delivery in progress: a push to one registration of a
// device (cancelled when its login or user loses access, or the endpoint is
// registered again), or a webhook (loginHash and endpoint empty; cancelled
// when the user loses access).
type sending struct {
	userID    int64
	loginHash string
	endpoint  string
	version   int64
	cancel    context.CancelFunc
}

type Options struct {
	Subscriber      string   // contact for push services, e.g. "https://ssh.example.com"
	ExtraPushHosts  []string // host suffixes to allow besides the major push services
	WebhookPrefixes []string // URL prefixes users may send webhooks to; empty disables webhooks
	// LoginValid reports whether the browser login a subscription belongs to
	// is still in force, or an error if that could not be checked; nil
	// treats every login as ended.
	LoginValid func(ctx context.Context, loginHash string) (bool, error)
	// WebhookChinese sends webhooks in Chinese (a webhook has no device
	// language to go by).
	WebhookChinese bool
}

func New(ctx context.Context, st *store.Store, log *slog.Logger, opt Options) (*Notifier, error) {
	prefixes, err := parseWebhookPrefixes(opt.WebhookPrefixes)
	if err != nil {
		return nil, err
	}
	pub, priv, err := vapidKeys(ctx, st)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		webhookZh: opt.WebhookChinese,
		store:     st, log: log, vapidPublic: pub, vapidPrivate: priv, subscriber: opt.Subscriber,
		pushHosts: append(append([]string{}, defaultPushHosts...), opt.ExtraPushHosts...), webhookPrefix: prefixes,
		// Never follow redirects, for push and webhooks alike: an allowed
		// endpoint must not be able to bounce the gateway to another address.
		http: &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		recent: map[string]time.Time{}, minGap: 20 * time.Second, sent: map[int64][]time.Time{}, rateMax: 10, revoked: map[int64]uint64{}, sending: map[*sending]struct{}{}, loginValid: opt.LoginValid,
	}, nil
}

// webhookPrefix is an allowed webhook destination: exact scheme and host
// (with port), and a path prefix matched on whole segments.
type webhookPrefix struct {
	scheme, host, path string // path always ends with "/"
}

func parseWebhookPrefixes(raw []string) ([]webhookPrefix, error) {
	var out []webhookPrefix
	for _, p := range raw {
		u, err := url.Parse(p)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || hasDotSegment(u.Path) {
			return nil, fmt.Errorf("webhook prefix %q must be an http(s) URL with a host and an optional path, nothing else", p)
		}
		path := u.EscapedPath()
		if !strings.HasSuffix(path, "/") {
			path += "/" // "https://hooks.example.com" means the whole host, never "hooks.example.com.evil"
		}
		out = append(out, webhookPrefix{scheme: u.Scheme, host: strings.ToLower(u.Host), path: path})
	}
	return out, nil
}

// hasDotSegment reports "." or ".." path segments, which a server may resolve
// to a path outside the allowed prefix.
func hasDotSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return true
		}
	}
	return false
}

func vapidKeys(ctx context.Context, st *store.Store) (string, string, error) {
	if pub, ok, err := st.AppSetting(ctx, "vapid_public"); err != nil {
		return "", "", err
	} else if ok {
		priv, _, err := st.AppSetting(ctx, "vapid_private")
		return pub, priv, err
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return "", "", err
	}
	// Two inserts that each keep an existing value: concurrent first starts agree.
	if priv, err = st.SetAppSettingOnce(ctx, "vapid_private", priv); err != nil {
		return "", "", err
	}
	if pub, err = st.SetAppSettingOnce(ctx, "vapid_public", pub); err != nil {
		return "", "", err
	}
	return pub, priv, nil
}

func (n *Notifier) PublicKey() string { return n.vapidPublic }

func (n *Notifier) WebhooksEnabled() bool { return len(n.webhookPrefix) > 0 }

var ErrEndpointNotAllowed = errors.New("push endpoint is not a known push service")

// CheckEndpoint validates a browser-provided push endpoint before we store it.
func (n *Notifier) CheckEndpoint(endpoint string) error {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ErrEndpointNotAllowed
	}
	host := strings.ToLower(u.Hostname())
	for _, h := range n.pushHosts {
		h = strings.ToLower(h)
		if strings.HasPrefix(h, ".") && strings.HasSuffix(host, h) || host == h {
			return nil
		}
	}
	return ErrEndpointNotAllowed
}

var ErrWebhookNotAllowed = errors.New("webhook URL is not allowed by the administrator")

func (n *Notifier) CheckWebhook(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.Opaque != "" ||
		hasDotSegment(u.Path) {
		return ErrWebhookNotAllowed
	}
	path := u.EscapedPath()
	if path == "" {
		path = "/"
	}
	host := strings.ToLower(u.Host)
	for _, p := range n.webhookPrefix {
		if u.Scheme == p.scheme && host == p.host && (path+"/" == p.path || strings.HasPrefix(path, p.path)) {
			return nil
		}
	}
	return ErrWebhookNotAllowed
}

// rateWindow is the window of the per-user limit (rateMax alerts in it).
const rateWindow = time.Minute

// Send delivers a note to every channel the user set up, and says whether it
// is on its way: bursts with the same tag inside minGap are collapsed, and a
// user gets at most rateMax alerts a minute, so chatty programs cannot flood
// a phone. A note that replaces an earlier one passes both; a distinct one
// passes the first.
func (n *Notifier) Send(ctx context.Context, userID int64, note Note) bool {
	key := fmt.Sprintf("%d|%s", userID, note.Tag)
	n.mu.Lock()
	now := time.Now()
	if !note.Replace {
		if last, ok := n.recent[key]; ok && now.Sub(last) < n.minGap && !note.Distinct {
			n.mu.Unlock()
			return false
		}
		recent := n.sent[userID][:0]
		for _, t := range n.sent[userID] {
			if now.Sub(t) < rateWindow {
				recent = append(recent, t)
			}
		}
		if len(recent) >= n.rateMax {
			n.sent[userID] = recent
			n.mu.Unlock()
			n.log.Info("push: rate limited", "user", userID, "tag", note.Tag)
			return false
		}
		n.sent[userID] = append(recent, now)
		n.recent[key] = now
	}
	if len(n.recent) > 5000 {
		for k, t := range n.recent {
			if now.Sub(t) > time.Hour {
				delete(n.recent, k)
			}
		}
		for u, ts := range n.sent {
			if len(ts) == 0 || now.Sub(ts[len(ts)-1]) > rateWindow {
				delete(n.sent, u)
			}
		}
	}
	gen := n.revoked[userID]
	// (Under the lock: the alerts get increasing numbers, in microseconds,
	// which a browser's numbers hold exactly.)
	if t := now.UnixMicro(); t > n.lastSeq {
		n.lastSeq = t
	} else {
		n.lastSeq++
	}
	note.Seq = n.lastSeq
	item := queued{ctx: context.WithoutCancel(ctx), gen: gen, note: note}
	pushIdle := n.pushes.add(userID, item)
	hookIdle := !note.Replace && n.hooks.add(userID, item) // (a replacement is for devices only)
	n.mu.Unlock()
	if pushIdle {
		go n.drain(&n.pushes, userID, func(q queued) { n.deliver(q.ctx, userID, q.gen, q.note) })
	}
	if hookIdle {
		go n.drain(&n.hooks, userID, func(q queued) {
			if !n.stale(userID, q.gen) {
				n.sendWebhook(q.ctx, userID, q.gen, q.note)
			}
		})
	}
	return true
}

// lines are the notes waiting per user, oldest first (guarded by n.mu); a
// user with a line has a drain running.
type lines map[int64][]queued

// add puts a note in the user's line and says whether no drain runs for it.
func (l *lines) add(userID int64, q queued) bool {
	if *l == nil {
		*l = lines{}
	}
	line, busy := (*l)[userID]
	if len(line) >= queueMax {
		line = line[1:] // (this slow: the oldest note waiting gives way)
	}
	(*l)[userID] = append(line, q)
	return !busy
}

// queueMax is how many of a user's notes may wait to be delivered.
const queueMax = 30

type queued struct {
	ctx  context.Context
	gen  uint64
	note Note
}

// drain sends a user's notes of a line in order until none is left.
func (n *Notifier) drain(l *lines, userID int64, send func(queued)) {
	for {
		n.mu.Lock()
		line := (*l)[userID]
		if len(line) == 0 {
			delete(*l, userID)
			n.mu.Unlock()
			return
		}
		(*l)[userID] = line[1:]
		n.mu.Unlock()
		send(line[0])
	}
}

// Revoke drops notes for a user that are queued but not yet sent and cancels
// pushes in progress, for when their access ends (disabled, signed out
// everywhere).
func (n *Notifier) Revoke(userID int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.revoked[userID]++
	n.cancelSending(func(sd *sending) bool { return sd.userID == userID })
}

// RevokeLogin cancels pushes in progress to the devices of one browser login
// (signed out). Their subscriptions go with the login row.
func (n *Notifier) RevokeLogin(userID int64, loginHash string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cancelSending(func(sd *sending) bool { return sd.userID == userID && sd.loginHash != "" && sd.loginHash == loginHash })
}

// RevokeOtherLogins is RevokeLogin for every login of the user but keep
// (after a password change).
func (n *Notifier) RevokeOtherLogins(userID int64, keep string) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cancelSending(func(sd *sending) bool { return sd.userID == userID && sd.loginHash != "" && sd.loginHash != keep })
}

// Rebound cancels deliveries to earlier registrations of an endpoint that was
// just registered again (perhaps by another user or login): they must not
// reach the device with the previous owner's notes. Versions only grow, so
// only strictly older ones are cancelled: a late call for an older
// registration leaves newer deliveries alone.
func (n *Notifier) Rebound(endpoint string, version int64) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.cancelSending(func(sd *sending) bool { return sd.endpoint == endpoint && sd.version < version })
}

// cancelSending is called with mu held.
func (n *Notifier) cancelSending(match func(*sending) bool) {
	for sd := range n.sending {
		if match(sd) {
			sd.cancel()
		}
	}
}

// begin registers a delivery so revocations cancel it; false when the
// user's access ended since the note was queued. Callers re-check what they
// rely on after this, so a revocation is either seen or cancels them.
func (n *Notifier) begin(ctx context.Context, sd *sending, gen uint64) (context.Context, func(), bool) {
	ctx, cancel := context.WithCancel(ctx)
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.revoked[sd.userID] != gen {
		cancel()
		return nil, nil, false
	}
	sd.cancel = cancel
	n.sending[sd] = struct{}{}
	return ctx, func() {
		n.mu.Lock()
		delete(n.sending, sd)
		n.mu.Unlock()
		cancel()
	}, true
}

func (n *Notifier) stale(userID int64, gen uint64) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.revoked[userID] != gen
}

func (n *Notifier) deliver(ctx context.Context, userID int64, gen uint64, note Note) {
	payload, _ := json.Marshal(note)
	subs, err := n.store.PushSubscriptions(ctx, userID)
	if err != nil {
		n.log.Error("push: list subscriptions", "err", err)
	}
	// The devices at once: a note takes as long as its slowest device, not
	// their sum. (The webhook goes in its own line, see Send.)
	var wg sync.WaitGroup
	for _, s := range subs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			n.pushOne(ctx, userID, gen, s, payload, note)
		}()
	}
	wg.Wait()
}

// pushOne delivers to one registration of a device; false when the user's
// access ended (the rest of the note is dropped).
func (n *Notifier) pushOne(ctx context.Context, userID int64, gen uint64, s *store.PushSubscription, payload []byte, note Note) bool {
	sctx, end, ok := n.begin(ctx, &sending{userID: userID, loginHash: s.LoginHash, endpoint: s.Endpoint, version: s.Version}, gen)
	if !ok {
		return false
	}
	defer end()
	// Checked after registering, so a sign-out or the endpoint being
	// registered again from here on cancels the send.
	if cur, err := n.store.PushSubscriptionCurrent(sctx, s.ID, s.Version); err != nil || !cur {
		return true // registered again (maybe by someone else), or cannot tell: not this note's device now
	}
	// A device whose login ended (expired, signed out, password changed) gets
	// nothing more, even before the login row is cleaned up. Only a login
	// known to be over removes it; one that could not be checked is skipped.
	valid, err := false, error(nil)
	if s.LoginHash != "" && n.loginValid != nil {
		valid, err = n.loginValid(sctx, s.LoginHash)
	}
	if err != nil {
		return true
	}
	if !valid || n.CheckEndpoint(s.Endpoint) != nil {
		_ = n.store.DeletePushSubscriptionHard(ctx, s.ID, s.Version)
		return true
	}
	res, err := n.push(sctx, s, payload, note)
	if err != nil {
		if sctx.Err() == nil {
			n.log.Warn("push: send failed", "user", userID, "err", err)
			_ = n.store.MarkPushResult(ctx, s.ID, s.Version, false)
		}
		return true
	}
	res.Body.Close()
	switch {
	case res.StatusCode == http.StatusGone || res.StatusCode == http.StatusNotFound:
		_ = n.store.DeletePushSubscriptionHard(ctx, s.ID, s.Version)
	case res.StatusCode >= 200 && res.StatusCode < 300:
		_ = n.store.MarkPushResult(ctx, s.ID, s.Version, true)
	default:
		n.log.Warn("push: rejected", "user", userID, "status", res.StatusCode)
		_ = n.store.MarkPushResult(ctx, s.ID, s.Version, false)
	}
	return true
}

// push sends one encrypted message to one browser.
func (n *Notifier) push(ctx context.Context, s *store.PushSubscription, payload []byte, note Note) (*http.Response, error) {
	urgency := webpush.UrgencyHigh
	if note.Quiet {
		urgency = webpush.UrgencyNormal
	}
	return webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
		Endpoint: s.Endpoint, Keys: webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
	}, &webpush.Options{
		HTTPClient: n.http, Subscriber: n.subscriber, VAPIDPublicKey: n.vapidPublic, VAPIDPrivateKey: n.vapidPrivate,
		TTL: 3600, Urgency: urgency, Topic: topic(note.Tag),
	})
}

// topic must be URL-safe base64 alphabet and at most 32 characters.
func topic(tag string) string {
	var b strings.Builder
	for _, r := range tag {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		}
		if b.Len() == 32 {
			break
		}
	}
	return b.String()
}

func (n *Notifier) sendWebhook(ctx context.Context, userID int64, gen uint64, note Note) {
	if !n.WebhooksEnabled() {
		return
	}
	// Registered like a push, so disabling the user cancels it; the account
	// is checked after registering.
	ctx, end, ok := n.begin(ctx, &sending{userID: userID}, gen)
	if !ok {
		return
	}
	defer end()
	if u, err := n.store.UserByID(ctx, userID); err != nil || u.Disabled {
		return
	}
	raw, err := n.store.UserSettings(ctx, userID)
	if err != nil {
		return
	}
	var prefs struct {
		Webhook string `json:"webhook"`
	}
	if json.Unmarshal([]byte(raw), &prefs) != nil || prefs.Webhook == "" || n.CheckWebhook(prefs.Webhook) != nil {
		return
	}
	title, text := note.Title, note.Body
	if n.webhookZh && note.TitleZh != "" {
		title, text = note.TitleZh, note.BodyZh
	}
	body, _ := json.Marshal(map[string]string{"title": title, "body": text, "url": note.URL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prefs.Webhook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := n.http.Do(req) // does not follow redirects
	if err != nil {
		n.log.Warn("webhook failed", "user", userID, "err", err)
		return
	}
	res.Body.Close()
}
