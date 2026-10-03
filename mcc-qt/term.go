package main

import (
	"image/color"
	"io"
	"math"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"unicode"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
)

// term is a device shell's terminal: a VT emulator keeps the screen, a
// QWidget paints it and turns keys, mouse and paste into input.
//
// Output from the device is written from the session goroutine, painting
// and input happen on the UI thread, mu guards the emulator between them.
// The emulator answers queries (cursor position, device attributes) through
// a synchronous pipe, so a goroutine always drains it into in, which never
// blocks, and a write can't stall on a device that stopped reading.
type term struct {
	w   *qt.QWidget
	mu  sync.Mutex
	emu *vt.Emulator
	in  *byteQueue // keystrokes and replies, read by the session

	// Set by the emulator's callbacks, under mu.
	cursorHidden bool
	mouseModes   map[ansi.Mode]bool
	altScreen    bool
	ended        bool // closed, output is dropped

	// UI thread only.
	cols, rows     int
	size           atomic.Uint64 // cols<<32 | rows, for the session goroutine
	onResize       func()
	font, bold     *qt.QFont
	cw, ch, ascent float64
	scroll         int // lines scrolled back into the history, 0 is live
	sel            struct {
		on, dragging bool
		a, b         cellPos // in lines of history+screen
	}
	pending atomic.Bool // a repaint is queued
	closed  bool
	colors  map[uint32]*qt.QColor
}

type cellPos struct{ line, col int }

func (p cellPos) before(o cellPos) bool {
	return p.line < o.line || p.line == o.line && p.col < o.col
}

func newTerm() *term {
	t := &term{emu: vt.NewEmulator(80, 24), in: newByteQueue(), mouseModes: map[ansi.Mode]bool{}, colors: map[uint32]*qt.QColor{}}
	t.emu.SetScrollbackSize(5000)
	t.emu.SetCallbacks(vt.Callbacks{
		CursorVisibility: func(v bool) { t.cursorHidden = !v },
		AltScreen:        func(on bool) { t.altScreen = on },
		EnableMode: func(m ansi.Mode) {
			if isMouseMode(m) {
				t.mouseModes[m] = true
			}
		},
		DisableMode: func(m ansi.Mode) { delete(t.mouseModes, m) },
	})
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := t.emu.Read(buf)
			if n > 0 {
				t.in.Write(buf[:n])
			}
			if err != nil {
				return
			}
		}
	}()

	t.w = qt.NewQWidget2()
	t.w.SetFocusPolicy(qt.StrongFocus)
	t.w.SetAttribute(qt.WA_InputMethodEnabled)
	t.w.SetAttribute(qt.WA_OpaquePaintEvent)
	cursor := qt.NewQCursor2(qt.IBeamCursor)
	t.w.SetCursor(cursor)
	cursor.Delete()
	t.w.SetMinimumSize2(80, 40)
	t.setFont()
	t.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) { t.paint() })
	t.w.OnResizeEvent(func(_ func(*qt.QResizeEvent), _ *qt.QResizeEvent) { t.relayout() })
	t.w.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if !t.key(e) {
			super(e)
		}
	})
	t.w.OnInputMethodEvent(func(_ func(*qt.QInputMethodEvent), e *qt.QInputMethodEvent) {
		if s := e.CommitString(); s != "" {
			t.input(func() { t.emu.SendText(s) })
		}
	})
	t.w.OnFocusNextPrevChild(func(func(bool) bool, bool) bool { return false }) // Tab belongs to the shell
	t.w.OnFocusInEvent(func(super func(*qt.QFocusEvent), e *qt.QFocusEvent) { super(e); t.w.Update() })
	t.w.OnFocusOutEvent(func(super func(*qt.QFocusEvent), e *qt.QFocusEvent) { super(e); t.w.Update() })
	t.w.OnMousePressEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { t.mouse(e, pressEvent) })
	t.w.OnMouseMoveEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { t.mouse(e, moveEvent) })
	t.w.OnMouseReleaseEvent(func(_ func(*qt.QMouseEvent), e *qt.QMouseEvent) { t.mouse(e, releaseEvent) })
	t.w.OnWheelEvent(func(_ func(*qt.QWheelEvent), e *qt.QWheelEvent) { t.wheel(e) })
	t.w.OnContextMenuEvent(func(_ func(*qt.QContextMenuEvent), e *qt.QContextMenuEvent) {
		if t.appMouse() && e.Modifiers()&qt.ShiftModifier == 0 {
			return
		}
		m := qt.NewQMenu(t.w)
		m.SetAttribute(qt.WA_DeleteOnClose)
		cp := m.AddActionWithText("Copy")
		cp.SetEnabled(t.sel.on)
		cp.OnTriggered(t.copySelection)
		m.AddActionWithText("Paste").OnTriggered(func() { t.paste(qt.QClipboard__Clipboard) })
		m.Popup(e.GlobalPos())
	})
	t.w.OnChangeEvent(func(super func(*qt.QEvent), e *qt.QEvent) {
		super(e)
		if e.Type() == qt.QEvent__FontChange && !t.closed {
			t.setFont()
			t.relayout()
		}
	})
	return t
}

