// Package push sends the "session is waiting for you" Web Push. A push carries
// only the session name and status: it goes through Google, Apple or Mozilla
// servers, and folder contents must not leak there.
package push

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/ilguzin/laavu/internal/store"
)

// Sender sends pushes to subscribers from the database.
type Sender struct {
	st         *store.Store
	public     string
	private    string
	subscriber string
	// Client is the HTTP client; replaced in tests.
	Client *http.Client
}

// Message is what the user sees in the notification.
type Message struct {
	Title   string `json:"title"`
	Body    string `json:"body"`
	Agent   string `json:"agent"`
	Session string `json:"session"`
	Tag     string `json:"tag"`
}

// New loads or creates the VAPID keys.
func New(st *store.Store, publicURL string) (*Sender, error) {
	pub, err1 := st.Get("vapid_public")
	priv, err2 := st.Get("vapid_private")
	if err1 != nil || err2 != nil {
		var err error
		priv, pub, err = webpush.GenerateVAPIDKeys()
		if err != nil {
			return nil, err
		}
		if err := st.Set("vapid_public", pub); err != nil {
			return nil, err
		}
		if err := st.Set("vapid_private", priv); err != nil {
			return nil, err
		}
	}
	return &Sender{st: st, public: pub, private: priv, subscriber: publicURL, Client: &http.Client{Timeout: 15 * time.Second}}, nil
}

// PublicKey is the key for pushManager.subscribe in the browser.
func (s *Sender) PublicKey() string { return s.public }

// Subscription is a PushSubscription from the browser.
type Subscription = webpush.Subscription

// pushHosts are the browser push services. Other addresses are rejected:
// otherwise a subscription would turn laavud into a POST sender to anywhere.
var pushHosts = []string{
	"fcm.googleapis.com", "android.googleapis.com", // Chrome, Edge on Android
	"updates.push.services.mozilla.com", // Firefox
	"web.push.apple.com",                // Safari
	".notify.windows.com",               // Edge on Windows
}

// AllowedEndpoint reports whether the subscription URL points to a known push service.
func AllowedEndpoint(endpoint string) bool {
	u, err := url.Parse(endpoint)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Port() != "" && u.Port() != "443") {
		return false
	}
	h := strings.ToLower(u.Hostname())
	for _, p := range pushHosts {
		if h == p || (strings.HasPrefix(p, ".") && strings.HasSuffix(h, p)) {
			return true
		}
	}
	return false
}

// ParseSubscription validates a subscription from the client.
func ParseSubscription(b []byte) (*Subscription, error) {
	var sub Subscription
	if err := json.Unmarshal(b, &sub); err != nil {
		return nil, err
	}
	if sub.Endpoint == "" || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return nil, errors.New("incomplete subscription")
	}
	return &sub, nil
}

// SendUser sends a message to all of the user's devices. Subscriptions the push
// service rejected (404, 410) are deleted.
func (s *Sender) SendUser(userID int64, m Message) int {
	subs, err := s.st.PushSubs(userID)
	if err != nil {
		return 0
	}
	payload, _ := json.Marshal(m)
	sent := 0
	for _, raw := range subs {
		sub, err := ParseSubscription([]byte(raw))
		if err != nil {
			continue
		}
		resp, err := webpush.SendNotification(payload, sub, &webpush.Options{
			HTTPClient:      s.Client,
			Subscriber:      s.subscriber,
			VAPIDPublicKey:  s.public,
			VAPIDPrivateKey: s.private,
			TTL:             600,
			Urgency:         webpush.UrgencyHigh,
			Topic:           topic(m.Tag),
		})
		if err != nil {
			continue
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			s.st.DeletePushSub(sub.Endpoint)
		case resp.StatusCode < 300:
			sent++
		}
	}
	return sent
}

// topic is the Topic header: at most 32 base64url characters, so a new push for
// the same session replaces the old one.
func topic(tag string) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, 32)
	for _, c := range []byte(tag) {
		if len(out) == 32 {
			break
		}
		out = append(out, alphabet[int(c)%len(alphabet)])
	}
	return string(out)
}
