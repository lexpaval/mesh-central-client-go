package main

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/draw"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/gorilla/websocket"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral/fakeagent"
)

// fakeDesktop has desktop sessions open on agent, returning what the agent
// got.
func fakeDesktop(t *testing.T, agent *fakeagent.DesktopAgent) func(want []byte) bool {
	var mu sync.Mutex
	var got [][]byte
	agent.Input = func(b []byte) {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, append([]byte(nil), b...))
	}
	srv := httptest.NewServer(agent)
	t.Cleanup(srv.Close)
	open := openDesktopSession
	openDesktopSession = func(_ string, session int, ev meshcentral.DesktopEvents) (*meshcentral.Desktop, error) {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			return nil, err
		}
		return meshcentral.NewDesktop(ws, session, ev)
	}
	t.Cleanup(func() { openDesktopSession = open })
	return func(want []byte) bool {
		mu.Lock()
		defer mu.Unlock()
		for _, b := range got {
			if bytes.Equal(b, want) {
				return true
			}
		}
		return false
	}
}

// viewPixel is the view's pixel at (x, y) as painted.
func viewPixel(dt *desktopTab, x, y int) (r, g, b int) {
	img := dt.view.Grab().ToImage() // garbage collected, as values miqt returns are
	c := qt.NewQColor4(uint(img.Pixel(x, y)))
	defer c.Delete()
	return c.Red(), c.Green(), c.Blue()
}

