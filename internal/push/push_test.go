package push

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ilguzin/laavu/internal/store"
)

// A local receiver instead of FCM/APNs: check that the push is sent, encrypted,
// signed with VAPID, and that dead subscriptions are deleted.
func TestSendUser(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u, _ := st.CreateUser("alice", "admin")
	s, err := New(st, "https://localhost:8443")
	if err != nil {
		t.Fatal(err)
	}
	// Keys stay the same when reopened.
	s2, _ := New(st, "https://localhost:8443")
	if s2.PublicKey() != s.PublicKey() {
		t.Fatal("VAPID keys must persist")
	}

	var mu sync.Mutex
	var got []*http.Request
	var bodies [][]byte
	gone := false
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		got = append(got, r)
		bodies = append(bodies, b)
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/gone") {
			gone = true
			w.WriteHeader(http.StatusGone)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	s.Client = srv.Client()

	sub := func(path string) string {
		priv, _ := ecdh.P256().GenerateKey(rand.Reader)
		auth := make([]byte, 16)
		rand.Read(auth)
		b, _ := json.Marshal(map[string]any{
			"endpoint": srv.URL + path,
			"keys": map[string]string{
				"p256dh": base64.RawURLEncoding.EncodeToString(priv.PublicKey().Bytes()),
				"auth":   base64.RawURLEncoding.EncodeToString(auth),
			},
		})
		return string(b)
	}
	st.AddPushSub(u.ID, srv.URL+"/ok", sub("/ok"))
	st.AddPushSub(u.ID, srv.URL+"/gone", sub("/gone"))

	n := s.SendUser(u.ID, Message{Title: "Laavu", Body: "Session Demo-1 is waiting for you", Agent: "demo", Session: "Demo-1", Tag: "demo/Demo-1"})
	if n != 1 || len(got) != 2 || !gone {
		t.Fatalf("sent %d, requests %d", n, len(got))
	}
	for i, r := range got {
		if r.Header.Get("Content-Encoding") != "aes128gcm" || !strings.HasPrefix(r.Header.Get("Authorization"), "vapid ") {
			t.Fatalf("push headers: %v", r.Header)
		}
		// The content is encrypted: no plaintext in the body.
		if strings.Contains(string(bodies[i]), "Demo-1") {
			t.Fatal("push sent as plaintext")
		}
		if len(r.Header.Get("Topic")) > 32 {
			t.Fatal("Topic longer than 32 characters")
		}
	}
	subs, _ := st.PushSubs(u.ID)
	if len(subs) != 1 {
		t.Fatalf("subscription with 410 must be deleted, %d left", len(subs))
	}
}

func TestAllowedEndpoint(t *testing.T) {
	for _, u := range []string{"https://fcm.googleapis.com/fcm/send/x", "https://updates.push.services.mozilla.com/wpush/v2/x", "https://web.push.apple.com/x", "https://db5p.notify.windows.com/w/?token=x"} {
		if !AllowedEndpoint(u) {
			t.Error("must be allowed:", u)
		}
	}
	for _, u := range []string{"https://127.0.0.1/x", "http://fcm.googleapis.com/x", "https://fcm.googleapis.com.evil.com/x", "https://fcm.googleapis.com:8443/x", "https://evil.com/notify.windows.com"} {
		if AllowedEndpoint(u) {
			t.Error("must be rejected:", u)
		}
	}
}

func TestParseSubscription(t *testing.T) {
	if _, err := ParseSubscription([]byte(`{"endpoint":"https://x"}`)); err == nil {
		t.Fatal("subscription without keys")
	}
}
