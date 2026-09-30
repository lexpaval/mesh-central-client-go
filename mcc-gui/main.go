// mcc-gui is a MeshRouter-style desktop front end for the same core as the
// mcc CLI: log in with a CLI profile, pick devices, run several port routes.
package main

import (
	"bytes"
	"cmp"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"image/color"
	"io"
	"maps"
	"net/url"
	"os"
	"os/exec"
	osuser "os/user"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/dialog"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"
	"github.com/fyne-io/terminal"
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
	// What connect used, for the SSH config snippet. id changes on every
	// connect and disconnect, so late results from an older session are
	// dropped. loading is set until the first device list arrives.
	session struct {
		profile  string
		insecure bool
		id       int
		loading  bool
	}
	devices    []meshcentral.Device // everything the server returned, sorted by name (setDevices)
	deviceIdx  map[string]int       // device ID -> index in devices
	selectedID string               // selected device, "" if none
	// Tree view of the devices passing the search/offline filter, by group.
	groupOrder    []string            // mesh IDs sorted by group name
	groupChildren map[string][]string // mesh ID -> device IDs sorted by name
	groupLabels   map[string][2]string
	shown         []bool // by device index, in the tree
	routes        []*activeRoute
	logLines      []string
	logList       *widget.List
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
	reloadTimer   *time.Timer // pending debounced device list reload
	// A reload is running, and another was asked for meanwhile. A large
	// server takes seconds to send the list, overlapping loads pile up.
	reloading, reloadAgain bool
	// Right panel tabs: Routes first (not closable), then one per shell.
	tabBar     *fyne.Container   // tab heads
	tabContent *fyne.Container   // tab panes, only the selected one visible
	tabScroll  *container.Scroll // lets the heads overflow, the wheel scrolls it sideways
	tabList    []*tab
	currentTab *tab
)

var presets = []string{"SSH (22)", "RDP (3389)", "HTTP (80)", "HTTPS (443)", "Cockpit (9090)", "VNC (5900)"}

// Font Awesome Free icons (CC BY 4.0, attribution in each file), themed so
// they follow the text color in both variants. Arcs in circle-info,
// circle-stop, magnifying-glass, opensuse, server and ubuntu were converted
// to cubic curves, Fyne's rasterizer fills the wrong side of exact half-circle
// arcs.
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

// desktopSizes pulls Fyne's touch-sized defaults toward a desktop density
// similar to Qt's Fusion style (default in comments).
var desktopSizes = map[fyne.ThemeSizeName]float32{
	theme.SizeNameText:            13, // 14
	theme.SizeNamePadding:         3,  // 4
	theme.SizeNameInnerPadding:    6,  // 8, sets button and input height
	theme.SizeNameInlineIcon:      16, // 20
	theme.SizeNameLineSpacing:     3,  // 4
	theme.SizeNameHeadingText:     20, // 24
	theme.SizeNameSubHeadingText:  16, // 18
	theme.SizeNameScrollBar:       10, // 12
	theme.SizeNameInputRadius:     3,  // 5
	theme.SizeNameButtonRadius:    3,  // 5
	theme.SizeNameSelectionRadius: 2,  // 3
	theme.SizeNameDialogRadius:    6,  // 10
}