func TestDesktopTab(t *testing.T) {
	screen := fakeagent.DemoScreen(400, 300)
	got := fakeDesktop(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: screen, 2: fakeagent.DemoScreen(200, 100)}})
	var dt *desktopTab
	ui(t, func() {
		setConnected(true)
		session.profile = "p"
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		tabs.Window().Resize(1100, 700)
		tabs.Window().Show()
		openDesktop(devices[0])
		dt = desktopTabs[0]
		dt.setViewMode(viewActual) // pixel for pixel, the view is the screen's size
	})
	t.Cleanup(func() { mainthread.Wait(func() { dt.close(); tabs.Window().Hide() }) })
	waitFor(t, "the screen", func() bool { return dt.status.Text() == "400 × 300" && dt.displaySel.Count() == 2 })
	// The window's title bar of the demo screen, painted.
	waitFor(t, "the paint", func() bool {
		r, g, b := viewPixel(dt, 100, 40)
		want := screen.RGBAAt(100, 40)
		return abs(r-int(want.R)) < 24 && abs(g-int(want.G)) < 24 && abs(b-int(want.B)) < 24
	})

	// A click lands on the screen pixel under it and comes back as a dot.
	mainthread.Wait(func() {
		for _, typ := range []qt.QEvent__Type{qt.QEvent__MouseButtonPress, qt.QEvent__MouseButtonRelease} {
			pos := qt.NewQPointF3(150, 120)
			buttons := qt.LeftButton
			if typ == qt.QEvent__MouseButtonRelease {
				buttons = qt.NoButton
			}
			e := qt.NewQMouseEvent(typ, pos, qt.LeftButton, buttons, qt.NoModifier)
			qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
			e.Delete()
			pos.Delete()
		}
	})
	waitFor(t, "the click", func() bool {
		return got([]byte{0, 2, 0, 10, 0, 0x02, 0, 150, 0, 120}) && got([]byte{0, 2, 0, 10, 0, 0x04, 0, 150, 0, 120})
	})
	waitFor(t, "the dot", func() bool {
		r, g, b := viewPixel(dt, 150, 120)
		return r > 200 && g < 120 && b < 120
	})

	// Fitting halves the screen into a 200 px tall view, centered, and
	// enlarges it in a larger one. Stretching fills the view.
	mainthread.Wait(func() {
		at := func(x, y float64) image.Point {
			rx, ry, ok := dt.view.toRemote(qt.NewQPointF3(x, y))
			if !ok {
				return image.Pt(-1, -1)
			}
			return image.Pt(rx, ry)
		}
		dt.setViewMode(viewFit)
		dt.view.SetFixedSize2(600, 150) // what the viewport would give it
		if p := at(300, 75); p != image.Pt(200, 150) {
			t.Errorf("the middle of the view is %v, want (200, 150)", p)
		}
		if p := at(50, 75); p.X >= 0 {
			t.Errorf("the letterbox is on the screen")
		}
		dt.view.SetFixedSize2(1200, 450)
		if p := at(310, 9); p != image.Pt(6, 6) {
			t.Errorf("(310, 9) of the view at 1.5x is %v, want (6, 6)", p)
		}
		dt.setViewMode(viewStretch)
		dt.view.SetFixedSize2(600, 150)
		if p := at(30, 15); p != image.Pt(20, 30) {
			t.Errorf("(30, 15) of the stretched view is %v, want (20, 30)", p)
		}
		dt.setViewMode(viewFit)
		dt.view.SetMinimumSize2(0, 0)
		dt.view.SetMaximumSize2(16777215, 16777215)
	})

	// The device scales its screen down, and back.
	mainthread.Wait(func() { dt.setScaling(512) })
	waitFor(t, "the half size screen", func() bool { return dt.status.Text() == "200 × 150" && got([]byte{0, 5, 0, 10, 1, 50, 2, 0, 0, 100}) })
	mainthread.Wait(func() { dt.setScaling(1024) })
	waitFor(t, "the full size screen", func() bool { return dt.status.Text() == "400 × 300" })

	// Typing: text as characters, keys as virtual keys, released alike.
	mainthread.Wait(func() {
		send := func(typ qt.QEvent__Type, key qt.Key, mods qt.KeyboardModifier, text string) {
			e := qt.NewQKeyEvent3(typ, int(key), mods, text)
			qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
			e.Delete()
		}
		send(qt.QEvent__KeyPress, qt.Key_A, qt.NoModifier, "a")
		send(qt.QEvent__KeyRelease, qt.Key_A, qt.NoModifier, "a")
		send(qt.QEvent__KeyPress, qt.Key_Left, qt.NoModifier, "")
		send(qt.QEvent__KeyRelease, qt.Key_Left, qt.NoModifier, "")
		send(qt.QEvent__KeyPress, qt.Key_Tab, qt.NoModifier, "\t")
		send(qt.QEvent__KeyRelease, qt.Key_Tab, qt.NoModifier, "\t")
	})
	for _, w := range [][]byte{
		{0, 85, 0, 7, 0, 0, 'a'}, {0, 85, 0, 7, 1, 0, 'a'},
		{0, 1, 0, 6, 4, 0x25}, {0, 1, 0, 6, 3, 0x25},
		{0, 1, 0, 6, 0, 0x09}, {0, 1, 0, 6, 1, 0x09},
	} {
		waitFor(t, "the keys", func() bool { return got(w) })
	}

	// View only: a held key is let go, then nothing goes until input is back.
	mainthread.Wait(func() {
		send := func(typ qt.QEvent__Type, key qt.Key, text string) {
			e := qt.NewQKeyEvent3(typ, int(key), qt.NoModifier, text)
			qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
			e.Delete()
		}
		send(qt.QEvent__KeyPress, qt.Key_Left, "")
		dt.inputBtn.SetChecked(false)
		if dt.typeBtn.IsEnabled() || dt.cadBtn.IsEnabled() {
			t.Error("typing and Ctrl+Alt+Del are enabled with input off")
		}
		send(qt.QEvent__KeyPress, qt.Key_B, "b")
		send(qt.QEvent__KeyRelease, qt.Key_B, "b")
		pos := qt.NewQPointF3(60, 50)
		e := qt.NewQMouseEvent(qt.QEvent__MouseButtonPress, pos, qt.LeftButton, qt.LeftButton, qt.NoModifier)
		qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
		e.Delete()
		pos.Delete()
		dt.inputBtn.SetChecked(true)
		send(qt.QEvent__KeyPress, qt.Key_C, "c")
	})
	waitFor(t, "input back", func() bool { return got([]byte{0, 85, 0, 7, 0, 0, 'c'}) })
	if !got([]byte{0, 1, 0, 6, 3, 0x25}) {
		t.Error("the held key was not let go")
	}
	if got([]byte{0, 85, 0, 7, 0, 0, 'b'}) || got([]byte{0, 2, 0, 10, 0, 0x02, 0, 60, 0, 50}) {
		t.Error("input went to the device while off")
	}
	mainthread.Wait(func() {
		e := qt.NewQKeyEvent3(qt.QEvent__KeyRelease, int(qt.Key_C), qt.NoModifier, "c")
		qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
		e.Delete()
	})

	// Another display.
	mainthread.Wait(func() { dt.displaySel.Activated(1) })
	waitFor(t, "display 2", func() bool { return dt.status.Text() == "200 × 100" })

	mainthread.Wait(func() {
		window := dt.w.Window()
		state := window.WindowState()
		var hidden []bool
		for _, w := range desktopChrome {
			hidden = append(hidden, w.IsHidden())
		}
		dt.fullBtn.Click()
		settle()
		if !window.IsFullScreen() || fullscreenDesktop != dt || !dt.scroll.IsVisible() {
			t.Error("desktop did not enter fullscreen")
		}
		for _, w := range desktopChrome {
			if !w.IsHidden() {
				t.Error("client chrome is visible in fullscreen")
			}
		}
		// Esc remains a remote key; Ctrl+Alt+Enter belongs to the viewer.
		for _, typ := range []qt.QEvent__Type{qt.QEvent__KeyPress, qt.QEvent__KeyRelease} {
			e := qt.NewQKeyEvent(typ, int(qt.Key_Escape), qt.NoModifier)
			qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
			e.Delete()
		}
		if !window.IsFullScreen() {
			t.Error("Esc left fullscreen")
		}
		e := qt.NewQKeyEvent(qt.QEvent__KeyPress, int(qt.Key_Return), qt.ControlModifier|qt.AltModifier)
		qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
		e.Delete()
		settle()
		if window.WindowState() != state || fullscreenDesktop != nil {
			t.Error("fullscreen shortcut did not restore the window")
		}
		for i, w := range desktopChrome {
			if w.IsHidden() != hidden[i] {
				t.Error("client chrome was not restored")
			}
		}
		dt.fullBtn.Click()
		tabs.SetCurrentIndex(0)
		if window.IsFullScreen() {
			t.Error("changing tabs did not leave fullscreen")
		}
		tabs.SetCurrentWidget(dt.w)
		dt.fullBtn.Click()
	})

	// The session ending leaves no stale screen.
	center := func() (r, g, b int) { return viewPixel(dt, dt.view.Width()/2, dt.view.Height()/2) }
	waitFor(t, "the screen shown", func() bool { r, g, b := center(); return r+g+b > 0 })
	mainthread.Wait(func() { go dt.sess.Close() })
	waitFor(t, "the end", func() bool { return !dt.live && dt.reconnect.IsVisible() })
	mainthread.Wait(func() {
		if r, g, b := center(); r+g+b != 0 {
			t.Errorf("the ended session's screen shows (%d, %d, %d)", r, g, b)
		}
	})

	mainthread.Wait(func() {
		if len(recent) != 1 || recent[0].Kind != "desktop" || tabs.TabText(tabs.IndexOf(dt.w)) != "dev · Desktop" {
			t.Errorf("recent %+v, tab %q", recent[0], tabs.TabText(tabs.IndexOf(dt.w)))
		}
		dt.close()
		if tabs.Window().IsFullScreen() || fullscreenDesktop != nil {
			t.Error("closing desktop did not restore the client")
		}
	})
	waitFor(t, "the close logged", func() bool {
		return len(logLines) > 0 && strings.HasSuffix(logLines[len(logLines)-1], "dev: desktop closed") && len(desktopTabs) == 0
	})
}

