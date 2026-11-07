package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"maps"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// The Desktop tab shows a device's screen over the agent's KVM channel and
// passes the mouse and keyboard on. Text typed goes as characters, so the
// local keyboard layout applies, other keys and shortcuts as Windows
// virtual-key codes by their US position.

// openDesktopSession opens the session, tests replace it and
// getUserSessions.
var openDesktopSession = meshcentral.OpenDesktop
var getUserSessions = meshcentral.UserSessions

// desktopSessionsTimeout bounds the wait for a device's sessions, servers
// and agents that don't list them get the default session after it.
const desktopSessionsTimeout = 5 * time.Second

var getDesktopClipboard = meshcentral.GetClipboard
var setDesktopClipboard = meshcentral.SetClipboard

// desktopQualities are the quality choices: JPEG quality and frame interval.
var desktopQualities = []struct {
	name    string
	quality int
	frame   time.Duration
}{
	{"High quality", 80, 50 * time.Millisecond},
	{"Balanced", 50, 100 * time.Millisecond},
	{"Low bandwidth", 25, 200 * time.Millisecond},
}

// desktopViews are the ways the screen fits the view, as the web UI's view
// modes: Fit and Stretch fill it, Actual size scrolls.
const (
	viewFit = iota
	viewStretch
	viewActual
)

var desktopViews = []struct{ pref, name string }{
	{"fit", "Fit, keeping the aspect ratio"},
	{"stretch", "Stretch to fill"},
	{"actual", "Actual size, pixel for pixel"},
}

// desktopScalings are the web UI's scalings, the screen scaled down on the
// device to scaling/1024 to send fewer pixels.
var desktopScalings = []int{1024, 896, 768, 640, 512, 384, 256, 128}

type desktopTab struct {
	d     meshcentral.Device
	name  string
	w     *qt.QWidget
	icons []func()

	view       *deskView
	scroll     *qt.QScrollArea
	box        *qt.QVBoxLayout
	sessions   *qt.QWidget // the session picker, while it's up
	status     *qt.QLabel
	displaySel *qt.QComboBox
	qualitySel *qt.QComboBox
	scaleBtn   *qt.QPushButton
	clipBtn    *qt.QPushButton
	inputBtn   *qt.QPushButton
	sysKeysBtn *qt.QPushButton
	fullBtn    *qt.QPushButton
	windowBtn  *qt.QPushButton
	splitBtn   *qt.QPushButton
	typeBtn    *qt.QPushButton
	cadBtn     *qt.QPushButton
	reconnect  *qt.QPushButton

	sess            *meshcentral.Desktop // the latest
	attempt         int                  // sessions opened, results of older ones are dropped
	live            bool                 // sess is open
	notice          string               // the agent's, until it takes it back
	notices         agentNotices
	recorded        bool
	detached        bool // in a window of its own rather than a tab
	displays        []int
	layout          map[int]image.Rectangle // the displays in the screen of all of them
	screens         []*deskView             // the displays' own windows
	cursor          qt.CursorShape
	down            map[uint64]sentKey // keys pressed here, released the way they were sent
	closed          bool
	clipCancel      context.CancelFunc
	clipTimer       *qt.QTimer
	clipEpoch       int
	clipLocal       string
	clipKnown       bool
	clipRemote      string
	clipRemoteKnown bool
	clipSendError   string
	windowState     qt.WindowState
	chromeHidden    []bool
	viewMode        int
	scaling         int // the device's, 1024 is 100 %

	// Frame changes queued by the session's reader for the Qt thread.
	mu      sync.Mutex
	dirty   image.Rectangle
	pending bool
}

// deskView shows the device's screen, or one of its displays in a window of
// its own, and passes the mouse and keyboard on.
type deskView struct {
	*qt.QWidget
	dt      *desktopTab
	display int             // the display in its own window, 0 in the tab
	crop    image.Rectangle // the display in the screen of all of them
	state   qt.WindowState  // before fullscreen
}

// sentKey is how a key press went to the device: a character or a key.
type sentKey struct {
	r   rune
	vk  int
	ext bool
}

var desktopTabs []*desktopTab
var desktopChrome []*qt.QWidget
var fullscreenDesktop *desktopTab

// openDesktop opens a tab with d's screen.
func openDesktop(d meshcentral.Device) {
	dt := &desktopTab{d: d, name: deviceName(d), down: map[uint64]sentKey{}}
	dt.notices = agentNotices{device: dt.name, kind: "desktop"}
	dt.build()
	desktopTabs = append(desktopTabs, dt)
	tabs.SetCurrentIndex(tabs.AddTab2(dt.w, icon("display"), dt.title()))
	dt.view.SetFocus()
	dt.connect()
	logf("%s: desktop opened", dt.name)
	recordRecent(recentAction{Kind: "desktop", NodeID: d.Id, Device: dt.name})
}

func (dt *desktopTab) title() string { return dt.titled("Desktop") }

func (dt *desktopTab) titled(s string) string {
	s = dt.name + " · " + s
	if dt.recorded {
		s += " · recorded"
	}
	return s
}

func desktopAt(i int) *desktopTab {
	w := tabs.Widget(i)
	if w == nil {
		return nil
	}
	for _, dt := range desktopTabs {
		if dt.w.UnsafePointer() == w.UnsafePointer() {
			return dt
		}
	}
	return nil
}

func (dt *desktopTab) close() {
	if dt.closed {
		return
	}
	dt.fullBtn.SetChecked(false)
	dt.splitBtn.SetChecked(false)
	dt.cancelClipboard()
	dt.closed = true
	dt.attempt++
	if dt.sess != nil {
		go dt.sess.Close()
	}
	for _, v := range dt.views() {
		v.dropSysKeys()
	}
	desktopTabs = slices.DeleteFunc(desktopTabs, func(x *desktopTab) bool { return x == dt })
	if !dt.detached {
		tabs.RemoveTab(tabs.IndexOf(dt.w))
	}
	dt.w.DeleteLater()
	logf("%s: desktop closed", dt.name)
}

