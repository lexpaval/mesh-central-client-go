package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// TestScreenshot is run by tools/guibench with a local dummy server, one
// palette per process so captures start with a fresh session and preferences.
func TestScreenshot(t *testing.T) {
	dir := os.Getenv("SHOT_DIR")
	if dir == "" {
		t.Skip("SHOT_DIR not set")
	}
	server, variant := os.Getenv("SHOT_SERVER"), os.Getenv("SHOT_THEME")
	if server == "" || (variant != "light" && variant != "dark") {
		t.Fatal("use make gui-shots to start the dummy server and capture both themes")
	}
	keyring.MockInit()
	viper.Set("profiles", []map[string]string{{"name": "Demo", "server": server, "username": "alice"}})
	viper.Set("default_profile", "Demo")
	defer viper.Reset()
	loadPrefs(filepath.Join(t.TempDir(), "prefs.json"))
	mainthread.Wait(func() {
		setPref("theme", variant)
		applyTheme()
		win = qt.NewQMainWindow2()
		win.SetCentralWidget(buildUI())
		win.Resize(1200, 760)
		win.Show()
		offlineChk.SetChecked(true)
		refreshProfiles()
		connect(true)
	})
	waitFor := func(label string, ready func() bool) {
		t.Helper()
		deadline := time.Now().Add(15 * time.Second)
		for time.Now().Before(deadline) {
			done := false
			mainthread.Wait(func() { done = ready() })
			if done {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatalf("timed out waiting for %s from the dummy server", label)
	}
	waitFor("device list", func() bool { return connected && !session.loading && len(devices) == 7 })
	mainthread.Wait(func() {
		selectDevice("1")
		for _, r := range []*meshcentral.Route{
			{NodeID: "1", RemotePort: 22},
			{NodeID: "2", RemotePort: 3389},
			{NodeID: "4", Target: "192.0.2.50", RemotePort: 443},
		} {
			if err := startRoute(deviceName(devices[deviceIdx[r.NodeID]]), r); err != nil {
				t.Error(err)
			}
		}
		tabs.SetCurrentWidget(routesTab) // the routes, Recent comes first
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+".png"))

		selectDevice("7")
		showSSHConfig()
		saveDialog(t, filepath.Join(dir, variant+"-ssh-config.png"))
		showProfileDialog(false, &config.Profile{Name: "Demo", Server: server, Username: "alice"})
		saveDialog(t, filepath.Join(dir, variant+"-profile.png"))
		showAbout()
		saveDialog(t, filepath.Join(dir, variant+"-about.png"))
		selectDevice("1")
		showNotes("1", deviceName(devices[deviceIdx["1"]]), false)
	})
	waitFor("notes", func() bool {
		for _, w := range qt.QApplication_TopLevelWidgets() {
			if w.IsVisible() && strings.HasPrefix(w.WindowTitle(), "Notes") {
				for _, c := range w.Children() {
					if c.Inherits("QPlainTextEdit") && !qt.UnsafeNewQPlainTextEdit(c.UnsafePointer()).IsReadOnly() {
						return true
					}
				}
			}
		}
		return false
	})
	mainthread.Wait(func() {
		saveDialog(t, filepath.Join(dir, variant+"-notes.png"))
		openShell(devices[deviceIdx["1"]], 1)
	})
	waitFor("shell output", func() bool {
		if len(shells) != 1 {
			return false
		}
		tm := shells[0].t
		tm.mu.Lock()
		defer tm.mu.Unlock()
		return strings.Contains(tm.emu.String(), "Demo session ready")
	})
	mainthread.Wait(func() {
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+"-shell.png"))
		closeShell(shells[0])
		openFilesTab(devices[deviceIdx["1"]])
		filesTabs[0].navigate("/home/alice")
	})
	waitFor("files listing", func() bool { return len(filesTabs) == 1 && filesTabs[0].cur == "/home/alice" })
	mainthread.Wait(func() {
		ft := filesTabs[0]
		ft.selectAfter = "notes.md"
		ft.fill()
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+"-files.png"))
		openEditor(ft, "/home/alice/notes.md")
	})
	waitFor("the editor", func() bool { return len(editors) == 1 })
	mainthread.Wait(func() {
		editors[0].w.Resize(640, 360)
		settle()
		save(t, editors[0].w, filepath.Join(dir, variant+"-editor.png"))
		closeEditors()
		filesTabs[0].close()
		openDetails(devices[deviceIdx["1"]])
	})
	waitFor("the details", func() bool { return len(detailsTabs) == 1 && detailsTabs[0].info != nil })
	mainthread.Wait(func() {
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+"-details.png"))
		detailsTabs[0].close()
		openDesktop(devices[deviceIdx["1"]])
	})
	waitFor("the desktop", func() bool {
		return len(desktopTabs) == 1 && desktopTabs[0].live && desktopTabs[0].displaySel.Count() == 2
	})
	mainthread.Wait(func() {
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+"-desktop.png"))
		desktopTabs[0].fullBtn.Click()
		settle()
		save(t, win.QWidget, filepath.Join(dir, variant+"-desktop-fullscreen.png"))
		desktopTabs[0].close()
		for _, ar := range routes {
			ar.route.Close()
		}
		win.Close()
	})
}

func save(t *testing.T, w *qt.QWidget, path string) {
	if !w.Grab().Save(path) {
		t.Errorf("saving %s failed", path)
	}
}

// saveDialog saves the open dialog and closes it.
func saveDialog(t *testing.T, path string) {
	settle()
	for _, w := range qt.QApplication_TopLevelWidgets() {
		if w.IsVisible() && w.UnsafePointer() != win.UnsafePointer() && w.Inherits("QDialog") {
			save(t, w, path)
			w.Close()
		}
	}
}
