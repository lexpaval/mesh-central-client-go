package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"image"
	"runtime"
	"slices"
	"strconv"
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

// openDesktopSession opens the session, tests replace it.
var openDesktopSession = meshcentral.OpenDesktop

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

type desktopTab struct {
	d     meshcentral.Device
	name  string
	w     *qt.QWidget
	icons []func()

	view       *qt.QWidget
	scroll     *qt.QScrollArea
	status     *qt.QLabel
	displaySel *qt.QComboBox
	qualitySel *qt.QComboBox
	fitBtn     *qt.QPushButton
	clipBtn    *qt.QPushButton
	fullBtn    *qt.QPushButton
	typeBtn    *qt.QPushButton
	cadBtn     *qt.QPushButton
	reconnect  *qt.QPushButton

	sess            *meshcentral.Desktop // the latest, kept after it ends to show its last frame
	attempt         int                  // sessions opened, results of older ones are dropped
	live            bool                 // sess is open
	notice          string
	displays        []int
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

	// Frame changes queued by the session's reader for the Qt thread.
	mu      sync.Mutex
	dirty   image.Rectangle
	pending bool
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
	dt.build()
	desktopTabs = append(desktopTabs, dt)
	tabs.SetCurrentIndex(tabs.AddTab2(dt.w, icon("display"), dt.name+" · Desktop"))
	dt.view.SetFocus()
	dt.connect()
	logf("%s: desktop opened", dt.name)
	recordRecent(recentAction{Kind: "desktop", NodeID: d.Id, Device: dt.name})
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
	dt.cancelClipboard()
	dt.closed = true
	dt.attempt++
	if dt.sess != nil {
		go dt.sess.Close()
	}
	desktopTabs = slices.DeleteFunc(desktopTabs, func(x *desktopTab) bool { return x == dt })
	tabs.RemoveTab(tabs.IndexOf(dt.w))
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
	dt.fitBtn = button("Fit", "arrows-left-right-to-line", "Scale the screen to the window, off shows it pixel for pixel")
	dt.fitBtn.SetCheckable(true)
	dt.fitBtn.SetChecked(pref("desktopActualSize") == "")
	dt.fitBtn.OnToggled(func(on bool) {
		v := ""
		if !on {
			v = "1"
		}
		setPref("desktopActualSize", v)
		dt.relayout()
		dt.view.SetFocus()
	})
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
	shortcut := qt.NewQShortcut2(qt.NewQKeySequence2("Ctrl+Alt+Return"), dt.w.QObject)
	shortcut.SetContext(qt.WidgetWithChildrenShortcut)
	shortcut.OnActivated(func() { dt.fullBtn.Toggle() })

	bar := hbox(false)
	bar.SetContentsMargins(6, 4, 0, 4) // the screen goes edge to edge, not the bar
	bar.AddWidget2(dt.status.QWidget, 1)
	for _, b := range []*qt.QPushButton{dt.fitBtn, dt.clipBtn, dt.typeBtn, dt.cadBtn, dt.fullBtn} {
		b.SetText("")
	}
	for _, w := range []*qt.QWidget{dt.displaySel.QWidget, dt.qualitySel.QWidget, dt.fitBtn.QWidget, dt.clipBtn.QWidget, dt.typeBtn.QWidget, dt.cadBtn.QWidget, dt.fullBtn.QWidget, dt.reconnect.QWidget} {
		bar.AddWidget(w)
	}

	dt.view = qt.NewQWidget2()
	dt.view.SetFocusPolicy(qt.StrongFocus)
	dt.view.SetMouseTracking(true)
	dt.view.SetAttribute(qt.WA_OpaquePaintEvent)
	dt.view.OnPaintEvent(func(_ func(*qt.QPaintEvent), e *qt.QPaintEvent) { dt.paint(e) })
	dt.view.OnResizeEvent(func(super func(*qt.QResizeEvent), e *qt.QResizeEvent) { super(e); dt.view.Update() })
	// Every key belongs to the device, shortcuts and Tab included.
	dt.view.OnEvent(func(super func(*qt.QEvent) bool, e *qt.QEvent) bool {
		if e.Type() == qt.QEvent__ShortcutOverride {
			k := qt.UnsafeNewQKeyEvent(e.UnsafePointer())
			if k.Key() == int(qt.Key_Return) && k.Modifiers() == qt.ControlModifier|qt.AltModifier {
				return super(e)
			}
			e.Accept()
			return true
		}
		return super(e)
	})
	dt.view.OnFocusNextPrevChild(func(func(bool) bool, bool) bool { return false })
	dt.view.OnKeyPressEvent(func(_ func(*qt.QKeyEvent), e *qt.QKeyEvent) { dt.key(e, true) })
	dt.view.OnKeyReleaseEvent(func(_ func(*qt.QKeyEvent), e *qt.QKeyEvent) { dt.key(e, false) })
	dt.view.OnFocusOutEvent(func(super func(*qt.QFocusEvent), e *qt.QFocusEvent) { super(e); dt.releaseKeys() })
	dt.view.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { dt.mouse(e, pressEvent) })
	dt.view.OnMouseDoubleClickEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { dt.mouse(e, pressEvent) })
	dt.view.OnMouseReleaseEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { dt.mouse(e, releaseEvent) })
	dt.view.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { dt.mouse(e, moveEvent) })
	dt.view.OnWheelEvent(func(_ func(*qt.QWheelEvent), e *qt.QWheelEvent) {
		if x, y, ok := dt.toRemote(e.Position()); ok && dt.live {
			dt.sess.MouseWheel(e.AngleDelta().Y(), x, y)
		}
	})
	dt.view.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), _ *qt.QContextMenuEvent) {}) // the right button is the device's

	dt.scroll = qt.NewQScrollArea2()
	dt.scroll.SetFrameShape(qt.QFrame__NoFrame)
	dt.scroll.SetAlignment(qt.AlignCenter)
	dt.scroll.SetWidget(dt.view)
	dt.scroll.SetBackgroundRole(qt.QPalette__Dark)

	l := qt.NewQVBoxLayout(dt.w)
	l.SetContentsMargins(0, 0, 0, 0)
	l.AddLayout(bar.QLayout)
	l.AddWidget2(dt.scroll.QWidget, 1)
	dt.relayout()
	dt.setLive(false)
}

