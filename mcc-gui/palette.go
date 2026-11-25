package main

import (
	"cmp"
	"fmt"
	"slices"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// Quick actions (Ctrl+K, Ctrl+Shift+K in a shell, where Ctrl+K is the
// shell's): a popup over the window that does from the keyboard what the
// device list and Recent do. Empty, it lists Recent. Typing searches the
// devices as the device list's search does, hidden or offline ones too,
// the most used first; Enter opens one as double-click does, Tab lists its
// actions.

// paletteRow is a row of the list and what Enter on it does.
type paletteRow struct {
	rowData
	run    func()
	device *meshcentral.Device // a device row, Tab lists its actions
}

type palette struct {
	w      *qt.QWidget // the popup
	edit   *qt.QLineEdit
	list   *qt.QListWidget
	hint   *qt.QLabel
	rows   []paletteRow
	device *meshcentral.Device // whose actions are listed, nil for the search
	query  string              // the search, back from the actions
}

// quick is the open palette, nil when closed.
var quick *palette

// paletteMax is how many devices a search lists, a palette isn't for
// browsing.
const paletteMax = 50

// paletteShortcuts opens the palette from anywhere in w's window. A shell
// keeps Ctrl+K, a desktop taking input every key.
func paletteShortcuts(w *qt.QWidget) {
	for _, k := range []string{"Ctrl+K", "Ctrl+Shift+K"} {
		qt.NewQShortcut2(qt.NewQKeySequence2(k), w.QObject).OnActivated(openPalette)
	}
}

// passPaletteKey lets Ctrl+K open the palette from e, where it would delete
// to the end of the line (Linux desktops).
func passPaletteKey(e *qt.QLineEdit) {
	e.OnEvent(func(super func(*qt.QEvent) bool, ev *qt.QEvent) bool {
		if ev.Type() == qt.QEvent__ShortcutOverride {
			if k := qt.UnsafeNewQKeyEvent(ev.UnsafePointer()); k.Key() == int(qt.Key_K) && k.Modifiers() == qt.ControlModifier {
				ev.Ignore()
				return true
			}
		}
		return super(ev)
	})
}

func openPalette() {
	if quick != nil {
		quick.w.ActivateWindow()
		return
	}
	p := &palette{}
	quick = p
	top := tabs.Window()
	frame := qt.NewQFrame3(top, qt.Popup)
	frame.SetFrameShape(qt.QFrame__StyledPanel) // a border, apart from the window under it
	p.w = frame.QWidget
	p.w.SetAttribute(qt.WA_DeleteOnClose)
	frame.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		super(e)
		if quick == p {
			quick = nil
		}
	})
	v := qt.NewQVBoxLayout(p.w)
	p.edit = qt.NewQLineEdit2()
	p.edit.SetPlaceholderText("Search devices, or pick a recent action")
	p.edit.OnFocusNextPrevChild(func(func(bool) bool, bool) bool { return false }) // Tab lists the actions
	p.edit.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if !p.key(e) {
			super(e)
		}
	})
	p.edit.OnTextChanged(func(string) { p.fill() })
	v.AddWidget(p.edit.QWidget)
	p.list = qt.NewQListWidget2()
	p.list.SetFocusPolicy(qt.NoFocus)
	p.list.SetUniformItemSizes(false)
	delegate := selectionDelegate(p.list.QAbstractItemView, func(pt *qt.QPainter, opt *qt.QStyleOptionViewItem, idx *qt.QModelIndex, selected bool) {
		if r := idx.Row(); r < len(p.rows) {
			paintRow(pt, opt.Rect(), opt.Palette(), selected, p.rows[r].rowData)
		}
	})
	full, inline := qt.NewQSize2(0, rowHeight(false)), qt.NewQSize2(0, rowHeight(true))
	delegate.OnSizeHint(func(_ func(*qt.QStyleOptionViewItem, *qt.QModelIndex) *qt.QSize, _ *qt.QStyleOptionViewItem, idx *qt.QModelIndex) *qt.QSize {
		if r := idx.Row(); r < len(p.rows) && p.rows[r].inline {
			return inline
		}
		return full
	})
	p.list.OnItemClicked(func(*qt.QListWidgetItem) { p.run() })
	v.AddWidget2(p.list.QWidget, 1)
	p.hint = qt.NewQLabel2()
	p.hint.SetForegroundRole(qt.QPalette__PlaceholderText)
	v.AddWidget(p.hint.QWidget)

	// Over the top of the window, as wide as it has room for.
	width := min(640, top.Width()-40)
	p.w.Resize(width, min(440, top.Height()-80))
	p.w.MoveWithQPoint(top.MapToGlobalWithQPoint(qt.NewQPoint2((top.Width()-width)/2, 60)))
	p.fill()
	p.w.Show()
	p.edit.SetFocus()
}