func isMouseMode(m ansi.Mode) bool {
	switch m {
	case ansi.ModeMouseX10, ansi.ModeMouseNormal, ansi.ModeMouseHighlight, ansi.ModeMouseButtonEvent, ansi.ModeMouseAnyEvent:
		return true
	}
	return false
}

// Write shows device output, from any goroutine.
func (t *term) Write(p []byte) (int, error) {
	t.mu.Lock()
	if t.ended {
		t.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	n, err := t.emu.Write(p)
	t.mu.Unlock()
	if !t.pending.Swap(true) {
		mainthread.Start(func() {
			t.pending.Store(false)
			if !t.closed {
				if t.scroll > 0 { // stay on the same history lines as they scroll up
					t.clampScroll()
				}
				t.w.Update()
			}
		})
	}
	return n, err
}

// Size reports the terminal size, from any goroutine.
func (t *term) Size() (cols, rows int) {
	s := t.size.Load()
	return int(s >> 32), int(uint32(s))
}

// close ends the input, which ends the session, and stops painting. The
// widget is deleted by its tab.
func (t *term) close() {
	if t.closed {
		return
	}
	t.closed = true
	t.in.Close()
	t.mu.Lock()
	t.ended = true
	// Ends the reply reader. Not emu.Close, which sets a flag Read checks
	// unlocked.
	t.emu.InputPipe().(io.Closer).Close()
	t.mu.Unlock()
	t.bold.Delete()
	for _, c := range t.colors {
		c.Delete()
	}
	clear(t.colors)
}

// input runs an emulator input call, which encodes for the current modes,
// and returns the view to the live screen.
func (t *term) input(f func()) {
	if t.closed {
		return
	}
	t.mu.Lock()
	f()
	t.mu.Unlock()
	if t.scroll != 0 {
		t.scroll = 0
		t.w.Update()
	}
}

func (t *term) setFont() {
	f := qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont)
	f.SetStyleHint2(qt.QFont__Monospace, qt.QFont__PreferDefault)
	t.font = f
	if t.bold != nil {
		t.bold.Delete()
	}
	t.bold = qt.NewQFont5(f)
	t.bold.SetBold(true)
	fm := qt.NewQFontMetricsF(f)
	defer fm.Delete()
	t.cw = fm.HorizontalAdvance("M")
	t.ch = math.Ceil(fm.Height())
	t.ascent = fm.Ascent() + (t.ch-fm.Height())/2
}

// relayout fits the grid to the widget and resizes the emulator.
func (t *term) relayout() {
	cols := max(2, int(float64(t.w.Width())/t.cw))
	rows := max(1, int(float64(t.w.Height())/t.ch))
	if cols == t.cols && rows == t.rows {
		return
	}
	t.cols, t.rows = cols, rows
	t.mu.Lock()
	t.emu.Resize(cols, rows)
	t.mu.Unlock()
	t.size.Store(uint64(cols)<<32 | uint64(rows))
	t.clampScroll()
	if t.onResize != nil {
		t.onResize()
	}
}

func (t *term) clampScroll() {
	t.mu.Lock()
	n := t.emu.ScrollbackLen()
	t.mu.Unlock()
	t.scroll = min(max(t.scroll, 0), n)
}

// cellAt returns the cell on a line of history+screen, locked by the caller.
func (t *term) cellAt(line, col int) *uv.Cell {
	if sb := t.emu.ScrollbackLen(); line < sb {
		return t.emu.ScrollbackCellAt(col, line)
	} else {
		return t.emu.CellAt(col, line-sb)
	}
}