func (appTheme) Size(n fyne.ThemeSizeName) float32 {
	if v, ok := desktopSizes[n]; ok {
		return v
	}
	return theme.DefaultTheme().Size(n)
}

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
			fyne.Do(refreshRouteRows)
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
				fyne.CurrentApp().Preferences().RemoveValue(routesKey(name))
				refreshProfiles()
			}
		}, win)
	})
	profileBar = []fyne.CanvasObject{widget.NewLabel("Profile"), profileSel, addProfileBtn, editProfileBtn, rmProfileBtn, insecureChk}
	topBar = container.NewHBox(append(profileBar, connectBtn)...)
	aboutBtn := widget.NewButtonWithIcon("", icons["circle-info"], showAbout)
	top := container.NewBorder(nil, nil, topBar, aboutBtn, statusLabel)

	searchEntry = widget.NewEntry()
	searchEntry.SetPlaceHolder("Search name, hostname, IP or OS")
	searchEntry.ActionItem = widget.NewIcon(icons["magnifying-glass"])
	// Typing opens the groups with a match. Only then, so a group collapsed
	// during a search stays collapsed through updates.
	searchEntry.OnChanged = func(q string) {
		if q == "" {
			applyFilter()
			return
		}
		filterDevices()
		deviceTree.OpenAllBranches()
	}
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
				return newRowText(true)
			}
			return newDeviceRow()
		},
		func(id widget.TreeNodeID, _ bool, o fyne.CanvasObject) {
			treeRows[o] = id
			bindTreeRow(id, o)
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
		widget.NewButtonWithIcon("Shell", icons["terminal"], nil),
		widget.NewButtonWithIcon("Add route", icons["plus"], showAddRoute),
		widget.NewButtonWithIcon("Run command", icons["play"], showRunCommand),
		widget.NewButtonWithIcon("Copy", icons["copy"], nil),
		widget.NewButtonWithIcon("", icons["rotate"], refreshDevices),
	}
	shellBtn := deviceBtns[0]
	shellBtn.OnTapped = func() {
		d, ok := selectedDevice()
		if !ok {
			return
		}
		if !strings.Contains(strings.ToLower(d.OS), "windows") {
			openShell(d, 1)
			return
		}
		menu := fyne.NewMenu("",
			fyne.NewMenuItem("Command prompt", func() { openShell(d, 1) }),
			fyne.NewMenuItem("PowerShell", func() { openShell(d, 6) }))
		widget.ShowPopUpMenuAtRelativePosition(menu, win.Canvas(), fyne.NewPos(0, shellBtn.Size().Height), shellBtn)
	}
	copyBtn := deviceBtns[3]
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
		container.NewVBox(searchEntry, offlineChk), container.NewHBox(deviceBtns[0], deviceBtns[1], deviceBtns[2], deviceBtns[3], deviceBtns[4]),
		nil, nil, deviceTree)

	routeList = widget.NewList(
		func() int { return len(routes) },
		func() fyne.CanvasObject {
			return container.NewBorder(nil, nil, nil,
				container.NewHBox(
					widget.NewButtonWithIcon("Open", icons["arrow-up-right-from-square"], nil),
					widget.NewButtonWithIcon("Copy", icons["copy"], nil),
					widget.NewButtonWithIcon("Stop", icons["circle-stop"], nil)),
				newRowText(false))
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			routeRows[o] = routes[id]
			bindRouteRow(o, routes[id])
		})
	// A hand-made tab strip, Fyne's DocTabs puts a close button on every tab
	// and Routes must not have one.
	routes := &tab{pane: routeList}
	routes.btn = widget.NewButtonWithIcon("Routes", icons["network-wired"], func() { selectTab(routes) })
	routes.head = routes.btn
	tabList = []*tab{routes}
	tabBar = container.NewHBox(routes.head)
	tabContent = container.NewStack(routes.pane)
	tabScroll = nil
	selectTab(routes)
	tabScroll = container.NewHScroll(tabBar)
	right := container.NewBorder(container.NewVBox(tabScroll, widget.NewSeparator()), nil, nil, nil, tabContent)

	logList = widget.NewList(
		func() int { return len(logLines) },
		func() fyne.CanvasObject {
			l := widget.NewLabel("")
			l.Selectable = true
			l.TextStyle.Monospace = true
			l.Truncation = fyne.TextTruncateEllipsis
			return container.New(logRowLayout{}, l)
		},
		func(id widget.ListItemID, o fyne.CanvasObject) {
			l := o.(*fyne.Container).Objects[0].(*widget.Label)
			logRows[l] = id
			l.SetText(logLines[id])
		})
	logList.HideSeparators = true

	hsplit := container.NewHSplit(left, right)
	hsplit.Offset = 0.45
	// The margin the single log label had, so the first and last lines clear the edges.
	pad := theme.Size(theme.SizeNameInnerPadding)
	vsplit := container.NewVSplit(hsplit, container.New(layout.NewCustomPaddedLayout(pad, pad, 0, 0), logList))
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
		// Not through the default profile, that's the CLI's and would be
		// saved with the next profile edit.
		p, _ := config.GetProfile(name)
		meshcentral.ApplySettings(insecure, false)
		meshcentral.ApplyAuth("", false, false)
		err := meshcentral.StartSocketAs(p)
		var id int
		fyne.DoAndWait(func() {
			// Disabled only while connecting, once connected the profile bar is hidden.
			connectBtn.Enable()
			profileSel.Enable()
			if err != nil {
				statusLabel.SetText("Disconnected")
				dialog.ShowError(err, win)
				return
			}
			setConnected(true)
			session.id++
			id = session.id
			session.profile, session.insecure, session.loading = name, insecure, true
			fyne.CurrentApp().Preferences().SetString("lastProfile", name)
			statusLabel.SetText(fmt.Sprintf("Connected to %s as %s (profile %s)", p.Server, p.Username, name))
			logf("Connected to %s as %s (profile %s), loading devices", p.Server, p.Username, name)
			restoreRoutes()
		})
		if err != nil {
			return
		}

		// Large servers take a while to send the list, the session is already
		// up (and reported) meanwhile.
		devs := meshcentral.GetDevices()
		fyne.Do(func() {
			if id != session.id { // disconnected while loading
				return
			}
			session.loading = false
			setDevices(devs)
			deviceTree.OpenAllBranches()
			logf("Loaded %d devices", len(devs))
		})
	}()
}