func (dt *desktopTab) build() {
	dt.w = qt.NewQWidget2()
	button := func(text, iconName, tip string) *qt.QPushButton { return tabButton(&dt.icons, text, iconName, tip) }
	dt.status = newElidedLabel("Connecting…")
	dt.displaySel = qt.NewQComboBox2()
	dt.displaySel.SetToolTip("The display shown")
	dt.displaySel.SetVisible(false)
	dt.displaySel.OnActivated(func(i int) {
		if dt.live && i < len(dt.displays) {
			dt.sess.SetDisplay(dt.displays[i])
		}
		dt.view.SetFocus()
	})
	dt.qualitySel = qt.NewQComboBox2()
	dt.qualitySel.SetToolTip("Image quality and frame rate")
	for _, q := range desktopQualities {
		dt.qualitySel.AddItem(q.name)
	}
	qi, err := strconv.Atoi(pref("desktopQuality"))
	if err != nil || qi < 0 || qi >= len(desktopQualities) {
		qi = 1
	}
	dt.qualitySel.SetCurrentIndex(qi)
	dt.qualitySel.OnActivated(func(i int) {
		setPref("desktopQuality", strconv.Itoa(i))
		if dt.live {
			q := desktopQualities[i]
			dt.sess.SetQuality(q.quality, q.frame)
		}
		dt.view.SetFocus()
	})
	dt.scaleBtn = button("Scaling", "arrows-left-right-to-line", "How the screen fits the window, and the resolution the device sends")
	dt.scaleBtn.OnClicked(dt.scaleMenu)
	for i, v := range desktopViews {
		if v.pref == pref("desktopView") {
			dt.viewMode = i
		}
	}
	dt.scaling = 1024
	if s, err := strconv.Atoi(pref("desktopScaling")); err == nil && slices.Contains(desktopScalings, s) {
		dt.scaling = s
	}
	// Off until asked for: on, the local clipboard goes to the device right
	// away, and whatever the device copies replaces it.
	dt.clipBtn = button("Clipboard sync", "clipboard", clipTip)
	dt.clipBtn.SetCheckable(true)
	dt.clipBtn.OnToggled(func(bool) {
		dt.resetClipboard()
		dt.showSize()
		dt.syncClipboard()
		dt.view.SetFocus()
	})
	dt.clipTimer = qt.NewQTimer2(dt.w.QObject)
	dt.clipTimer.OnTimeout(dt.syncClipboard)
	dt.clipTimer.Start(1000)

	// Off, the screen is only watched: nothing typed or clicked goes to the
	// device, and local shortcuts work again. It starts as Settings say.
	dt.inputBtn = button("Input", "computer-mouse", "Send the mouse and keyboard to the device, off is view only")
	dt.inputBtn.SetCheckable(true)
	dt.inputBtn.SetChecked(desktopInputStart())
	dt.inputBtn.OnToggled(func(on bool) {
		setPref("desktopInputLast", map[bool]string{true: "", false: "off"}[on])
		dt.releaseKeys()
		dt.setLive(dt.live)
		dt.view.SetFocus()
	})

	// On, Alt+Tab, the Super or Windows key and the like go to the device
	// while its screen has the keyboard, instead of to the local system.
	dt.sysKeysBtn = button("System keys", "key", "Send system shortcuts like Alt+Tab and the Super or Windows key to the device while its screen has the keyboard, off they stay with this computer")
	dt.sysKeysBtn.SetCheckable(true)
	dt.sysKeysBtn.SetChecked(pref("desktopSysKeys") == "on")
	dt.sysKeysBtn.SetVisible(sysKeysSupported)
	dt.sysKeysBtn.OnToggled(func(on bool) {
		v := ""
		if on {
			v = "on"
		}
		setPref("desktopSysKeys", v)
		dt.updateSysKeys()
		dt.view.SetFocus()
	})

	dt.typeBtn = button("Type clipboard", "keyboard", "Type the clipboard's text on the device")
	dt.typeBtn.OnClicked(func() {
		if dt.live {
			for _, r := range qt.QGuiApplication_Clipboard().Text() {
				if r == '\n' {
					dt.sess.Key(13, true, false)
					dt.sess.Key(13, false, false)
				} else if r != '\r' {
					dt.sess.Unicode(r, true)
					dt.sess.Unicode(r, false)
				}
			}
		}
		dt.view.SetFocus()
	})
	dt.cadBtn = button("Ctrl+Alt+Del", "shield-halved", "Send Ctrl+Alt+Del, which the local system keeps")
	dt.cadBtn.OnClicked(func() {
		if dt.live {
			dt.sess.CtrlAltDel()
		}
		dt.view.SetFocus()
	})
	dt.reconnect = button("Reconnect", "rotate", "Open the desktop again")
	dt.reconnect.OnClicked(dt.connect)
	dt.fullBtn = button("Fullscreen", "expand", "Desktop-only fullscreen (Ctrl+Alt+Enter)")
	dt.fullBtn.SetCheckable(true)
	dt.fullBtn.OnToggled(dt.fullscreen)
	dt.icons = append(dt.icons, func() {
		if dt.fullBtn.IsChecked() {
			dt.fullBtn.SetIcon(icon("compress"))
		}
	})
	dt.windowBtn = button("Separate window", "window-restore", "Move the desktop to a window of its own")
	dt.windowBtn.SetCheckable(true)
	dt.windowBtn.OnToggled(dt.detach)
	dt.splitBtn = button("Window per display", "table-columns", "Show each of the device's displays in a window of its own, Ctrl+Alt+Enter in one makes it fullscreen")
	dt.splitBtn.SetCheckable(true)
	dt.splitBtn.SetVisible(false)
	dt.splitBtn.OnToggled(dt.split)
	// Closing its window ends the desktop, as closing its tab does.
	dt.w.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		super(e)
		dt.close()
	})
	shortcut := qt.NewQShortcut2(qt.NewQKeySequence2("Ctrl+Alt+Return"), dt.w.QObject)
	shortcut.SetContext(qt.WidgetWithChildrenShortcut)
	shortcut.OnActivated(func() {
		if dt.fullBtn.IsEnabled() {
			dt.fullBtn.Toggle()
		}
	})

	bar := hbox(false)
	bar.SetContentsMargins(6, 4, 0, 4) // the screen goes edge to edge, not the bar
	bar.AddWidget2(dt.status.QWidget, 1)
	for _, b := range []*qt.QPushButton{dt.scaleBtn, dt.clipBtn, dt.inputBtn, dt.sysKeysBtn, dt.typeBtn, dt.cadBtn, dt.fullBtn, dt.windowBtn, dt.splitBtn} {
		b.SetText("")
	}
	for _, w := range []*qt.QWidget{dt.displaySel.QWidget, dt.qualitySel.QWidget, dt.scaleBtn.QWidget, dt.clipBtn.QWidget, dt.inputBtn.QWidget, dt.sysKeysBtn.QWidget, dt.typeBtn.QWidget, dt.cadBtn.QWidget, dt.fullBtn.QWidget, dt.windowBtn.QWidget, dt.splitBtn.QWidget, dt.reconnect.QWidget} {
		bar.AddWidget(w)
	}

	dt.view = dt.newView(0)

	dt.scroll = qt.NewQScrollArea2()
	dt.scroll.SetFrameShape(qt.QFrame__NoFrame)
	dt.scroll.SetAlignment(qt.AlignCenter)
	dt.scroll.SetWidget(dt.view.QWidget)
	dt.scroll.SetBackgroundRole(qt.QPalette__Dark)

	dt.box = qt.NewQVBoxLayout(dt.w)
	dt.box.SetContentsMargins(0, 0, 0, 0)
	dt.box.AddLayout(bar.QLayout)
	dt.box.AddWidget2(dt.scroll.QWidget, 1)
	dt.relayout()
	dt.setLive(false)
}

// scaleMenu offers the view modes and the device's scalings.
func (dt *desktopTab) scaleMenu() {
	m := qt.NewQMenu(dt.scaleBtn.QWidget)
	m.SetAttribute(qt.WA_DeleteOnClose)
	item := func(group *qt.QActionGroup, text string, checked bool, f func()) {
		a := m.AddActionWithText(text)
		a.SetCheckable(true)
		a.SetChecked(checked)
		group.AddAction(a)
		a.OnTriggered(func() { f(); dt.view.SetFocus() })
	}
	m.AddActionWithText("View").SetEnabled(false)
	views := qt.NewQActionGroup(m.QObject)
	for i, v := range desktopViews {
		item(views, v.name, i == dt.viewMode, func() { dt.setViewMode(i) })
	}
	m.AddSeparator()
	m.AddActionWithText("Resolution sent, Windows devices").SetEnabled(false)
	scalings := qt.NewQActionGroup(m.QObject)
	for _, sc := range desktopScalings {
		item(scalings, strconv.FormatFloat(float64(sc)*100/1024, 'f', -1, 64)+" %", sc == dt.scaling, func() { dt.setScaling(sc) })
	}
	popupBelow(m, dt.scaleBtn.QWidget)
}