// The 16 ANSI colors, for dark and light backgrounds.
var (
	ansiDark = [16]uint32{
		0x2e3436, 0xef5350, 0x8ae234, 0xfce94f, 0x729fcf, 0xad7fa8, 0x34e2e2, 0xd3d7cf,
		0x777a75, 0xff7b72, 0xa6f36b, 0xfff27a, 0x9cc4f5, 0xd7a3d2, 0x7ff5f5, 0xffffff,
	}
	ansiLight = [16]uint32{
		0x2e3436, 0xcc0000, 0x3e8a06, 0x9a7300, 0x2d62b8, 0x75507b, 0x06989a, 0xa8aca4,
		0x555753, 0xef2929, 0x4e9a06, 0xb58900, 0x3465a4, 0x8f5a8a, 0x118a8c, 0x2e3436,
	}
)

// rgb resolves a cell color, def for the default.
func (t *term) rgb(c color.Color, def uint32, dark bool) uint32 {
	pal := &ansiLight
	if dark {
		pal = &ansiDark
	}
	switch c := c.(type) {
	case nil:
		return def
	case ansi.BasicColor:
		return pal[c&15]
	case ansi.IndexedColor:
		i := int(c)
		switch {
		case i < 16:
			return pal[i]
		case i < 232: // 6x6x6 cube
			i -= 16
			lv := func(v int) uint32 {
				if v == 0 {
					return 0
				}
				return uint32(55 + v*40)
			}
			return lv(i/36)<<16 | lv(i/6%6)<<8 | lv(i%6)
		default: // grays
			v := uint32(8 + (i-232)*10)
			return v<<16 | v<<8 | v
		}
	}
	r, g, b, _ := c.RGBA()
	return (r>>8)<<16 | (g>>8)<<8 | b>>8
}

func (t *term) qcolor(rgb uint32) *qt.QColor {
	c, ok := t.colors[rgb]
	if !ok {
		c = qt.NewQColor3(int(rgb>>16), int(rgb>>8&0xff), int(rgb&0xff))
		t.colors[rgb] = c
	}
	return c
}

func colorRGB(c *qt.QColor) uint32 {
	return uint32(c.Red())<<16 | uint32(c.Green())<<8 | uint32(c.Blue())
}