func TestDesktopClipboardSync(t *testing.T) {
	fakeDesktop(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48)}})
	get, set := getDesktopClipboard, setDesktopClipboard
	t.Cleanup(func() { getDesktopClipboard, setDesktopClipboard = get, set })
	requests := make(chan chan string, 8)
	sent := make(chan string, 8)
	var failWrite atomic.Bool
	getDesktopClipboard = func(ctx context.Context, _ string) (string, error) {
		reply := make(chan string, 1)
		requests <- reply
		select {
		case value := <-reply:
			return value, nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	setDesktopClipboard = func(_ context.Context, _, text string) error {
		sent <- text
		if failWrite.Load() {
			return context.DeadlineExceeded
		}
		return nil
	}
	var dt *desktopTab
	ui(t, func() {
		tabs.Window().Show()
		tabs.Window().ActivateWindow()
		openDesktop(meshcentral.Device{Id: "n", Name: "clipboard"})
		dt = desktopTabs[0]
		dt.clipTimer.Stop()
	})
	t.Cleanup(func() { mainthread.Wait(func() { dt.close(); tabs.Window().Hide() }) })
	waitFor(t, "live desktop", func() bool { return dt.live })
	// Off by default, nothing local goes to the device until asked.
	mainthread.Wait(func() {
		qt.QGuiApplication_Clipboard().SetText("secret")
		dt.syncClipboard()
		if dt.clipboardActive() || dt.clipCancel != nil {
			t.Error("clipboard sync started without being enabled")
		}
		dt.clipBtn.SetChecked(true)
	})
	if got := <-sent; got != "secret" {
		t.Fatalf("enabling sync sent %q", got)
	}
	waitFor(t, "active desktop", func() bool { return dt.live && dt.clipboardActive() && dt.clipCancel == nil })
	idle := func() { waitFor(t, "clipboard request completed", func() bool { return dt.clipCancel == nil }) }
	request := func() chan string {
		mainthread.Wait(dt.syncClipboard)
		select {
		case r := <-requests:
			return r
		case <-time.After(3 * time.Second):
			t.Fatal("no clipboard read")
			return nil
		}
	}
	mainthread.Wait(func() { qt.QGuiApplication_Clipboard().SetText("local é\n👋"); dt.syncClipboard() })
	idle()
	if got := <-sent; got != "local é\n👋" {
		t.Fatalf("sent %q", got)
	}
	request() <- "remote 世界"
	idle()
	mainthread.Wait(func() {
		if got := qt.QGuiApplication_Clipboard().Text(); got != "remote 世界" {
			t.Errorf("received %q", got)
		}
	})
	request() <- "remote 世界"
	idle()
	if len(sent) != 0 {
		t.Fatal("remote clipboard echoed back")
	}
	pending := request()
	mainthread.Wait(func() { qt.QGuiApplication_Clipboard().SetText("new local") })
	pending <- "stale remote"
	idle()
	mainthread.Wait(func() {
		if qt.QGuiApplication_Clipboard().Text() != "new local" {
			t.Error("late reply overwrote local copy")
		}
		dt.syncClipboard()
	})
	idle()
	if got := <-sent; got != "new local" {
		t.Errorf("sent %q", got)
	}
	pending = request()
	mainthread.Wait(func() { dt.clipBtn.SetChecked(false) })
	pending <- "disabled reply"
	mainthread.Wait(func() {
		dt.syncClipboard()
		settle()
		if qt.QGuiApplication_Clipboard().Text() != "new local" {
			t.Error("disabled sync changed clipboard")
		}
		if dt.clipCancel != nil {
			t.Error("disabled sync started a request")
		}
		dt.clipBtn.SetChecked(true)
	})
	idle()
	<-sent // Re-enabling shares the current local text.
	pending = request()
	mainthread.Wait(func() { tabs.SetCurrentIndex(0) })
	pending <- "inactive reply"
	mainthread.Wait(func() {
		settle()
		if qt.QGuiApplication_Clipboard().Text() != "new local" {
			t.Error("inactive tab changed clipboard")
		}
		dt.syncClipboard()
		if dt.clipCancel != nil {
			t.Error("inactive tab started a request")
		}
		tabs.SetCurrentWidget(dt.w)
	})

	// A missing write acknowledgement must not starve reads or retry forever.
	failWrite.Store(true)
	mainthread.Wait(func() {
		qt.QGuiApplication_Clipboard().SetText("failed local write")
		dt.syncClipboard()
	})
	idle()
	if got := <-sent; got != "failed local write" {
		t.Errorf("sent %q", got)
	}
	mainthread.Wait(func() {
		if dt.clipSendError == "" || !strings.Contains(dt.status.Text(), "Clipboard send unconfirmed") {
			t.Error("clipboard write failure was not visible")
		}
	})
	request() <- "received despite failed write"
	idle()
	mainthread.Wait(func() {
		if qt.QGuiApplication_Clipboard().Text() != "received despite failed write" {
			t.Error("failed write blocked receiving")
		}
	})
	request() <- "received despite failed write"
	idle()
	if len(sent) != 0 {
		t.Error("unacknowledged write was retried or remote text echoed")
	}

	// Copy remotely, then switch to a local app before the reply arrives.
	pending = request()
	var other *qt.QWidget
	mainthread.Wait(func() {
		other = qt.NewQWidget2()
		other.Show()
		other.ActivateWindow()
	})
	t.Cleanup(func() { mainthread.Wait(func() { other.Close(); other.DeleteLater() }) })
	waitFor(t, "local app active", func() bool { return !dt.w.Window().IsActiveWindow() })
	mainthread.Wait(func() {
		dt.syncClipboard()
		if dt.clipCancel == nil {
			t.Error("switching applications canceled the pending read")
		}
	})
	pending <- "copied before switching apps"
	idle()
	mainthread.Wait(func() {
		if qt.QGuiApplication_Clipboard().Text() != "copied before switching apps" {
			t.Error("remote copy was lost on application switch")
		}
		other.Hide()
		dt.w.Window().ActivateWindow()
	})
	waitFor(t, "desktop reactivated", func() bool { return dt.w.Window().IsActiveWindow() })
	pending = request()
	mainthread.Wait(dt.close)
	pending <- "closed reply"
	mainthread.Wait(func() { settle() })
}

func TestDesktopKeys(t *testing.T) {
	mainthread.Wait(func() {
		for _, c := range []struct {
			key  qt.Key
			mods qt.KeyboardModifier
			text string
			want sentKey
		}{
			{qt.Key_A, qt.ShiftModifier, "A", sentKey{r: 'A'}},
			{qt.Key_A, ctrlModifier, "\x01", sentKey{vk: 0x41}},
			{qt.Key_At, qt.ControlModifier | qt.AltModifier, "@", sentKey{r: '@'}}, // Ctrl+Alt, AltGr to Windows
			{qt.Key_Bar, qt.GroupSwitchModifier, "|", sentKey{r: '|'}},             // AltGr on Windows
			{qt.Key_Exclam, ctrlModifier | qt.ShiftModifier, "!", sentKey{vk: 0x31}},
			{qt.Key_Left, qt.KeypadModifier, "", sentKey{vk: 0x25}},
			{qt.Key_Enter, qt.KeypadModifier, "\r", sentKey{vk: 0x0d, ext: true}},
			{qt.Key_Slash, qt.KeypadModifier | ctrlModifier, "/", sentKey{vk: 0x6f, ext: true}},
			{qt.Key_5, qt.KeypadModifier | ctrlModifier, "5", sentKey{vk: 0x65}},
			{qt.Key_F5, qt.NoModifier, "", sentKey{vk: 0x74}},
			{qt.Key_Space, qt.NoModifier, " ", sentKey{r: ' '}},
			{qt.Key_Escape, qt.NoModifier, "\x1b", sentKey{vk: 0x1b}},
			{qt.Key_Eacute, qt.NoModifier, "é", sentKey{r: 'é'}},
		} {
			e := qt.NewQKeyEvent3(qt.QEvent__KeyPress, int(c.key), c.mods, c.text)
			if got, ok := keyFor(e); !ok || got != c.want {
				t.Errorf("key %x mods %x %q: %+v %v, want %+v", c.key, c.mods, c.text, got, ok, c.want)
			}
			e.Delete()
		}
	})
}

// Once the local user allowed access the agent takes its message back, the
// status then shows the screen alone and the log says so.
func TestDesktopConsentClears(t *testing.T) {
	fakeDesktop(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48)},
		Consent: []string{"Waiting for user to grant access..."}})
	var dt *desktopTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openDesktop(devices[0])
		dt = desktopTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { dt.close() }) })
	waitFor(t, "the screen", func() bool { return dt.status.Text() == "64 × 48" })
	settle()
	mainthread.Wait(func() {
		var got []string
		for _, l := range logLines {
			if strings.Contains(l, "dev: desktop: ") {
				got = append(got, l[strings.Index(l, "dev: "):])
			}
		}
		if want := []string{"dev: desktop: Waiting for user to grant access...", "dev: desktop: access allowed"}; !slices.Equal(got, want) {
			t.Errorf("logged %q", got)
		}
	})
}