func disconnect() {
	session.id++
	session.loading = false
	reloading, reloadAgain = false, false
	if reloadTimer != nil {
		reloadTimer.Stop()
		reloadTimer = nil
	}
	for _, ar := range routes {
		ar.route.Close()
	}
	for _, tb := range slices.Clone(tabList[1:]) {
		tb.close()
	}
	routes = nil
	routeList.Refresh()
	meshcentral.StopSocket()
	setDevices(nil)
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

// refreshDevices reloads the list in the background. One load runs at a
// time, a request made during one reloads again once it's done.
func refreshDevices() {
	if session.loading { // the first list is on its way
		return
	}
	if reloading {
		reloadAgain = true
		return
	}
	reloading = true
	id := session.id
	go func() {
		devs := meshcentral.GetDevices()
		fyne.Do(func() {
			if !connected || id != session.id {
				return
			}
			reloading = false
			setDevices(devs)
			if reloadAgain {
				reloadAgain = false
				refreshDevices()
			}
		})
	}()
}

// scheduleReload reloads the list once a burst of changes has settled.
func scheduleReload() {
	if reloadTimer != nil {
		return
	}
	reloadTimer = time.AfterFunc(2*time.Second, func() {
		fyne.Do(func() {
			reloadTimer = nil
			if connected {
				refreshDevices()
			}
		})
	})
}

func refreshProfiles() {
	var names []string
	for _, p := range config.GetProfiles() {
		names = append(names, p.Name)
	}
	profileSel.SetOptions(names)
	// The last one connected to, then the CLI's default.
	for _, n := range []string{fyne.CurrentApp().Preferences().String("lastProfile"), config.GetDefaultProfileName()} {
		if slices.Contains(names, n) {
			profileSel.SetSelected(n)
			return
		}
	}
	if len(names) > 0 {
		profileSel.SetSelected(names[0])
	} else {
		profileSel.ClearSelected()
	}
}

// routeRows maps the route list's rows to the route each shows, so the
// periodic update can redraw them in place. routeList.Refresh builds a
// throwaway template row that Fyne only frees once the window repaints,
// which a minimized window never does.
var routeRows = map[fyne.CanvasObject]*activeRoute{}

// refreshRouteRows redraws the shown routes, for their connection counts and
// device state.
func refreshRouteRows() {
	for o, ar := range routeRows {
		if slices.Contains(routes, ar) {
			bindRouteRow(o, ar)
		} else {
			delete(routeRows, o) // stopped, the list rebinds the row if it reuses it
		}
	}
}

func bindRouteRow(o fyne.CanvasObject, ar *activeRoute) {
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
	text := objs[0].(*rowText)
	text.Title = ar.device + " · " + svc
	text.Sub = fmt.Sprintf("%s » %s:%d · %d active", localAddr(r), target, r.RemotePort, r.Active())
	text.Icon = res
	if i, ok := deviceIdx[r.NodeID]; ok && devices[i].Pwr == 0 {
		text.Icon = disabledIcon(res)
		text.Sub += " · device offline"
	}
	if r.Recorded() {
		text.Sub += " · recorded"
	}
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
		saveRoutes()
		logf("%s: stopped %s", ar.device, localAddr(r))
	}
}

// Node events are queued by the control socket reader and applied together,
// since every tree update walks all devices and a large server sends many
// events per second.
var pendingEvents struct {
	sync.Mutex
	events []meshcentral.NodeEvent
	armed  bool // a flush is scheduled
}

const eventFlushDelay = time.Second

func onNodeEvent(e meshcentral.NodeEvent) {
	pendingEvents.Lock()
	defer pendingEvents.Unlock()
	pendingEvents.events = append(pendingEvents.events, e)
	if !pendingEvents.armed {
		pendingEvents.armed = true
		time.AfterFunc(eventFlushDelay, func() { fyne.Do(flushNodeEvents) })
	}
}

// flushNodeEvents applies the queued events. The tree is only rebuilt when a
// device was added, removed, changed or went on/offline, rows show nothing
// else of the connection state.
func flushNodeEvents() {
	pendingEvents.Lock()
	events := pendingEvents.events
	pendingEvents.events, pendingEvents.armed = nil, false
	pendingEvents.Unlock()

	// Events arriving while the first list loads are already reflected in it,
	// and a reload then would only queue behind the load.
	if len(events) == 0 || !connected || session.loading {
		return
	}
	// resort: names changed or devices were added. refilter: the tree's
	// contents may change, otherwise only rows are redrawn.
	resort, refilter := false, searchEntry.Text != ""
	// Rows that may have changed, devices and their groups ("n of m online").
	touched := map[string]bool{}
	touch := func(d meshcentral.Device) { touched[d.Id], touched[d.MeshID] = true, true }
	for _, e := range events {
		i, known := deviceIdx[e.NodeID]
		switch {
		case e.Action == "nodeconnect":
			if !known {
				continue
			}
			d := &devices[i]
			if (d.Pwr == 0) != (e.Pwr == 0) {
				state := "online"
				if e.Pwr == 0 {
					state = "offline"
				}
				logf("%s is %s", deviceName(*d), state)
				touch(*d)
				refilter = true
			}
			d.Conn, d.Pwr = e.Conn, e.Pwr
		case e.Action == "removenode" && known:
			touch(devices[i])
			devices = slices.Delete(devices, i, i+1)
			reindexDevices()
			refilter = true
		case e.Device != nil: // addnode, changenode
			d := *e.Device
			if known { // the event's state isn't live, nodeconnect keeps it
				touch(devices[i]) // it may have moved group
				d.Conn, d.Pwr = devices[i].Conn, devices[i].Pwr
				resort = resort || deviceName(d) != deviceName(devices[i])
				refilter = refilter || d.MeshID != devices[i].MeshID || d.Group != devices[i].Group
				devices[i] = d
			} else {
				deviceIdx[d.Id] = len(devices)
				devices = append(devices, d)
				resort = true
			}
			touch(d)
		case e.Action != "removenode":
			// Group changes, or a device change without the device, reload
			// once the burst settles.
			scheduleReload()
		}
	}
	if resort {
		sortDevices()
		refilter = true
	}
	if len(touched) == 0 {
		return
	}
	// Fyne's Tree.Refresh builds throwaway template rows that are only freed
	// once the window repaints, which a minimized window never does. One
	// RefreshItem lays out rows that appeared or went away (the canvas fits
	// the scroll extent on its next paint), rows kept are redrawn here.
	if refilter && filterDevices() {
		deviceTree.RefreshItem("")
	}
	if len(treeRows) > 500 { // rows the tree dropped add up, start over
		clear(treeRows)
		deviceTree.Refresh()
	} else {
		for o, id := range treeRows {
			if touched[id] {
				bindTreeRow(id, o)
			}
		}
	}
	refreshRouteRows()
}