func (dt *desktopTab) setViewMode(mode int) {
	dt.viewMode = mode
	setPref("desktopView", desktopViews[mode].pref)
	dt.relayout()
}

// setScaling has the device scale its screen to scaling/1024, it sends the
// new size.
func (dt *desktopTab) setScaling(scaling int) {
	dt.scaling = scaling
	setPref("desktopScaling", strconv.Itoa(scaling))
	if dt.live {
		dt.sess.SetScaling(scaling)
	}
}

// newView makes a view of the screen, display 0 for the tab's.
func (dt *desktopTab) newView(display int) *deskView {
	v := &deskView{QWidget: qt.NewQWidget2(), dt: dt, display: display}
	v.SetFocusPolicy(qt.StrongFocus)
	v.SetMouseTracking(true)
	v.SetAttribute(qt.WA_OpaquePaintEvent)
	v.OnPaintEvent(func(_ func(*qt.QPaintEvent), e *qt.QPaintEvent) { v.paint(e) })
	v.OnResizeEvent(func(super func(*qt.QResizeEvent), e *qt.QResizeEvent) { super(e); v.Update() })
	// Every key belongs to the device, shortcuts and Tab included, while
	// input goes to it.
	v.OnEvent(func(super func(*qt.QEvent) bool, e *qt.QEvent) bool {
		if e.Type() == qt.QEvent__ShortcutOverride && v.inputOn() {
			k := qt.UnsafeNewQKeyEvent(e.UnsafePointer())
			if k.Key() == int(qt.Key_Return) && k.Modifiers() == qt.ControlModifier|qt.AltModifier {
				return super(e)
			}
			e.Accept()
			return true
		}
		return super(e)
	})
	v.OnFocusNextPrevChild(func(super func(bool) bool, next bool) bool { return !v.inputOn() && super(next) })
	v.OnKeyPressEvent(func(_ func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if v.inputOn() {
			dt.key(e, true)
		}
	})
	v.OnKeyReleaseEvent(func(_ func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if v.inputOn() {
			dt.key(e, false)
		}
	})
	v.OnFocusInEvent(func(super func(*qt.QFocusEvent), e *qt.QFocusEvent) { super(e); v.updateSysKeys(true) })
	v.OnFocusOutEvent(func(super func(*qt.QFocusEvent), e *qt.QFocusEvent) {
		super(e)
		v.dropSysKeys()
		dt.releaseKeys()
	})
	v.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.mouse(e, pressEvent) })
	v.OnMouseDoubleClickEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.mouse(e, pressEvent) })
	v.OnMouseReleaseEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.mouse(e, releaseEvent) })
	v.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { v.mouse(e, moveEvent) })
	v.OnWheelEvent(func(super func(*qt.QWheelEvent), e *qt.QWheelEvent) {
		if !v.inputOn() {
			super(e) // scrolls the view instead
		} else if x, y, ok := v.toRemote(e.Position()); ok {
			dt.sess.MouseWheel(e.AngleDelta().Y(), x, y)
		}
	})
	v.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), _ *qt.QContextMenuEvent) {}) // the right button is the device's
	v.setCursor(dt.cursor)
	return v
}

func (v *deskView) setCursor(shape qt.CursorShape) {
	cur := qt.NewQCursor2(shape)
	v.SetCursor(cur)
	cur.Delete()
}

// views are the tab's view and the display windows.
func (dt *desktopTab) views() []*deskView { return append([]*deskView{dt.view}, dt.screens...) }

// overview is whether the view is the tab's while the displays have windows
// of their own: it shows where they are and takes no input.
func (v *deskView) overview() bool { return v.display == 0 && len(v.dt.screens) > 0 }

// inputOn is whether the view passes the mouse and keyboard on.
func (v *deskView) inputOn() bool { return v.dt.inputOn() && !v.overview() }

func (dt *desktopTab) retheme() {
	for _, set := range dt.icons {
		set()
	}
	for _, v := range dt.screens {
		v.SetWindowIcon(icon("display"))
	}
}

func (dt *desktopTab) fullscreen(on bool) {
	window := dt.w.Window()
	dt.releaseKeys()
	if dt.detached {
		// Its own window, no client chrome to hide.
		if on {
			dt.windowState = window.WindowState()
			window.ShowFullScreen()
		} else {
			window.SetWindowState(dt.windowState)
		}
		dt.fullscreenTip(on)
	} else if on {
		if fullscreenDesktop != nil && fullscreenDesktop != dt {
			fullscreenDesktop.fullBtn.SetChecked(false)
		}
		fullscreenDesktop = dt
		dt.windowState = window.WindowState()
		dt.chromeHidden = nil
		for _, w := range desktopChrome {
			dt.chromeHidden = append(dt.chromeHidden, w.IsHidden())
			w.Hide()
		}
		window.ShowFullScreen()
		dt.fullscreenTip(true)
	} else if fullscreenDesktop == dt {
		fullscreenDesktop = nil
		for i, w := range desktopChrome {
			w.SetVisible(!dt.chromeHidden[i])
		}
		window.SetWindowState(dt.windowState)
		dt.fullscreenTip(false)
	}
	dt.retheme()
	dt.relayout()
	dt.view.SetFocus()
}

func (dt *desktopTab) fullscreenTip(on bool) {
	if on {
		dt.fullBtn.SetToolTip("Leave fullscreen (Ctrl+Alt+Enter)")
		dt.fullBtn.SetAccessibleName("Leave fullscreen")
	} else {
		dt.fullBtn.SetToolTip("Desktop-only fullscreen (Ctrl+Alt+Enter)")
		dt.fullBtn.SetAccessibleName("Fullscreen")
	}
}

// detach moves the desktop to a window of its own, or back to a tab.
func (dt *desktopTab) detach(on bool) {
	if on == dt.detached {
		return
	}
	dt.fullBtn.SetChecked(false)
	dt.releaseKeys()
	if on {
		size := dt.w.Size()
		tabs.RemoveTab(tabs.IndexOf(dt.w))
		dt.detached = true
		dt.w.SetParent2(nil, qt.Window)
		dt.w.SetWindowTitle(dt.title())
		dt.w.SetWindowIcon(icon("display"))
		dt.w.Resize(max(size.Width(), 800), max(size.Height(), 600))
		dt.w.Show()
		dt.w.ActivateWindow()
		dt.windowBtn.SetToolTip("Put the desktop back in a tab")
		logf("%s: desktop moved to its own window", dt.name)
	} else {
		dt.detached = false
		tabs.SetCurrentIndex(tabs.AddTab2(dt.w, icon("display"), dt.title()))
		tabs.Window().ActivateWindow()
		dt.windowBtn.SetToolTip("Move the desktop to a window of its own")
		logf("%s: desktop moved back to a tab", dt.name)
	}
	dt.relayout()
	dt.view.SetFocus()
}

