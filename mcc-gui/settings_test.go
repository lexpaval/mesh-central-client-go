package main

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
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