// treeRows maps the tree's rows to the device or group each shows, so event
// updates can redraw them directly. Tree.RefreshItem walks all the nodes.
var treeRows = map[fyne.CanvasObject]string{}

func bindTreeRow(id string, o fyne.CanvasObject) {
	if text, ok := o.(*rowText); ok { // a group
		text.Title, text.Sub = groupLabels[id][0], "  "+groupLabels[id][1]
		text.Refresh()
		return
	}
	d, _ := shownDevice(id)
	row := o.(*deviceRow)
	row.id = id
	text := row.text
	text.Title = deviceName(d)
	text.Sub = strings.Join(slices.DeleteFunc([]string{d.Name, d.IP, d.OS}, func(s string) bool { return s == "" }), " · ")
	text.Muted = d.Pwr == 0
	text.Icon = osIcon(d.OS)
	if d.Pwr == 0 {
		text.Icon = disabledIcon(text.Icon)
		text.Sub = "offline · " + text.Sub
	}
	text.Refresh()
}

// setDevices replaces the device list and rebuilds the tree.
func setDevices(devs []meshcentral.Device) {
	devices = devs
	sortDevices()
	applyFilter()
}

// sortDevices sorts devices by name, the order the tree shows them in.
func sortDevices() {
	keys := make(map[string]string, len(devices))
	for _, d := range devices {
		keys[d.Id] = strings.ToLower(deviceName(d))
	}
	slices.SortFunc(devices, func(a, b meshcentral.Device) int {
		return cmp.Or(strings.Compare(keys[a.Id], keys[b.Id]), strings.Compare(a.Id, b.Id))
	})
	reindexDevices()
}

func reindexDevices() {
	deviceIdx = make(map[string]int, len(devices))
	for i, d := range devices {
		deviceIdx[d.Id] = i
	}
}

// shownDevice returns the device with id if the tree shows it.
func shownDevice(id string) (meshcentral.Device, bool) {
	i, ok := deviceIdx[id]
	if !ok || !shown[i] {
		return meshcentral.Device{}, false
	}
	return devices[i], true
}

// applyFilter rebuilds the device tree from devices, keeping the selected
// device selected if it's still shown. Groups with no matching device are
// left out, the rest keep their open/closed state.
func applyFilter() {
	filterDevices()
	deviceTree.Refresh()
}

// groupAcc collects each group's counts and shown devices in filterDevices.
// children has one slice per treeBuf, so the previous shape stays intact for
// the comparison and both keep their storage.
type groupAcc struct {
	name          string
	online, total int
	children      [2][]string
}

var (
	groups  = map[string]*groupAcc{}
	treeBuf int // which children and order filterDevices fills
	orders  [2][]string
	kids    = [2]map[string][]string{{}, {}}
)

// filterDevices recomputes the tree's contents and reports whether its shape
// (the groups or the devices in them) changed.
func filterDevices() (reshaped bool) {
	oldOrder, oldChildren := groupOrder, groupChildren
	treeBuf ^= 1
	children := kids[treeBuf]
	clear(children)
	for _, g := range groups {
		g.online, g.total, g.children[treeBuf] = 0, 0, g.children[treeBuf][:0]
	}
	shown = slices.Grow(shown[:0], len(devices))[:len(devices)]
	clear(shown)

	q := strings.ToLower(searchEntry.Text)
	for i, d := range devices {
		g := groups[d.MeshID]
		if g == nil {
			g = &groupAcc{}
			groups[d.MeshID] = g
		}
		g.name = cmp.Or(d.Group, "Unnamed group")
		g.total++
		if d.Pwr != 0 {
			g.online++
		}
		if d.Pwr == 0 && !offlineChk.Checked {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(d.DisplayName+" "+d.Name+" "+d.IP+" "+d.OS+" "+d.Group), q) {
			continue
		}
		g.children[treeBuf] = append(g.children[treeBuf], d.Id)
		shown[i] = true
	}

	order := orders[treeBuf][:0]
	for id, g := range groups {
		if len(g.children[treeBuf]) > 0 {
			children[id] = g.children[treeBuf]
			order = append(order, id)
		}
	}
	slices.SortFunc(order, func(a, b string) int {
		// Same-named groups keep a stable order, map order changes every pass.
		return cmp.Or(strings.Compare(strings.ToLower(groups[a].name), strings.ToLower(groups[b].name)), strings.Compare(a, b))
	})
	orders[treeBuf] = order
	groupLabels = map[string][2]string{}
	for _, id := range order {
		g := groups[id]
		groupLabels[id] = [2]string{g.name, fmt.Sprintf("%d of %d online", g.online, g.total)}
	}
	groupOrder, groupChildren = order, children

	if _, ok := shownDevice(selectedID); selectedID != "" && !ok {
		deviceTree.UnselectAll()
	}
	return !slices.Equal(oldOrder, groupOrder) || !maps.EqualFunc(oldChildren, groupChildren, slices.Equal)
}

// disabledIcons holds the greyed out icons, rows ask for them on every update.
var disabledIcons = map[fyne.Resource]fyne.Resource{}

