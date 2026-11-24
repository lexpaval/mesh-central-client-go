package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// Double-click and Enter open what the deviceAction pref names.
func TestDeviceAction(t *testing.T) {
	if runtime.GOOS == "linux" {
		// ssh opens in the first terminal found, a stand-in here.
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "ptyxis"), []byte("#!/bin/sh\n"), 0o755)
		t.Setenv("PATH", dir)
	}
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		defer disconnect()
		selectDevice("n")
		enter := qt.NewQKeyEvent(qt.QEvent__KeyPress, int(qt.Key_Return), qt.NoModifier)
		defer enter.Delete()
		for _, c := range []struct {
			action string
			byKey  bool
			opened func() bool
		}{
			{"", false, func() bool { return len(shells) == 1 }},
			{"files", true, func() bool { return len(filesTabs) == 1 }},
			{"desktop", false, func() bool { return len(desktopTabs) == 1 }},
			{"ssh", true, func() bool {
				return len(routes) == 1 && routes[0].route.RemotePort == 22 && tabs.CurrentWidget().UnsafePointer() == routesTab.UnsafePointer()
			}},
		} {
			if c.action == "ssh" && runtime.GOOS != "linux" {
				continue
			}
			setPref("deviceAction", c.action)
			if c.byKey {
				qt.QCoreApplication_SendEvent(deviceTree.QObject, enter.QEvent)
			} else {
				deviceTree.ItemDoubleClicked(treeItems["n"], 0)
			}
			if !c.opened() {
				t.Errorf("action %q: %d shells, %d files, %d desktops, %d routes", c.action, len(shells), len(filesTabs), len(desktopTabs), len(routes))
			}
		}
	})
}

// Light and Dark force Fusion with its palette, System puts back what Qt
// picked.
func TestTheme(t *testing.T) {
	ui(t, func() {
		dark := func() bool { return qt.QGuiApplication_Palette().ColorWithCr(qt.QPalette__Base).Lightness() < 128 }
		system := qt.QApplication_Style().Name()
		for _, c := range []struct {
			theme string
			dark  bool
		}{{"dark", true}, {"light", false}, {"dark", true}, {"", false}} {
			setPref("theme", c.theme)
			applyTheme()
			if dark() != c.dark {
				t.Errorf("theme %q: dark %v", c.theme, dark())
			}
		}
		if s := qt.QApplication_Style().Name(); s != system || forced {
			t.Errorf("System left style %q (was %q), forced %v", s, system, forced)
		}
		if !slices.ContainsFunc(themes, func(c choice) bool { return c.value == "" }) {
			t.Error("no System theme")
		}
	})
}

// Every icon in the window reads on what's behind it (WCAG's 3:1 for
// graphics) after any theme change: tinted again for the new palette, none
// left in the old one's colors.
func TestIconContrast(t *testing.T) {
	ui(t, func() {
		win = qt.NewQMainWindow2()
		win.SetCentralWidget(buildUI())
		watchPalette(win)
		win.Resize(1200, 760)
		win.Show()
		defer func() { win.Close(); setPref("theme", ""); applyTheme() }()
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		selectDevice("n")
		openSettings()
		for _, theme := range []string{"dark", "light", "dark", "", "dark", "light"} {
			setPref("theme", theme)
			applyTheme()
			settle()
			for _, bad := range lowContrastIcons(win.QWidget) {
				t.Errorf("theme %q: %s", theme, bad)
			}
			// Rows paint their pixmaps on the list's base, or its highlight once selected.
			pal := qt.QGuiApplication_Palette()
			for _, name := range []string{"terminal", "globe", "folder"} {
				if c := contrast(iconColor(rowPixmap(name, qt.QIcon__Normal)), pal.ColorWithCr(qt.QPalette__Base)); c < 3 {
					t.Errorf("theme %q: row icon %s %.1f:1", theme, name, c)
				}
				if c := contrast(iconColor(rowPixmap(name, qt.QIcon__Selected)), pal.ColorWithCr(qt.QPalette__Highlight)); c < 3 {
					t.Errorf("theme %q: selected row icon %s %.1f:1", theme, name, c)
				}
			}
		}
	})
}

