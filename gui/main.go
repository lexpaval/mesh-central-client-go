// mcc-gui is a MeshRouter-style desktop front end for the same core as the
// mcc CLI: log in with a CLI profile, pick devices, run several port routes.
package main

import (
	"cmp"
	"embed"
	"errors"
	"fmt"
	"image/color"
	"maps"
	"net/url"
	"os"
	"os/exec"
	osuser "os/user"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/spf13/viper"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

type activeRoute struct {
	route  *meshcentral.Route
	device string
}

// UI state, only touched from the Fyne goroutine (background work hands
// results back through fyne.Do).
var (
	win       fyne.Window
	connected bool
	// What connect used, for the SSH config snippet.
	session struct {
		profile  string
		insecure bool
	}
	devices    []meshcentral.Device // everything the server returned
	selectedID string               // selected device, "" if none
	// Tree view of the devices passing the search/offline filter, by group.
	groupOrder    []string            // mesh IDs sorted by group name
	groupChildren map[string][]string // mesh ID -> device IDs sorted by name
	groupLabels   map[string][2]string
	shownByID     map[string]meshcentral.Device
	routes        []*activeRoute
	logLines      []string
	logLabel      *widget.Label
	logScroll     *container.Scroll
	statusLabel   *widget.Label
	profileSel    *widget.Select
	profileBar    []fyne.CanvasObject // hidden while connected, the status shows the server
	topBar        *fyne.Container
	connectBtn    *widget.Button
	searchEntry   *widget.Entry
	offlineChk    *widget.Check
	deviceTree    *widget.Tree
	routeList     *widget.List
	deviceBtns    []*widget.Button
	reloading     bool // a debounced device list reload is scheduled
)

var presets = []string{"SSH (22)", "RDP (3389)", "HTTP (80)", "HTTPS (443)", "Cockpit (9090)", "VNC (5900)"}

// Font Awesome Free icons (CC BY 4.0, attribution in each file), themed so
// they follow the text color in both variants. Arcs in circle-stop,
// magnifying-glass, opensuse, server and ubuntu were converted to cubic
// curves, Fyne's rasterizer fills the wrong side of exact half-circle arcs.
//
//go:embed icons/*.svg
var iconFiles embed.FS

var icons = map[string]fyne.Resource{}

func init() {
	entries, _ := iconFiles.ReadDir("icons")
	for _, e := range entries {
		b, _ := iconFiles.ReadFile("icons/" + e.Name())
		icons[strings.TrimSuffix(e.Name(), ".svg")] = theme.NewThemedResource(fyne.NewStaticResource(e.Name(), b))
	}
}

// appTheme lifts the default colors that are too faint to read: dark
// disabled text is #39393a on #171718 and light disabled is #e3e3e3 on white.
type appTheme struct{}

func (appTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	dark := v == theme.VariantDark
	switch {
	case n == theme.ColorNameDisabled && dark:
		return color.NRGBA{0x74, 0x74, 0x7a, 0xff}
	case n == theme.ColorNameDisabled:
		return color.NRGBA{0x9a, 0x9a, 0x9a, 0xff}
	case n == theme.ColorNameSeparator && dark:
		return color.NRGBA{0x2e, 0x2e, 0x33, 0xff}
	case n == theme.ColorNameForeground && !dark:
		return color.NRGBA{0x1f, 0x1f, 0x1f, 0xff}
	case n == theme.ColorNamePlaceHolder && !dark:
		return color.NRGBA{0x6b, 0x6b, 0x6b, 0xff}
	}
	return theme.DefaultTheme().Color(n, v)
}

func (appTheme) Font(s fyne.TextStyle) fyne.Resource     { return theme.DefaultTheme().Font(s) }
func (appTheme) Icon(n fyne.ThemeIconName) fyne.Resource { return theme.DefaultTheme().Icon(n) }
func (appTheme) Size(n fyne.ThemeSizeName) float32       { return theme.DefaultTheme().Size(n) }

func main() {
	a := app.NewWithID("com.github.lexpaval.mcc-gui")
	a.Settings().SetTheme(appTheme{})
	win = a.NewWindow("MeshCentral Client")
	viper.SetConfigFile(config.DefaultConfigPath)

	meshcentral.TokenPrompt = promptToken
	meshcentral.OnNodeEvent = onNodeEvent
	meshcentral.OnConnectionLost = func(err error) {
		fyne.Do(func() {
			disconnect()
			dialog.ShowError(err, win)
		})
	}

	win.SetContent(buildUI())
	win.Resize(fyne.NewSize(1100, 700))

	// Active-connection counts change without any UI event.
	go func() {
		for range time.Tick(time.Second) {
			fyne.Do(func() {
				if len(routes) > 0 {
					routeList.Refresh()
				}
			})
		}
	}()

	if _, err := os.Stat(config.DefaultConfigPath); errors.Is(err, os.ErrNotExist) {
		showProfileDialog(true, nil)
	} else if err := config.LoadConfig(); err != nil {
		dialog.ShowError(fmt.Errorf("unable to read %s: %w", config.DefaultConfigPath, err), win)
	} else {
		refreshProfiles()
	}

	win.SetOnClosed(func() {
		if connected {
			disconnect()
		}
	})
	win.ShowAndRun()
}

// buildUI creates the widgets into the package state, split from main so the
// screenshot test can render it without a real driver.
func buildUI() fyne.CanvasObject {
	statusLabel = widget.NewLabel("Disconnected")
	statusLabel.Alignment = fyne.TextAlignTrailing
	profileSel = widget.NewSelect(nil, nil)
	insecureChk := widget.NewCheck("Skip TLS verify", nil)
	connectBtn = widget.NewButtonWithIcon("Connect", icons["right-to-bracket"], func() {
		if connected {
			disconnect()
		} else {
			connect(insecureChk.Checked)
		}
	})
	connectBtn.Importance = widget.HighImportance
	addProfileBtn := widget.NewButtonWithIcon("", icons["plus"], func() { showProfileDialog(false, nil) })
	editProfileBtn := widget.NewButtonWithIcon("", icons["pen-to-square"], func() {
		for _, p := range config.GetProfiles() {
			if p.Name == profileSel.Selected {
				showProfileDialog(false, &p)
			}
		}
	})
	rmProfileBtn := widget.NewButtonWithIcon("", icons["trash-can"], func() {
		name := profileSel.Selected
		if name == "" || connected {
			return
		}
		dialog.ShowConfirm("Remove profile", "Remove profile "+name+" and its stored password?", func(ok bool) {
			if ok {
				config.RemoveProfile(name)
				refreshProfiles()
			}
		}, win)
	})
	profileBar = []fyne.CanvasObject{widget.NewLabel("Profile"), profileSel, addProfileBtn, editProfileBtn, rmProfileBtn, insecureChk}
	topBar = container.NewHBox(append(profileBar, connectBtn)...)
	top := container.NewBorder(nil, nil, topBar, nil, statusLabel)

	searchEntry = widget.NewEntry()
	searchEntry.SetPlaceHolder("Search name, hostname, IP or OS")
	searchEntry.ActionItem = widget.NewIcon(icons["magnifying-glass"])
	searchEntry.OnChanged = func(string) { applyFilter() }
	offlineChk = widget.NewCheck("Show offline", func(bool) { applyFilter() })
	deviceTree = widget.NewTree(
		func(id widget.TreeNodeID) []widget.TreeNodeID {
			if id == "" {
				return groupOrder
			}
			return groupChildren[id]
		},
		func(id widget.TreeNodeID) bool {
			_, ok := groupChildren[id]
			return id == "" || ok
		},
		func(branch bool) fyne.CanvasObject {
			if branch {
				text := widget.NewRichText(
					&widget.TextSegment{Style: widget.RichTextStyle{Inline: true, TextStyle: fyne.TextStyle{Bold: true}}},
					&widget.TextSegment{Style: widget.RichTextStyle{Inline: true, SizeName: theme.SizeNameCaptionText, ColorName: theme.ColorNamePlaceHolder}})
				text.Truncation = fyne.TextTruncateEllipsis
				return text
			}
			return twoLineRow()
		},
		func(id widget.TreeNodeID, branch bool, o fyne.CanvasObject) {
			if branch {
				text := o.(*widget.RichText)
				text.Segments[0].(*widget.TextSegment).Text = groupLabels[id][0]
				text.Segments[1].(*widget.TextSegment).Text = "  " + groupLabels[id][1]
				text.Refresh()
				return
			}
			d := shownByID[id]
			img, text, title, sub := rowParts(o)
			title.Text = deviceName(d)
			sub.Text = strings.Join(slices.DeleteFunc([]string{d.Name, d.IP, d.OS}, func(s string) bool { return s == "" }), " · ")
			img.Resource = osIcon(d.OS)
			title.Style.ColorName = theme.ColorNameForeground
			if d.Pwr == 0 {
				title.Style.ColorName = theme.ColorNamePlaceHolder
				img.Resource = theme.NewDisabledResource(img.Resource)
				sub.Text = "offline · " + sub.Text
			}
			img.Refresh()
			text.Refresh()
		})
	deviceTree.OnSelected = func(id widget.TreeNodeID) {
		if _, ok := groupChildren[id]; ok {
			// A group row isn't a device, clicking it folds the group instead.
			deviceTree.Unselect(id)
			deviceTree.ToggleBranch(id)
			return
		}
		selectedID = id
	}
	deviceTree.OnUnselected = func(widget.TreeNodeID) { selectedID = "" }
	deviceBtns = []*widget.Button{
		widget.NewButtonWithIcon("Add route", icons["plus"], showAddRoute),
		widget.NewButtonWithIcon("Run command", icons["play"], showRunCommand),
		widget.NewButtonWithIcon("Copy", icons["copy"], nil),
		widget.NewButtonWithIcon("Refresh", icons["rotate"], refreshDevices),
	}
	copyBtn := deviceBtns[2]
	copyBtn.OnTapped = func() {
		menu := fyne.NewMenu("",
			fyne.NewMenuItem("Node ID", func() {
				if d, ok := selectedDevice(); ok {
					fyne.CurrentApp().Clipboard().SetContent(d.Id)
					logf("Copied node ID of %s", deviceName(d))
				}
			}),
			fyne.NewMenuItem("SSH config…", showSSHConfig))
		widget.ShowPopUpMenuAtRelativePosition(menu, win.Canvas(), fyne.NewPos(0, copyBtn.Size().Height), copyBtn)
	}
	left := container.NewBorder(
		container.NewVBox(searchEntry, offlineChk), container.NewHBox(deviceBtns[0], deviceBtns[1], deviceBtns[2], deviceBtns[3]),
		nil, nil, container.NewThemeOverride(deviceTree, treeTheme))

	routeList = widget.NewList(
		func() int { return len(routes) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(
					widget.NewButtonWithIcon("Open", icons["arrow-up-right-from-square"], nil),
					widget.NewButtonWithIcon("Copy", icons["copy"], nil),
					widget.NewButtonWithIcon("Stop", icons["circle-stop"], nil)),
				twoLineRow())
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			ar := routes[id]
			r := ar.route
			objs := o.(*fyne.Container).Objects
			btns := objs[1].(*fyne.Container).Objects
			open, cp, stop := btns[0].(*widget.Button), btns[1].(*widget.Button), btns[2].(*widget.Button)
			target := r.Target
			if target == "" {
				target = "device"
			}
			svc, res := fmt.Sprintf("Port %d", r.RemotePort), icons["network-wired"]
			switch r.RemotePort {
			case 22:
				svc, res = "SSH", icons["terminal"]
			case 3389:
				svc, res = "RDP", icons["desktop"]
			case 80, 8080:
				svc, res = "HTTP", icons["globe"]
			case 443, 8443:
				svc, res = "HTTPS", icons["globe"]
			case 9090:
				svc, res = "Cockpit", icons["globe"]
			case 5900:
				svc, res = "VNC", icons["display"]
			}
			img, text, title, sub := rowParts(objs[0])
			img.Resource = res
			title.Text = ar.device + " · " + svc
			sub.Text = fmt.Sprintf("%s » %s:%d · %d active", localAddr(r), target, r.RemotePort, r.Active())
			if i := slices.IndexFunc(devices, func(d meshcentral.Device) bool { return d.Id == r.NodeID }); i >= 0 && devices[i].Pwr == 0 {
				img.Resource = theme.NewDisabledResource(res)
				sub.Text += " · device offline"
			}
			img.Refresh()
			text.Refresh()
			if openCmd(r) == nil {
				open.Hide()
			} else {
				open.Show()
				open.OnTapped = func() {
					if err := openCmd(r)(); err != nil {
						dialog.ShowError(err, win)
					}
				}
			}
			cp.OnTapped = func() {
				s := copyText(r)
				fyne.CurrentApp().Clipboard().SetContent(s)
				logf("Copied %q", s)
			}
			stop.OnTapped = func() {
				r.Close()
				routes = slices.DeleteFunc(routes, func(x *activeRoute) bool { return x == ar })
				routeList.Refresh()
				logf("%s: stopped %s", ar.device, localAddr(r))
			}
		})
	right := container.NewBorder(widget.NewLabelWithStyle("Routes", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}),
		nil, nil, nil, container.NewThemeOverride(routeList, compactTheme))

	logLabel = widget.NewLabel("")
	logLabel.Selectable = true
	logLabel.TextStyle.Monospace = true
	logScroll = container.NewVScroll(logLabel)

	hsplit := container.NewHSplit(left, right)
	hsplit.Offset = 0.45
	vsplit := container.NewVSplit(hsplit, logScroll)
	vsplit.Offset = 0.78
	setConnected(false)
	return container.NewBorder(top, nil, nil, nil, vsplit)
}