func (dt *desktopTab) retheme() {
	for _, set := range dt.icons {
		set()
	}
}

func (dt *desktopTab) fullscreen(on bool) {
	window := dt.w.Window()
	dt.releaseKeys()
	if on {
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
		dt.fullBtn.SetToolTip("Leave fullscreen (Ctrl+Alt+Enter)")
		dt.fullBtn.SetAccessibleName("Leave fullscreen")
	} else if fullscreenDesktop == dt {
		fullscreenDesktop = nil
		for i, w := range desktopChrome {
			w.SetVisible(!dt.chromeHidden[i])
		}
		window.SetWindowState(dt.windowState)
		dt.fullBtn.SetToolTip("Desktop-only fullscreen (Ctrl+Alt+Enter)")
		dt.fullBtn.SetAccessibleName("Fullscreen")
	}
	dt.retheme()
	dt.relayout()
	dt.view.SetFocus()
}

func (dt *desktopTab) clipboardActive() bool {
	return !dt.closed && dt.live && dt.clipBtn.IsChecked() &&
		desktopAt(tabs.CurrentIndex()) == dt
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

const clipTip = "Automatically sync text with this device while its Desktop tab is active"

func (dt *desktopTab) syncClipboard() {
	if !dt.clipboardActive() {
		dt.cancelClipboard()
		return
	}
	// Finish reads started here even after switching to a local application.
	if !dt.w.Window().IsActiveWindow() {
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
	for _, w := range []*qt.QWidget{dt.clipBtn.QWidget, dt.typeBtn.QWidget, dt.cadBtn.QWidget, dt.displaySel.QWidget} {
		w.SetEnabled(live)
	}
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
	setStatus(dt.status, "Connecting…", false)
	q := desktopQualities[dt.qualitySel.CurrentIndex()]
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
		Notice: func(s string) { later(func() { dt.notice = s; dt.showSize() }) },
		Cursor: func(c int) {
			later(func() {
				cur := qt.NewQCursor2(desktopCursor(c))
				dt.view.SetCursor(cur)
				cur.Delete()
			})
		},
	}
	go func() {
		s, err := openDesktopSession(dt.d.Id, ev)
		if err == nil {
			s.SetQuality(q.quality, q.frame)
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
		tr, scale, ok := dt.target()
		if !ok {
			return
		}
		defer tr.Delete()
		rect := qt.NewQRect4(int(tr.X()+float64(r.Min.X)*scale)-1, int(tr.Y()+float64(r.Min.Y)*scale)-1,
			int(float64(r.Dx())*scale)+3, int(float64(r.Dy())*scale)+3)
		dt.view.UpdateWithQRect(rect)
		rect.Delete()
	})
}

// target is where the screen is drawn in the view, and the view pixels per
// screen pixel.
func (dt *desktopTab) target() (r *qt.QRectF, scale float64, ok bool) {
	if dt.sess == nil {
		return nil, 0, false
	}
	w, h := dt.sess.Size()
	if w == 0 || h == 0 {
		return nil, 0, false
	}
	dpr := dt.view.DevicePixelRatioF()
	scale = 1 / dpr // a screen pixel to a device pixel
	vw, vh := float64(dt.view.Width()), float64(dt.view.Height())
	if dt.fitBtn.IsChecked() {
		scale = min(vw/float64(w), vh/float64(h), scale)
	}
	tw, th := float64(w)*scale, float64(h)*scale
	return qt.NewQRectF4((vw-tw)/2, (vh-th)/2, tw, th), scale, true
}

// relayout sizes the view: the scroll area's viewport when fitting, the
// screen's size otherwise.
func (dt *desktopTab) relayout() {
	fit := dt.fitBtn.IsChecked()
	dt.scroll.SetWidgetResizable(fit)
	if !fit && dt.sess != nil {
		if w, h := dt.sess.Size(); w > 0 {
			dpr := dt.view.DevicePixelRatioF()
			dt.view.Resize(int(float64(w)/dpr+0.5), int(float64(h)/dpr+0.5))
		}
	}
	dt.view.Update()
}

func (dt *desktopTab) paint(e *qt.QPaintEvent) {
	p := qt.NewQPainter2(dt.view.QPaintDevice)
	defer p.Delete()
	tr, scale, ok := dt.target()
	if ok {
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
	if scale*dt.view.DevicePixelRatioF() != 1 {
		p.SetRenderHint2(qt.QPainter__SmoothPixmapTransform, true)
	}
	dt.sess.Frame(func(pix []byte, w, h int) {
		if len(pix) < w*h*4 || w == 0 {
			return
		}
		// Wraps the frame for the call, the session holds it still meanwhile.
		img := qt.NewQImage4(&pix[0], w, h, qt.QImage__Format_RGB32)
		src := qt.NewQRectF4(0, 0, float64(w), float64(h))
		p.DrawImage(tr, img, src)
		src.Delete()
		img.Delete()
	})
	if !dt.live {
		shade := qt.NewQColor3(0, 0, 0)
		shade.SetAlpha(96)
		p.FillRect6(dt.view.Rect(), shade)
		shade.Delete()
	}
}

// toRemote maps a view position to screen pixels, ok while on the screen.
func (dt *desktopTab) toRemote(pos *qt.QPointF) (x, y int, ok bool) {
	tr, scale, ok := dt.target()
	if !ok {
		return 0, 0, false
	}
	defer tr.Delete()
	w, h := dt.sess.Size()
	fx, fy := (pos.X()-tr.X())/scale, (pos.Y()-tr.Y())/scale
	if fx < 0 || fy < 0 || fx >= float64(w) || fy >= float64(h) {
		return min(max(int(fx), 0), w-1), min(max(int(fy), 0), h-1), false
	}
	return int(fx), int(fy), true
}

func (dt *desktopTab) mouse(e *qt.QMouseEvent, kind int) {
	if kind == pressEvent {
		dt.view.SetFocus()
	}
	if !dt.live {
		return
	}
	x, y, on := dt.toRemote(e.Position())
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
	if !dt.live {
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

func (dt *desktopTab) sendKey(k sentKey, down bool) {
	if k.r != 0 {
		dt.sess.Unicode(k.r, down)
	} else {
		dt.sess.Key(k.vk, down, k.ext)
	}
}

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