// split shows each of the device's displays in a window of its own, cut
// from the screen of all of them, or closes those windows. Meanwhile the tab
// shows where they are, and its controls apply to all of them.
func (dt *desktopTab) split(on bool) {
	dt.fullBtn.SetChecked(false)
	dt.releaseKeys()
	if on && dt.screens == nil {
		if dt.live {
			dt.sess.SetDisplay(meshcentral.AllDisplays)
		}
		for _, id := range dt.layoutOrder() {
			dt.openScreen(id)
		}
		logf("%s: %d displays in windows of their own", dt.name, len(dt.screens))
	} else if !on && dt.screens != nil {
		views := dt.screens
		dt.screens = nil
		for _, v := range views {
			v.Close()
		}
		logf("%s: display windows closed", dt.name)
	}
	split := dt.screens != nil
	dt.displaySel.SetEnabled(dt.live && !split)
	dt.fullBtn.SetEnabled(!split)
	if split {
		dt.view.setCursor(qt.ArrowCursor)
	} else {
		dt.view.setCursor(dt.cursor)
	}
	dt.relayout()
}

// layoutOrder is the display IDs as the displays are placed, left to right.
func (dt *desktopTab) layoutOrder() []int {
	return slices.SortedFunc(maps.Keys(dt.layout), func(a, b int) int {
		ra, rb := dt.layout[a], dt.layout[b]
		return cmp.Or(ra.Min.X-rb.Min.X, ra.Min.Y-rb.Min.Y, a-b)
	})
}

// openScreen opens display id's window, on the local screen in the same
// place in the left to right order as the display.
func (dt *desktopTab) openScreen(id int) {
	v := dt.newView(id)
	v.crop = dt.layout[id]
	dt.screens = append(dt.screens, v)
	v.SetWindowTitle(dt.titled(fmt.Sprintf("Display %d", id)))
	v.SetWindowIcon(icon("display"))
	v.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		super(e)
		v.dropSysKeys()
		v.DeleteLater()
		if j := slices.Index(dt.screens, v); j >= 0 {
			dt.screens = slices.Delete(dt.screens, j, j+1)
			if len(dt.screens) == 0 {
				dt.splitBtn.SetChecked(false)
			}
			dt.view.Update()
		}
	})
	shortcut := qt.NewQShortcut2(qt.NewQKeySequence2("Ctrl+Alt+Return"), v.QObject)
	shortcut.SetContext(qt.WindowShortcut)
	shortcut.OnActivated(func() {
		dt.releaseKeys()
		if v.IsFullScreen() {
			v.SetWindowState(v.state)
		} else {
			v.state = v.WindowState()
			v.ShowFullScreen()
		}
	})
	local := qt.QGuiApplication_Screens()
	slices.SortFunc(local, func(a, b *qt.QScreen) int {
		ga, gb := a.Geometry(), b.Geometry()
		return cmp.Or(ga.X()-gb.X(), ga.Y()-gb.Y())
	})
	screen := dt.w.Screen()
	if i := slices.Index(dt.layoutOrder(), id); i < len(local) {
		screen = local[i]
	}
	v.SetScreen(screen)
	g, dpr := screen.AvailableGeometry(), screen.DevicePixelRatio()
	w := min(int(float64(v.crop.Dx())/dpr), g.Width()*9/10)
	h := min(int(float64(v.crop.Dy())/dpr), g.Height()*9/10)
	v.Resize(w, h)
	v.Move(g.X()+(g.Width()-w)/2, g.Y()+(g.Height()-h)/2)
	v.Show()
}

// showScreen brings up display id's window, reopening it if closed.
func (dt *desktopTab) showScreen(id int) {
	for _, v := range dt.screens {
		if v.display == id {
			v.SetWindowState(v.WindowState() &^ qt.WindowMinimized)
			v.Show()
			v.Raise()
			v.ActivateWindow()
			return
		}
	}
	dt.openScreen(id)
	dt.view.Update()
}

// overviewBox is a display's place in the tab's overview, in view pixels.
type overviewBox struct {
	id int
	r  image.Rectangle
}

// boxes places the displays in the view as they are placed, above a line
// for the hint.
func (v *deskView) boxes() []overviewBox {
	var all image.Rectangle
	for _, r := range v.dt.layout {
		all = all.Union(r)
	}
	line := v.FontMetrics().Height()
	area := image.Rect(24, 24, v.Width()-24, v.Height()-24-2*line)
	if all.Empty() || area.Dx() <= 0 || area.Dy() <= 0 {
		return nil
	}
	scale := min(float64(area.Dx())/float64(all.Dx()), float64(area.Dy())/float64(all.Dy()),
		640/float64(all.Dx()), 360/float64(all.Dy()))
	off := area.Min.Add(image.Pt(int(float64(area.Dx())-float64(all.Dx())*scale)/2, int(float64(area.Dy())-float64(all.Dy())*scale)/2))
	at := func(x, y int) image.Point {
		return off.Add(image.Pt(int(float64(x-all.Min.X)*scale), int(float64(y-all.Min.Y)*scale)))
	}
	var boxes []overviewBox
	for _, id := range v.dt.layoutOrder() {
		r := v.dt.layout[id]
		boxes = append(boxes, overviewBox{id, image.Rectangle{at(r.Min.X, r.Min.Y), at(r.Max.X, r.Max.Y)}.Inset(3)})
	}
	return boxes
}

// boxAt is the display at a view position in the overview.
func (v *deskView) boxAt(pos *qt.QPointF) (id int, ok bool) {
	pt := image.Pt(int(pos.X()), int(pos.Y()))
	for _, b := range v.boxes() {
		if pt.In(b.r) {
			return b.id, true
		}
	}
	return 0, false
}

// paintOverview draws the displays as they are placed, those with a window
// filled, closed ones dashed and greyed, each a monitor above its name
// where there's room.
func (v *deskView) paintOverview(p *qt.QPainter) {
	pal := v.Palette()
	p.FillRect6(v.Rect(), pal.ColorWithCr(qt.QPalette__Window))
	boxes := v.boxes()
	for _, b := range boxes {
		r := qt.NewQRect4(b.r.Min.X, b.r.Min.Y, b.r.Dx()-1, b.r.Dy()-1)
		text := pal.ColorWithCr(qt.QPalette__PlaceholderText)
		pen := qt.NewQPen3(pal.ColorWithCr(qt.QPalette__Mid))
		mode := qt.QIcon__Disabled
		if slices.ContainsFunc(v.dt.screens, func(s *deskView) bool { return s.display == b.id }) {
			p.FillRect6(r, pal.ColorWithCr(qt.QPalette__Base))
			text = pal.ColorWithCr(qt.QPalette__Text)
			mode = qt.QIcon__Normal
		} else {
			pen.SetStyle(qt.DashLine)
		}
		p.SetPenWithPen(pen)
		p.DrawRectWithRect(r)
		p.SetPen(text)
		label := fmt.Sprintf("Display %d", b.id)
		const gap = 4
		line := v.FontMetrics().Height()
		if size := min(b.r.Dy()-2*line, b.r.Dx()/2, 64); size >= 16 {
			top := b.r.Min.Y + (b.r.Dy()-size-gap-line)/2
			icon("display").Paint7(p, b.r.Min.X+(b.r.Dx()-size)/2, top, size, size, qt.AlignCenter, mode)
			p.DrawText7(b.r.Min.X, top+size+gap, b.r.Dx(), line, int(qt.AlignHCenter|qt.AlignTop), label)
		} else {
			p.DrawText6(r, int(qt.AlignCenter), label)
		}
		pen.Delete()
		r.Delete()
	}
	if len(boxes) > 0 {
		bottom := boxes[0].r.Max.Y
		for _, b := range boxes {
			bottom = max(bottom, b.r.Max.Y)
		}
		hint := qt.NewQRect4(0, bottom+v.FontMetrics().Height()/2, v.Width(), 2*v.FontMetrics().Height())
		p.SetPen(pal.ColorWithCr(qt.QPalette__PlaceholderText))
		p.DrawText6(hint, int(qt.AlignHCenter|qt.AlignTop), "Click a display to bring up its window, or reopen it")
		hint.Delete()
	}
}