func (t *term) paint() {
	if t.closed {
		return
	}
	p := qt.NewQPainter2(t.w.QPaintDevice)
	defer p.Delete()
	r := qt.NewQRectF()
	defer r.Delete()
	fill := func(x, y, w, h float64, c uint32) {
		r.SetRect(x, y, w, h)
		p.FillRect4(r, t.qcolor(c))
	}
	pal := t.w.Palette()
	defBg, defFg := colorRGB(pal.ColorWithCr(qt.QPalette__Base)), colorRGB(pal.ColorWithCr(qt.QPalette__Text))
	selBg, selFg := colorRGB(pal.ColorWithCr(qt.QPalette__Highlight)), colorRGB(pal.ColorWithCr(qt.QPalette__HighlightedText))
	dark := pal.ColorWithCr(qt.QPalette__Base).Lightness() < 128
	p.FillRect6(t.w.Rect(), t.qcolor(defBg))

	t.mu.Lock()
	defer t.mu.Unlock()
	sb := t.emu.ScrollbackLen()
	top := sb - t.scroll
	selA, selB := t.sel.a, t.sel.b
	if selB.before(selA) {
		selA, selB = selB, selA
	}
	var run strings.Builder
	for row := 0; row < t.rows; row++ {
		line := top + row
		y := float64(row) * t.ch
		// Text is drawn in runs of the same color and weight, which keep to
		// the grid since a monospace font advances by the cell width. Other
		// characters are drawn one by one, a fallback font may be wider.
		var runX float64
		var runFg uint32
		var runBold bool
		flush := func() {
			if run.Len() == 0 {
				return
			}
			if runBold {
				p.SetFont(t.bold)
			} else {
				p.SetFont(t.font)
			}
			p.SetPen(t.qcolor(runFg))
			pt := qt.NewQPointF3(runX, y+t.ascent)
			p.DrawText(pt, run.String())
			pt.Delete()
			run.Reset()
		}
		for col := 0; col < t.cols; {
			c := t.cellAt(line, col)
			width, content := 1, " "
			var st uv.Style
			if c != nil {
				width, content, st = max(c.Width, 1), c.Content, c.Style
			}
			fg, bg := t.rgb(st.Fg, defFg, dark), t.rgb(st.Bg, defBg, dark)
			if st.Attrs&uv.AttrReverse != 0 {
				fg, bg = bg, fg
			}
			if st.Attrs&uv.AttrFaint != 0 {
				fg = blend(fg, bg)
			}
			if pos := (cellPos{line, col}); t.sel.on && !pos.before(selA) && pos.before(selB) {
				fg, bg = selFg, selBg
			}
			if st.Attrs&uv.AttrConceal != 0 {
				fg = bg
			}
			x, w := float64(col)*t.cw, float64(width)*t.cw
			if bg != defBg {
				fill(x, y, w, t.ch, bg)
			}
			if st.Underline != ansi.UnderlineNone {
				fill(x, y+t.ascent+1, w, 1, fg)
			}
			if st.Attrs&uv.AttrStrikethrough != 0 {
				fill(x, y+t.ascent*0.65, w, 1, fg)
			}
			bold := st.Attrs&uv.AttrBold != 0
			ascii := len(content) == 1 && content[0] >= 0x20 && content[0] < 0x7f
			if run.Len() > 0 && (!ascii || fg != runFg || bold != runBold) {
				flush()
			}
			switch {
			case ascii && run.Len() > 0:
				run.WriteString(content)
			case ascii && content != " ": // runs start at the first character
				runX, runFg, runBold = x, fg, bold
				run.WriteString(content)
			case content != "" && content != " ":
				runX, runFg, runBold = x, fg, bold
				run.WriteString(content)
				flush()
			}
			col += width
		}
		flush()
	}

	// The cursor, a block while focused and an outline otherwise.
	cur := t.emu.CursorPosition()
	if !t.cursorHidden && t.scroll == 0 && cur.Y < t.rows && cur.X < t.cols {
		r.SetRect(float64(cur.X)*t.cw, float64(cur.Y)*t.ch, t.cw, t.ch)
		if t.w.HasFocus() {
			p.FillRect4(r, t.qcolor(defFg))
			if c := t.emu.CellAt(cur.X, cur.Y); c != nil && c.Content != "" && c.Content != " " {
				p.SetFont(t.font)
				p.SetPen(t.qcolor(defBg))
				pt := qt.NewQPointF3(r.X(), r.Y()+t.ascent)
				p.DrawText(pt, c.Content)
				pt.Delete()
			}
		} else {
			p.SetPen(t.qcolor(defFg))
			r.SetRect(r.X()+0.5, r.Y()+0.5, t.cw-1, t.ch-1)
			p.DrawRect(r)
		}
	}
}

func blend(a, b uint32) uint32 {
	ch := func(s int) uint32 { return ((a>>s&0xff + b>>s&0xff) / 2) << s }
	return ch(16) | ch(8) | ch(0)
}

// qtKeys maps Qt's keys to the emulator's, which encodes them for the modes
// the shell set (application cursor keys and such).
var qtKeys = map[int]rune{
	int(qt.Key_Up): vt.KeyUp, int(qt.Key_Down): vt.KeyDown, int(qt.Key_Left): vt.KeyLeft, int(qt.Key_Right): vt.KeyRight,
	int(qt.Key_Home): vt.KeyHome, int(qt.Key_End): vt.KeyEnd, int(qt.Key_PageUp): vt.KeyPgUp, int(qt.Key_PageDown): vt.KeyPgDown,
	int(qt.Key_Insert): vt.KeyInsert, int(qt.Key_Delete): vt.KeyDelete, int(qt.Key_Backspace): vt.KeyBackspace,
	int(qt.Key_Tab): vt.KeyTab, int(qt.Key_Return): vt.KeyEnter, int(qt.Key_Enter): vt.KeyEnter, int(qt.Key_Escape): vt.KeyEscape,
	int(qt.Key_F1): vt.KeyF1, int(qt.Key_F2): vt.KeyF2, int(qt.Key_F3): vt.KeyF3, int(qt.Key_F4): vt.KeyF4,
	int(qt.Key_F5): vt.KeyF5, int(qt.Key_F6): vt.KeyF6, int(qt.Key_F7): vt.KeyF7, int(qt.Key_F8): vt.KeyF8,
	int(qt.Key_F9): vt.KeyF9, int(qt.Key_F10): vt.KeyF10, int(qt.Key_F11): vt.KeyF11, int(qt.Key_F12): vt.KeyF12,
}

