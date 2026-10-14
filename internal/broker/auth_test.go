package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestHTTPAuthorizationBeforeMutation(t *testing.T) {
	s := NewServer("")
	s.token = strings.Repeat("a", 64)
	calls := 0
	s.HandleFunc("task.start", func(context.Context, json.RawMessage) (any, error) {
		calls++
		return map[string]bool{"ok": true}, nil
	})
	for _, tc := range []struct {
		name, auth, origin, host, media string
		status                          int
	}{
		{"absent", "", "", "127.0.0.1:1234", "application/json", 401},
		{"wrong", "Bearer wrong", "", "127.0.0.1:1234", "application/json", 401},
		{"browser", "Bearer " + s.token, "https://attacker.example", "127.0.0.1:1234", "application/json", 403},
		{"dns-rebinding", "Bearer " + s.token, "", "attacker.example:1234", "application/json", 403},
		{"plain", "Bearer " + s.token, "", "127.0.0.1:1234", "text/plain", 415},
		{"valid", "Bearer " + s.token, "", "127.0.0.1:1234", "application/json", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := calls
			r := httptest.NewRequest("POST", "http://"+tc.host+"/rpc", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"task.start","params":{}}`))
			r.Header.Set("Authorization", tc.auth)
			r.Header.Set("Origin", tc.origin)
			r.Header.Set("Content-Type", tc.media)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != tc.status {
				t.Fatalf("status %d, want %d", w.Code, tc.status)
			}
			if tc.status != 200 && calls != before {
				t.Fatal("unauthorized mutation")
			}
		})
	}
	if calls != 1 {
		t.Fatalf("handler calls = %d, want 1", calls)
	}
}

func TestDiscoveryCredentialAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broker.addr")
	start := func() (string, func()) {
		s := NewServer("127.0.0.1:0")
		s.SetAddrFile(path)
		s.HandleFunc("broker.ping", func(context.Context, json.RawMessage) (any, error) { return map[string]string{"version": "test"}, nil })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- s.Serve(ctx) }()
		t.Cleanup(cancel)
		return waitAddrFile(t, path), func() { cancel(); <-done }
	}
	first, stop := start()
	e, err := parseEndpoint(first)
	if err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(path); err != nil || (runtime.GOOS != "windows" && fi.Mode().Perm() != 0600) {
		t.Fatalf("credential file permissions: %v %v", fi, err)
	}
	c, err := Dial(first)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	stop()
	second, stop := start()
	defer stop()
	e2, err := parseEndpoint(second)
	if err != nil {
		t.Fatal(err)
	}
	if e.Token == e2.Token {
		t.Fatal("credential reused across restart")
	}
}

func TestDialRejectsUnsafeDiscovery(t *testing.T) {
	for _, raw := range []string{"127.0.0.1:1234", "", `{"version":2,"address":"example.com:80","token":"` + strings.Repeat("a", 64) + `"}`, `{"version":2,"address":"127.0.0.1:1234","token":"short"}`} {
		if _, err := Dial(raw); err != ErrUnauthenticatedEndpoint {
			t.Fatalf("expected safe discovery error, got %v", err)
		}
	}
}

func TestServerRejectsNonLoopbackListener(t *testing.T) {
	s := NewServer("0.0.0.0:0")
	if err := s.Serve(context.Background()); err == nil {
		t.Fatal("non-loopback listener accepted")
	}
}

func TestServerRejectsBrowserFetchMetadata(t *testing.T) {
	s := NewServer("")
	s.token = strings.Repeat("a", 64)
	r := httptest.NewRequest("POST", "http://127.0.0.1:1234/rpc", nil)
	r.Header.Set("Authorization", "Bearer "+s.token)
	r.Header.Set("Sec-Fetch-Site", "same-origin")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status %d", w.Code)
	}
}

func TestClientIgnoresProxyConfiguration(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("ALL_PROXY", "http://127.0.0.1:1")
	b, _ := json.Marshal(endpointRecord{Version: 2, Address: "127.0.0.1:1234", Token: strings.Repeat("a", 64)})
	c, err := Dial(string(b))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	transport, ok := c.http.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatal("credential transport may use a proxy")
	}
}

func TestClientDoesNotForwardCredentialOnRedirect(t *testing.T) {
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { forwarded = true }))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	b, _ := json.Marshal(endpointRecord{Version: 2, Address: strings.TrimPrefix(source.URL, "http://"), Token: strings.Repeat("a", 64)})
	c, err := Dial(string(b))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Ping(context.Background()); err == nil {
		t.Fatal("redirect accepted")
	}
	if forwarded {
		t.Fatal("credential forwarded")
	}
}