// key handles the keys that move in the list, run its row or switch
// between devices and actions, reporting whether it used e.
func (p *palette) key(e *qt.QKeyEvent) bool {
	k, n, cur := e.Key(), p.list.Count(), p.list.CurrentRow()
	move := func(to int) {
		if n > 0 {
			p.list.SetCurrentRow(max(0, min(n-1, to)))
		}
	}
	switch {
	case k == int(qt.Key_Escape):
		p.w.Close()
	case k == int(qt.Key_Down):
		move(cur + 1)
	case k == int(qt.Key_Up):
		move(cur - 1)
	case k == int(qt.Key_PageDown):
		move(cur + 10)
	case k == int(qt.Key_PageUp):
		move(cur - 10)
	case k == int(qt.Key_Return), k == int(qt.Key_Enter):
		p.run()
	case k == int(qt.Key_Tab), k == int(qt.Key_Right) && p.edit.CursorPosition() == len(p.edit.Text()):
		if cur < 0 || cur >= len(p.rows) || p.rows[cur].device == nil {
			return k == int(qt.Key_Tab)
		}
		d := *p.rows[cur].device
		p.query, p.device = p.edit.Text(), &d
		p.edit.SetPlaceholderText("Actions for " + deviceName(d))
		p.setText("")
	case p.device != nil && (k == int(qt.Key_Backtab) || p.edit.CursorPosition() == 0 &&
		(k == int(qt.Key_Left) || k == int(qt.Key_Backspace) && p.edit.Text() == "")):
		p.device = nil
		p.edit.SetPlaceholderText("Search devices, or pick a recent action")
		p.setText(p.query)
	default:
		return false
	}
	return true
}

// setText changes the text and fills the list, also when the text was
// already that.
func (p *palette) setText(s string) {
	p.edit.BlockSignals(true)
	p.edit.SetText(s)
	p.edit.BlockSignals(false)
	p.fill()
}

// run closes the palette and does what the current row does.
func (p *palette) run() {
	r := p.list.CurrentRow()
	if r < 0 || r >= len(p.rows) {
		return
	}
	run := p.rows[r].run
	p.w.Close()
	run()
}

// fill lists what the text finds: Recent, devices, or the device's actions.
func (p *palette) fill() {
	text := strings.TrimSpace(p.edit.Text())
	switch {
	case p.device != nil:
		p.rows = slices.DeleteFunc(paletteActions(*p.device), func(r paletteRow) bool {
			return !strings.Contains(strings.ToLower(r.title), strings.ToLower(text))
		})
		p.hint.SetText("Enter runs it · ← back to the devices · Esc closes")
	case !connected:
		p.rows = nil
		p.hint.SetText("Connect to search the devices")
	case text == "":
		p.rows = nil
		for _, a := range recent {
			p.rows = append(p.rows, paletteRow{rowData: recentRowData(a), run: func() { replayRecent(a) }})
		}
		p.hint.SetText("Recent · type to search the devices · Esc closes")
		if len(p.rows) == 0 {
			p.hint.SetText("Type to search the devices · Esc closes")
		}
	default:
		p.rows = searchRows(text)
		p.hint.SetText("Enter opens it · Tab lists its actions · Esc closes")
		if len(p.rows) == 0 {
			p.hint.SetText("No device matches · Esc closes")
		}
	}
	p.list.Clear()
	for range p.rows {
		p.list.AddItem("")
	}
	if len(p.rows) > 0 {
		p.list.SetCurrentRow(0)
	}
}