func connect(insecure bool) {
	name := profileSel.Selected
	if name == "" {
		dialog.ShowInformation("No profile", "Add a profile first.", win)
		return
	}
	connectBtn.Disable()
	profileSel.Disable()
	statusLabel.SetText("Connecting to " + name + "...")
	go func() {
		config.SetDefaultProfile(name, false)
		meshcentral.ApplySettings(insecure, false)
		meshcentral.ApplyAuth("", false, false)
		err := meshcentral.StartSocket()
		var devs []meshcentral.Device
		if err == nil {
			devs = meshcentral.GetDevices()
		}
		fyne.Do(func() {
			// Disabled only while connecting, once connected the profile bar is hidden.
			connectBtn.Enable()
			profileSel.Enable()
			if err != nil {
				statusLabel.SetText("Disconnected")
				dialog.ShowError(err, win)
				return
			}
			setConnected(true)
			p := config.GetDefaultProfile()
			statusLabel.SetText(fmt.Sprintf("Connected to %s as %s (profile %s)", p.Server, p.Username, name))
			logf("Connected with profile %s, %d devices", name, len(devs))
			session.profile, session.insecure = name, insecure
			devices = devs
			applyFilter()
			deviceTree.OpenAllBranches()
		})
	}()
}

func disconnect() {
	for _, ar := range routes {
		ar.route.Close()
	}
	routes = nil
	routeList.Refresh()
	meshcentral.StopSocket()
	devices = nil
	applyFilter()
	setConnected(false)
	statusLabel.SetText("Disconnected")
	logf("Disconnected")
}