// lowContrastIcons lists the enabled buttons, tabs and combo items under w
// whose icon is under 3:1 against their background.
func lowContrastIcons(w *qt.QWidget) []string {
	var bad []string
	check := func(what string, ic *qt.QIcon, bg *qt.QColor) {
		if ic.IsNull() {
			return
		}
		if c := contrast(iconColor(ic.Pixmap7(16, 16, qt.QIcon__Normal)), bg); c < 3 {
			bad = append(bad, fmt.Sprintf("%s %.1f:1", what, c))
		}
	}
	var walk func(o *qt.QObject)
	walk = func(o *qt.QObject) {
		for _, c := range o.Children() {
			walk(c)
		}
		if !o.IsWidgetType() {
			return
		}
		cw := qt.UnsafeNewQWidget(o.UnsafePointer())
		if !cw.IsVisible() || !cw.IsEnabled() {
			return
		}
		bg := cw.Palette().ColorWithCr(cw.BackgroundRole())
		name := o.MetaObject().ClassName()
		switch {
		case o.Inherits("QAbstractButton"):
			b := qt.UnsafeNewQAbstractButton(o.UnsafePointer())
			check(fmt.Sprintf("%s %q %q", name, b.Text(), b.ToolTip()), b.Icon(), bg)
		case o.Inherits("QTabBar"):
			tb := qt.UnsafeNewQTabBar(o.UnsafePointer())
			for i := range tb.Count() {
				check(fmt.Sprintf("tab %q", tb.TabText(i)), tb.TabIcon(i), bg)
			}
		case o.Inherits("QComboBox"):
			cb := qt.UnsafeNewQComboBox(o.UnsafePointer())
			for i := range cb.Count() {
				check(fmt.Sprintf("combo item %q", cb.ItemText(i)), cb.ItemIcon(i), bg)
			}
		}
	}
	walk(w.QObject)
	return bad
}

// iconColor is the average color of an icon's opaque pixels.
func iconColor(pm *qt.QPixmap) *qt.QColor {
	img := pm.ToImage()
	var r, g, b, n int
	for y := range img.Height() {
		for x := range img.Width() {
			if c := img.PixelColor(x, y); c.Alpha() > 192 {
				r, g, b, n = r+c.Red(), g+c.Green(), b+c.Blue(), n+1
			}
		}
	}
	if n == 0 {
		return qt.NewQColor11(0, 0, 0, 0)
	}
	return qt.NewQColor3(r/n, g/n, b/n)
}

