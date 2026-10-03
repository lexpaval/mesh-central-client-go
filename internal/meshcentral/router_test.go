package meshcentral

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestIPv6RouteForwarding(t *testing.T) {
	listener, err := net.Listen("tcp6", "[::1]:0")
	if err != nil {
		t.Skipf("IPv6 loopback unavailable: %v", err)
	}
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tcpaddr") != "::1" || r.URL.Query().Get("tcpport") != "22" {
			t.Errorf("incorrect relay target: %s", r.URL.RawQuery)
		}
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		ws.SetReadDeadline(time.Now().Add(5 * time.Second))
		ws.WriteMessage(websocket.TextMessage, []byte("c"))
		for {
			kind, data, err := ws.ReadMessage()
			if err != nil {
				return
			}
			if kind == websocket.BinaryMessage {
				if err := ws.WriteMessage(kind, data); err != nil {
					return
				}
			}
		}
	}))
	server.Listener.Close()
	server.Listener = listener
	server.StartTLS()
	defer server.Close()

	serverURL, insecure := settings.ServerURL, settings.Insecure
	settings.ServerURL = "wss" + strings.TrimPrefix(server.URL, "https") + "/meshrelay.ashx"
	settings.Insecure = true
	defer func() { settings.ServerURL, settings.Insecure = serverURL, insecure }()
	route := &Route{BindAddress: "::1", Target: "::1", RemotePort: 22, Out: io.Discard}
	if err := route.Start(); err != nil {
		t.Fatal(err)
	}
	defer route.Close()
	client, err := net.DialTimeout("tcp6", net.JoinHostPort("::1", strconv.Itoa(route.LocalPort)), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	client.SetDeadline(time.Now().Add(5 * time.Second))
	payload := []byte("IPv6 tunnel round trip")
	if _, err := client.Write(payload); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(client, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("received %q, want %q", got, payload)
	}
	client.Close()
	deadline := time.Now().Add(5 * time.Second)
	for route.Active() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if route.Active() != 0 {
		t.Fatal("route did not release the closed connection")
	}
}

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