func setConnected(c bool) {
	connected = c
	if c {
		connectBtn.SetText("Disconnect")
		connectBtn.SetIcon(icons["right-from-bracket"])
	} else {
		connectBtn.SetText("Connect")
		connectBtn.SetIcon(icons["right-to-bracket"])
	}
	for _, o := range profileBar {
		if c {
			o.Hide()
		} else {
			o.Show()
		}
	}
	topBar.Refresh()
	for _, b := range deviceBtns {
		if c {
			b.Enable()
		} else {
			b.Disable()
		}
	}
}

func refreshDevices() {
	go func() {
		devs := meshcentral.GetDevices()
		fyne.Do(func() {
			devices = devs
			applyFilter()
		})
	}()
}

func refreshProfiles() {
	var names []string
	for _, p := range config.GetProfiles() {
		names = append(names, p.Name)
	}
	profileSel.SetOptions(names)
	if def := config.GetDefaultProfileName(); slices.Contains(names, def) {
		profileSel.SetSelected(def)
	} else if len(names) > 0 {
		profileSel.SetSelected(names[0])
	} else {
		profileSel.ClearSelected()
	}
}

// onNodeEvent runs on the control socket reader, so it only hands off to the
// UI goroutine.
func onNodeEvent(action, nodeID string, conn, pwr int) {
	fyne.Do(func() {
		if action != "nodeconnect" {
			// Adds, removes and renames arrive in bursts (e.g. agents updating
			// their info), reload once they settle.
			if !reloading {
				reloading = true
				time.AfterFunc(2*time.Second, func() {
					fyne.Do(func() { reloading = false })
					refreshDevices()
				})
			}
			return
		}
		i := slices.IndexFunc(devices, func(d meshcentral.Device) bool { return d.Id == nodeID })
		if i < 0 {
			return
		}
		if (devices[i].Pwr == 0) != (pwr == 0) {
			state := "online"
			if pwr == 0 {
				state = "offline"
			}
			logf("%s is %s", deviceName(devices[i]), state)
		}
		devices[i].Conn, devices[i].Pwr = conn, pwr
		applyFilter()
		routeList.Refresh()
	})
}