func disabledIcon(r fyne.Resource) fyne.Resource {
	d, ok := disabledIcons[r]
	if !ok {
		d = theme.NewDisabledResource(r)
		disabledIcons[r] = d
	}
	return d
}

// deviceRow is a device in the tree. Fyne's tree has no double-click, so the
// row takes taps itself: a tap selects, a second tap on the same row within
// the double-click delay opens a shell. Not fyne.DoubleTappable: the driver
// then holds every tap for the delay and drops both when the second lands on
// another row, which made quick selection unresponsive.
type deviceRow struct {
	widget.BaseWidget
	text *rowText
	id   string
}

var lastTap struct {
	id string
	at time.Time
}

func newDeviceRow() *deviceRow {
	r := &deviceRow{text: newRowText(false)}
	r.ExtendBaseWidget(r)
	return r
}

func (r *deviceRow) CreateRenderer() fyne.WidgetRenderer { return widget.NewSimpleRenderer(r.text) }
func (r *deviceRow) Tapped(*fyne.PointEvent) {
	deviceTree.Select(r.id)
	now := time.Now()
	if r.id != lastTap.id || now.Sub(lastTap.at) > fyne.CurrentApp().Driver().DoubleTapDelay() {
		lastTap.id, lastTap.at = r.id, now
		return
	}
	lastTap.id = "" // a third tap starts over
	if d, ok := shownDevice(r.id); ok {
		openShell(d, 1)
	}
}

// openShell opens a tab with a shell on d, protocol 1 is the device's
// default shell, 6 PowerShell. Closing the tab ends the session.
func openShell(d meshcentral.Device, protocol int) {
	name := deviceName(d)
	t := terminal.New()

	inR, inW := io.Pipe()   // keystrokes, terminal -> device
	outR, outW := io.Pipe() // output, device -> terminal

	// The terminal reports its size on every layout, keep the latest for the
	// session's handshake and nudge it to resend.
	var mu sync.Mutex
	var cols, rows int
	resize := make(chan struct{}, 1)
	configs := make(chan terminal.Config, 1)
	done := make(chan struct{})
	t.AddListener(configs)
	go func() {
		for {
			select {
			case c := <-configs:
				mu.Lock()
				cols, rows = int(c.Columns), int(c.Rows)
				mu.Unlock()
				select {
				case resize <- struct{}{}:
				default:
				}
			case <-done:
				return
			}
		}
	}()
	size := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return cols, rows
	}

	title := name
	if protocol == 6 {
		title += " · PowerShell"
	}
	tb := &tab{pane: t}
	tb.btn = widget.NewButtonWithIcon(title, icons["terminal"], func() { selectTab(tb) })
	recorded := false
	onRecorded := func() {
		fyne.Do(func() {
			if !recorded {
				recorded = true
				tb.btn.SetText(title + " · recorded")
				logf("%s: the server records this shell", name)
			}
		})
	}

	// Shutdown order matters: the terminal only stops reading on EOF (a
	// closed reader makes it retry forever), so closing the tab ends the
	// input, RunShell then closes the output writer, and the reader is only
	// closed once the terminal is done, in case it never started reading.
	go func() {
		t.RunWithConnection(inW, outR)
		outR.Close()
	}()
	go func() {
		err := meshcentral.RunShell(d.Id, protocol, inR, outW, size, resize, onRecorded)
		msg := "\r\n[session closed]\r\n"
		if err != nil {
			msg = fmt.Sprintf("\r\n[%v]\r\n", err)
		}
		outW.Write([]byte(msg))
		outW.Close()
		logf("%s: shell closed", name)
	}()

	closeBtn := widget.NewButtonWithIcon("", icons["xmark"], nil)
	closeBtn.Importance = widget.LowImportance
	tb.head = container.New(layout.NewCustomPaddedHBoxLayout(0), tb.btn, closeBtn)
	tb.close = func() {
		if !slices.Contains(tabList, tb) {
			return
		}
		tabList = slices.DeleteFunc(tabList, func(x *tab) bool { return x == tb })
		tabBar.Remove(tb.head)
		tabContent.Remove(tb.pane)
		if currentTab == tb {
			selectTab(tabList[0])
		}
		t.Close()
		inW.Close() // t.Close only does this once it has connected
		close(done)
	}
	closeBtn.OnTapped = tb.close
	tabList = append(tabList, tb)
	tabBar.Add(tb.head)
	tabContent.Add(tb.pane)
	selectTab(tb)
	win.Canvas().Focus(t)
	logf("%s: shell opened", name)
}

type tab struct {
	btn   *widget.Button
	head  fyne.CanvasObject // btn, plus the close button for shells
	pane  fyne.CanvasObject
	close func() // nil for Routes
}

func selectTab(sel *tab) {
	currentTab = sel
	for _, tb := range tabList {
		tb.btn.Importance = widget.LowImportance
		tb.pane.Hide()
		if tb == sel {
			tb.btn.Importance = widget.MediumImportance
			tb.pane.Show()
		}
		tb.btn.Refresh()
	}
	// Bring the selected head into view when the strip overflows.
	if tabScroll != nil {
		x, w, view := sel.head.Position().X, sel.head.Size().Width, tabScroll.Size().Width
		if x < tabScroll.Offset.X {
			tabScroll.ScrollToOffset(fyne.NewPos(x, 0))
		} else if x+w > tabScroll.Offset.X+view {
			tabScroll.ScrollToOffset(fyne.NewPos(x+w-view, 0))
		}
	}
}