// setLayout takes the displays' places in the screen of all of them, the
// windows of displays gone close.
func (dt *desktopTab) setLayout(layout map[int]image.Rectangle) {
	dt.layout = layout
	dt.splitBtn.SetVisible(len(layout) > 1)
	dt.view.Update()
	for _, v := range slices.Clone(dt.screens) {
		if r, ok := layout[v.display]; ok {
			v.crop = r
			v.Update()
		} else {
			v.Close()
		}
	}
}

func (dt *desktopTab) clipboardActive() bool {
	return !dt.closed && dt.live && dt.clipBtn.IsChecked() &&
		(dt.detached || len(dt.screens) > 0 || desktopAt(tabs.CurrentIndex()) == dt)
}

// active is whether one of the desktop's windows is the active window.
func (dt *desktopTab) active() bool {
	return dt.w.Window().IsActiveWindow() || slices.ContainsFunc(dt.screens, func(v *deskView) bool { return v.IsActiveWindow() })
}

func (dt *desktopTab) cancelClipboard() {
	dt.clipEpoch++
	if dt.clipCancel != nil {
		dt.clipCancel()
		dt.clipCancel = nil
	}
}

// resetClipboard forgets both clipboards, the next sync starts over.
func (dt *desktopTab) resetClipboard() {
	dt.cancelClipboard()
	dt.clipKnown, dt.clipRemoteKnown = false, false
	dt.clipSendError = ""
}

const clipTip = "Automatically sync text with this device while its Desktop tab or window is active"

func (dt *desktopTab) syncClipboard() {
	if !dt.clipboardActive() {
		dt.cancelClipboard()
		return
	}
	// Finish reads started here even after switching to a local application.
	if !dt.active() {
		return
	}
	if dt.clipCancel != nil {
		return
	}
	clipboard := qt.QGuiApplication_Clipboard()
	local := clipboard.Text()
	hasText := clipboard.MimeData().HasText()
	sending := hasText && (!dt.clipKnown || local != dt.clipLocal)
	if sending {
		// Observe each local value once: an unacknowledged Windows write must
		// not be retried forever at the expense of remote clipboard reads.
		dt.clipLocal, dt.clipKnown = local, true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	dt.clipCancel = cancel
	epoch, nodeID := dt.clipEpoch, dt.d.Id
	go func() {
		var remote string
		var err error
		if sending {
			err = setDesktopClipboard(ctx, nodeID, local)
		} else {
			remote, err = getDesktopClipboard(ctx, nodeID)
		}
		mainthread.Start(func() {
			cancel()
			if dt.closed || epoch != dt.clipEpoch {
				return
			}
			dt.clipCancel = nil
			if !dt.clipboardActive() {
				return
			}
			if err != nil {
				if sending && !errors.Is(err, context.Canceled) {
					message := "Clipboard send failed: " + err.Error()
					if errors.Is(err, context.DeadlineExceeded) {
						message = "Clipboard send unconfirmed; the remote agent may be busy"
					}
					if dt.clipSendError != message {
						logf("%s: %s; receiving remains enabled", dt.name, message)
					}
					dt.clipSendError = message
					dt.clipBtn.SetToolTip(message + ". Toggle clipboard sync off and on to retry.")
					dt.showSize()
					return
				}
				if dt.clipSendError != "" {
					return
				}
				if errors.Is(err, context.DeadlineExceeded) {
					dt.clipBtn.SetToolTip("Clipboard sync: no reply (clipboard may be empty or unavailable)")
				} else if !errors.Is(err, context.Canceled) {
					dt.clipBtn.SetToolTip("Clipboard sync: " + err.Error())
				}
				return
			}
			if sending {
				dt.clipSendError = ""
				dt.clipRemote, dt.clipRemoteKnown = local, true
				dt.clipBtn.SetToolTip(clipTip)
				dt.showSize()
				return
			}
			if dt.clipSendError == "" {
				dt.clipBtn.SetToolTip(clipTip)
			}
			// A remote reply must not replace text copied while it was in flight.
			if clipboard.Text() != local || clipboard.MimeData().HasText() != hasText {
				return
			}
			if !dt.clipRemoteKnown || remote != dt.clipRemote {
				dt.clipRemote, dt.clipRemoteKnown = remote, true
				dt.clipLocal, dt.clipKnown = remote, true
				clipboard.SetText(remote)
			}
		})
	}()
}

func (dt *desktopTab) setLive(live bool) {
	if !live {
		dt.resetClipboard()
	}
	dt.live = live
	dt.reconnect.SetVisible(!live)
	dt.clipBtn.SetEnabled(live)
	dt.displaySel.SetEnabled(live && !dt.splitBtn.IsChecked())
	for _, w := range []*qt.QWidget{dt.typeBtn.QWidget, dt.cadBtn.QWidget} {
		w.SetEnabled(dt.inputOn())
	}
	dt.sysKeysBtn.SetEnabled(dt.inputOn())
	for _, v := range dt.views() {
		v.Update() // an ended session's screen goes
	}
	dt.updateSysKeys()
}

// showSize puts the screen size in the status, after the agent's notice.
func (dt *desktopTab) showSize() {
	if !dt.live || dt.sess == nil {
		return
	}
	w, h := dt.sess.Size()
	if w == 0 {
		setStatus(dt.status, cmp.Or(dt.notice, "Waiting for the screen…"), false)
		return
	}
	s := fmt.Sprintf("%d × %d", w, h)
	if dt.notice != "" {
		s += " · " + dt.notice
	}
	if dt.clipSendError != "" {
		s += " · " + dt.clipSendError
	}
	setStatus(dt.status, s, false)
}

// connect opens a session, replacing an ended one.
func (dt *desktopTab) connect() {
	if dt.live || dt.closed {
		return
	}
	dt.attempt++
	attempt := dt.attempt
	dt.notice, dt.displays = "", nil
	dt.displaySel.Clear()
	dt.displaySel.SetVisible(false)
	dt.reconnect.SetVisible(false)
	if !strings.Contains(strings.ToLower(dt.d.OS), "windows") {
		dt.open(meshcentral.DefaultSession)
		return
	}
	// Windows has a session per user signed in, remote desktop ones too, and
	// shows the console's by default, where nobody may be to allow access.
	// As in the web UI, one of several is the user's to pick.
	setStatus(dt.status, "Looking for sessions…", false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), desktopSessionsTimeout)
		defer cancel()
		list, err := getUserSessions(ctx, dt.d.Id)
		mainthread.Start(func() {
			if dt.closed || attempt != dt.attempt {
				return
			}
			switch {
			case err != nil:
				logf("%s: no session list (%v), showing the default session", dt.name, err)
				dt.open(meshcentral.DefaultSession)
			case len(list) == 0:
				dt.open(meshcentral.DefaultSession)
			case len(list) == 1:
				dt.open(list[0].ID)
			default:
				dt.showSessions(list)
			}
		})
	}()
}