// ctrlModifier is the physical Control key, which Qt reports as Meta on macOS.
var ctrlModifier = func() qt.KeyboardModifier {
	if runtime.GOOS == "darwin" {
		return qt.MetaModifier
	}
	return qt.ControlModifier
}()

// key handles a key press, reporting whether it was used.
func (t *term) key(e *qt.QKeyEvent) bool {
	k, mods, text := e.Key(), e.Modifiers(), e.Text()
	ctrl, shift, alt := mods&ctrlModifier != 0, mods&qt.ShiftModifier != 0, mods&qt.AltModifier != 0

	// Copy and paste, and paging through the history.
	switch {
	case ctrl && shift && k == int(qt.Key_C), k == int(qt.Key_Copy):
		t.copySelection()
		return true
	case ctrl && shift && k == int(qt.Key_V), shift && k == int(qt.Key_Insert), k == int(qt.Key_Paste):
		t.paste(qt.QClipboard__Clipboard)
		return true
	case shift && (k == int(qt.Key_PageUp) || k == int(qt.Key_PageDown)) && !t.onAltScreen():
		d := t.rows - 1
		if k == int(qt.Key_PageDown) {
			d = -d
		}
		t.scroll += d
		t.clampScroll()
		t.w.Update()
		return true
	}

	var mod vt.KeyMod
	if ctrl {
		mod |= vt.ModCtrl
	}
	if shift {
		mod |= vt.ModShift
	}
	if alt {
		mod |= vt.ModAlt
	}
	if k == int(qt.Key_Backtab) {
		t.input(func() { t.emu.SendText("\x1b[Z") })
		return true
	}
	if code, ok := qtKeys[k]; ok {
		t.input(func() { t.emu.SendKey(vt.KeyPressEvent{Code: code, Mod: mod}) })
		return true
	}
	// Control combinations by the key, the text differs between platforms.
	if ctrl && k >= 0x20 && k < 0x7f {
		code := unicode.ToLower(rune(k))
		t.input(func() { t.emu.SendKey(vt.KeyPressEvent{Code: code, Mod: mod &^ vt.ModShift}) })
		return true
	}
	if text == "" {
		return false
	}
	if alt {
		text = "\x1b" + text
	}
	t.input(func() { t.emu.SendText(text) })
	return true
}

const (
	pressEvent = iota
	moveEvent
	releaseEvent
)

// appMouse reports whether the shell asked for mouse events.
func (t *term) appMouse() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.mouseModes) > 0
}

// onAltScreen reports whether a full screen program runs, which has no history.
func (t *term) onAltScreen() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.altScreen
}

func (t *term) cellFromPos(x, y float64) (col, row int) {
	return min(max(int(x/t.cw), 0), t.cols-1), min(max(int(y/t.ch), 0), t.rows-1)
}

// mouse forwards to the shell when it asked for the mouse (Shift selects
// anyway), otherwise it selects text: releasing sets the primary selection,
// the middle button pastes it.
func (t *term) mouse(e *qt.QMouseEvent, kind int) {
	pos := e.Position()
	col, row := t.cellFromPos(pos.X(), pos.Y())
	if kind == pressEvent {
		t.w.SetFocus()
	}
	if t.appMouse() && e.Modifiers()&qt.ShiftModifier == 0 {
		btn := vt.MouseNone
		switch e.Button() {
		case qt.LeftButton:
			btn = vt.MouseLeft
		case qt.MiddleButton:
			btn = vt.MouseMiddle
		case qt.RightButton:
			btn = vt.MouseRight
		}
		if kind == moveEvent {
			switch {
			case e.Buttons()&qt.LeftButton != 0:
				btn = vt.MouseLeft
			case e.Buttons()&qt.MiddleButton != 0:
				btn = vt.MouseMiddle
			case e.Buttons()&qt.RightButton != 0:
				btn = vt.MouseRight
			}
		}
		m := uv.Mouse{X: col, Y: row, Button: btn}
		t.input(func() {
			switch kind {
			case pressEvent:
				t.emu.SendMouse(vt.MouseClick(m))
			case releaseEvent:
				t.emu.SendMouse(vt.MouseRelease(m))
			default:
				t.emu.SendMouse(vt.MouseMotion(m))
			}
		})
		return
	}

	t.mu.Lock()
	at := cellPos{t.emu.ScrollbackLen() - t.scroll + row, col}
	t.mu.Unlock()
	switch {
	case kind == pressEvent && e.Button() == qt.LeftButton:
		t.sel.on, t.sel.dragging, t.sel.a, t.sel.b = false, true, at, at
		t.w.Update()
	case kind == pressEvent && e.Button() == qt.MiddleButton:
		t.paste(qt.QClipboard__Selection)
	case kind == moveEvent && t.sel.dragging:
		// Past the right half of a cell takes it in.
		if pos.X()-float64(col)*t.cw > t.cw/2 {
			at.col++
		}
		t.sel.b, t.sel.on = at, at != t.sel.a
		t.w.Update()
	case kind == releaseEvent && t.sel.dragging:
		t.sel.dragging = false
		if s := t.selectionText(); s != "" {
			qt.QGuiApplication_Clipboard().SetText2(s, qt.QClipboard__Selection)
		}
	}
}

