package meshcentral

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral/fakeagent"
)

func TestDesktopClipboard(t *testing.T) {
	var clipboard string
	controlServer(t, func(ws *websocket.Conn) {
		for {
			var m map[string]interface{}
			if ws.ReadJSON(&m) != nil {
				return
			}
			if m["action"] != "msg" {
				t.Errorf("clipboard action: %v", m["action"])
			}
			if m["nodeid"] == "silent" {
				continue
			}
			if m["type"] == "setclip" {
				clipboard, _ = m["data"].(string)
				// The agent does not echo tags on writes.
				ws.WriteJSON(map[string]any{"action": "msg", "nodeid": m["nodeid"], "type": "setclip", "success": m["nodeid"] != "denied"})
			} else {
				ws.WriteJSON(map[string]any{"action": "msg", "nodeid": "wrong", "type": "getclip", "tag": m["tag"], "data": "wrong node"})
				ws.WriteJSON(map[string]any{"action": "msg", "nodeid": m["nodeid"], "type": "getclip", "tag": "old request", "data": "stale"})
				ws.WriteJSON(map[string]any{"action": "msg", "nodeid": m["nodeid"], "type": "getclip", "tag": m["tag"], "data": clipboard})
			}
		}
	}, handleClipboardCommand)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	for _, text := range []string{"héllo \"remote\"\n世界 👋", ""} {
		if err := SetClipboard(ctx, "node", text); err != nil {
			t.Fatal(err)
		}
		got, err := GetClipboard(ctx, "node")
		if err != nil || got != text {
			t.Fatalf("clipboard = %q, %v; want %q", got, err, text)
		}
	}
	if err := SetClipboard(ctx, "denied", "text"); err == nil {
		t.Fatal("denied write succeeded")
	}
	short, stop := context.WithTimeout(ctx, 30*time.Millisecond)
	defer stop()
	if _, err := GetClipboard(short, "silent"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("no reply: %v", err)
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if _, err := GetClipboard(canceled, "node"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	clipboardRequests.Lock()
	defer clipboardRequests.Unlock()
	if len(clipboardRequests.pending) != 0 {
		t.Fatal("clipboard requests leaked")
	}
}

// desktopEvents collects a session's events.
type desktopEvents struct {
	mu       sync.Mutex
	screens  [][2]int
	painted  image.Rectangle
	displays []int
	selected int
	notices  []string
}

func (e *desktopEvents) events() DesktopEvents {
	return DesktopEvents{
		Screen: func(w, h int) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.screens = append(e.screens, [2]int{w, h})
			e.painted = image.Rectangle{}
		},
		Frame: func(r image.Rectangle) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.painted = e.painted.Union(r)
		},
		Displays: func(ids []int, sel int) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.displays, e.selected = ids, sel
		},
		Notice: func(s string) {
			e.mu.Lock()
			defer e.mu.Unlock()
			e.notices = append(e.notices, s)
		},
	}
}

func (e *desktopEvents) wait(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		e.mu.Lock()
		ok := f()
		e.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// inputLog collects the commands a fake agent got.
type inputLog struct {
	mu   sync.Mutex
	cmds [][]byte
}

func (l *inputLog) add(b []byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.cmds = append(l.cmds, append([]byte(nil), b...))
}

func (l *inputLog) has(want []byte) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, c := range l.cmds {
		if bytes.Equal(c, want) {
			return true
		}
	}
	return false
}

func desktopSession(t *testing.T, agent *fakeagent.DesktopAgent) (*Desktop, *desktopEvents) {
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	ev := &desktopEvents{}
	d, err := NewDesktop(ws, ev.events())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(d.Close)
	return d, ev
}

// near reports whether the frame's pixel at (x, y) is about c, JPEG is lossy.
func near(d *Desktop, x, y int, r, g, b uint8) bool {
	ok := false
	d.Frame(func(pix []byte, w, h int) {
		o := (y*w + x) * 4
		diff := func(a, b uint8) int { return max(int(a), int(b)) - min(int(a), int(b)) }
		ok = diff(pix[o+2], r) < 24 && diff(pix[o+1], g) < 24 && diff(pix[o], b) < 24 && pix[o+3] == 0xff
	})
	return ok
}