// showSessions has the user pick the session to show, in place of the
// screen.
func (dt *desktopTab) showSessions(list []meshcentral.UserSession) {
	attempt := dt.attempt
	setStatus(dt.status, "Choose a session", false)
	dt.sessions = qt.NewQWidget2()
	l := qt.NewQVBoxLayout(dt.sessions)
	l.AddStretch()
	label := qt.NewQLabel3("The device has more than one session. Which one should the desktop show?")
	label.SetAlignment(qt.AlignCenter)
	label.SetWordWrap(true)
	l.AddWidget(label.QWidget)
	var first *qt.QPushButton
	for _, s := range list {
		text := s.State
		if s.Station != "" {
			text += ", " + s.Station
		}
		if s.User != "" {
			text += " · " + s.User
		}
		b := qt.NewQPushButton3(text)
		b.OnClicked(func() {
			if !dt.closed && attempt == dt.attempt {
				logf("%s: desktop of session %d (%s)", dt.name, s.ID, text)
				dt.open(s.ID)
			}
		})
		l.AddWidget3(b.QWidget, 0, qt.AlignHCenter)
		if first == nil {
			first = b
		}
	}
	l.AddStretch()
	dt.scroll.Hide()
	dt.box.AddWidget2(dt.sessions, 1)
	first.SetFocus()
}

// open opens a session showing session, a UserSession's ID or
// meshcentral.DefaultSession.
func (dt *desktopTab) open(session int) {
	attempt := dt.attempt
	if dt.sessions != nil {
		dt.sessions.DeleteLater()
		dt.sessions = nil
		dt.scroll.Show()
		dt.view.SetFocus()
	}
	setStatus(dt.status, "Connecting…", false)
	q, scaling := desktopQualities[dt.qualitySel.CurrentIndex()], dt.scaling
	// Events of this attempt, on the Qt thread while it's the tab's latest.
	later := func(f func()) {
		mainthread.Start(func() {
			if !dt.closed && attempt == dt.attempt {
				f()
			}
		})
	}
	ev := meshcentral.DesktopEvents{
		Screen: func(int, int) { later(func() { dt.relayout(); dt.showSize() }) },
		Layout: func(l map[int]image.Rectangle) { later(func() { dt.setLayout(l) }) },
		Frame:  dt.queueFrame,
		Displays: func(ids []int, sel int) {
			later(func() {
				dt.displays = ids
				dt.displaySel.Clear()
				for _, id := range ids {
					if id == meshcentral.AllDisplays {
						dt.displaySel.AddItem("All displays")
					} else {
						dt.displaySel.AddItem(fmt.Sprintf("Display %d", id))
					}
				}
				dt.displaySel.SetCurrentIndex(slices.Index(ids, sel))
				dt.displaySel.SetVisible(len(ids) > 1)
			})
		},
		Notice: func(s string) {
			later(func() {
				dt.notices.log(s)
				dt.notice = s
				dt.showSize()
			})
		},
		Recorded: func() {
			later(func() {
				if !dt.recorded {
					dt.recorded = true
					tabs.SetTabText(tabs.IndexOf(dt.w), dt.title())
					dt.w.SetWindowTitle(dt.title())
					for _, v := range dt.screens {
						v.SetWindowTitle(dt.titled(fmt.Sprintf("Display %d", v.display)))
					}
					logf("%s: the server records this desktop session", dt.name)
				}
			})
		},
		Cursor: func(c int) {
			later(func() {
				dt.cursor = desktopCursor(c)
				for _, v := range dt.views() {
					if !v.overview() {
						v.setCursor(dt.cursor)
					}
				}
			})
		},
	}
	split := dt.splitBtn.IsChecked()
	go func() {
		s, err := openDesktopSession(dt.d.Id, session, ev)
		if err == nil {
			s.SetQuality(q.quality, q.frame)
			s.SetScaling(scaling)
			if split {
				s.SetDisplay(meshcentral.AllDisplays)
			}
		}
		mainthread.Start(func() {
			if dt.closed || attempt != dt.attempt {
				if s != nil {
					go s.Close()
				}
				return
			}
			if err != nil {
				setStatus(dt.status, err.Error(), true)
				dt.setLive(false)
				logf("%s: desktop failed: %v", dt.name, err)
				return
			}
			dt.sess = s
			dt.setLive(true)
			dt.showSize()
			dt.relayout()
			go func() {
				err := s.Err()
				later(func() {
					dt.releaseKeysLocal()
					dt.setLive(false)
					if err != nil {
						setStatus(dt.status, fmt.Sprintf("Session lost: %v", err), true)
						logf("%s: desktop lost: %v", dt.name, err)
					} else {
						setStatus(dt.status, cmp.Or(dt.notice, "The device closed the session"), false)
						logf("%s: the device ended the desktop session", dt.name)
					}
				})
			}()
		})
	}()
}

// queueFrame has the Qt thread repaint r, from the session's reader.
func (dt *desktopTab) queueFrame(r image.Rectangle) {
	dt.mu.Lock()
	defer dt.mu.Unlock()
	dt.dirty = dt.dirty.Union(r)
	if dt.pending {
		return
	}
	dt.pending = true
	mainthread.Start(func() {
		dt.mu.Lock()
		r := dt.dirty
		dt.dirty, dt.pending = image.Rectangle{}, false
		dt.mu.Unlock()
		if dt.closed {
			return
		}
		for _, v := range dt.views() {
			at, ok := v.target()
			if !ok {
				continue
			}
			if r := r.Intersect(at.src).Sub(at.src.Min); !r.Empty() {
				rect := qt.NewQRect4(int(at.x+float64(r.Min.X)*at.sx)-1, int(at.y+float64(r.Min.Y)*at.sy)-1,
					int(float64(r.Dx())*at.sx)+3, int(float64(r.Dy())*at.sy)+3)
				v.UpdateWithQRect(rect)
				rect.Delete()
			}
		}
	})
}

// shown is the part of the screen the view shows, none while there's no
// screen: a display's window waits for the screen of all displays.
func (v *deskView) shown() image.Rectangle {
	dt := v.dt
	if !dt.live || dt.sess == nil || v.overview() {
		return image.Rectangle{}
	}
	w, h := dt.sess.Size()
	if v.display == 0 {
		return image.Rect(0, 0, w, h)
	}
	// The layout is in the device's pixels, the screen as scaled.
	scaled := func(r image.Rectangle) image.Rectangle {
		return image.Rect(r.Min.X*dt.scaling/1024, r.Min.Y*dt.scaling/1024, r.Max.X*dt.scaling/1024, r.Max.Y*dt.scaling/1024)
	}
	var all image.Rectangle
	for _, r := range dt.layout {
		all = all.Union(r)
	}
	if scaled(all).Size() != image.Pt(w, h) {
		return image.Rectangle{}
	}
	return scaled(v.crop)
}

// placement is where src, the part of the screen shown, is drawn in a view:
// at (x, y), sx and sy view pixels per screen pixel.
type placement struct {
	src          image.Rectangle
	x, y, sx, sy float64
}