func TestDesktopWindow(t *testing.T) {
	fakeDesktop(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48)}})
	var dt *desktopTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openDesktop(devices[0])
		dt = desktopTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { dt.close() }) })
	waitFor(t, "the screen", func() bool { return dt.status.Text() == "64 × 48" })
	mainthread.Wait(func() {
		tabCount := tabs.Count()
		dt.windowBtn.Click()
		if !dt.w.IsWindow() || !dt.w.IsVisible() || tabs.IndexOf(dt.w) != -1 || tabs.Count() != tabCount-1 {
			t.Error("the desktop did not move to its own window")
		}
		if dt.w.WindowTitle() != "dev · Desktop" {
			t.Errorf("window %q", dt.w.WindowTitle())
		}
		dt.fullBtn.Click()
		settle()
		if !dt.w.IsFullScreen() || fullscreenDesktop != nil {
			t.Error("the window did not go fullscreen on its own")
		}
		dt.windowBtn.Click()
		if dt.w.IsWindow() || dt.fullBtn.IsChecked() || tabs.CurrentWidget().UnsafePointer() != dt.w.UnsafePointer() {
			t.Error("the desktop did not move back to a tab")
		}
		dt.windowBtn.Click()
		dt.w.Close()
		if !dt.closed || len(desktopTabs) != 0 || tabs.Count() != tabCount-1 {
			t.Error("closing the window did not close the desktop")
		}
	})
}