// applyFilter rebuilds the device tree from devices, keeping the selected
// device selected if it's still shown. Groups with no matching device are
// left out, the rest keep their open/closed state.
func applyFilter() {
	q := strings.ToLower(searchEntry.Text)
	sorted := slices.Clone(devices)
	slices.SortFunc(sorted, func(a, b meshcentral.Device) int {
		return strings.Compare(strings.ToLower(deviceName(a)), strings.ToLower(deviceName(b)))
	})
	groupChildren, shownByID = map[string][]string{}, map[string]meshcentral.Device{}
	online, total := map[string]int{}, map[string]int{}
	names := map[string]string{}
	for _, d := range sorted {
		names[d.MeshID] = cmp.Or(d.Group, "Unnamed group")
		total[d.MeshID]++
		if d.Pwr != 0 {
			online[d.MeshID]++
		}
		if d.Pwr == 0 && !offlineChk.Checked {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(d.DisplayName+" "+d.Name+" "+d.IP+" "+d.OS+" "+d.Group), q) {
			continue
		}
		groupChildren[d.MeshID] = append(groupChildren[d.MeshID], d.Id)
		shownByID[d.Id] = d
	}
	groupOrder = slices.Collect(maps.Keys(groupChildren))
	slices.SortFunc(groupOrder, func(a, b string) int {
		return strings.Compare(strings.ToLower(names[a]), strings.ToLower(names[b]))
	})
	groupLabels = map[string][2]string{}
	for _, id := range groupOrder {
		groupLabels[id] = [2]string{names[id], fmt.Sprintf("%d of %d online", online[id], total[id])}
	}

	sel := selectedID
	deviceTree.UnselectAll()
	if _, ok := shownByID[sel]; ok {
		deviceTree.Select(sel)
	}
	if q != "" {
		deviceTree.OpenAllBranches()
	}
	deviceTree.Refresh()
}