func (at placement) rect() *qt.QRectF {
	return qt.NewQRectF4(at.x, at.y, float64(at.src.Dx())*at.sx, float64(at.src.Dy())*at.sy)
}

// target is the view's placement of the screen, while there is one.
// Display windows fit it unless stretching.
func (v *deskView) target() (at placement, ok bool) {
	at.src = v.shown()
	if at.src.Empty() || v.Width() <= 0 || v.Height() <= 0 {
		return at, false
	}
	vw, vh := float64(v.Width()), float64(v.Height())
	sw, sh := float64(at.src.Dx()), float64(at.src.Dy())
	switch {
	case v.dt.viewMode == viewStretch:
		at.sx, at.sy = vw/sw, vh/sh
	case v.dt.viewMode == viewFit || v.display != 0:
		at.sx = min(vw/sw, vh/sh)
		at.sy = at.sx
	default:
		at.sx = 1 / v.DevicePixelRatioF() // a screen pixel to a device pixel
		at.sy = at.sx
	}
	at.x, at.y = (vw-sw*at.sx)/2, (vh-sh*at.sy)/2
	return at, true
}

// relayout sizes the view: the scroll area's viewport unless at actual
// size, the screen's size then.
func (dt *desktopTab) relayout() {
	fit := dt.viewMode != viewActual || dt.screens != nil
	dt.scroll.SetWidgetResizable(fit)
	if !fit && dt.sess != nil {
		if w, h := dt.sess.Size(); w > 0 {
			dpr := dt.view.DevicePixelRatioF()
			dt.view.Resize(int(float64(w)/dpr+0.5), int(float64(h)/dpr+0.5))
		}
	}
	for _, v := range dt.views() {
		v.Update()
	}
}

func (v *deskView) paint(e *qt.QPaintEvent) {
	p := qt.NewQPainter2(v.QPaintDevice)
	defer p.Delete()
	if v.overview() {
		v.paintOverview(p)
		return
	}
	at, ok := v.target()
	var tr *qt.QRectF
	if ok {
		tr = at.rect()
		defer tr.Delete()
	}
	// The frame is opaque, only what it doesn't cover needs the background.
	area := qt.NewQRectF5(e.Rect())
	defer area.Delete()
	if !ok || !tr.Contains(area) {
		bg := qt.NewQColor3(0, 0, 0)
		p.FillRect6(e.Rect(), bg)
		bg.Delete()
	}
	if !ok {
		return
	}
	if dpr := v.DevicePixelRatioF(); at.sx*dpr != 1 || at.sy*dpr != 1 {
		p.SetRenderHint2(qt.QPainter__SmoothPixmapTransform, true)
	}
	shown := at.src
	v.dt.sess.Frame(func(pix []byte, w, h int) {
		if len(pix) < w*h*4 || w == 0 || !shown.In(image.Rect(0, 0, w, h)) {
			return
		}
		// Wraps the frame for the call, the session holds it still meanwhile.
		img := qt.NewQImage4(&pix[0], w, h, qt.QImage__Format_RGB32)
		src := qt.NewQRectF4(float64(shown.Min.X), float64(shown.Min.Y), float64(shown.Dx()), float64(shown.Dy()))
		p.DrawImage(tr, img, src)
		src.Delete()
		img.Delete()
	})
}

// toRemote maps a view position to screen pixels, ok while on the part
// shown.
func (v *deskView) toRemote(pos *qt.QPointF) (x, y int, ok bool) {
	at, ok := v.target()
	if !ok {
		return 0, 0, false
	}
	src := at.src
	fx, fy := (pos.X()-at.x)/at.sx, (pos.Y()-at.y)/at.sy
	if fx < 0 || fy < 0 || fx >= float64(src.Dx()) || fy >= float64(src.Dy()) {
		return src.Min.X + min(max(int(fx), 0), src.Dx()-1), src.Min.Y + min(max(int(fy), 0), src.Dy()-1), false
	}
	return src.Min.X + int(fx), src.Min.Y + int(fy), true
}

func (v *deskView) mouse(e *qt.QMouseEvent, kind int) {
	dt := v.dt
	if v.overview() {
		id, on := v.boxAt(e.Position())
		if on && kind == pressEvent {
			dt.showScreen(id)
		} else if on {
			v.setCursor(qt.PointingHandCursor)
		} else {
			v.setCursor(qt.ArrowCursor)
		}
		return
	}
	if kind == pressEvent {
		v.SetFocus()
	}
	if !v.inputOn() {
		return
	}
	x, y, on := v.toRemote(e.Position())
	// Off the screen only a drag carries on, clamped to its edge.
	if !on && (kind == pressEvent || kind == moveEvent && e.Buttons() == qt.NoButton) {
		return
	}
	var btn int
	switch e.Button() {
	case qt.LeftButton:
		btn = meshcentral.MouseLeft
	case qt.RightButton:
		btn = meshcentral.MouseRight
	case qt.MiddleButton:
		btn = meshcentral.MouseMiddle
	}
	switch {
	case kind == moveEvent:
		dt.sess.MouseMove(x, y)
	case btn != 0:
		dt.sess.MouseButton(btn, kind == pressEvent, x, y)
	}
}

// keyID tells keys apart for their release, by the physical key where the
// platform reports it.
func keyID(e *qt.QKeyEvent) uint64 {
	if sc := e.NativeScanCode(); sc != 0 {
		return uint64(sc)
	}
	return 1<<32 | uint64(uint32(e.Key()))
}

// superModifier is the Windows or Command key, which Qt reports as Control
// on macOS.
var superModifier = func() qt.KeyboardModifier {
	if runtime.GOOS == "darwin" {
		return qt.ControlModifier
	}
	return qt.MetaModifier
}()

func (dt *desktopTab) key(e *qt.QKeyEvent, down bool) {
	if !dt.inputOn() {
		return
	}
	id := keyID(e)
	if !down {
		if e.IsAutoRepeat() { // X11 repeats as release and press pairs
			return
		}
		if k, ok := dt.down[id]; ok {
			delete(dt.down, id)
			dt.sendKey(k, false)
		}
		return
	}
	if k, ok := dt.down[id]; ok && e.IsAutoRepeat() {
		dt.sendKey(k, true)
		return
	}
	k, ok := keyFor(e)
	if !ok {
		return
	}
	dt.down[id] = k
	dt.sendKey(k, true)
}

// sysKeysView is the view the local system's shortcuts go to.
var sysKeysView *deskView

// updateSysKeys takes the local system's shortcuts for v while it has the
// keyboard and passes it on, again with force, as its window may have
// changed.
func (v *deskView) updateSysKeys(force bool) {
	if v.HasFocus() && v.inputOn() && v.dt.sysKeysBtn.IsChecked() {
		if force || sysKeysView != v {
			sysKeysView = v
			grabSysKeys(v.QWidget, v.dt.systemKey)
		}
	} else {
		v.dropSysKeys()
	}
}

func (v *deskView) dropSysKeys() {
	if sysKeysView == v {
		sysKeysView = nil
		releaseSysKeys()
	}
}

func (dt *desktopTab) updateSysKeys() {
	for _, v := range dt.views() {
		v.updateSysKeys(false)
	}
}