func TestDesktopDisplayWindows(t *testing.T) {
	// Display 2 is left of display 1, so its window comes first.
	second := image.NewRGBA(image.Rect(0, 0, 32, 24))
	draw.Draw(second, second.Bounds(), image.NewUniform(color.RGBA{0, 200, 0, 255}), image.Point{}, draw.Src)
	got := fakeDesktop(t, &fakeagent.DesktopAgent{
		Screens:   map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48), 2: second},
		Positions: map[int]image.Point{1: {0, 0}, 2: {-32, 8}},
	})
	var dt *desktopTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openDesktop(devices[0])
		dt = desktopTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { dt.close() }) })
	waitFor(t, "the layout", func() bool { return dt.splitBtn.IsVisibleTo(dt.w) })
	mainthread.Wait(func() {
		dt.splitBtn.Click()
		if len(dt.screens) != 2 || dt.screens[0].display != 2 || !dt.screens[0].IsVisible() || dt.displaySel.IsEnabled() {
			t.Fatal("no window per display")
		}
		if title := dt.screens[0].WindowTitle(); title != "dev · Display 2" {
			t.Errorf("window %q", title)
		}
		// The tab shows where the displays are, its own view's controls off.
		if !dt.view.overview() || dt.view.inputOn() || dt.fullBtn.IsEnabled() || !dt.screens[0].inputOn() {
			t.Error("the tab did not become the displays' overview")
		}
	})
	waitFor(t, "the screen of all displays", func() bool { return dt.status.Text() == "96 × 48" })
	v := dt.screens[0]
	waitFor(t, "display 2 in its window", func() bool {
		img := v.Grab().ToImage()
		c := qt.NewQColor4(uint(img.Pixel(v.Width()/2, v.Height()/2)))
		defer c.Delete()
		return c.Red() < 60 && c.Green() > 160 && c.Blue() < 60
	})
	mainthread.Wait(func() {
		// The window's middle is display 2's, at (16, 20) on the screen of all.
		x, y, ok := v.toRemote(qt.NewQPointF3(float64(v.Width())/2, float64(v.Height())/2))
		if !ok || x != 16 || y != 20 {
			t.Errorf("the middle of display 2 is (%d, %d) %v, want (16, 20)", x, y, ok)
		}
		pos := qt.NewQPointF3(float64(v.Width())/2, float64(v.Height())/2)
		for _, typ := range []qt.QEvent__Type{qt.QEvent__MouseButtonPress, qt.QEvent__MouseButtonRelease} {
			e := qt.NewQMouseEvent(typ, pos, qt.LeftButton, qt.NoButton, qt.NoModifier)
			qt.QCoreApplication_SendEvent(v.QObject, e.QEvent)
		}
	})
	waitFor(t, "the click", func() bool { return got([]byte{0, 2, 0, 10, 0, 0x02, 0, 16, 0, 20}) })

	// Scaled down, the displays are cut from the smaller screen.
	mainthread.Wait(func() { dt.setScaling(512) })
	t.Cleanup(func() { mainthread.Wait(func() { dt.setScaling(1024) }) })
	waitFor(t, "the half size screen", func() bool { return dt.status.Text() == "48 × 24" })
	waitFor(t, "display 2 scaled in its window", func() bool {
		x, y, ok := v.toRemote(qt.NewQPointF3(float64(v.Width())/2, float64(v.Height())/2))
		img := v.Grab().ToImage() // off the click's dot in the middle
		c := qt.NewQColor4(uint(img.Pixel(v.Width()/4, v.Height()/4)))
		defer c.Delete()
		return ok && x == 8 && y == 10 && c.Red() < 60 && c.Green() > 160 && c.Blue() < 60
	})
	mainthread.Wait(func() {
		v.Close()
		if len(dt.screens) != 1 || !dt.splitBtn.IsChecked() {
			t.Error("closing one display window closed the others")
		}
		// Clicking its place in the overview opens it again.
		dt.view.SetFixedSize2(400, 300)
		boxes := dt.view.boxes()
		if len(boxes) != 2 || boxes[0].id != 2 || boxes[0].r.Max.X > boxes[1].r.Min.X {
			t.Fatalf("overview %v", boxes)
		}
		c := boxes[0].r.Min.Add(boxes[0].r.Max).Div(2)
		e := qt.NewQMouseEvent(qt.QEvent__MouseButtonPress, qt.NewQPointF3(float64(c.X), float64(c.Y)), qt.LeftButton, qt.LeftButton, qt.NoModifier)
		qt.QCoreApplication_SendEvent(dt.view.QObject, e.QEvent)
		if len(dt.screens) != 2 || dt.screens[1].display != 2 || !dt.screens[1].IsVisible() {
			t.Error("clicking display 2 did not reopen its window")
		}
		dt.screens[1].Close()
		dt.screens[0].Close()
		if dt.screens != nil || dt.splitBtn.IsChecked() || !dt.displaySel.IsEnabled() || dt.view.overview() {
			t.Error("closing the last display window did not end the split")
		}
	})
}