// searchRows are the devices q finds: the most used first, by Recent, then
// the online ones, then by name.
func searchRows(q string) []paletteRow {
	use := map[string]float64{}
	now := timeNow()
	for _, a := range recent {
		use[a.NodeID] += a.score(now)
	}
	s := meshcentral.ParseSearch(q)
	var found []int
	for i := range devices {
		if s.Match(&devices[i]) {
			found = append(found, i)
		}
	}
	slices.SortFunc(found, func(a, b int) int {
		da, db := &devices[a], &devices[b]
		return cmp.Or(cmp.Compare(use[db.Id], use[da.Id]), cmp.Compare(min(db.Pwr, 1), min(da.Pwr, 1)),
			cmp.Compare(strings.ToLower(deviceName(*da)), strings.ToLower(deviceName(*db))))
	})
	rows := make([]paletteRow, 0, min(len(found), paletteMax))
	for _, i := range found[:min(len(found), paletteMax)] {
		d := devices[i]
		rd := treeRowData(d.Id)
		if d.Group != "" {
			rd.sub = d.Group + " · " + rd.sub
		}
		rows = append(rows, paletteRow{rowData: rd, run: func() { openDefault(d) }, device: &d})
	}
	return rows
}

// paletteActions are what the palette offers for d, as its right-click menu
// does, with a route for each preset.
func paletteActions(d meshcentral.Device) []paletteRow {
	name := deviceName(d)
	act := func(title, ic string, run func()) paletteRow {
		return paletteRow{rowData: rowData{title: title, icon: ic, inline: true}, run: run}
	}
	var rows []paletteRow
	if strings.Contains(strings.ToLower(d.OS), "windows") {
		rows = append(rows, act("Command prompt", "terminal", func() { openShell(d, 1) }),
			act("PowerShell", "terminal", func() { openShell(d, 6) }))
	} else {
		rows = append(rows, act("Shell", "terminal", func() { openShell(d, 1) }))
	}
	rows = append(rows, act("Files", "folder", func() { openFilesTab(d) }),
		act("Desktop", "display", func() { openDesktop(d) }),
		act("Details", "circle-info", func() { openDetails(d) }))
	for _, pr := range routeChoices() {
		if own, ok := presetFor(pr.Port); ok && own != pr { // a built-in one the user's replaces
			continue
		}
		_, ic := service(pr.Port)
		r := act(pr.Name+" route", ic, func() {
			user := ""
			if openKind(pr.Port) == "ssh" {
				user = lastSSHUser(d.Id)
			}
			useRoute(name, &meshcentral.Route{NodeID: d.Id, Target: pr.Target, RemotePort: pr.Port}, user, true)
			tabs.SetCurrentWidget(routesTab)
		})
		r.sub = fmt.Sprintf("  port %d, starts and opens it", pr.Port)
		if pr.Target != "" {
			r.sub = fmt.Sprintf("  %s:%d, starts and opens it", pr.Target, pr.Port)
		}
		rows = append(rows, r)
	}
	rows = append(rows, act("Add route…", "plus", func() { showAddRouteFor(d) }),
		act("Run command…", "play", func() { showRunCommandFor(d, "", false) }),
		act("Notes…", "note-sticky", func() { showNotes(d.Id, name, false) }),
		act("Wake up", "sun", func() { wake(d.Id, name) }))
	if d.Conn&1 != 0 { // the agent's, it has to be connected
		for _, pa := range powerActions {
			rows = append(rows, act(pa.label+"…", pa.icon, func() { askPower(d, pa) }))
		}
	}
	return rows
}
