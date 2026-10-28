package meshcentral

import (
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/zalando/go-keyring"
)

func TestLoginTimeout(t *testing.T) {
	settings = Settings{Insecure: true, Token: "cookie=test"}
	defer func() { settings = Settings{} }()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		// Bound the test even if the client forgets its authentication deadline.
		ws.SetReadDeadline(time.Now().Add(controlAuthTimeout + 5*time.Second))
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"serverinfo"}`))
		for {
			if _, _, err := ws.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	err := StartSocketAs(config.Profile{Name: "test", Server: strings.TrimPrefix(server.URL, "https://")})
	var timeout net.Error
	if !errors.As(err, &timeout) || !timeout.Timeout() {
		t.Fatalf("login returned %v, want an authentication timeout", err)
	}
	if settings.WebSocket != nil {
		t.Error("timed-out login left the control socket open")
	}
}

func TestLoginClearsReadDeadline(t *testing.T) {
	settings = Settings{WebChannel: make(chan struct{}), AuthErrChannel: make(chan error, 1)}
	lostHook := OnConnectionLost
	defer func() {
		if settings.RenewCookieTimer != nil {
			settings.RenewCookieTimer.Stop()
		}
		settings = Settings{}
		OnConnectionLost = lostHook
	}()
	closeServer := make(chan struct{})
	defer close(closeServer)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie","cookie":"a","rcookie":"r"}`))
		// Keep the authenticated socket idle beyond its original login deadline.
		select {
		case <-time.After(2 * time.Second):
			ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		case <-closeServer:
		}
	}))
	defer server.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ws.Close()
	ws.SetReadDeadline(time.Now().Add(time.Second))
	var lost error
	OnConnectionLost = func(err error) { lost = err }
	onServerWebSocket(ws, func() (*websocket.Conn, error) {
		t.Error("authenticated socket reached its login deadline")
		settings.closing.Store(true)
		return nil, errors.New("test stopped reconnecting")
	})
	if !settings.initialAuthDone || len(settings.AuthErrChannel) != 0 {
		t.Fatal("login did not complete")
	}
	if !websocket.IsCloseError(errors.Unwrap(lost), websocket.CloseNormalClosure) {
		t.Fatalf("connection ended with %v, want the server's normal close", lost)
	}
}

func TestControlSocketCloseNotification(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		code                   int // zero closes TCP without a WebSocket close frame
		authenticated, stopped bool
	}{
		{"normal close", websocket.CloseNormalClosure, true, false},
		{"server going away", websocket.CloseGoingAway, true, false},
		{"close without status", websocket.CloseNoStatusReceived, true, false},
		{"close during login", websocket.CloseNormalClosure, false, false},
		{"drop during login", 0, false, false},
		{"drop after login", 0, true, false},
		{"client shutdown", websocket.CloseNormalClosure, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer ws.Close()
				if tc.code != 0 {
					ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(tc.code, ""))
				}
			}))
			defer server.Close()
			ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ws.Close()

			lostHook, authErrors := OnConnectionLost, settings.AuthErrChannel
			authenticated, closing := settings.initialAuthDone, settings.closing.Load()
			defer func() {
				OnConnectionLost, settings.AuthErrChannel = lostHook, authErrors
				settings.initialAuthDone = authenticated
				settings.closing.Store(closing)
			}()
			var lost []error
			OnConnectionLost = func(err error) { lost = append(lost, err) }
			settings.AuthErrChannel = make(chan error, 1)
			settings.initialAuthDone = tc.authenticated
			settings.closing.Store(tc.stopped)
			retries := 0
			onServerWebSocket(ws, func() (*websocket.Conn, error) {
				retries++
				// Stop after the first retry so this test doesn't wait through backoff.
				settings.closing.Store(true)
				return nil, errors.New("test stopped reconnecting")
			})
			wantLost, wantAuth, wantRetries := 0, 0, 0
			if !tc.stopped {
				if tc.authenticated {
					if tc.code == 0 {
						wantRetries = 1
					} else {
						wantLost = 1
					}
				} else {
					wantAuth = 1
				}
			}
			if len(lost) != wantLost || len(settings.AuthErrChannel) != wantAuth {
				t.Errorf("close notifications: lost=%d login=%d, want %d/%d", len(lost), len(settings.AuthErrChannel), wantLost, wantAuth)
			}
			if retries != wantRetries {
				t.Errorf("reconnect attempts: %d, want %d", retries, wantRetries)
			}
		})
	}
}

