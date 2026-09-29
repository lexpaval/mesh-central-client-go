package main

import (
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/test"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// TestScreenshot renders the main window with sample data in both theme
// variants to SHOT_DIR, for checking layout and contrast without a server.
func TestScreenshot(t *testing.T) {
	dir := os.Getenv("SHOT_DIR")
	if dir == "" {
		t.Skip("SHOT_DIR not set")
	}
	a := test.NewTempApp(t)
	for name, v := range map[string]fyne.ThemeVariant{"dark": theme.VariantDark, "light": theme.VariantLight} {
		a.Settings().SetTheme(variantTheme{v})
		win = test.NewTempWindow(t, nil)
		win.SetContent(buildUI())
		win.Resize(fyne.NewSize(1100, 700))

		setConnected(true)
		statusLabel.SetText("Connected to mesh.example.com")
		profileSel.SetOptions([]string{"default"})
		profileSel.SetSelected("default")
		// Fictional names, IPs from the RFC 5737 documentation ranges.
		devices = []meshcentral.Device{
			{Id: "1", MeshID: "mesh//lab", Group: "Lab", DisplayName: "Lab - Bench 1", Name: "lab-bench-1", IP: "192.0.2.10", OS: "Fedora Linux 44 (Server Edition)", Pwr: 1},
			{Id: "2", MeshID: "mesh//custa", Group: "Customer A", DisplayName: "Site A - Office PC", Name: "DESKTOP-EXAMPLE1", IP: "198.51.100.20", OS: "Microsoft Windows 10 Pro - 22H2/19045", Pwr: 1},
			{Id: "3", MeshID: "mesh//custa", Group: "Customer A", DisplayName: "Site B - Workstation 3", Name: "WS003", IP: "198.51.100.33", OS: "Microsoft Windows 11 Pro - 24H2/26100", Pwr: 1},
			{Id: "4", MeshID: "mesh//gw", Group: "Gateways", DisplayName: "Site C - Gateway", Name: "gateway-n100-1", IP: "203.0.113.40", OS: "Fedora Linux 43 (Server Edition)", Pwr: 1},
			{Id: "5", MeshID: "mesh//lab", Group: "Lab", Name: "raspberrypi", IP: "192.0.2.12", OS: "Raspbian GNU/Linux 12 (bookworm)", Pwr: 1},
			{Id: "7", MeshID: "mesh//lab", Group: "Lab", DisplayName: "Build Server", Name: "build-01", IP: "203.0.113.70", OS: "openSUSE Tumbleweed", Pwr: 1},
			{Id: "6", MeshID: "mesh//office", Group: "Office", DisplayName: "Office Mac", Name: "office-mac", IP: "192.0.2.20", OS: "macOS 15.3", Pwr: 0},
		}
		offlineChk.SetChecked(true)
		applyFilter()
		deviceTree.OpenAllBranches()
		deviceTree.Select("1")
		routes = []*activeRoute{
			{device: "Lab - Bench 1", route: &meshcentral.Route{NodeID: "1", LocalPort: 40123, RemotePort: 22}},
			{device: "Site A - Office PC", route: &meshcentral.Route{NodeID: "2", LocalPort: 40124, RemotePort: 3389}},
			{device: "Site C - Gateway", route: &meshcentral.Route{NodeID: "4", LocalPort: 8080, Target: "192.0.2.50", RemotePort: 443}},
			{device: "Build Server", route: &meshcentral.Route{NodeID: "7", LocalPort: 40125, RemotePort: 9090}},
		}
		routeList.Refresh()
		logf("Connected with profile default, 6 devices")
		// The selected device drops off while it's selected, as a server event would report.
		onNodeEvent("nodeconnect", "1", 0, 0)
		logf("Site A - Office PC: Tunnel to remote port 3389 failed: device accepted the tunnel but closed it without sending data")

		savePNG(t, filepath.Join(dir, name+".png"), win)
	}

	// Every embedded icon at a large size, to catch SVGs the rasterizer mangles.
	a.Settings().SetTheme(variantTheme{theme.VariantDark})
	var cells []fyne.CanvasObject
	var names []string
	for n := range icons {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		img := canvas.NewImageFromResource(icons[n])
		img.FillMode = canvas.ImageFillContain
		img.SetMinSize(fyne.NewSquareSize(48))
		cells = append(cells, container.NewVBox(img, widget.NewLabel(n)))
	}
	win = test.NewTempWindow(t, container.NewGridWithColumns(6, cells...))
	win.Resize(fyne.NewSize(1100, 700))
	savePNG(t, filepath.Join(dir, "icons.png"), win)
}

func savePNG(t *testing.T, path string, w fyne.Window) {
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	png.Encode(f, w.Canvas().Capture())
}

type variantTheme struct{ v fyne.ThemeVariant }

func (t variantTheme) Color(n fyne.ThemeColorName, _ fyne.ThemeVariant) color.Color {
	return appTheme{}.Color(n, t.v)
}
func (variantTheme) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (variantTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }
func (variantTheme) Size(n fyne.ThemeSizeName) float32       { return theme.DefaultTheme().Size(n) }