// The user's route presets are offered by Add route, name the routes to
// their port and pick how they open; the Settings tab lists and removes them.
func TestRoutePresets(t *testing.T) {
	ui(t, func() {
		saveCustomPresets([]routePreset{
			{Name: "Grafana", Port: 3000, Open: "http"},
			{Name: "Portal", Port: 4443, Target: "192.0.2.50", Open: "https"},
			{Name: "Jump host", Port: 2222, Open: "ssh"},
		})
		choices := routeChoices()
		if last := choices[len(choices)-1]; len(choices) != len(presets)+3 || last.Name != "Jump host" || choices[len(presets)+1].Target != "192.0.2.50" {
			t.Errorf("Add route offers %+v", choices)
		}
		if name, ic := service(4443); name != "Portal" || ic != "globe" {
			t.Errorf("port 4443 is %q with %q", name, ic)
		}
		if name, _ := service(22); name != "SSH" {
			t.Errorf("port 22 is %q", name)
		}
		r := &meshcentral.Route{NodeID: "n", LocalPort: 40000, RemotePort: 4443}
		if got := copyText(r, ""); got != "https://127.0.0.1:40000" {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 3000
		if got := copyText(r, ""); got != "http://127.0.0.1:40000" {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 2222
		if got := copyText(r, ""); !strings.HasPrefix(got, "ssh -p 40000 ") {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 5055 // no preset, the address alone
		if got := copyText(r, ""); got != "127.0.0.1:40000" {
			t.Errorf("copy %q", got)
		}

		openSettings()
		openSettings()
		if tabs.Count() != 3 || settingsTab.presets.TopLevelItemCount() != 3 {
			t.Errorf("%d tabs, %d presets listed", tabs.Count(), settingsTab.presets.TopLevelItemCount())
			return
		}
		if item := settingsTab.presets.TopLevelItem(1); item.Text(2) != "192.0.2.50" || item.Text(3) != "Browser (https://)" {
			t.Errorf("listed %q, %q", item.Text(2), item.Text(3))
		}
		settingsTab.presets.SetCurrentItem(settingsTab.presets.TopLevelItem(0))
		settingsTab.remove.Click()
		if p := customPresets(); len(p) != 2 || p[0].Name != "Portal" || settingsTab.presets.TopLevelItemCount() != 2 {
			t.Errorf("after removing: %+v", p)
		}
		if _, ok := presetFor(3000); ok {
			t.Error("the removed preset still names its port")
		}
		settingsTab.close()
		if tabs.Count() != 2 || settingsTab != nil {
			t.Errorf("%d tabs after closing Settings", tabs.Count())
		}
	})
}

// A program's arguments split like a command line without a shell, the
// route filled in after, so a device name with spaces stays one argument.
func TestProgramArgs(t *testing.T) {
	for in, want := range map[string][]string{
		"":                              nil,
		"sftp://%L:%P/":                 {"sftp://127.0.0.1:40000/"},
		`-P %P  %L -title "%N"`:         {"-P", "40000", "127.0.0.1", "-title", "Lab PC 1"},
		`/ini="C:\Program Files\x.ini"`: {`/ini=C:\Program Files\x.ini`},
		`"" 100%% %%P`:                  {"", "100%", "%P"},
		`%N`:                            {"Lab PC 1"},
	} {
		if got := programArgs(in, "127.0.0.1", "40000", "Lab PC 1"); !slices.Equal(got, want) {
			t.Errorf("%s: %q, want %q", in, got, want)
		}
	}
	r := &meshcentral.Route{LocalPort: 40000, RemotePort: 5900}
	p := routePreset{Program: "/Applications/TigerVNC Viewer.app", Args: "%L::%P"}
	if got := programCmd(p, r, "", "darwin").Args; !slices.Equal(got, []string{"open", "-n", "-a", p.Program, "--args", "127.0.0.1::40000"}) {
		t.Errorf("darwin app: %q", got)
	}
	if got := programCmd(p, r, "", "linux").Args; !slices.Equal(got, []string{p.Program, "127.0.0.1::40000"}) {
		t.Errorf("linux: %q", got)
	}
}

// Link and program presets open their client and Copy gives the link or
// the address. The preset dialog shows each one's own fields.
func TestCustomClientPresets(t *testing.T) {
	ui(t, func() {
		setDevices([]meshcentral.Device{{Id: "node//1", MeshID: "m", Name: "Lab PC", Pwr: 1}})
		saveCustomPresets([]routePreset{
			{Name: "Files", Port: 22, Open: "link", Scheme: "sftp"},
			{Name: "VNC", Port: 5900, Open: "program", Program: "vncviewer", Args: `%L::%P -title "%N"`},
		})
		r := &meshcentral.Route{NodeID: "node//1", LocalPort: 40000, RemotePort: 22}
		if got := copyText(r, ""); got != "sftp://127.0.0.1:40000" {
			t.Errorf("link copy %q", got)
		}
		if name, ic := service(22); name != "Files" || ic != "arrow-up-right-from-square" {
			t.Errorf("link preset is %q with %q", name, ic)
		}
		r.RemotePort = 5900
		if got := copyText(r, ""); got != "127.0.0.1:40000" {
			t.Errorf("program copy %q", got)
		}
		openSettings()
		if got := settingsTab.presets.TopLevelItem(0).Text(3); got != "Link (sftp://)" {
			t.Errorf("link listed as %q", got)
		}
		if got := settingsTab.presets.TopLevelItem(1).Text(3); got != `vncviewer %L::%P -title "%N"` {
			t.Errorf("program listed as %q", got)
		}

		// The dialog's Open with, its line edits in order (Name, Remote
		// port, Target host, Scheme, Arguments; Program sits in a row with
		// Browse) and its form.
		settingsTab.editPreset(-1)
		var d *qt.QDialog
		var open *qt.QComboBox
		var edits []*qt.QLineEdit
		var ok *qt.QPushButton
		for _, w := range qt.QApplication_TopLevelWidgets() {
			if w.WindowTitle() != "Add route preset" || !w.IsVisible() {
				continue
			}
			d = qt.UnsafeNewQDialog(w.UnsafePointer())
			for _, o := range w.Children() {
				switch {
				case o.Inherits("QComboBox"):
					open = qt.UnsafeNewQComboBox(o.UnsafePointer())
				case o.Inherits("QLineEdit"):
					edits = append(edits, qt.UnsafeNewQLineEdit(o.UnsafePointer()))
				case o.Inherits("QDialogButtonBox"):
					ok = qt.UnsafeNewQDialogButtonBox(o.UnsafePointer()).Button(qt.QDialogButtonBox__Ok)
				}
			}
		}
		if d == nil || open == nil || ok == nil || len(edits) != 5 {
			t.Fatalf("dialog %v, combo %v, %d line edits", d != nil, open != nil, len(edits))
		}
		fl := qt.UnsafeNewQFormLayout(d.Layout().ItemAt(0).Layout().UnsafePointer())
		shown := func(e *qt.QLineEdit) bool { return fl.IsRowVisibleWithWidget(e.QWidget) }
		scheme, args := edits[3], edits[4]
		edits[0].SetText("Share")
		edits[1].SetText("445")
		if shown(scheme) || shown(args) {
			t.Error("link or program rows shown for a browser")
		}
		open.SetCurrentIndex(slices.IndexFunc(openKinds, func(c choice) bool { return c.value == "program" }))
		if shown(scheme) || !shown(args) || ok.IsEnabled() {
			t.Errorf("program: scheme %v, args %v, OK %v without a program", shown(scheme), shown(args), ok.IsEnabled())
		}
		open.SetCurrentIndex(slices.IndexFunc(openKinds, func(c choice) bool { return c.value == "link" }))
		if !shown(scheme) || shown(args) || ok.IsEnabled() {
			t.Errorf("link: scheme %v, args %v, OK %v without a scheme", shown(scheme), shown(args), ok.IsEnabled())
		}
		args.SetText("left from program")
		scheme.SetText("SMB://")
		if !ok.IsEnabled() {
			t.Error("OK disabled with a scheme")
		}
		d.Accept()
		if got := customPresets(); len(got) != 3 || got[2] != (routePreset{Name: "Share", Port: 445, Open: "link", Scheme: "smb"}) {
			t.Errorf("saved %+v", got)
		}
	})
	if runtime.GOOS != "linux" {
		return
	}
	// The program gets the route's address and port, and the device's name
	// as one argument.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "vncviewer"), []byte("#!/bin/sh\nprintf '%s|' \"$@\" > \"$0.args\"\n"), 0o755)
	t.Setenv("PATH", dir)
	r := &meshcentral.Route{NodeID: "node//1", LocalPort: 40000, RemotePort: 5900}
	if err := openCmd(r, "")(); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for range 50 {
		if got, _ = os.ReadFile(filepath.Join(dir, "vncviewer.args")); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(got) != "127.0.0.1::40000|-title|Lab PC|" {
		t.Errorf("program got %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if err := openCmd(r, "")(); err == nil || !strings.HasPrefix(err.Error(), "VNC: ") {
		t.Errorf("missing program: %v", err)
	}
}

// New desktops start with Input as Settings say, As last left by the last
// toggle in any tab.
func TestDesktopInputStart(t *testing.T) {
	ui(t, func() {
		for _, c := range []struct {
			start, last string
			on          bool
		}{
			{"", "off", true}, {"off", "", false}, {"last", "", true}, {"last", "off", false},
		} {
			setPref("desktopInput", c.start)
			setPref("desktopInputLast", c.last)
			if desktopInputStart() != c.on {
				t.Errorf("%q with last %q: input on is %v", c.start, c.last, !c.on)
			}
		}
	})
}

// Connect on start logs in to the last profile when the app opens, its
// routes from the last session open again. Off, the app waits for Connect.
func TestConnectAtStart(t *testing.T) {
	keyring.MockInit()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie","cookie":"c","rcookie":"r"}`))
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			for req, reply := range map[string]string{
				`"meshes"`: `{"action":"meshes","meshes":[]}`,
				`"nodes"`:  `{"action":"nodes","nodes":{}}`,
			} {
				if strings.Contains(string(msg), req) {
					ws.WriteMessage(websocket.TextMessage, []byte(reply))
				}
			}
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	viper.Set("profiles", []map[string]any{{"name": "home", "server": host}, {"name": "work", "server": host}})
	t.Cleanup(viper.Reset)

	ui(t, func() {
		setPref("lastProfile", "work")
		setProfilePref("work", "routes", `[{"Device":"dev","NodeID":"n","RemotePort":22}]`)
		refreshProfiles()
		insecureChk.SetChecked(true)
		connectAtStart()
		if !connectBtn.IsEnabled() || connected {
			t.Error("connected with the setting off")
		}
		openSettings()
		settingsTab.connectOnStart.Click()
		if prefs.m["connectOnStart"] != "1" {
			t.Errorf("app prefs %v", prefs.m)
		}
		settingsTab.close()
		connectAtStart()
	})
	defer mainthread.Wait(func() {
		for _, ar := range routes {
			ar.route.Close()
		}
		disconnect()
	})
	deadline := time.Now().Add(5 * time.Second)
	for done := false; !done; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("did not connect")
		}
		mainthread.Wait(func() {
			if !connected || session.loading {
				return
			}
			done = true
			if session.profile != "work" || len(routes) != 1 || routes[0].route.RemotePort != 22 {
				t.Errorf("connected %q with %d routes", session.profile, len(routes))
			}
		})
	}
}
