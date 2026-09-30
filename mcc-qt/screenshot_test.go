package main

import (
	"os"
	"path/filepath"
	"testing"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// TestScreenshot renders the main window with sample data in a dark and a
// light palette to SHOT_DIR, plus dialogs and a shell, for checking layout
// and contrast without a server.
func TestScreenshot(t *testing.T) {
	dir := os.Getenv("SHOT_DIR")
	if dir == "" {
		t.Skip("SHOT_DIR not set")
	}
	for _, variant := range []string{"dark", "light"} {
		ui(t, func() {
			setPalette(variant == "dark")
			win = qt.NewQMainWindow2()
			win.SetCentralWidget(buildUI())
			win.Resize(1100, 700)
			win.Show()
			setDevices(nil)

			setConnected(true)
			statusLabel.SetText("Connected to mesh.example.com as alice (profile default)")
			// Fictional names, IPs from the RFC 5737 documentation ranges.
			devs := []meshcentral.Device{
				{Id: "1", MeshID: "mesh//lab", Group: "Lab", DisplayName: "Lab - Bench 1", Name: "lab-bench-1", IP: "192.0.2.10", OS: "Fedora Linux 44 (Server Edition)", Pwr: 1},
				{Id: "2", MeshID: "mesh//custa", Group: "Customer A", DisplayName: "Site A - Office PC", Name: "DESKTOP-EXAMPLE1", IP: "198.51.100.20", OS: "Microsoft Windows 10 Pro - 22H2/19045", Pwr: 1},
				{Id: "3", MeshID: "mesh//custa", Group: "Customer A", DisplayName: "Site B - Workstation 3", Name: "WS003", IP: "198.51.100.33", OS: "Microsoft Windows 11 Pro - 24H2/26100", Pwr: 1},
				{Id: "4", MeshID: "mesh//gw", Group: "Gateways", DisplayName: "Site C - Gateway", Name: "gateway-n100-1", IP: "203.0.113.40", OS: "Fedora Linux 43 (Server Edition)", Pwr: 1},
				{Id: "5", MeshID: "mesh//lab", Group: "Lab", Name: "raspberrypi", IP: "192.0.2.12", OS: "Raspbian GNU/Linux 12 (bookworm)", Pwr: 1},
				{Id: "7", MeshID: "mesh//lab", Group: "Lab", DisplayName: "Build Server", Name: "build-01", IP: "203.0.113.70", OS: "openSUSE Tumbleweed", Pwr: 1},
				{Id: "6", MeshID: "mesh//office", Group: "Office", DisplayName: "Office Mac", Name: "office-mac", IP: "192.0.2.20", OS: "macOS 15.3", Pwr: 0},
			}
			offlineChk.SetChecked(true)
			setDevices(devs)
			selectDevice("1")
			routes = []*activeRoute{
				{device: "Lab - Bench 1", route: &meshcentral.Route{NodeID: "1", LocalPort: 40123, RemotePort: 22}},
				{device: "Site A - Office PC", route: &meshcentral.Route{NodeID: "2", LocalPort: 40124, RemotePort: 3389}},
				{device: "Site C - Gateway", route: &meshcentral.Route{NodeID: "4", LocalPort: 8080, Target: "192.0.2.50", RemotePort: 443}},
				{device: "Office Mac", route: &meshcentral.Route{NodeID: "6", LocalPort: 40125, RemotePort: 5900}},
			}
			rebuildRoutes()
			logf("Connected with profile default, 7 devices")
			logf("Site A - Office PC: Tunnel to remote port 3389 failed: device accepted the tunnel but closed it without sending data")
			settle()
			save(t, win.QWidget, filepath.Join(dir, variant+".png"))

			session.profile = "default"
			selectDevice("7")
			showSSHConfig()
			saveDialog(t, filepath.Join(dir, variant+"-ssh-config.png"))
			profileSel.AddItems([]string{"default", "work"})
			showProfileDialog(false, &config.Profile{Name: "work", Server: "mesh.example.com", Username: "alice"})
			saveDialog(t, filepath.Join(dir, variant+"-profile.png"))
			showAbout()
			saveDialog(t, filepath.Join(dir, variant+"-about.png"))

			// A shell with sample output in place of a session.
			tm := newTerm()
			tabs.SetCurrentIndex(tabs.AddTab2(tm.w, icon("terminal"), "Lab - Bench 1"))
			settle()
			tm.Write([]byte(sampleShell))
			settle()
			save(t, win.QWidget, filepath.Join(dir, variant+"-shell.png"))
			tm.close()
			routes = nil
			win.Close()
		})
	}
	mainthread.Wait(func() { setPalette(false) })
}

const sampleShell = "\x1b[1;32malice@lab-bench-1\x1b[0m:\x1b[1;34m~\x1b[0m$ ls --color\r\n" +
	"\x1b[1;34mbin\x1b[0m  \x1b[1;34mprojects\x1b[0m  notes.txt  \x1b[1;32mrun.sh\x1b[0m  \x1b[1;31marchive.tar.gz\x1b[0m\r\n" +
	"\x1b[1;32malice@lab-bench-1\x1b[0m:\x1b[1;34m~\x1b[0m$ systemctl status sshd --no-pager\r\n" +
	"\x1b[1;32m●\x1b[0m sshd.service - OpenSSH server daemon\r\n" +
	"     Loaded: loaded (/usr/lib/systemd/system/sshd.service; \x1b[1;32menabled\x1b[0m)\r\n" +
	"     Active: \x1b[1;32mactive (running)\x1b[0m since Tue 2026-09-29 08:12:03 CEST\r\n" +
	"\x1b[7m reverse \x1b[0m \x1b[4munderline\x1b[0m \x1b[2mfaint\x1b[0m \x1b[38;5;208m256-color\x1b[0m \x1b[38;2;120;180;255mtruecolor\x1b[0m 世界 ─┼─\r\n" +
	"\x1b[1;32malice@lab-bench-1\x1b[0m:\x1b[1;34m~\x1b[0m$ "

// setPalette switches Fusion between a dark palette and its default light one.
func setPalette(dark bool) {
	pal := qt.QApplication_Style().StandardPalette()
	if dark {
		c := func(hex string) *qt.QColor { return qt.NewQColor6(hex) }
		for role, hex := range map[qt.QPalette__ColorRole]string{
			qt.QPalette__Window: "#202124", qt.QPalette__WindowText: "#e8e8e8", qt.QPalette__Base: "#17171a",
			qt.QPalette__AlternateBase: "#26262a", qt.QPalette__Text: "#e8e8e8", qt.QPalette__Button: "#2b2b30",
			qt.QPalette__ButtonText: "#e8e8e8", qt.QPalette__Highlight: "#3d6fd9", qt.QPalette__HighlightedText: "#ffffff",
			qt.QPalette__PlaceholderText: "#8c8c94", qt.QPalette__Link: "#8ab4f8", qt.QPalette__ToolTipBase: "#2b2b30",
			qt.QPalette__ToolTipText: "#e8e8e8", qt.QPalette__Light: "#3a3a40", qt.QPalette__Mid: "#2e2e33", qt.QPalette__Dark: "#101012",
		} {
			pal.SetColor2(role, c(hex))
		}
		for _, role := range []qt.QPalette__ColorRole{qt.QPalette__WindowText, qt.QPalette__Text, qt.QPalette__ButtonText} {
			pal.SetColor(qt.QPalette__Disabled, role, c("#74747a"))
		}
	}
	qt.QApplication_SetPalette(pal)
	resetIcons()
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