// sizeTheme overrides some sizes for a subtree (container.NewThemeOverride),
// everything else comes from the active theme. Apply one per list, never per
// row: Fyne caches a full set of parsed fonts per override and never frees it.
type sizeTheme map[fyne.ThemeSizeName]float32

// compactTheme tightens RichText padding in the route list, which otherwise
// pads each row as much as a standalone paragraph.
var compactTheme = sizeTheme{theme.SizeNameInnerPadding: 3, theme.SizeNameLineSpacing: 1}

// treeTheme is compactTheme for the device tree, plus a smaller indent, which
// Fyne derives from the inline icon size and padding (the expand arrow too).
var treeTheme = sizeTheme{theme.SizeNameInnerPadding: 3, theme.SizeNameLineSpacing: 1,
	theme.SizeNameInlineIcon: 14, theme.SizeNamePadding: 2}

func (sizeTheme) Color(n fyne.ThemeColorName, v fyne.ThemeVariant) color.Color {
	return fyne.CurrentApp().Settings().Theme().Color(n, v)
}
func (sizeTheme) Font(s fyne.TextStyle) fyne.Resource {
	return fyne.CurrentApp().Settings().Theme().Font(s)
}
func (sizeTheme) Icon(n fyne.ThemeIconName) fyne.Resource {
	return fyne.CurrentApp().Settings().Theme().Icon(n)
}
func (t sizeTheme) Size(n fyne.ThemeSizeName) float32 {
	if v, ok := t[n]; ok {
		return v
	}
	return fyne.CurrentApp().Settings().Theme().Size(n)
}

// twoLineRow is the list item template shared by devices and routes: an
// icon beside a bold title and a smaller muted line, both ellipsized.
func twoLineRow() fyne.CanvasObject {
	img := canvas.NewImageFromResource(nil)
	img.FillMode = canvas.ImageFillContain
	img.SetMinSize(fyne.NewSquareSize(20))
	text := widget.NewRichText(
		&widget.TextSegment{Style: widget.RichTextStyle{TextStyle: fyne.TextStyle{Bold: true}}},
		&widget.TextSegment{Style: widget.RichTextStyle{SizeName: theme.SizeNameCaptionText, ColorName: theme.ColorNamePlaceHolder}})
	text.Truncation = fyne.TextTruncateEllipsis
	return container.NewBorder(nil, nil, container.NewPadded(img), nil, text)
}