func TestDesktopScreen(t *testing.T) {
	// Big enough for jumbo tiles, the first one split over two messages.
	screen := fakeagent.DemoScreen(800, 600)
	d, ev := desktopSession(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: screen, 2: fakeagent.DemoScreen(640, 480)}})
	ev.wait(t, "the whole screen", func() bool { return ev.painted == image.Rect(0, 0, 800, 600) })
	if len(ev.screens) != 1 || ev.screens[0] != [2]int{800, 600} {
		t.Fatalf("screens %v", ev.screens)
	}
	for _, p := range []image.Point{{5, 5}, {400, 100}, {799, 599}, {300, 300}} {
		c := screen.RGBAAt(p.X, p.Y)
		if !near(d, p.X, p.Y, c.R, c.G, c.B) {
			t.Errorf("pixel %v differs from %v", p, c)
		}
	}

	ev.wait(t, "the displays", func() bool { return len(ev.displays) == 2 })
	if ev.displays[0] != 1 || ev.displays[1] != 2 || ev.selected != 1 {
		t.Fatalf("displays %v, selected %d", ev.displays, ev.selected)
	}
	d.SetDisplay(2)
	ev.wait(t, "the second display", func() bool { return ev.painted == image.Rect(0, 0, 640, 480) })
	if w, h := d.Size(); w != 640 || h != 480 {
		t.Fatalf("size %dx%d after switching", w, h)
	}
}

func TestDesktopInput(t *testing.T) {
	var log inputLog
	d, ev := desktopSession(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(320, 240)}, Input: log.add})
	ev.wait(t, "the screen", func() bool { return ev.painted == image.Rect(0, 0, 320, 240) })

	// A click round-trips as a red dot.
	d.MouseMove(100, 50)
	d.MouseButton(MouseLeft, true, 100, 50)
	d.MouseButton(MouseLeft, false, 100, 50)
	ev.wait(t, "the dot", func() bool { return near(d, 100, 50, 229, 83, 75) })

	d.MouseButton(MouseRight, false, 1, 2)
	d.MouseWheel(120, 3, 4)
	d.MouseWheel(-120, 3, 4)
	d.Key(0x41, true, false)
	d.Key(0x41, false, false)
	d.Key(0x25, true, true)
	d.Key(0x25, false, true)
	d.Unicode('é', true)
	d.Unicode('😀', false)
	d.CtrlAltDel()
	d.SetQuality(80, 50*time.Millisecond)
	want := [][]byte{
		{0, 2, 0, 10, 0, 0, 0, 100, 0, 50},          // move
		{0, 2, 0, 10, 0, 0x02, 0, 100, 0, 50},       // left down
		{0, 2, 0, 10, 0, 0x04, 0, 100, 0, 50},       // left up
		{0, 2, 0, 10, 0, 0x10, 0, 1, 0, 2},          // right up
		{0, 2, 0, 12, 0, 0, 0, 3, 0, 4, 0x01, 0x68}, // wheel 360
		{0, 2, 0, 12, 0, 0, 0, 3, 0, 4, 0xfe, 0x97}, // wheel -360, as the web UI encodes it
		{0, 1, 0, 6, 0, 0x41},                       // A down
		{0, 1, 0, 6, 1, 0x41},                       // A up
		{0, 1, 0, 6, 4, 0x25},                       // left arrow, extended down
		{0, 1, 0, 6, 3, 0x25},                       // extended up
		{0, 85, 0, 7, 0, 0x00, 0xe9},                // é down
		{0, 85, 0, 7, 1, 0xd8, 0x3d},                // 😀 up, surrogates
		{0, 85, 0, 7, 1, 0xde, 0x00},
		{0, 10, 0, 4},                      // Ctrl+Alt+Del
		{0, 5, 0, 10, 1, 80, 4, 0, 0, 50},  // JPEG 80, 100 %, 50 ms
		{0, 1, 0, 6, 1, 17},                // Ctrl released on start
		{0, 8, 0, 5, 0},                    // unpaused
		{0, 5, 0, 10, 1, 50, 4, 0, 0, 100}, // the default compression
		{0, 11, 0, 4},                      // displays asked for
	}
	for _, w := range want {
		deadline := time.Now().Add(5 * time.Second)
		for !log.has(w) {
			if time.Now().After(deadline) {
				t.Fatalf("the agent didn't get % x", w)
			}
			time.Sleep(5 * time.Millisecond)
		}
	}
}

func TestDesktopConsentAndClose(t *testing.T) {
	d, ev := desktopSession(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48)}, Consent: []string{"Waiting for user to grant access..."}})
	// The agent takes its message back once the user allowed access.
	ev.wait(t, "the notice and its end", func() bool {
		return slices.Equal(ev.notices, []string{"Waiting for user to grant access...", ""})
	})
	ev.wait(t, "the screen", func() bool { return ev.painted == image.Rect(0, 0, 64, 48) })
	d.Close()
	if err := d.Err(); err != nil {
		t.Fatalf("closing gave %v", err)
	}
}

