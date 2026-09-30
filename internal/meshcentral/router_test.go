package meshcentral

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

func TestShellResizeSendsTermsize(t *testing.T) {
	msgs := make(chan string, 16)
	up := websocket.Upgrader{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		c.WriteMessage(websocket.TextMessage, []byte("c"))
		for {
			mt, m, err := c.ReadMessage()
			if err != nil {
				return
			}
			if mt == websocket.TextMessage && !strings.Contains(string(m), `"rtt"`) {
				msgs <- string(m)
			}
		}
	}))
	defer srv.Close()
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	cols, rows := 80, 24
	size := func() (int, int) { mu.Lock(); defer mu.Unlock(); return cols, rows }
	resize, input := make(chan struct{}, 1), make(chan []byte)
	go runShellSession(ws, 1, input, io.Discard, size, resize, nil)
	next := func() string {
		select {
		case m := <-msgs:
			return m
		case <-time.After(5 * time.Second):
			t.Fatal("relay got nothing")
			return ""
		}
	}
	if m := next(); !strings.Contains(m, `"type":"options"`) || !strings.Contains(m, `"cols":80`) {
		t.Fatalf("first message %s", m)
	}
	for next() != "1" { // the protocol, after it a termsize of the same size
	}
	next()

	mu.Lock()
	cols, rows = 200, 50
	mu.Unlock()
	resize <- struct{}{}
	if m := next(); m != `{"ctrlChannel":"102938","type":"termsize","cols":200,"rows":50}` {
		t.Fatalf("resize sent %s", m)
	}
	close(input)
}