// rowText is the row shared by devices, groups and routes: an icon beside a
// bold title over a smaller muted line (after it when inline), ellipsized.
// RichText pads like a paragraph, making it compact took a ThemeOverride,
// whose rows Fyne leaks on every list or tree Refresh.
type rowText struct {
	widget.BaseWidget
	Title, Sub string
	Icon       fyne.Resource // nil for none
	Muted      bool          // title in the placeholder color, for offline devices
	inline     bool
}

// Padding around and between the lines, RichText's is 6 and 3.
const rowPad, rowLineGap, rowIconSize = 3, 1, 18

func newRowText(inline bool) *rowText {
	t := &rowText{inline: inline}
	t.ExtendBaseWidget(t)
	return t
}

func (t *rowText) CreateRenderer() fyne.WidgetRenderer {
	r := &rowTextRenderer{t: t, icon: canvas.NewImageFromResource(nil), title: canvas.NewText("", nil), sub: canvas.NewText("", nil)}
	r.icon.FillMode = canvas.ImageFillContain
	r.title.TextStyle.Bold = true
	r.Refresh()
	return r
}

type rowTextRenderer struct {
	t          *rowText
	icon       *canvas.Image
	variant    fyne.ThemeVariant // the icon was drawn for
	pad        float32           // around the icon
	title, sub *canvas.Text
}

func (r *rowTextRenderer) Destroy() {}
func (r *rowTextRenderer) Objects() []fyne.CanvasObject {
	return []fyne.CanvasObject{r.icon, r.title, r.sub}
}

func (r *rowTextRenderer) MinSize() fyne.Size {
	h := fyne.MeasureText("M", r.title.TextSize, r.title.TextStyle).Height
	if !r.t.inline {
		h += rowLineGap + fyne.MeasureText("M", r.sub.TextSize, r.sub.TextStyle).Height
	}
	h += 2 * rowPad
	if r.t.Icon != nil {
		h = max(h, rowIconSize+2*r.pad)
	}
	return fyne.NewSize(0, h)
}

// Refresh only touches lines that changed, rows are rebound on every update
// and a Text.Refresh repaints the window even when nothing did.
func (r *rowTextRenderer) Refresh() {
	th, v := r.t.Theme(), fyne.CurrentApp().Settings().ThemeVariant()
	fg, muted := th.Color(theme.ColorNameForeground, v), th.Color(theme.ColorNamePlaceHolder, v)
	if r.t.Muted {
		fg = muted
	}
	setStyle(r.title, th.Size(theme.SizeNameText), fg)
	setStyle(r.sub, th.Size(theme.SizeNameCaptionText), muted)
	// The SVG is colored for the theme, a pooled row missed the theme change.
	if r.icon.Resource != r.t.Icon || r.variant != v {
		r.icon.Resource, r.variant = r.t.Icon, v
		r.icon.Refresh()
	}
	r.pad = th.Size(theme.SizeNamePadding)
	r.Layout(r.t.Size())
}

func setStyle(t *canvas.Text, size float32, c color.Color) {
	if t.TextSize != size || t.Color != c {
		t.TextSize, t.Color = size, c
		t.Refresh()
	}
}

// setText sets a line's text, redrawing it only if that changed it.
func setText(t *canvas.Text, s string) {
	if t.Text != s {
		t.Text = s
		t.Refresh()
	}
}

func (r *rowTextRenderer) Layout(s fyne.Size) {
	x := float32(rowPad)
	if r.t.Icon != nil {
		r.icon.Move(fyne.NewPos(r.pad, (s.Height-rowIconSize)/2))
		r.icon.Resize(fyne.NewSquareSize(rowIconSize))
		x += rowIconSize + 3*r.pad // padded icon, then a gap
	}
	w := s.Width - x - rowPad
	setText(r.title, ellipsize(r.t.Title, w, r.title.TextSize, r.title.TextStyle))
	ts := fyne.MeasureText(r.title.Text, r.title.TextSize, r.title.TextStyle)
	r.title.Move(fyne.NewPos(x, rowPad))
	r.title.Resize(ts)
	pos := fyne.NewPos(x, rowPad+ts.Height+rowLineGap)
	if r.t.inline {
		w -= ts.Width
	}
	setText(r.sub, ellipsize(r.t.Sub, w, r.sub.TextSize, r.sub.TextStyle))
	ss := fyne.MeasureText(r.sub.Text, r.sub.TextSize, r.sub.TextStyle)
	if r.t.inline { // bottoms aligned, close to a shared baseline
		pos = fyne.NewPos(x+ts.Width, rowPad+ts.Height-ss.Height)
	}
	r.sub.Move(pos)
	r.sub.Resize(ss)
}

// ellipsize shortens s to fit width, ending it with "…".
func ellipsize(s string, width, size float32, style fyne.TextStyle) string {
	if fyne.MeasureText(s, size, style).Width <= width {
		return s
	}
	runes := []rune(s)
	lo, hi := 0, len(runes) // longest prefix that fits with the ellipsis
	for lo < hi {
		m := (lo + hi + 1) / 2
		if fyne.MeasureText(string(runes[:m])+"…", size, style).Width <= width {
			lo = m
		} else {
			hi = m - 1
		}
	}
	return string(runes[:lo]) + "…"
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
	d, ok := shownDevice(selectedID)
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
		err := startRoute(name, &meshcentral.Route{
			NodeID:      d.Id,
			BindAddress: strings.TrimSpace(bind.Text),
			LocalPort:   lp,
			Target:      t,
			RemotePort:  rp,
		})
		if err != nil {
			dialog.ShowError(err, win)
			return
		}
		saveRoutes()
	}, win)
}

