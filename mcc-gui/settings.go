package main

import (
	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// A setting's choices, by pref value, label and icon (none if ""). The
// first, value "", is the default.
type choice struct{ value, label, icon string }

var (
	themes        = []choice{{"", "System", ""}, {"light", "Light", ""}, {"dark", "Dark", ""}}
	deviceActions = []choice{{"", "Shell", "terminal"}, {"files", "Files", "folder"}, {"desktop", "Desktop", "display"},
		{"ssh", "SSH in a terminal", "terminal"}}
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
	case "ssh":
		useRoute(deviceName(d), &meshcentral.Route{NodeID: d.Id, RemotePort: 22}, true)
		tabs.SetCurrentWidget(routesTab)
	default:
		openShell(d, 1)
	}
}

func showSettings() {
	f := newForm("Settings", "Save")
	combo := func(label, key string, choices []choice) func() {
		c := qt.NewQComboBox2()
		for i, ch := range choices {
			if ch.icon != "" {
				c.AddItem2(icon(ch.icon), ch.label)
			} else {
				c.AddItem(ch.label)
			}
			if ch.value == pref(key) {
				c.SetCurrentIndex(i)
			}
		}
		f.add(label, c.QWidget)
		return func() { setPref(key, choices[c.CurrentIndex()].value) }
	}
	saveTheme := combo("Theme", "theme", themes)
	saveAction := combo("Double-click a device", "deviceAction", deviceActions)
	f.show(0, func() {
		saveAction()
		theme := pref("theme")
		saveTheme()
		if pref("theme") != theme {
			applyTheme()
		}
	})
}
