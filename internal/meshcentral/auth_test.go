package meshcentral

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

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
			authenticated, closing := settings.initialAuthDone, settings.closing
			defer func() {
				OnConnectionLost, settings.AuthErrChannel = lostHook, authErrors
				settings.initialAuthDone, settings.closing = authenticated, closing
			}()
			var lost []error
			OnConnectionLost = func(err error) { lost = append(lost, err) }
			settings.AuthErrChannel = make(chan error, 1)
			settings.initialAuthDone, settings.closing = tc.authenticated, tc.stopped
			retries := 0
			onServerWebSocket(ws, func() (*websocket.Conn, error) {
				retries++
				// Stop after the first retry so this test doesn't wait through backoff.
				settings.closing = true
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