func startRoute(device string, r *meshcentral.Route) error {
	r.Out = logWriter(device)
	if err := r.Start(); err != nil {
		return err
	}
	routes = append(routes, &activeRoute{route: r, device: device})
	routeList.Refresh()
	logf("%s: listening on %s for port %d", device, localAddr(r), r.RemotePort)
	return nil
}

// savedRoute is a route remembered per profile, reopened on the next connect.
// LocalPort is the one it got, so clients pointed at it keep working.
type savedRoute struct {
	Device, NodeID, BindAddress, Target string
	LocalPort, RemotePort               int
}

func routesKey(profile string) string { return "routes/" + profile }

// saveRoutes remembers the running routes. Adding and stopping one saves,
// disconnecting doesn't, those are the routes to reopen.
func saveRoutes() {
	var saved []savedRoute
	for _, ar := range routes {
		r := ar.route
		saved = append(saved, savedRoute{ar.device, r.NodeID, r.BindAddress, r.Target, r.LocalPort, r.RemotePort})
	}
	prefs := fyne.CurrentApp().Preferences()
	if len(saved) == 0 {
		prefs.RemoveValue(routesKey(session.profile))
		return
	}
	b, _ := json.Marshal(saved)
	prefs.SetString(routesKey(session.profile), string(b))
}

// restoreRoutes reopens the routes running at the last disconnect, one that
// can't bind its port any more is logged and forgotten.
func restoreRoutes() {
	var saved []savedRoute
	if json.Unmarshal([]byte(fyne.CurrentApp().Preferences().String(routesKey(session.profile))), &saved) != nil {
		return
	}
	for _, sr := range saved {
		err := startRoute(sr.Device, &meshcentral.Route{NodeID: sr.NodeID, BindAddress: sr.BindAddress, LocalPort: sr.LocalPort, Target: sr.Target, RemotePort: sr.RemotePort})
		if err != nil {
			logf("%s: route for port %d not reopened, %v", sr.Device, sr.RemotePort, err)
		}
	}
	if len(routes) != len(saved) {
		saveRoutes()
	}
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
	host.SetText(sshAlias(d))
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

func showAbout() {
	repo, _ := url.Parse("https://github.com/alexpaval/mesh-central-client-go")
	title := widget.NewLabelWithStyle("MeshCentral Client", fyne.TextAlignLeading, fyne.TextStyle{Bold: true})
	title.SizeName = theme.SizeNameSubHeadingText
	// Build details come as fyne package metadata from the Makefile, plain go
	// builds have none.
	meta := fyne.CurrentApp().Metadata().Custom
	info := widget.NewLabel(fmt.Sprintf("Version %s\nCommit %s, built %s\n%s, Fyne GUI",
		cmp.Or(meta["version"], "dev"), cmp.Or(meta["commit"], "unknown"), cmp.Or(meta["buildDate"], "unknown"), runtime.Version()))
	info.Selectable = true
	desc := widget.NewLabel("Desktop client for MeshCentral: devices, port routes and remote commands,\nsharing profiles and 2FA login with the mcc CLI. Not affiliated with MeshCentral.")
	credits := widget.NewLabel("MIT License. Icons by Font Awesome Free (CC BY 4.0), UI by Fyne (BSD-3).")
	credits.Importance = widget.LowImportance
	dialog.ShowCustom("About", "Close",
		container.NewVBox(title, info, desc, widget.NewHyperlink("github.com/alexpaval/mesh-central-client-go", repo), credits), win)
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
			if err = config.UpdateProfile(edit.Name, p, password.Text, makeDefault.Checked); err == nil && p.Name != edit.Name {
				// The GUI's own memory of the profile follows a rename.
				prefs := fyne.CurrentApp().Preferences()
				prefs.SetString(routesKey(p.Name), prefs.String(routesKey(edit.Name)))
				prefs.RemoveValue(routesKey(edit.Name))
				if prefs.String("lastProfile") == edit.Name {
					prefs.SetString("lastProfile", p.Name)
				}
			}
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

// sshAlias names a device for ssh, the SSH config snippet's Host alias.
func sshAlias(d meshcentral.Device) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '.' {
			return r
		}
		return '-'
	}, strings.ToLower(cmp.Or(d.Name, deviceName(d))))
}

// sshArgs is the ssh command for an SSH route. The host key is stored under
// the device, like with the SSH config snippet, not under 127.0.0.1:port,
// which later routes to other devices reuse.
func sshArgs(r *meshcentral.Route) []string {
	host, port, _ := strings.Cut(localAddr(r), ":")
	args := []string{"ssh", "-p", port}
	if i, ok := deviceIdx[r.NodeID]; ok {
		args = append(args, "-o", "HostKeyAlias="+sshAlias(devices[i]))
	}
	return append(args, host)
}

// terminals are tried in order on Linux, with the flag that runs a command.
var terminals = [][]string{
	{"xdg-terminal-exec"}, {"x-terminal-emulator", "-e"}, {"ptyxis", "--"}, {"gnome-terminal", "--"},
	{"konsole", "-e"}, {"xfce4-terminal", "-x"}, {"alacritty", "-e"}, {"kitty"}, {"foot"}, {"xterm", "-e"},
}