func TestSSOLogin(t *testing.T) {
	keyring.MockInit()
	settings = Settings{Insecure: true}
	prompt := SSOPrompt
	defer func() { settings, SSOPrompt = Settings{}, prompt }()

	// Accepts the Router link's cookie, or the token it mints, until revoked.
	var mu sync.Mutex
	tokenValid := true
	var logins []string
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		mu.Lock()
		login := r.URL.Query().Get("auth")
		if login == "" {
			login = r.Header.Get("x-meshauth")
		}
		logins = append(logins, login)
		ok := login == "c$1@=" || (tokenValid && login == base64.StdEncoding.EncodeToString([]byte("~t:u"))+","+base64.StdEncoding.EncodeToString([]byte("tp")))
		mu.Unlock()
		if !ok {
			ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"close","cause":"noauth","msg":"noauth-2b"}`))
			return
		}
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie","cookie":"renewed","rcookie":"r"}`))
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if strings.Contains(string(msg), `"createLoginToken"`) && strings.Contains(string(msg), `"expire":43200`) {
				ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"createLoginToken","tokenUser":"~t:u","tokenPass":"tp"}`))
			}
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	prompts := 0
	SSOPrompt = func(s string) (string, bool) {
		prompts++
		return "mcrouter://" + strings.ToUpper(s) + "/control.ashx?c=c%241%40%3D&t=hash&l=en", true
	}
	p := config.Profile{Name: "sso", Server: host, SSO: true}
	login := func() {
		t.Helper()
		if err := StartSocketAs(p); err != nil {
			t.Fatal(err)
		}
		StopSocket()
	}

	login()
	if u, pw, _ := p.GetLoginToken(); u != "~t:u" || pw != "tp" || prompts != 1 {
		t.Fatalf("first login stored token %q/%q after %d prompts", u, pw, prompts)
	}
	if settings.AuthCookie != "" || settings.Username != "~t:u" {
		t.Errorf("reconnects would use cookie %q, user %q, want the token", settings.AuthCookie, settings.Username)
	}
	login()
	if prompts != 1 {
		t.Error("login with a stored token asked for the browser")
	}

	mu.Lock()
	tokenValid = false
	mu.Unlock()
	login()
	if prompts != 2 {
		t.Error("revoked token didn't ask for the browser again")
	}
	if len(logins) != 4 {
		t.Errorf("logins %q, want link, token, revoked token, link", logins)
	}

	SSOPrompt = func(string) (string, bool) { return "mcrouter://other.example.com/control.ashx?c=x", true }
	p.DeleteLoginToken()
	if err := StartSocketAs(p); err == nil || !strings.Contains(err.Error(), "other.example.com") {
		t.Errorf("link for another server: %v", err)
	}
}

func TestParseRouterLink(t *testing.T) {
	for _, tc := range []struct{ link, server, cookie string }{
		{"mcrouter://mesh.example.com/control.ashx?c=abc$@=&t=h&l=en", "mesh.example.com", "abc$@="},
		{" mcrouter://mesh.example.com:8443/tenant/control.ashx?c=x&t=h\n", "mesh.example.com:8443/tenant", "x"},
		{"https://mesh.example.com/control.ashx?c=x", "", ""},
		{"mcrouter://mesh.example.com/control.ashx?t=h", "", ""},
	} {
		server, cookie, err := ParseRouterLink(tc.link)
		if server != tc.server || cookie != tc.cookie || (err != nil) != (tc.server == "") {
			t.Errorf("ParseRouterLink(%q) = %q, %q, %v", tc.link, server, cookie, err)
		}
	}
	if !SameServer("Mesh.example.com:443/", "mesh.example.com") || SameServer("mesh.example.com/a", "mesh.example.com") {
		t.Error("SameServer")
	}
}
