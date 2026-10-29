package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

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
		if got := copyText(r); got != "https://127.0.0.1:40000" {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 3000
		if got := copyText(r); got != "http://127.0.0.1:40000" {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 2222
		if got := copyText(r); !strings.HasPrefix(got, "ssh -p 40000 ") {
			t.Errorf("copy %q", got)
		}
		r.RemotePort = 5055 // no preset, the address alone
		if got := copyText(r); got != "127.0.0.1:40000" {
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
