package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	qt "github.com/mappu/miqt/qt6"

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