// Windows devices list their sessions: one of several is picked, a single
// one is shown, and without a list the agent's default is.
func TestDesktopSessions(t *testing.T) {
	got := fakeDesktop(t, &fakeagent.DesktopAgent{Screens: map[int]*image.RGBA{1: fakeagent.DemoScreen(64, 48)}})
	sessions := getUserSessions
	t.Cleanup(func() { getUserSessions = sessions })
	var list []meshcentral.UserSession
	var listErr error
	getUserSessions = func(context.Context, string) ([]meshcentral.UserSession, error) { return list, listErr }
	open := func() *desktopTab {
		var dt *desktopTab
		ui(t, func() {
			setConnected(true)
			setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", OS: "Microsoft Windows 11 Pro", Pwr: 1}})
			openDesktop(devices[0])
			dt = desktopTabs[0]
		})
		t.Cleanup(func() { mainthread.Wait(func() { dt.close() }) })
		return dt
	}

	list = []meshcentral.UserSession{{ID: 1, State: "Connected", Station: "Console"}, {ID: 3, State: "Active", Station: "RDP-Tcp#0", User: `LAB\alice`}}
	dt := open()
	var buttons []*qt.QPushButton
	waitFor(t, "the session picker", func() bool {
		if dt.sessions == nil {
			return false
		}
		buttons = nil
		for _, o := range dt.sessions.Children() {
			if o.Inherits("QPushButton") {
				buttons = append(buttons, qt.UnsafeNewQPushButton(o.UnsafePointer()))
			}
		}
		return len(buttons) == 2
	})
	mainthread.Wait(func() {
		if text := buttons[1].Text(); text != `Active, RDP-Tcp#0 · LAB\alice` {
			t.Errorf("button %q", text)
		}
		if dt.live || dt.scroll.IsVisible() || dt.status.Text() != "Choose a session" {
			t.Error("the picker isn't shown in place of the screen")
		}
		buttons[1].Click()
	})
	waitFor(t, "session 3", func() bool { return dt.status.Text() == "64 × 48" && dt.sessions == nil })
	if !got([]byte(`{"type":"options","tsid":3}`)) {
		t.Error("the agent wasn't asked for session 3")
	}
	mainthread.Wait(func() { dt.close() })

	list = list[1:]
	dt = open()
	waitFor(t, "the only session", func() bool { return dt.status.Text() == "64 × 48" })
	mainthread.Wait(func() { dt.close() })

	list, listErr = nil, context.DeadlineExceeded
	dt = open()
	waitFor(t, "the default session", func() bool { return dt.status.Text() == "64 × 48" })
}