func rowParts(o fyne.CanvasObject) (img *canvas.Image, text *widget.RichText, title, sub *widget.TextSegment) {
	objs := o.(*fyne.Container).Objects
	text = objs[0].(*widget.RichText)
	img = objs[1].(*fyne.Container).Objects[0].(*canvas.Image)
	return img, text, text.Segments[0].(*widget.TextSegment), text.Segments[1].(*widget.TextSegment)
}

func osIcon(desc string) fyne.Resource {
	s := strings.ToLower(desc)
	for _, m := range [][2]string{
		{"windows", "windows"}, {"raspbian", "raspberry-pi"}, {"raspberry", "raspberry-pi"},
		{"ubuntu", "ubuntu"}, {"fedora", "fedora"}, {"debian", "debian"}, {"suse", "opensuse"}, {"red hat", "redhat"},
		{"rhel", "redhat"}, {"macos", "apple"}, {"mac os", "apple"}, {"darwin", "apple"}, {"linux", "linux"},
	} {
		if strings.Contains(s, m[0]) {
			return icons[m[1]]
		}
	}
	return icons["server"]
}

func deviceName(d meshcentral.Device) string {
	if d.DisplayName != "" {
		return d.DisplayName
	}
	return d.Name
}

func selectedDevice() (meshcentral.Device, bool) {
	d, ok := shownByID[selectedID]
	if !ok {
		dialog.ShowInformation("No device", "Select a device first.", win)
	}
	return d, ok
}

func showAddRoute() {
	d, ok := selectedDevice()
	if !ok {
		return
	}
	name := deviceName(d)
	remotePort := widget.NewEntry()
	remotePort.SetText("22")
	remotePort.Validator = portValidator(false)
	preset := widget.NewSelect(presets, func(s string) {
		remotePort.SetText(strings.TrimSuffix(s[strings.Index(s, "(")+1:], ")"))
	})
	preset.SetSelected(presets[0])
	target := widget.NewEntry()
	target.SetPlaceHolder("the device itself")
	localPort := widget.NewEntry()
	localPort.SetPlaceHolder("auto")
	localPort.Validator = portValidator(true)
	bind := widget.NewEntry()
	bind.SetPlaceHolder("127.0.0.1")

	dialog.ShowForm("Route to "+name, "Start", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Preset", preset),
		widget.NewFormItem("Remote port", remotePort),
		widget.NewFormItem("Target host", target),
		widget.NewFormItem("Local port", localPort),
		widget.NewFormItem("Bind address", bind),
	}, func(ok bool) {
		if !ok {
			return
		}
		rp, _ := strconv.Atoi(remotePort.Text)
		lp, _ := strconv.Atoi(localPort.Text)
		t := strings.TrimSpace(target.Text)
		if t == "127.0.0.1" { // same as the device itself, matches the CLI
			t = ""
		}
		r := &meshcentral.Route{
			NodeID:      d.Id,
			BindAddress: strings.TrimSpace(bind.Text),
			LocalPort:   lp,
			Target:      t,
			RemotePort:  rp,
			Out:         logWriter(name),
		}
		if err := r.Start(); err != nil {
			dialog.ShowError(err, win)
			return
		}
		routes = append(routes, &activeRoute{route: r, device: name})
		routeList.Refresh()
		logf("%s: listening on %s for port %d", name, localAddr(r), rp)
	}, win)
}

func showRunCommand() {
	d, ok := selectedDevice()
	if !ok {
		return
	}
	cmd := widget.NewEntry()
	cmd.SetPlaceHolder("e.g. systemctl restart myservice")
	cmd.Validator = func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	}
	asUser := widget.NewCheck("Run as the logged-in user instead of SYSTEM/root", nil)
	dialog.ShowForm("Run on "+deviceName(d), "Run", "Cancel", []*widget.FormItem{
		widget.NewFormItem("Command", cmd),
		widget.NewFormItem("", asUser),
	}, func(ok bool) {
		if !ok {
			return
		}
		runAsUser := 0
		if asUser.Checked {
			runAsUser = 1
		}
		if err := meshcentral.RunCommand(d.Id, cmd.Text, runAsUser); err != nil {
			dialog.ShowError(err, win)
			return
		}
		logf("%s: sent %q (no output is returned)", deviceName(d), cmd.Text)
	}, win)
}