// systemKey is a key the local system would have kept, as a Windows
// virtual-key code.
func (dt *desktopTab) systemKey(vk int, ext, down bool) {
	if !dt.inputOn() {
		return
	}
	id, k := 2<<32|uint64(vk), sentKey{vk: vk, ext: ext}
	if down {
		dt.down[id] = k
	} else if _, ok := dt.down[id]; !ok {
		return
	} else {
		delete(dt.down, id)
	}
	dt.sendKey(k, down)
}

func (dt *desktopTab) sendKey(k sentKey, down bool) {
	if k.r != 0 {
		dt.sess.Unicode(k.r, down)
	} else {
		dt.sess.Key(k.vk, down, k.ext)
	}
}

// inputOn is whether the mouse and keyboard go to the device.
func (dt *desktopTab) inputOn() bool { return dt.live && dt.inputBtn.IsChecked() }

// releaseKeys lets go of the keys held when the view loses focus, their
// releases would go elsewhere.
func (dt *desktopTab) releaseKeys() {
	if dt.live {
		for _, k := range dt.down {
			dt.sendKey(k, false)
		}
	}
	dt.releaseKeysLocal()
}

func (dt *desktopTab) releaseKeysLocal() { clear(dt.down) }

// keyFor picks what a key press sends: text as characters, unless a
// shortcut modifier is held (Ctrl and Alt together are AltGr on Windows),
// everything else as a key.
func keyFor(e *qt.QKeyEvent) (sentKey, bool) {
	mods := e.Modifiers()
	ctrl, alt, super := mods&ctrlModifier != 0, mods&qt.AltModifier != 0, mods&superModifier != 0
	if r := []rune(e.Text()); len(r) == 1 && unicode.IsPrint(r[0]) && !super && ctrl == alt {
		return sentKey{r: r[0]}, true
	}
	vk, ext, ok := virtualKey(e.Key(), mods&qt.KeypadModifier != 0)
	return sentKey{vk: vk, ext: ext}, ok
}

// virtualKey maps a Qt key to its Windows virtual-key code, symbols by
// their place on a US keyboard.
func virtualKey(k int, keypad bool) (vk int, ext, ok bool) {
	switch {
	case k >= int(qt.Key_A) && k <= int(qt.Key_Z):
		return k, false, true
	case k >= int(qt.Key_0) && k <= int(qt.Key_9):
		if keypad {
			return 0x60 + k - int(qt.Key_0), false, true
		}
		return k, false, true
	case k >= int(qt.Key_F1) && k <= int(qt.Key_F24):
		return 0x70 + k - int(qt.Key_F1), false, true
	}
	if keypad {
		switch qt.Key(k) {
		case qt.Key_Asterisk:
			return 0x6a, false, true
		case qt.Key_Plus:
			return 0x6b, false, true
		case qt.Key_Minus:
			return 0x6d, false, true
		case qt.Key_Period, qt.Key_Comma:
			return 0x6e, false, true
		case qt.Key_Slash:
			return 0x6f, true, true
		case qt.Key_Enter:
			return 0x0d, true, true
		}
	}
	if v, ok := navKeys[qt.Key(k)]; ok {
		return v, !keypad, true // keypad navigation keys aren't extended
	}
	if v, ok := otherKeys[qt.Key(k)]; ok {
		return v.vk, v.ext, true
	}
	return 0, false, false
}

// navKeys are extended unless on the keypad.
var navKeys = map[qt.Key]int{
	qt.Key_PageUp: 0x21, qt.Key_PageDown: 0x22, qt.Key_End: 0x23, qt.Key_Home: 0x24,
	qt.Key_Left: 0x25, qt.Key_Up: 0x26, qt.Key_Right: 0x27, qt.Key_Down: 0x28,
	qt.Key_Insert: 0x2d, qt.Key_Delete: 0x2e,
}

var otherKeys = func() map[qt.Key]sentKey {
	m := map[qt.Key]sentKey{}
	add := func(vk int, ext bool, keys ...qt.Key) {
		for _, k := range keys {
			m[k] = sentKey{vk: vk, ext: ext}
		}
	}
	add(0x08, false, qt.Key_Backspace)
	add(0x09, false, qt.Key_Tab, qt.Key_Backtab)
	add(0x0d, false, qt.Key_Return, qt.Key_Enter)
	add(0x10, false, qt.Key_Shift)
	add(0x12, false, qt.Key_Alt)
	add(0x13, false, qt.Key_Pause)
	add(0x14, false, qt.Key_CapsLock)
	add(0x1b, false, qt.Key_Escape)
	add(0x20, false, qt.Key_Space)
	add(0x2c, false, qt.Key_Print)
	add(0x5d, false, qt.Key_Menu)
	add(0x90, true, qt.Key_NumLock)
	add(0x91, false, qt.Key_ScrollLock)
	add(0x5b, true, qt.Key_Super_L)
	add(0x5c, true, qt.Key_Super_R)
	// Control and Meta swap on macOS: Command is the Windows key there.
	if runtime.GOOS == "darwin" {
		add(0x11, false, qt.Key_Meta)
		add(0x5b, true, qt.Key_Control)
	} else {
		add(0x11, false, qt.Key_Control)
		add(0x5b, true, qt.Key_Meta)
	}
	add(0xba, false, qt.Key_Semicolon, qt.Key_Colon)
	add(0xbb, false, qt.Key_Equal, qt.Key_Plus)
	add(0xbc, false, qt.Key_Comma, qt.Key_Less)
	add(0xbd, false, qt.Key_Minus, qt.Key_Underscore)
	add(0xbe, false, qt.Key_Period, qt.Key_Greater)
	add(0xbf, false, qt.Key_Slash, qt.Key_Question)
	add(0xc0, false, qt.Key_QuoteLeft, qt.Key_AsciiTilde)
	add(0xdb, false, qt.Key_BracketLeft, qt.Key_BraceLeft)
	add(0xdc, false, qt.Key_Backslash, qt.Key_Bar)
	add(0xdd, false, qt.Key_BracketRight, qt.Key_BraceRight)
	add(0xde, false, qt.Key_Apostrophe, qt.Key_QuoteDbl)
	// Shifted digits, as Ctrl+Shift+1 reports them.
	for i, k := range []qt.Key{qt.Key_ParenRight, qt.Key_Exclam, qt.Key_At, qt.Key_NumberSign, qt.Key_Dollar,
		qt.Key_Percent, qt.Key_AsciiCircum, qt.Key_Ampersand, qt.Key_Asterisk, qt.Key_ParenLeft} {
		add(0x30+i, false, k)
	}
	return m
}()

// desktopCursor is the Qt cursor for the agent's cursor shape.
func desktopCursor(c int) qt.CursorShape {
	shapes := []qt.CursorShape{qt.ArrowCursor, qt.BusyCursor, qt.CrossCursor, qt.PointingHandCursor, qt.WhatsThisCursor,
		qt.IBeamCursor, qt.ForbiddenCursor, qt.SizeAllCursor, qt.SizeBDiagCursor, qt.SizeVerCursor, qt.SizeFDiagCursor,
		qt.SizeHorCursor, qt.DragLinkCursor, qt.WaitCursor, qt.BlankCursor, qt.ForbiddenCursor, qt.SplitHCursor,
		qt.SplitVCursor, qt.DragCopyCursor, qt.ArrowCursor, qt.ArrowCursor}
	if c < 0 || c >= len(shapes) {
		return qt.ArrowCursor
	}
	return shapes[c]
}
