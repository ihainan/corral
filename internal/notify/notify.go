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
	URL   string `json:"url"`   // path inside Corral to open on tap
	Tag   string `json:"tag"`   // same tag replaces the previous alert on the device
	Quiet bool   `json:"quiet"` // informational: no banner on platforms that support it
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
	webhookPrefix []string
	http          *http.Client
	mu            sync.Mutex
	recent        map[string]time.Time // per user+tag, to collapse bursts
	minGap        time.Duration
}

type Options struct {
	Subscriber      string   // contact for push services, e.g. "https://ssh.example.com"
	ExtraPushHosts  []string // host suffixes to allow besides the major push services
	WebhookPrefixes []string // URL prefixes users may send webhooks to; empty disables webhooks
}

func New(ctx context.Context, st *store.Store, log *slog.Logger, opt Options) (*Notifier, error) {
	pub, priv, err := vapidKeys(ctx, st)
	if err != nil {
		return nil, err
	}
	return &Notifier{
		store: st, log: log, vapidPublic: pub, vapidPrivate: priv, subscriber: opt.Subscriber,
		pushHosts: append(append([]string{}, defaultPushHosts...), opt.ExtraPushHosts...), webhookPrefix: opt.WebhookPrefixes,
		http:   &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{Proxy: http.ProxyFromEnvironment}},
		recent: map[string]time.Time{}, minGap: 20 * time.Second,
	}, nil
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
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil {
		return ErrWebhookNotAllowed
	}
	for _, p := range n.webhookPrefix {
		if p != "" && strings.HasPrefix(raw, p) {
			return nil
		}
	}
	return ErrWebhookNotAllowed
}

// Send delivers a note to every channel the user set up. Bursts with the same
// tag inside minGap are collapsed so a chatty program cannot flood a phone.
func (n *Notifier) Send(ctx context.Context, userID int64, note Note) {
	key := fmt.Sprintf("%d|%s", userID, note.Tag)
	n.mu.Lock()
	now := time.Now()
	if last, ok := n.recent[key]; ok && now.Sub(last) < n.minGap {
		n.mu.Unlock()
		return
	}
	n.recent[key] = now
	if len(n.recent) > 5000 {
		for k, t := range n.recent {
			if now.Sub(t) > time.Hour {
				delete(n.recent, k)
			}
		}
	}
	n.mu.Unlock()

	go n.deliver(context.WithoutCancel(ctx), userID, note)
}

func (n *Notifier) deliver(ctx context.Context, userID int64, note Note) {
	payload, _ := json.Marshal(note)
	subs, err := n.store.PushSubscriptions(ctx, userID)
	if err != nil {
		n.log.Error("push: list subscriptions", "err", err)
	}
	for _, s := range subs {
		if n.CheckEndpoint(s.Endpoint) != nil {
			_ = n.store.DeletePushSubscriptionHard(ctx, s.ID)
			continue
		}
		urgency := webpush.UrgencyHigh
		if note.Quiet {
			urgency = webpush.UrgencyNormal
		}
		res, err := webpush.SendNotificationWithContext(ctx, payload, &webpush.Subscription{
			Endpoint: s.Endpoint, Keys: webpush.Keys{P256dh: s.P256dh, Auth: s.Auth},
		}, &webpush.Options{
			HTTPClient: n.http, Subscriber: n.subscriber, VAPIDPublicKey: n.vapidPublic, VAPIDPrivateKey: n.vapidPrivate,
			TTL: 3600, Urgency: urgency, Topic: topic(note.Tag),
		})
		if err != nil {
			n.log.Warn("push: send failed", "user", userID, "err", err)
			_ = n.store.MarkPushResult(ctx, s.ID, false)
			continue
		}
		res.Body.Close()
		switch {
		case res.StatusCode == http.StatusGone || res.StatusCode == http.StatusNotFound:
			_ = n.store.DeletePushSubscriptionHard(ctx, s.ID)
		case res.StatusCode >= 200 && res.StatusCode < 300:
			_ = n.store.MarkPushResult(ctx, s.ID, true)
		default:
			n.log.Warn("push: rejected", "user", userID, "status", res.StatusCode)
			_ = n.store.MarkPushResult(ctx, s.ID, false)
		}
	}
	n.sendWebhook(ctx, userID, note)
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

func (n *Notifier) sendWebhook(ctx context.Context, userID int64, note Note) {
	if !n.WebhooksEnabled() {
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
	body, _ := json.Marshal(map[string]string{"title": note.Title, "body": note.Body, "url": note.URL})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, prefs.Webhook, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	// Do not follow redirects: an allowed prefix must not bounce us elsewhere.
	client := *n.http
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, err := client.Do(req)
	if err != nil {
		n.log.Warn("webhook failed", "user", userID, "err", err)
		return
	}
	res.Body.Close()
}