func TestDesktopDropped(t *testing.T) {
	// The agent joins, then its connection breaks without a close.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		c.WriteMessage(websocket.TextMessage, []byte("c"))
		c.ReadMessage()
		c.NetConn().Close()
	}))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	d, err := NewDesktop(ws, DesktopEvents{})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the session didn't end")
	}
	if d.Err() == nil {
		t.Fatal("a dropped tunnel reported no error")
	}
}

// A device's commands can't make the viewer allocate without bound.
func TestDesktopLimits(t *testing.T) {
	d := &Desktop{out: make(chan []byte, 1024), done: make(chan struct{})}
	cmd := func(c int, payload ...byte) []byte { return kvmCmd(c, payload...) }
	jumbo := func(size uint32) []byte {
		return append(cmd(kvmJumbo, byte(size>>24), byte(size>>16), byte(size>>8), byte(size)), cmd(kvmTile, 0, 0, 0, 0)...)
	}
	if err := d.data(jumbo(1 << 30)); err == nil || d.acc != nil {
		t.Error("a 1 GB command was accepted")
	}
	if err := d.data(cmd(kvmScreen, be16(65535)...)); err != nil {
		t.Fatal(err) // too short, ignored
	}
	if err := d.data(cmd(kvmScreen, be16(65535, 65535)...)); err == nil || d.pix != nil {
		t.Error("a 65535 × 65535 screen was allocated")
	}
	if err := d.data(cmd(kvmScreen, be16(64, 48)...)); err != nil {
		t.Fatal(err)
	}

	// An edge tile reaching into the agent's tile padding is drawn, clipped.
	var edge bytes.Buffer
	jpeg.Encode(&edge, image.NewGray(image.Rect(0, 0, 32, 64)), nil)
	var drawn image.Rectangle
	d.ev.Frame = func(r image.Rectangle) { drawn = r }
	if err := d.data(cmd(kvmTile, append(be16(48, 0), edge.Bytes()...)...)); err != nil || drawn != image.Rect(48, 0, 64, 48) {
		t.Errorf("padded edge tile: %v, drew %v", err, drawn)
	}

	// A tile whose header claims far more than the screen isn't decoded.
	var buf bytes.Buffer
	jpeg.Encode(&buf, image.NewGray(image.Rect(0, 0, 8, 8)), nil)
	huge := buf.Bytes()
	sof := bytes.Index(huge, []byte{0xff, 0xc0})
	copy(huge[sof+5:], be16(60000, 60000))
	painted := false
	d.ev.Frame = func(image.Rectangle) { painted = true }
	if err := d.data(cmd(kvmTile, append([]byte{0, 0, 0, 0}, huge...)...)); err != nil || painted {
		t.Errorf("oversized tile: %v, painted %v", err, painted)
	}

	listed := false
	d.ev.Displays = func([]int, int) { listed = true }
	if err := d.data(cmd(kvmGetDisplays, append(be16(100), make([]byte, 2*100+2)...)...)); err != nil || listed {
		t.Errorf("100 displays: %v, listed %v", err, listed)
	}
	if err := d.data(cmd(kvmGetDisplays, append(be16(2), make([]byte, 2*2+2)...)...)); err != nil || !listed {
		t.Errorf("2 displays: %v, listed %v", err, listed)
	}
}

// blit's YCbCr path matches image.YCbCr.At for every subsampling.
func TestBlitYCbCr(t *testing.T) {
	for ratio := image.YCbCrSubsampleRatio444; ratio <= image.YCbCrSubsampleRatio410; ratio++ {
		src := image.NewYCbCr(image.Rect(0, 0, 13, 9), ratio)
		for i := range src.Y {
			src.Y[i] = byte(i * 7)
		}
		for i := range src.Cb {
			src.Cb[i], src.Cr[i] = byte(i*13), byte(255-i*5)
		}
		const w, h = 20, 12
		got := make([]byte, w*h*4)
		blit(got, w, h, src, 9, 5) // clipped on the right and bottom
		want := make([]byte, w*h*4)
		blit(want, w, h, struct{ image.Image }{src}, 9, 5) // the generic path
		if !bytes.Equal(got, want) {
			t.Errorf("subsampling %v differs", ratio)
		}
	}
}
