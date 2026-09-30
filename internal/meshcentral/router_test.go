package meshcentral

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

// relay plays the relay's side of a tunnel: the join message, one chunk of
// device data, then a normal close.
func relay(t *testing.T, join string) *websocket.Conn {
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte(join))
		c.WriteMessage(websocket.BinaryMessage, []byte("data"))
		c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
		c.ReadMessage() // until the client closes
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestPumpReportsRecording(t *testing.T) {
	for join, want := range map[string]bool{"c": false, "cr": true} {
		src, srcW := io.Pipe()
		defer srcW.Close()
		var dst bytes.Buffer
		recorded := false
		if err := pumpBidirectional(relay(t, join), src, &dst, io.Discard, func() { recorded = true }); err != nil {
			t.Fatalf("%q: %v", join, err)
		}
		if recorded != want || dst.String() != "data" {
			t.Errorf("%q: recorded=%v, got %q", join, recorded, dst.String())
		}
	}
}

func TestShellReportsRecording(t *testing.T) {
	for join, want := range map[string]bool{"c": false, "cr": true} {
		var out bytes.Buffer
		recorded := false
		size := func() (int, int) { return 80, 24 }
		if _, err := runShellSession(relay(t, join), 1, make(chan []byte), &out, size, nil, func() { recorded = true }); err != nil {
			t.Fatalf("%q: %v", join, err)
		}
		if recorded != want || out.String() != "data" {
			t.Errorf("%q: recorded=%v, got %q", join, recorded, out.String())
		}
	}
}