func (t *term) wheel(e *qt.QWheelEvent) {
	d := e.AngleDelta().Y() / 40 // 3 lines a notch
	if d == 0 {
		return
	}
	if t.appMouse() && e.Modifiers()&qt.ShiftModifier == 0 {
		pos := e.Position()
		col, row := t.cellFromPos(pos.X(), pos.Y())
		btn := vt.MouseWheelUp
		if d < 0 {
			btn = vt.MouseWheelDown
		}
		t.input(func() { t.emu.SendMouse(vt.MouseWheel(uv.Mouse{X: col, Y: row, Button: btn})) })
		return
	}
	if t.onAltScreen() { // full screen programs have no history, scroll them with arrows
		code := vt.KeyUp
		if d < 0 {
			code = vt.KeyDown
		}
		t.input(func() {
			for range abs(d) {
				t.emu.SendKey(vt.KeyPressEvent{Code: code})
			}
		})
		return
	}
	t.scroll += d
	t.clampScroll()
	t.w.Update()
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// selectionText returns the selected text, lines without trailing blanks.
func (t *term) selectionText() string {
	if !t.sel.on {
		return ""
	}
	a, b := t.sel.a, t.sel.b
	if b.before(a) {
		a, b = b, a
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	var lines []string
	for line := a.line; line <= b.line; line++ {
		from, to := 0, t.cols
		if line == a.line {
			from = a.col
		}
		if line == b.line {
			to = min(b.col, t.cols)
		}
		var s strings.Builder
		for col := from; col < to; col++ {
			if c := t.cellAt(line, col); c == nil {
				s.WriteByte(' ')
			} else {
				s.WriteString(c.Content) // "" for the second half of wide characters
			}
		}
		lines = append(lines, strings.TrimRight(s.String(), " "))
	}
	return strings.Join(lines, "\n")
}

func (t *term) copySelection() {
	if s := t.selectionText(); s != "" {
		qt.QGuiApplication_Clipboard().SetText(s)
	}
}

func (t *term) paste(mode qt.QClipboard__Mode) {
	s := qt.QGuiApplication_Clipboard().TextWithMode(mode)
	if s == "" {
		return
	}
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\r"), "\n", "\r")
	t.input(func() { t.emu.Paste(s) })
}

// byteQueue is an unbounded pipe: writes never block, reads wait for data
// and return EOF once it's closed and drained.
type byteQueue struct {
	mu     sync.Mutex
	cond   *sync.Cond
	buf    []byte
	closed bool
}

func newByteQueue() *byteQueue {
	q := &byteQueue{}
	q.cond = sync.NewCond(&q.mu)
	return q
}

func (q *byteQueue) Write(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return 0, io.ErrClosedPipe
	}
	q.buf = append(q.buf, p...)
	q.cond.Signal()
	return len(p), nil
}

func (q *byteQueue) Read(p []byte) (int, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.buf) == 0 && !q.closed {
		q.cond.Wait()
	}
	if len(q.buf) == 0 {
		return 0, io.EOF
	}
	n := copy(p, q.buf)
	q.buf = q.buf[n:]
	return n, nil
}

func (q *byteQueue) Close() {
	q.mu.Lock()
	q.closed = true
	q.cond.Broadcast()
	q.mu.Unlock()
}