// openTerminal runs args in a new terminal window.
func openTerminal(args []string) error {
	switch runtime.GOOS {
	case "windows": // a console program started from a GUI gets its own window
		return exec.Command(args[0], args[1:]...).Start()
	case "darwin": // the args are plain words, no quoting needed
		script := fmt.Sprintf(`tell application "Terminal" to do script "%s"`, strings.Join(args, " "))
		return exec.Command("osascript", "-e", script, "-e", `tell application "Terminal" to activate`).Start()
	}
	for _, t := range terminals {
		if _, err := exec.LookPath(t[0]); err == nil {
			return exec.Command(t[0], append(t[1:], args...)...).Start()
		}
	}
	return errors.New("no terminal emulator found, use Copy for the ssh command")
}

func copyText(r *meshcentral.Route) string {
	addr := localAddr(r)
	switch r.RemotePort {
	case 22:
		return strings.Join(sshArgs(r), " ")
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
	case 22:
		return func() error { return openTerminal(sshArgs(r)) }
	case 80, 8080, 443, 8443, 9090:
		return func() error {
			u, err := url.Parse(copyText(r))
			if err != nil {
				return err
			}
			return fyne.CurrentApp().OpenURL(u)
		}
	case 3389:
		if runtime.GOOS == "windows" { // mstsc doesn't register rdp://
			return func() error { return exec.Command("mstsc", "/v:"+localAddr(r)).Start() }
		}
		return func() error {
			if runtime.GOOS == "linux" { // xdg-open fails silently without a handler
				if out, err := exec.Command("xdg-mime", "query", "default", "x-scheme-handler/rdp").Output(); err == nil && len(bytes.TrimSpace(out)) == 0 {
					return errors.New("no application opens rdp:// links, install an RDP client such as Remmina")
				}
			}
			return fyne.CurrentApp().OpenURL(rdpURL(r, runtime.GOOS))
		}
	}
	return nil
}

// rdpURL is the rdp:// link for a route: Microsoft's clients on macOS take
// their own form, the Linux ones (Remmina, KRDC, GNOME Connections) host:port.
func rdpURL(r *meshcentral.Route, goos string) *url.URL {
	if goos == "darwin" { // url.Parse rejects the %20 in the key
		return &url.URL{Scheme: "rdp", Opaque: "//full%20address=s:" + localAddr(r)}
	}
	return &url.URL{Scheme: "rdp", Host: localAddr(r)}
}

// logWriter tags a route's tunnel output with its device name.
type logWriter string

func (w logWriter) Write(p []byte) (int, error) {
	logf("%s: %s", string(w), strings.TrimSpace(string(p)))
	return len(p), nil
}

// Lines logged since the log view was last updated, a burst (devices going
// on/offline) updates it once.
var pendingLog struct {
	sync.Mutex
	lines []string
}

const logMax = 500

// logf is safe from any goroutine, the log view keeps the last logMax lines.
func logf(format string, args ...any) {
	line := time.Now().Format("15:04:05 ") + fmt.Sprintf(format, args...)
	pendingLog.Lock()
	pendingLog.lines = append(pendingLog.lines, line)
	first := len(pendingLog.lines) == 1
	pendingLog.Unlock()
	if first {
		fyne.Do(flushLog)
	}
}

func flushLog() {
	pendingLog.Lock()
	lines := pendingLog.lines
	pendingLog.lines = nil
	pendingLog.Unlock()
	grew := len(logLines) < logMax
	logLines = append(logLines, lines...)
	if len(logLines) > logMax {
		logLines = slices.Clone(logLines[len(logLines)-logMax:])
	}
	// List.Refresh builds a throwaway template row that Fyne frees only once
	// the window repaints, so it's only used while the log grows. Once full,
	// every line moves up one row and the shown rows are relabeled in place.
	// Rows the list dropped stay in logRows, it's reset once that adds up.
	if grew || len(logRows) > 100 {
		clear(logRows)
		logList.Refresh()
	} else {
		for l, id := range logRows {
			l.SetText(logLines[id])
		}
	}
	logList.ScrollToBottom()
}

// logRows maps the log's rows to the line each shows.
var logRows = map[*widget.Label]widget.ListItemID{}

// logRowLayout spaces log lines like the lines of one label: a Label pads
// itself like a standalone widget and the list adds padding between rows.
// The label's padding overlaps the neighbouring rows, it holds no text.
type logRowLayout struct{}

func (logRowLayout) MinSize(objs []fyne.CanvasObject) fyne.Size {
	th := fyne.CurrentApp().Settings().Theme()
	h := fyne.MeasureText("M", th.Size(theme.SizeNameText), objs[0].(*widget.Label).TextStyle).Height
	return fyne.NewSize(objs[0].MinSize().Width, h-th.Size(theme.SizeNamePadding))
}

// Layout puts the bottom of the text on the row's, so the overlap is the
// headroom above the line and the last row isn't cut off.
func (logRowLayout) Layout(objs []fyne.CanvasObject, s fyne.Size) {
	h := objs[0].MinSize().Height
	pad := fyne.CurrentApp().Settings().Theme().Size(theme.SizeNameInnerPadding)
	objs[0].Move(fyne.NewPos(0, s.Height-h+pad))
	objs[0].Resize(fyne.NewSize(s.Width, h))
}