// showSSHConfig builds a ~/.ssh/config Host block that tunnels through
// "mcc ssh --proxy", the setup VSCode Remote-SSH and plain ssh use.
func showSSHConfig() {
	d, ok := selectedDevice()
	if !ok {
		return
	}
	host := widget.NewEntry()
	host.SetText(strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, strings.ToLower(cmp.Or(d.Name, deviceName(d)))))
	// Same default as ssh itself: the local user. Windows reports DOMAIN\user.
	user := widget.NewEntry()
	user.SetText("root")
	if u, err := osuser.Current(); err == nil {
		user.SetText(u.Username[strings.LastIndex(u.Username, `\`)+1:])
	}
	port := widget.NewEntry()
	port.SetText("22")
	port.Validator = portValidator(false)
	mcc := widget.NewEntry()
	mcc.SetText("mcc")
	if p, err := exec.LookPath("mcc"); err == nil {
		mcc.SetText(p)
	}
	preview := widget.NewLabel("")
	preview.TextStyle.Monospace = true
	preview.Selectable = true

	snippet := func() string {
		bin := mcc.Text
		if strings.ContainsAny(bin, " \t") {
			bin = `"` + bin + `"`
		}
		// Single quotes keep $ in node IDs away from sh -c, mcc strips them
		// for launchers that pass them through (VSCodium). The profile is only
		// quoted when it has to be, mcc doesn't strip those.
		profile := session.profile
		if strings.Trim(profile, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789._-") != "" {
			profile = "'" + profile + "'"
		}
		cmd := fmt.Sprintf("%s ssh -i '%s' -P %s", bin, d.Id, profile)
		if port.Text != "22" {
			cmd += " -p " + port.Text
		}
		if session.insecure {
			cmd += " -k"
		}
		return fmt.Sprintf("Host %s\n  User %s\n  ProxyCommand %s --proxy\n", host.Text, user.Text, cmd)
	}
	update := func(string) { preview.SetText(snippet()) }
	host.OnChanged, user.OnChanged, port.OnChanged, mcc.OnChanged = update, update, update, update
	update("")

	form := widget.NewForm(
		widget.NewFormItem("Host alias", host),
		widget.NewFormItem("User", user),
		widget.NewFormItem("SSH port", port),
		widget.NewFormItem("mcc path", mcc))
	d2 := dialog.NewCustomConfirm("SSH config for "+deviceName(d), "Copy", "Cancel", container.NewVBox(form, preview), func(ok bool) {
		if ok {
			fyne.CurrentApp().Clipboard().SetContent(snippet())
			logf("Copied SSH config for %s, paste it into ~/.ssh/config", deviceName(d))
		}
	}, win)
	d2.Resize(fyne.NewSize(640, 0))
	d2.Show()
}

// showProfileDialog adds a profile, or edits one when edit is set. The first
// run goes through CreateConfig, which writes the config file and always names
// the profile "default".
func showProfileDialog(firstRun bool, edit *config.Profile) {
	name := widget.NewEntry()
	name.Validator = func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		if slices.Contains(profileSel.Options, s) && (edit == nil || s != edit.Name) {
			return errors.New("already exists")
		}
		return nil
	}
	server := widget.NewEntry()
	server.SetPlaceHolder("mesh.example.com")
	server.Validator = func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	}
	username := widget.NewEntry()
	password := widget.NewPasswordEntry()
	makeDefault := widget.NewCheck("Use as default profile", nil)

	items := []*widget.FormItem{
		widget.NewFormItem("Server", server),
		widget.NewFormItem("Username", username),
		widget.NewFormItem("Password", password),
	}
	title := "Set up MeshCentral server"
	if !firstRun {
		title = "Add profile"
		items = append([]*widget.FormItem{widget.NewFormItem("Name", name)}, items...)
		items = append(items, widget.NewFormItem("", makeDefault))
	}
	if edit != nil {
		title = "Edit profile"
		name.SetText(edit.Name)
		server.SetText(edit.Server)
		username.SetText(edit.Username)
		password.SetPlaceHolder("unchanged")
		makeDefault.SetChecked(config.GetDefaultProfileName() == edit.Name)
	}
	d := dialog.NewForm(title, "Save", "Cancel", items, func(ok bool) {
		if !ok {
			return
		}
		var err error
		if firstRun {
			if err = config.CreateConfig(server.Text, username.Text, password.Text); err == nil {
				err = config.LoadConfig()
			}
		} else if edit != nil {
			p := config.Profile{Name: name.Text, Server: server.Text, Username: username.Text}
			err = config.UpdateProfile(edit.Name, p, password.Text, makeDefault.Checked)
		} else {
			_, err = config.AddProfile(name.Text, makeDefault.Checked, server.Text, username.Text, password.Text)
		}
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		refreshProfiles()
		if !firstRun {
			profileSel.SetSelected(name.Text)
		}
	}, win)
	d.Resize(fyne.NewSize(460, 0))
	d.Show()
}

// promptToken runs on the StartSocket goroutine and blocks it until the user
// answers the dialog.
func promptToken(email2fa, sms2fa, emailSent bool) (string, bool) {
	type answer struct {
		token string
		ok    bool
	}
	ch := make(chan answer, 1)
	fyne.Do(func() {
		msg := "Enter your 2FA token."
		if emailSent {
			msg = "A login token was sent by email. " + msg
		}
		entry := widget.NewPasswordEntry()
		content := container.NewVBox(widget.NewLabel(msg), entry)
		var d *dialog.ConfirmDialog
		// ch holds one answer, so Hide's own cancel callback is dropped.
		reply := func(a answer) {
			select {
			case ch <- a:
			default:
			}
		}
		if email2fa {
			content.Add(widget.NewButton("Send token by email", func() { reply(answer{"email", true}); d.Hide() }))
		}
		if sms2fa {
			content.Add(widget.NewButton("Send token by SMS", func() { reply(answer{"sms", true}); d.Hide() }))
		}
		d = dialog.NewCustomConfirm("Two-factor authentication", "Log in", "Cancel", content, func(ok bool) {
			token := strings.TrimSpace(entry.Text)
			reply(answer{token, ok && token != ""})
		}, win)
		entry.OnSubmitted = func(string) { d.Confirm() }
		d.Show()
		win.Canvas().Focus(entry)
	})
	a := <-ch
	return a.token, a.ok
}

func portValidator(optional bool) func(string) error {
	return func(s string) error {
		if s == "" && optional {
			return nil
		}
		if p, err := strconv.Atoi(s); err != nil || p < 1 || p > 65535 {
			return errors.New("port 1-65535")
		}
		return nil
	}
}

// localAddr is where a local client should connect, which for a wildcard
// bind is loopback.
func localAddr(r *meshcentral.Route) string {
	host := r.BindAddress
	if host == "" || host == "0.0.0.0" {
		host = "127.0.0.1"
	}
	return fmt.Sprintf("%s:%d", host, r.LocalPort)
}

func copyText(r *meshcentral.Route) string {
	addr := localAddr(r)
	host, port, _ := strings.Cut(addr, ":")
	switch r.RemotePort {
	case 22:
		return fmt.Sprintf("ssh -p %s %s", port, host)
	case 80, 8080:
		return "http://" + addr
	case 443, 8443, 9090: // Cockpit serves TLS on 9090
		return "https://" + addr
	}
	return addr
}

// openCmd returns the action for the route's Open button, or nil if the port
// has no known client on this platform.
func openCmd(r *meshcentral.Route) func() error {
	switch r.RemotePort {
	case 80, 8080, 443, 8443, 9090:
		return func() error {
			u, err := url.Parse(copyText(r))
			if err != nil {
				return err
			}
			return fyne.CurrentApp().OpenURL(u)
		}
	case 3389:
		if runtime.GOOS == "windows" {
			return func() error { return exec.Command("mstsc", "/v:"+localAddr(r)).Start() }
		}
	}
	return nil
}

// logWriter tags a route's tunnel output with its device name.
type logWriter string

func (w logWriter) Write(p []byte) (int, error) {
	logf("%s: %s", string(w), strings.TrimSpace(string(p)))
	return len(p), nil
}

// logf is safe from any goroutine, the log view keeps the last 500 lines.
func logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	fyne.Do(func() {
		logLines = append(logLines, line)
		if len(logLines) > 500 {
			logLines = logLines[len(logLines)-500:]
		}
		logLabel.SetText(strings.Join(logLines, "\n"))
		logScroll.ScrollToBottom()
	})
}
