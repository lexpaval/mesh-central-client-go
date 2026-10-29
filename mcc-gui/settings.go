package main

import (
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// A setting's choices, by pref value, label and icon (none if ""). The
// first, value "", is the default.
type choice struct{ value, label, icon string }

var (
	themes        = []choice{{"", "System", ""}, {"light", "Light", ""}, {"dark", "Dark", ""}}
	deviceActions = []choice{{"", "Shell", "terminal"}, {"files", "Files", "folder"}, {"desktop", "Desktop", "display"},
		{"ssh", "SSH in a terminal", "terminal"}, {"details", "Details", "circle-info"}}
)

// systemStyle is the style Qt picked at start, the System theme goes back
// to it once forced is set.
var (
	systemStyle string
	forced      bool
)

// applyTheme sets the "theme" pref. Qt 6.8 and later switch the native
// style through the color scheme, where the platform theme supports that.
// Otherwise (Qt 6.4, the oldest supported, or the xdg portal theme) Light
// and Dark use Fusion with a palette of its own.
func applyTheme() {
	if systemStyle == "" {
		systemStyle = qt.QApplication_Style().Name()
	}
	t := pref("theme")
	if t != "light" && t != "dark" {
		t = ""
	}
	setColorScheme(t)
	switch {
	case t == "" && forced:
		forced = false
		qt.QApplication_SetStyleWithStyle(systemStyle)
		qt.QApplication_SetPalette(qt.NewQPalette()) // no roles set, all from the system
	case t != "" && (forced || isDark() != (t == "dark")):
		setColorScheme("")
		forced = true
		qt.QApplication_SetStyleWithStyle("Fusion")
		qt.QApplication_SetPalette(fusionPalette(t == "dark"))
	}
	resetIcons() // the window does this on palette changes, but may not exist yet
}

// setColorScheme asks the platform theme for "light", "dark" or "" for the
// system's. It's QStyleHints.colorScheme from Qt 6.8, past the 6.4 the
// bindings are made for, so it goes by the property's name. The palette
// changes before it returns, if the theme does it at all.
func setColorScheme(t string) {
	hints := qt.QGuiApplication_StyleHints()
	if hints.MetaObject().IndexOfProperty("colorScheme") >= 0 {
		hints.SetProperty("colorScheme", qt.NewQVariant4(map[string]int{"light": 1, "dark": 2}[t])) // Qt::ColorScheme
	}
}

// isDark is how the terminal tells, by the palette's base color.
func isDark() bool {
	return qt.QGuiApplication_Palette().ColorWithCr(qt.QPalette__Base).Lightness() < 128
}

// fusionPalette is a light or dark palette for Fusion. Not its standard
// palette, from Qt 6.5 that follows the system's color scheme.
func fusionPalette(dark bool) *qt.QPalette {
	colors, disabled := map[qt.QPalette__ColorRole]string{ // Fusion's own light colors
		qt.QPalette__Window: "#efefef", qt.QPalette__WindowText: "#000000", qt.QPalette__Base: "#ffffff",
		qt.QPalette__AlternateBase: "#f7f7f7", qt.QPalette__Text: "#000000", qt.QPalette__Button: "#efefef",
		qt.QPalette__ButtonText: "#000000", qt.QPalette__Highlight: "#308cc6", qt.QPalette__HighlightedText: "#ffffff",
		qt.QPalette__PlaceholderText: "#808080", qt.QPalette__Link: "#0000ff", qt.QPalette__ToolTipBase: "#ffffdc",
		qt.QPalette__ToolTipText: "#000000", qt.QPalette__Light: "#ffffff", qt.QPalette__Mid: "#b8b8b8", qt.QPalette__Dark: "#9f9f9f",
	}, "#bebebe"
	if dark {
		colors, disabled = map[qt.QPalette__ColorRole]string{
			qt.QPalette__Window: "#202124", qt.QPalette__WindowText: "#e8e8e8", qt.QPalette__Base: "#17171a",
			qt.QPalette__AlternateBase: "#26262a", qt.QPalette__Text: "#e8e8e8", qt.QPalette__Button: "#2b2b30",
			qt.QPalette__ButtonText: "#e8e8e8", qt.QPalette__Highlight: "#3d6fd9", qt.QPalette__HighlightedText: "#ffffff",
			qt.QPalette__PlaceholderText: "#8c8c94", qt.QPalette__Link: "#8ab4f8", qt.QPalette__ToolTipBase: "#2b2b30",
			qt.QPalette__ToolTipText: "#e8e8e8", qt.QPalette__Light: "#3a3a40", qt.QPalette__Mid: "#2e2e33", qt.QPalette__Dark: "#101012",
		}, "#74747a"
	}
	c := func(hex string) *qt.QColor { return qt.NewQColor6(hex) }
	// Every role set, the ones not listed derived from the button and window.
	pal := qt.NewQPalette4(c(colors[qt.QPalette__Button]), c(colors[qt.QPalette__Window]))
	for role, hex := range colors {
		pal.SetColor2(role, c(hex))
	}
	for _, role := range []qt.QPalette__ColorRole{qt.QPalette__WindowText, qt.QPalette__Text, qt.QPalette__ButtonText} {
		pal.SetColor(qt.QPalette__Disabled, role, c(disabled))
	}
	return pal
}

// openDefault runs the double-click (and Enter) action on d.
func openDefault(d meshcentral.Device) {
	switch pref("deviceAction") {
	case "files":
		openFilesTab(d)
	case "desktop":
		openDesktop(d)
	case "details":
		openDetails(d)
	case "ssh":
		useRoute(deviceName(d), &meshcentral.Route{NodeID: d.Id, RemotePort: 22}, true)
		tabs.SetCurrentWidget(routesTab)
	default:
		openShell(d, 1)
	}
}

// routePreset is a remote port the Add route dialog offers, the user's own
// besides the built-in ones. Routes to its port take its name, and Open
// starts its client.
type routePreset struct {
	Name   string
	Port   int
	Target string `json:",omitempty"` // "" for the device itself
	Open   string // an openKinds value
}

// openKinds are the clients a route can open with.
var openKinds = []choice{{"http", "Browser (http://)", "globe"}, {"https", "Browser (https://)", "globe"},
	{"ssh", "SSH in a terminal", "terminal"}, {"rdp", "RDP client", "desktop"}}

// The user's presets, parsed again when the pref changes.
var presetCache struct {
	raw     string
	presets []routePreset
}

// customPresets are the user's route presets, in the "routePresets" pref.
func customPresets() []routePreset {
	if raw := pref("routePresets"); raw != presetCache.raw {
		presetCache.raw, presetCache.presets = raw, nil
		json.Unmarshal([]byte(raw), &presetCache.presets)
	}
	return presetCache.presets
}

// saveCustomPresets stores the user's presets, routes and Recent then show
// their names.
func saveCustomPresets(p []routePreset) {
	b := []byte(nil)
	if len(p) > 0 {
		b, _ = json.Marshal(p)
	}
	setPref("routePresets", string(b))
	refreshRows()
}

// presetFor is the user's preset for port, if any.
func presetFor(port int) (routePreset, bool) {
	i := slices.IndexFunc(customPresets(), func(p routePreset) bool { return p.Port == port })
	if i < 0 {
		return routePreset{}, false
	}
	return customPresets()[i], true
}

// openKind is how a route to port opens: a preset's choice, or by the
// well-known ports. "" opens the browser over HTTP, Copy then copies the
// address alone.
func openKind(port int) string {
	if p, ok := presetFor(port); ok {
		return p.Open
	}
	switch port {
	case 22:
		return "ssh"
	case 3389:
		return "rdp"
	case 80, 8080:
		return "http"
	case 443, 8443, 9090: // Cockpit serves TLS on 9090
		return "https"
	}
	return ""
}

// settingsPage is the Settings tab, one at most.
type settingsPage struct {
	w       *qt.QWidget
	icons   []func() // set again on a palette change
	presets *qt.QTreeWidget
	remove  *qt.QPushButton
}

var settingsTab *settingsPage

// openSettings shows the Settings tab, opening it if needed. Changes apply
// as they're made.
func openSettings() {
	if settingsTab == nil {
		settingsTab = &settingsPage{}
		settingsTab.build()
		tabs.AddTab2(settingsTab.w, icon("gear"), "Settings")
	}
	tabs.SetCurrentWidget(settingsTab.w)
}

func settingsAt(i int) *settingsPage {
	if w := tabs.Widget(i); settingsTab != nil && w != nil && w.UnsafePointer() == settingsTab.w.UnsafePointer() {
		return settingsTab
	}
	return nil
}

func (sp *settingsPage) close() {
	tabs.RemoveTab(tabs.IndexOf(sp.w))
	sp.w.DeleteLater()
	settingsTab = nil
}

func (sp *settingsPage) retheme() {
	for _, set := range sp.icons {
		set()
	}
}

func (sp *settingsPage) build() {
	page := qt.NewQWidget2()
	v := qt.NewQVBoxLayout(page)
	section := func(title string) *qt.QFormLayout {
		box := qt.NewQGroupBox3(title)
		fl := qt.NewQFormLayout(box.QWidget)
		v.AddWidget(box.QWidget)
		return fl
	}
	// combo shows a pref's choices, apply runs after one is picked.
	combo := func(key string, choices []choice, apply func()) *qt.QComboBox {
		c := qt.NewQComboBox2()
		for i, ch := range choices {
			c.AddItem(ch.label)
			if ch.value == pref(key) {
				c.SetCurrentIndex(i)
			}
		}
		sp.icons = append(sp.icons, func() { setChoiceIcons(c, choices) })
		setChoiceIcons(c, choices)
		c.OnActivated(func(i int) {
			setPref(key, choices[i].value)
			if apply != nil {
				apply()
			}
		})
		return c
	}
	section("Appearance").AddRow3("Theme", combo("theme", themes, applyTheme).QWidget)
	section("Devices").AddRow3("Double-click a device", combo("deviceAction", deviceActions, nil).QWidget)

	box := qt.NewQGroupBox3("Route presets")
	bl := qt.NewQVBoxLayout(box.QWidget)
	hint := qt.NewQLabel3("Your own ports for Add route, after the built-in ones. Routes to a preset's port take its name and open with its client.")
	hint.SetWordWrap(true)
	hint.SetForegroundRole(qt.QPalette__PlaceholderText)
	bl.AddWidget(hint.QWidget)
	sp.presets = qt.NewQTreeWidget2()
	sp.presets.SetHeaderLabels([]string{"Name", "Port", "Target", "Open with"})
	sp.presets.SetRootIsDecorated(false)
	sp.presets.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	sp.presets.SetMinimumHeight(80)
	button := func(text, iconName, tip string) *qt.QPushButton { return tabButton(&sp.icons, text, iconName, tip) }
	add := button("Add…", "plus", "Add a route preset")
	edit := button("Edit…", "pen", "Edit the selected preset")
	remove := button("Remove", "trash-can", "Remove the selected preset")
	sp.remove = remove
	add.OnClicked(func() { sp.editPreset(-1) })
	edit.OnClicked(func() { sp.editPreset(sp.selected()) })
	remove.OnClicked(func() {
		if i := sp.selected(); i >= 0 {
			saveCustomPresets(slices.Delete(slices.Clone(customPresets()), i, i+1))
			sp.fill()
		}
	})
	sp.presets.OnItemDoubleClicked(func(*qt.QTreeWidgetItem, int) { sp.editPreset(sp.selected()) })
	sp.presets.OnItemSelectionChanged(func() {
		edit.SetEnabled(sp.selected() >= 0)
		remove.SetEnabled(sp.selected() >= 0)
	})
	// The buttons above the list, in view however short the tab is.
	row := hbox(false, add.QWidget, edit.QWidget, remove.QWidget)
	row.AddStretch()
	bl.AddLayout(row.QLayout)
	bl.AddWidget2(sp.presets.QWidget, 1)
	v.AddWidget2(box.QWidget, 1) // the list takes the room left
	sp.fill()

	scroll := qt.NewQScrollArea2()
	scroll.SetWidgetResizable(true)
	scroll.SetWidget(page)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	sp.w = scroll.QWidget
}

// setChoiceIcons tints a combo's choice icons for the palette.
func setChoiceIcons(c *qt.QComboBox, choices []choice) {
	for i, ch := range choices {
		if ch.icon != "" {
			c.SetItemIcon(i, icon(ch.icon))
		}
	}
}

func (sp *settingsPage) selected() int {
	if sel := sp.presets.SelectedItems(); len(sel) > 0 {
		return sp.presets.IndexOfTopLevelItem(sel[0])
	}
	return -1
}

// fill lists the presets, in the order they're offered.
func (sp *settingsPage) fill() {
	sp.presets.Clear()
	for _, p := range customPresets() {
		open := p.Open
		if i := slices.IndexFunc(openKinds, func(c choice) bool { return c.value == p.Open }); i >= 0 {
			open = openKinds[i].label
		}
		sp.presets.AddTopLevelItem(qt.NewQTreeWidgetItem2([]string{p.Name, strconv.Itoa(p.Port), cmp.Or(p.Target, "device"), open}))
	}
	for i := range 4 {
		sp.presets.ResizeColumnToContents(i)
	}
	sp.presets.ItemSelectionChanged()
}

// editPreset edits the preset at i, or adds one for -1.
func (sp *settingsPage) editPreset(i int) {
	presets := slices.Clone(customPresets())
	p := routePreset{Open: "http"}
	title := "Add route preset"
	if i >= 0 {
		p, title = presets[i], "Edit route preset"
	}
	f := newForm(title, "Save")
	name := f.entry("Name", p.Name, "e.g. Grafana", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	})
	portText := ""
	if p.Port > 0 {
		portText = strconv.Itoa(p.Port)
	}
	port := f.entry("Remote port", portText, "", func(s string) error {
		if err := portValidator(false)(s); err != nil {
			return err
		}
		n, _ := strconv.Atoi(s)
		for j, o := range presets {
			if j != i && o.Port == n {
				return fmt.Errorf("%s already uses port %d", o.Name, n)
			}
		}
		return nil
	})
	target := f.entry("Target host", p.Target, "the device itself", nil)
	open := qt.NewQComboBox2()
	for j, k := range openKinds {
		open.AddItem2(icon(k.icon), k.label)
		if k.value == p.Open {
			open.SetCurrentIndex(j)
		}
	}
	f.add("Open with", open.QWidget)
	name.SetMinimumWidth(240)
	f.show(0, func() {
		p.Name, p.Target, p.Open = strings.TrimSpace(name.Text()), strings.TrimSpace(target.Text()), openKinds[open.CurrentIndex()].value
		p.Port, _ = strconv.Atoi(port.Text())
		if p.Target == "127.0.0.1" { // the device itself, as Add route takes it
			p.Target = ""
		}
		if i >= 0 {
			presets[i] = p
		} else {
			presets = append(presets, p)
		}
		saveCustomPresets(presets)
		if settingsTab == sp {
			sp.fill()
		}
	})
}
