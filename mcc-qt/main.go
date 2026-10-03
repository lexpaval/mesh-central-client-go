// mcc-qt is the Qt (miqt) desktop front end for the same core as the mcc
// CLI: log in with a CLI profile, pick devices, run several port routes and
// shells. It replaces the Fyne GUI in mcc-gui once it's on par.
package main

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"os/exec"
	osuser "os/user"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
	"github.com/spf13/viper"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// Build details, set by the Makefile through -ldflags -X.
var version, commit, buildDate string

const appID = "com.github.lexpaval.mcc-gui"

type activeRoute struct {
	route  *meshcentral.Route
	device string
}

// UI state, only touched from the Qt thread (background work hands results
// back through mainthread.Start).
var (
	win       *qt.QMainWindow
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
	logView       *qt.QPlainTextEdit
	statusLabel   *qt.QLabel
	profileSel    *qt.QComboBox
	profileBar    *qt.QWidget // hidden while connected, the status shows the server
	connectBtn    *qt.QPushButton
	searchEntry   *qt.QLineEdit
	offlineChk    *qt.QCheckBox
	deviceTree    *qt.QTreeWidget
	treeItems     = map[string]*qt.QTreeWidgetItem{} // group and device rows by ID
	collapsed     = map[string]bool{}                // groups the user closed
	treeOrder     []string
	treeChildren  = map[string][]string{}
	treeDirty     bool
	treeBuilding  bool // selection signals from a rebuild are ignored
	routeBox      *qt.QVBoxLayout
	routeHint     *qt.QLabel
	routeRows     []*routeRow
	deviceBtns    []*qt.QPushButton
	reloadTimer   *time.Timer // pending debounced device list reload
	// A reload is running, and another was asked for meanwhile. A large
	// server takes seconds to send the list, overlapping loads pile up.
	reloading, reloadAgain bool
	// Right panel tabs: Routes first (not closable), then one per shell.
	tabs   *qt.QTabWidget
	shells []*shellTab
	// Icon setters of the main window's widgets, rerun when the palette
	// changes (dark mode switched).
	iconSetters []func()
)

var presets = []string{"SSH (22)", "RDP (3389)", "HTTP (80)", "HTTPS (443)", "Cockpit (9090)", "VNC (5900)"}

func main() {
	qt.NewQApplication(os.Args)
	qt.QGuiApplication_SetDesktopFileName(appID)
	qt.QGuiApplication_SetWindowIcon(appIcon())
	viper.SetConfigFile(config.DefaultConfigPath)
	loadPrefs(filepath.Join(filepath.Dir(config.DefaultConfigPath), "mcc-gui.json"))

	meshcentral.TokenPrompt = promptToken
	meshcentral.OnNodeEvent = onNodeEvent
	meshcentral.OnConnectionLost = func(err error) {
		mainthread.Start(func() {
			disconnect()
			showError(err)
		})
	}

	win = qt.NewQMainWindow2()
	win.SetWindowTitle("MeshCentral Client")
	win.SetCentralWidget(buildUI())
	win.Resize(1100, 700)
	win.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		if connected {
			disconnect()
		}
		super(e)
	})
	win.OnShowEvent(func(super func(*qt.QShowEvent), e *qt.QShowEvent) {
		super(e)
		if treeDirty {
			rebuildTree()
		}
		refreshRouteRows()
	})
	win.OnChangeEvent(func(super func(*qt.QEvent), e *qt.QEvent) {
		super(e)
		if e.Type() == qt.QEvent__PaletteChange {
			resetIcons()
			for _, set := range iconSetters {
				set()
			}
			rebuildRoutes()
		}
	})

	// Active-connection counts change without any UI event.
	timer := qt.NewQTimer2(win.QObject)
	timer.OnTimeout(refreshRouteRows)
	timer.Start(1000)

	win.Show()
	if _, err := os.Stat(config.DefaultConfigPath); errors.Is(err, os.ErrNotExist) {
		showProfileDialog(true, nil)
	} else if err := config.LoadConfig(); err != nil {
		showError(fmt.Errorf("unable to read %s: %w", config.DefaultConfigPath, err))
	} else {
		refreshProfiles()
		benchStart()
	}
	qt.QApplication_Exec()
}

// themed runs set now and again after a palette change, for icons of
// widgets that live as long as the window.
func themed(set func()) {
	set()
	iconSetters = append(iconSetters, set)
}

func iconButton(text, iconName, tip string) *qt.QPushButton {
	b := qt.NewQPushButton3(text)
	themed(func() { b.SetIcon(icon(iconName)) })
	if tip != "" {
		b.SetToolTip(tip)
	}
	return b
}

func hbox(margins bool, ws ...*qt.QWidget) *qt.QHBoxLayout {
	l := qt.NewQHBoxLayout2()
	if !margins {
		l.SetContentsMargins(0, 0, 0, 0)
	}
	for _, w := range ws {
		l.AddWidget(w)
	}
	return l
}

// buildUI creates the widgets into the package state, split from main so
// tests can build it without a window.
func buildUI() *qt.QWidget {
	iconSetters = nil
	clear(treeItems)
	treeOrder = nil
	clear(treeChildren)
	treeDirty = false
	clear(collapsed)
	routeRows, shells = nil, nil
	root := qt.NewQWidget2()

	statusLabel = newElidedLabel("Disconnected")
	statusLabel.SetAlignment(qt.AlignRight | qt.AlignVCenter)
	profileSel = qt.NewQComboBox2()
	profileSel.SetSizeAdjustPolicy(qt.QComboBox__AdjustToContents)
	profileSel.SetMinimumContentsLength(12)
	insecureChk := qt.NewQCheckBox3("Skip TLS verify")
	connectBtn = qt.NewQPushButton3("Connect")
	connectBtn.SetDefault(true)
	themed(func() {
		if connected {
			connectBtn.SetIcon(icon("right-from-bracket"))
		} else {
			connectBtn.SetIcon(icon("right-to-bracket"))
		}
	})
	connectBtn.OnClicked(func() {
		if connected {
			disconnect()
		} else {
			connect(insecureChk.IsChecked())
		}
	})
	addProfileBtn := iconButton("", "plus", "Add profile")
	addProfileBtn.OnClicked(func() { showProfileDialog(false, nil) })
	editProfileBtn := iconButton("", "pen-to-square", "Edit profile")
	editProfileBtn.OnClicked(func() {
		for _, p := range config.GetProfiles() {
			if p.Name == profileSel.CurrentText() {
				showProfileDialog(false, &p)
			}
		}
	})
	rmProfileBtn := iconButton("", "trash-can", "Remove profile")
	rmProfileBtn.OnClicked(func() {
		name := profileSel.CurrentText()
		if name == "" || connected {
			return
		}
		confirm("Remove profile", "Remove profile "+name+" and its stored password?", func() {
			config.RemoveProfile(name)
			setPref(routesKey(name), "")
			refreshProfiles()
		})
	})
	profileBar = qt.NewQWidget2()
	profileBar.SetLayout(hbox(false, qt.NewQLabel3("Profile").QWidget, profileSel.QWidget, addProfileBtn.QWidget,
		editProfileBtn.QWidget, rmProfileBtn.QWidget, insecureChk.QWidget).QLayout)
	aboutBtn := iconButton("", "circle-info", "About")
	aboutBtn.SetFlat(true)
	aboutBtn.OnClicked(showAbout)
	top := hbox(false, profileBar, connectBtn.QWidget)
	top.AddWidget2(statusLabel.QWidget, 1)
	top.AddWidget(aboutBtn.QWidget)

	searchEntry = qt.NewQLineEdit2()
	searchEntry.SetPlaceholderText("Search name, hostname, IP or OS")
	searchEntry.SetClearButtonEnabled(true)
	searchAction := searchEntry.AddAction2(icon("magnifying-glass"), qt.QLineEdit__LeadingPosition)
	themed(func() { searchAction.SetIcon(icon("magnifying-glass")) })
	// Typing opens the groups with a match. Only then, so a group collapsed
	// during a search stays collapsed through updates.
	searchEntry.OnTextChanged(func(q string) {
		if q == "" {
			applyFilter()
			return
		}
		filterDevices()
		clear(collapsed)
		rebuildTree()
	})
	offlineChk = qt.NewQCheckBox3("Show offline")
	offlineChk.OnToggled(func(bool) { applyFilter() })
	buildTree()

	deviceBtns = []*qt.QPushButton{
		iconButton("Shell", "terminal", "Open a shell on the device"),
		iconButton("Add route", "plus", "Forward a local port to the device"),
		iconButton("Run command", "play", "Run a command on the device"),
		iconButton("Copy", "copy", ""),
		iconButton("", "rotate", "Reload the device list"),
	}
	shellBtn, copyBtn := deviceBtns[0], deviceBtns[3]
	shellBtn.OnClicked(func() {
		if d, ok := selectedDevice(); ok {
			if menu := shellMenu(d); menu != nil {
				popupBelow(menu, shellBtn.QWidget)
			} else {
				openShell(d, 1)
			}
		}
	})
	deviceBtns[1].OnClicked(showAddRoute)
	deviceBtns[2].OnClicked(showRunCommand)
	copyBtn.OnClicked(func() {
		m := qt.NewQMenu(copyBtn.QWidget)
		m.SetAttribute(qt.WA_DeleteOnClose)
		m.AddActionWithText("Node ID").OnTriggered(copyNodeID)
		m.AddActionWithText("SSH config…").OnTriggered(showSSHConfig)
		popupBelow(m, copyBtn.QWidget)
	})
	deviceBtns[4].OnClicked(refreshDevices)
	btnRow := hbox(false)
	for _, b := range deviceBtns {
		btnRow.AddWidget(b.QWidget)
	}
	btnRow.AddStretch()

	left := qt.NewQWidget2()
	ll := qt.NewQVBoxLayout(left)
	ll.SetContentsMargins(0, 0, 0, 0)
	ll.AddWidget(searchEntry.QWidget)
	ll.AddWidget(offlineChk.QWidget)
	ll.AddWidget2(deviceTree.QWidget, 1)
	ll.AddLayout(btnRow.QLayout)

	// The Routes tab, a column of route rows.
	routeHint = qt.NewQLabel3("No routes. Select a device and add one.")
	routeHint.SetForegroundRole(qt.QPalette__PlaceholderText)
	routeHint.SetAlignment(qt.AlignCenter)
	routePane := qt.NewQWidget2()
	routeBox = qt.NewQVBoxLayout(routePane)
	routeBox.AddWidget(routeHint.QWidget)
	routeBox.AddStretch()
	scroll := qt.NewQScrollArea2()
	scroll.SetWidgetResizable(true)
	scroll.SetWidget(routePane)
	scroll.SetFrameShape(qt.QFrame__NoFrame)
	tabs = qt.NewQTabWidget2()
	tabs.SetDocumentMode(true)
	tabs.SetTabsClosable(true)
	tabs.SetUsesScrollButtons(true)
	tabs.SetElideMode(qt.ElideRight)
	tabs.AddTab2(scroll.QWidget, icon("network-wired"), "Routes")
	themed(func() {
		tabs.SetTabIcon(0, icon("network-wired"))
		for _, st := range shells {
			tabs.SetTabIcon(tabs.IndexOf(st.t.w), icon("terminal"))
		}
	})
	tabs.TabBar().SetTabButton(0, qt.QTabBar__RightSide, nil)
	tabs.TabBar().SetTabButton(0, qt.QTabBar__LeftSide, nil)
	tabs.OnTabCloseRequested(func(i int) {
		if st := shellAt(i); st != nil {
			closeShell(st)
		}
	})

	logView = qt.NewQPlainTextEdit2()
	logView.SetReadOnly(true)
	logView.SetUndoRedoEnabled(false)
	logView.SetMaximumBlockCount(logMax)
	logView.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	logView.SetFont(qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont))
	for _, l := range logLines {
		logView.AppendPlainText(l)
	}

	hsplit := qt.NewQSplitter3(qt.Horizontal)
	hsplit.AddWidget(left)
	hsplit.AddWidget(tabs.QWidget)
	hsplit.SetStretchFactor(1, 1)
	hsplit.SetSizes([]int{450, 650})
	hsplit.SetChildrenCollapsible(false)
	vsplit := qt.NewQSplitter3(qt.Vertical)
	vsplit.AddWidget(hsplit.QWidget)
	vsplit.AddWidget(logView.QWidget)
	vsplit.SetStretchFactor(0, 1)
	vsplit.SetSizes([]int{540, 140})
	vsplit.SetChildrenCollapsible(false)

	rl := qt.NewQVBoxLayout(root)
	rl.AddLayout(top.QLayout)
	rl.AddWidget2(vsplit.QWidget, 1)
	setConnected(false)
	return root
}

// buildTree creates the device tree. Rows only carry their device or group
// ID, the delegate draws them from the device list, so updates that keep the
// tree's shape only repaint it.
func buildTree() {
	deviceTree = qt.NewQTreeWidget2()
	deviceTree.SetHeaderHidden(true)
	deviceTree.SetExpandsOnDoubleClick(false)
	deviceTree.SetAnimated(false)
	deviceTree.SetSelectionMode(qt.QAbstractItemView__SingleSelection)
	deviceTree.SetContextMenuPolicy(qt.CustomContextMenu)
	delegate := qt.NewQStyledItemDelegate2(deviceTree.QObject)
	deviceSize, groupSize := qt.NewQSize2(0, rowHeight(false)), qt.NewQSize2(0, rowHeight(true))
	delegate.OnSizeHint(func(_ func(*qt.QStyleOptionViewItem, *qt.QModelIndex) *qt.QSize, _ *qt.QStyleOptionViewItem, idx *qt.QModelIndex) *qt.QSize {
		if _, ok := groupChildren[itemID(idx)]; ok {
			return groupSize
		}
		return deviceSize
	})
	delegate.OnPaint(func(super func(*qt.QPainter, *qt.QStyleOptionViewItem, *qt.QModelIndex), p *qt.QPainter, opt *qt.QStyleOptionViewItem, idx *qt.QModelIndex) {
		super(p, opt, idx) // the background and selection, rows have no text of their own
		paintRow(p, opt.Rect(), opt.Palette(), opt.State()&qt.QStyle__State_Selected != 0, treeRowData(itemID(idx)))
	})
	deviceTree.SetItemDelegate(delegate.QAbstractItemDelegate)

	deviceTree.OnItemSelectionChanged(func() {
		if treeBuilding {
			return
		}
		selectedID = ""
		if sel := deviceTree.SelectedItems(); len(sel) > 0 {
			selectedID = sel[0].Data(0, int(qt.UserRole)).ToString()
		}
	})
	// A group row isn't a device, clicking it folds the group instead.
	deviceTree.OnItemClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if _, ok := groupChildren[item.Data(0, int(qt.UserRole)).ToString()]; ok {
			item.SetExpanded(!item.IsExpanded())
		}
	})
	deviceTree.OnItemDoubleClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if d, ok := shownDevice(item.Data(0, int(qt.UserRole)).ToString()); ok {
			openShell(d, 1)
		}
	})
	deviceTree.OnItemExpanded(func(item *qt.QTreeWidgetItem) {
		if !treeBuilding {
			delete(collapsed, item.Data(0, int(qt.UserRole)).ToString())
		}
	})
	deviceTree.OnItemCollapsed(func(item *qt.QTreeWidgetItem) {
		if !treeBuilding {
			collapsed[item.Data(0, int(qt.UserRole)).ToString()] = true
		}
	})
	deviceTree.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		if k := e.Key(); (k == int(qt.Key_Return) || k == int(qt.Key_Enter)) && connected {
			if d, ok := shownDevice(selectedID); ok {
				openShell(d, 1)
				return
			}
		}
		super(e)
	})
	deviceTree.OnCustomContextMenuRequested(func(pos *qt.QPoint) {
		item := deviceTree.ItemAt(pos)
		if item == nil || !connected {
			return
		}
		d, ok := shownDevice(item.Data(0, int(qt.UserRole)).ToString())
		if !ok {
			return
		}
		deviceTree.SetCurrentItem(item)
		m := qt.NewQMenu(deviceTree.QWidget)
		m.SetAttribute(qt.WA_DeleteOnClose)
		if strings.Contains(strings.ToLower(d.OS), "windows") {
			m.AddAction2(icon("terminal"), "Command prompt").OnTriggered(func() { openShell(d, 1) })
			m.AddAction2(icon("terminal"), "PowerShell").OnTriggered(func() { openShell(d, 6) })
		} else {
			m.AddAction2(icon("terminal"), "Shell").OnTriggered(func() { openShell(d, 1) })
		}
		m.AddAction2(icon("plus"), "Add route…").OnTriggered(showAddRoute)
		m.AddAction2(icon("play"), "Run command…").OnTriggered(showRunCommand)
		m.AddSeparator()
		m.AddAction2(icon("copy"), "Copy node ID").OnTriggered(copyNodeID)
		m.AddActionWithText("Copy SSH config…").OnTriggered(showSSHConfig)
		gp := deviceTree.Viewport().MapToGlobalWithQPoint(pos)
		m.Popup(gp)
	})
}

func itemID(idx *qt.QModelIndex) string { return idx.DataWithRole(int(qt.UserRole)).ToString() }

// shellMenu offers the shells of a Windows device, nil for others, which
// have one.
func shellMenu(d meshcentral.Device) *qt.QMenu {
	if !strings.Contains(strings.ToLower(d.OS), "windows") {
		return nil
	}
	m := qt.NewQMenu2()
	m.SetAttribute(qt.WA_DeleteOnClose)
	m.AddActionWithText("Command prompt").OnTriggered(func() { openShell(d, 1) })
	m.AddActionWithText("PowerShell").OnTriggered(func() { openShell(d, 6) })
	return m
}

func popupBelow(m *qt.QMenu, w *qt.QWidget) {
	p := qt.NewQPoint2(0, w.Height())
	defer p.Delete()
	m.Popup(w.MapToGlobalWithQPoint(p))
}

func copyNodeID() {
	if d, ok := selectedDevice(); ok {
		qt.QGuiApplication_Clipboard().SetText(d.Id)
		logf("Copied node ID of %s", deviceName(d))
	}
}

func connect(insecure bool) {
	name := profileSel.CurrentText()
	if name == "" {
		showInfo("No profile", "Add a profile first.")
		return
	}
	connectBtn.SetEnabled(false)
	profileSel.SetEnabled(false)
	statusLabel.SetText("Connecting to " + name + "...")
	go func() {
		// Not through the default profile, that's the CLI's and would be
		// saved with the next profile edit.
		p, _ := config.GetProfile(name)
		meshcentral.ApplySettings(insecure, false)
		meshcentral.ApplyAuth("", false, false)
		err := meshcentral.StartSocketAs(p)
		var id int
		mainthread.Wait(func() {
			// Disabled only while connecting, once connected the profile bar is hidden.
			connectBtn.SetEnabled(true)
			profileSel.SetEnabled(true)
			if err != nil {
				statusLabel.SetText("Disconnected")
				showError(err)
				return
			}
			setConnected(true)
			session.id++
			id = session.id
			session.profile, session.insecure, session.loading = name, insecure, true
			setPref("lastProfile", name)
			statusLabel.SetText(fmt.Sprintf("Connected to %s as %s (profile %s)", p.Server, p.Username, name))
			logf("Connected to %s as %s (profile %s), loading devices", p.Server, p.Username, name)
			restoreRoutes()
		})
		if err != nil {
			return
		}

		// Large servers take a while to send the list, the session is already
		// up (and reported) meanwhile.
		devs, err := meshcentral.QueryDevices()
		mainthread.Start(func() {
			if id != session.id { // disconnected while loading
				return
			}
			session.loading = false
			if err != nil {
				logf("Device list failed: %v", err)
				showError(err)
				return
			}
			clear(collapsed)
			setDevices(devs)
			logf("Loaded %d devices", len(devs))
			benchLoaded()
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
	for _, st := range slices.Clone(shells) {
		closeShell(st)
	}
	routes = nil
	rebuildRoutes()
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
		connectBtn.SetIcon(icon("right-from-bracket"))
	} else {
		connectBtn.SetText("Connect")
		connectBtn.SetIcon(icon("right-to-bracket"))
	}
	profileBar.SetVisible(!c)
	for _, b := range deviceBtns {
		b.SetEnabled(c)
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
		devs, err := meshcentral.QueryDevices()
		mainthread.Start(func() {
			if !connected || id != session.id {
				return
			}
			reloading = false
			if err != nil {
				logf("Device refresh failed: %v", err)
				showError(err)
			} else {
				setDevices(devs)
			}
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
		mainthread.Start(func() {
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
	profileSel.Clear()
	profileSel.AddItems(names)
	// The last one connected to, then the CLI's default.
	for _, n := range []string{pref("lastProfile"), config.GetDefaultProfileName()} {
		if i := slices.Index(names, n); i >= 0 {
			profileSel.SetCurrentIndex(i)
			return
		}
	}
	if len(names) > 0 {
		profileSel.SetCurrentIndex(0)
	}
}

// routeRow is a route in the Routes tab.
type routeRow struct {
	ar   *activeRoute
	w    *qt.QWidget
	text *rowText
	open *qt.QPushButton
}

// rebuildRoutes recreates the route rows after routes changed.
func rebuildRoutes() {
	for _, rr := range routeRows {
		rr.w.Hide()
		rr.w.DeleteLater()
	}
	routeRows = nil
	for i, ar := range routes {
		rr := &routeRow{ar: ar, w: qt.NewQWidget2(), text: newRowText(false)}
		rr.open = qt.NewQPushButton4(icon("arrow-up-right-from-square"), "Open")
		cp := qt.NewQPushButton4(icon("copy"), "Copy")
		stop := qt.NewQPushButton4(icon("circle-stop"), "Stop")
		row := hbox(false, rr.text.w, rr.open.QWidget, cp.QWidget, stop.QWidget)
		row.SetStretch(0, 1)
		rr.w.SetLayout(row.QLayout)
		r := ar.route
		rr.open.OnClicked(func() {
			if open := openCmd(r); open != nil {
				if err := open(); err != nil {
					showError(err)
				}
			}
		})
		cp.OnClicked(func() {
			s := copyText(r)
			qt.QGuiApplication_Clipboard().SetText(s)
			logf("Copied %q", s)
		})
		stop.OnClicked(func() {
			r.Close()
			routes = slices.DeleteFunc(routes, func(x *activeRoute) bool { return x == ar })
			rebuildRoutes()
			saveRoutes()
			logf("%s: stopped %s", ar.device, localAddr(r))
		})
		routeBox.InsertWidget(i, rr.w)
		routeRows = append(routeRows, rr)
		bindRouteRow(rr)
	}
	routeHint.SetVisible(len(routes) == 0)
}

// refreshRouteRows redraws the routes, for their connection counts and
// device state.
func refreshRouteRows() {
	for _, rr := range routeRows {
		bindRouteRow(rr)
	}
}

func bindRouteRow(rr *routeRow) {
	ar := rr.ar
	r := ar.route
	target := r.Target
	if target == "" {
		target = "device"
	}
	svc, ic := fmt.Sprintf("Port %d", r.RemotePort), "network-wired"
	switch r.RemotePort {
	case 22:
		svc, ic = "SSH", "terminal"
	case 3389:
		svc, ic = "RDP", "desktop"
	case 80, 8080:
		svc, ic = "HTTP", "globe"
	case 443, 8443:
		svc, ic = "HTTPS", "globe"
	case 9090:
		svc, ic = "Cockpit", "globe"
	case 5900:
		svc, ic = "VNC", "display"
	}
	d := rowData{
		title: ar.device + " · " + svc,
		sub:   fmt.Sprintf("%s » %s:%d · %d active", localAddr(r), target, r.RemotePort, r.Active()),
		icon:  ic,
	}
	if i, ok := deviceIdx[r.NodeID]; ok && devices[i].Pwr == 0 {
		d.iconOff = true
		d.sub += " · device offline"
	}
	if r.Recorded() {
		d.sub += " · recorded"
	}
	rr.text.set(d)
	rr.open.SetVisible(openCmd(r) != nil)
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
		time.AfterFunc(eventFlushDelay, func() { mainthread.Start(flushNodeEvents) })
	}
}

// flushNodeEvents applies the queued events. The tree is only rebuilt when
// its shape changed, otherwise the rows are repainted.
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
	resort, refilter := false, searchEntry.Text() != ""
	changed := false
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
				changed, refilter = true, true
			}
			d.Conn, d.Pwr = e.Conn, e.Pwr
		case e.Action == "removenode" && known:
			devices = slices.Delete(devices, i, i+1)
			reindexDevices()
			changed, refilter = true, true
		case e.Device != nil: // addnode, changenode
			d := *e.Device
			if known { // the event's state isn't live, nodeconnect keeps it
				d.Conn, d.Pwr = devices[i].Conn, devices[i].Pwr
				resort = resort || deviceName(d) != deviceName(devices[i])
				refilter = refilter || d.MeshID != devices[i].MeshID || d.Group != devices[i].Group
				devices[i] = d
			} else {
				deviceIdx[d.Id] = len(devices)
				devices = append(devices, d)
				resort = true
			}
			changed = true
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
	if !changed {
		return
	}
	if refilter && filterDevices() {
		rebuildTree()
	} else {
		deviceTree.Viewport().Update()
	}
	refreshRouteRows()
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
	rebuildTree()
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

	q := strings.ToLower(searchEntry.Text())
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
		if d.Pwr == 0 && !offlineChk.IsChecked() {
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
		selectedID = ""
		treeBuilding = true
		deviceTree.ClearSelection()
		treeBuilding = false
	}
	return !slices.Equal(oldOrder, groupOrder) || !maps.EqualFunc(oldChildren, groupChildren, slices.Equal)
}

// rebuildTree reconciles the visible rows, retaining unaffected Qt items.
// The Go order avoids crossing into Qt for every unchanged device.
func rebuildTree() {
	if win != nil && !win.IsVisible() {
		treeDirty = true
		return
	}
	treeDirty = false
	treeBuilding = true
	defer func() { treeBuilding = false }()
	sb := deviceTree.VerticalScrollBar()
	pos := sb.Value()
	deviceTree.SetUpdatesEnabled(false)

	// Detach moved devices before removing their old groups.
	for gid, ids := range treeChildren {
		g := treeItems[gid]
		for i := len(ids) - 1; i >= 0; i-- {
			d, visible := shownDevice(ids[i])
			if visible && d.MeshID == gid {
				continue
			}
			item := g.TakeChild(i)
			if !visible {
				item.Delete()
				delete(treeItems, ids[i])
			}
			ids = slices.Delete(ids, i, i+1)
		}
		treeChildren[gid] = ids
	}
	for i := len(treeOrder) - 1; i >= 0; i-- {
		gid := treeOrder[i]
		if _, keep := groupChildren[gid]; !keep {
			deviceTree.TakeTopLevelItem(i).Delete()
			delete(treeItems, gid)
			delete(treeChildren, gid)
			treeOrder = slices.Delete(treeOrder, i, i+1)
		}
	}
	for i, gid := range groupOrder {
		g := treeItems[gid]
		if g == nil {
			g = qt.NewQTreeWidgetItem()
			v := qt.NewQVariant11(gid)
			g.SetData(0, int(qt.UserRole), v)
			v.Delete()
			g.SetFlags(qt.ItemIsEnabled)
			treeItems[gid] = g
		}
		if i >= len(treeOrder) || treeOrder[i] != gid {
			if old := slices.Index(treeOrder, gid); old >= 0 {
				deviceTree.TakeTopLevelItem(old)
				treeOrder = slices.Delete(treeOrder, old, old+1)
			}
			deviceTree.InsertTopLevelItem(i, g)
			treeOrder = slices.Insert(treeOrder, i, gid)
		}
		ids := treeChildren[gid]
		for j, id := range groupChildren[gid] {
			if j < len(ids) && ids[j] == id {
				continue
			}
			item := treeItems[id]
			if item == nil {
				item = qt.NewQTreeWidgetItem()
				v := qt.NewQVariant11(id)
				item.SetData(0, int(qt.UserRole), v)
				v.Delete()
				treeItems[id] = item
			} else if old := slices.Index(ids, id); old >= 0 {
				g.TakeChild(old)
				ids = slices.Delete(ids, old, old+1)
			}
			g.InsertChild(j, item)
			ids = slices.Insert(ids, j, id)
		}
		treeChildren[gid] = ids
		g.SetExpanded(!collapsed[gid])
	}
	if sel := treeItems[selectedID]; sel != nil {
		deviceTree.SetCurrentItem(sel)
	} else {
		deviceTree.ClearSelection()
	}
	deviceTree.SetUpdatesEnabled(true)
	sb.SetValue(pos)
	deviceTree.Viewport().Update()
}

// selectDevice selects a device row, as a click would.
func selectDevice(id string) {
	if item, ok := treeItems[id]; ok {
		deviceTree.SetCurrentItem(item)
	}
}

// treeRowData is what a tree row shows, a group or a device.
func treeRowData(id string) rowData {
	if l, ok := groupLabels[id]; ok {
		return rowData{title: l[0], sub: "  " + l[1], inline: true}
	}
	i, ok := deviceIdx[id]
	if !ok {
		return rowData{}
	}
	d := devices[i]
	rd := rowData{
		title: deviceName(d),
		sub:   strings.Join(slices.DeleteFunc([]string{d.Name, d.IP, d.OS}, func(s string) bool { return s == "" }), " · "),
		icon:  osIconName(d.OS),
	}
	if d.Pwr == 0 {
		rd.muted, rd.iconOff = true, true
		rd.sub = "offline · " + rd.sub
	}
	return rd
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
		showInfo("No device", "Select a device first.")
	}
	return d, ok
}

// shellTab is a shell in its own tab, closing the tab ends the session.
type shellTab struct {
	t        *term
	title    string
	recorded bool
}

// openShell opens a tab with a shell on d, protocol 1 is the device's
// default shell, 6 PowerShell.
func openShell(d meshcentral.Device, protocol int) {
	name := deviceName(d)
	t := newTerm()
	resize := make(chan struct{}, 1)
	t.onResize = func() {
		select {
		case resize <- struct{}{}:
		default:
		}
	}
	title := name
	if protocol == 6 {
		title += " · PowerShell"
	}
	st := &shellTab{t: t, title: title}
	onRecorded := func() {
		mainthread.Start(func() {
			if !st.recorded && !t.closed {
				st.recorded = true
				tabs.SetTabText(tabs.IndexOf(t.w), title+" · recorded")
				logf("%s: the server records this shell", name)
			}
		})
	}
	go func() {
		err := meshcentral.RunShell(d.Id, protocol, t.in, t, t.Size, resize, onRecorded)
		msg := "\r\n[session closed]\r\n"
		if err != nil {
			msg = fmt.Sprintf("\r\n[%v]\r\n", err)
		}
		t.Write([]byte(msg))
		t.finish()
		mainthread.Start(func() {
			if !t.closed {
				closedTitle := title
				if st.recorded {
					closedTitle += " · recorded"
				}
				tabs.SetTabText(tabs.IndexOf(t.w), closedTitle+" · closed")
			}
		})
		logf("%s: shell closed", name)
	}()

	shells = append(shells, st)
	tabs.SetCurrentIndex(tabs.AddTab2(t.w, icon("terminal"), title))
	t.w.SetFocus()
	logf("%s: shell opened", name)
}

func shellAt(i int) *shellTab {
	w := tabs.Widget(i)
	if w == nil {
		return nil
	}
	for _, st := range shells {
		if st.t.w.UnsafePointer() == w.UnsafePointer() {
			return st
		}
	}
	return nil
}

func closeShell(st *shellTab) {
	if !slices.Contains(shells, st) {
		return
	}
	shells = slices.DeleteFunc(shells, func(x *shellTab) bool { return x == st })
	tabs.RemoveTab(tabs.IndexOf(st.t.w))
	st.t.close()
	st.t.w.DeleteLater()
}

func showAddRoute() {
	d, ok := selectedDevice()
	if !ok {
		return
	}
	name := deviceName(d)
	f := newForm("Route to "+name, "Start")
	preset := qt.NewQComboBox2()
	preset.AddItems(presets)
	f.add("Preset", preset.QWidget)
	remotePort := f.entry("Remote port", "22", "", portValidator(false))
	preset.OnCurrentTextChanged(func(s string) {
		remotePort.SetText(strings.TrimSuffix(s[strings.Index(s, "(")+1:], ")"))
	})
	target := f.entry("Target host", "", "the device itself", nil)
	localPort := f.entry("Local port", "", "auto", portValidator(true))
	bind := f.entry("Bind address", "", "127.0.0.1", nil)
	f.show(0, func() {
		rp, _ := strconv.Atoi(remotePort.Text())
		lp, _ := strconv.Atoi(localPort.Text())
		t := strings.TrimSpace(target.Text())
		if t == "127.0.0.1" { // same as the device itself, matches the CLI
			t = ""
		}
		err := startRoute(name, &meshcentral.Route{
			NodeID:      d.Id,
			BindAddress: strings.TrimSpace(bind.Text()),
			LocalPort:   lp,
			Target:      t,
			RemotePort:  rp,
		})
		if err != nil {
			showError(err)
			return
		}
		saveRoutes()
	})
}

func startRoute(device string, r *meshcentral.Route) error {
	r.Out = logWriter(device)
	if err := r.Start(); err != nil {
		return err
	}
	routes = append(routes, &activeRoute{route: r, device: device})
	rebuildRoutes()
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
	if len(saved) == 0 {
		setPref(routesKey(session.profile), "")
		return
	}
	b, _ := json.Marshal(saved)
	setPref(routesKey(session.profile), string(b))
}

// restoreRoutes reopens the routes running at the last disconnect, one that
// can't bind its port any more is logged and forgotten.
func restoreRoutes() {
	var saved []savedRoute
	if json.Unmarshal([]byte(pref(routesKey(session.profile))), &saved) != nil {
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
	f := newForm("Run on "+deviceName(d), "Run")
	cmd := f.entry("Command", "", "e.g. systemctl restart myservice", func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	})
	cmd.SetMinimumWidth(320)
	asUser := qt.NewQCheckBox3("Run as the logged-in user instead of SYSTEM/root")
	f.add("", asUser.QWidget)
	f.show(0, func() {
		runAsUser := 0
		if asUser.IsChecked() {
			runAsUser = 1
		}
		if err := meshcentral.RunCommand(d.Id, cmd.Text(), runAsUser); err != nil {
			showError(err)
			return
		}
		logf("%s: sent %q (no output is returned)", deviceName(d), cmd.Text())
	})
}

// showSSHConfig builds a ~/.ssh/config Host block that tunnels through
// "mcc ssh --proxy", the setup VSCode Remote-SSH and plain ssh use.
func showSSHConfig() {
	d, ok := selectedDevice()
	if !ok {
		return
	}
	f := newForm("SSH config for "+deviceName(d), "Copy")
	host := f.entry("Host alias", sshAlias(d), "", nil)
	// Same default as ssh itself: the local user. Windows reports DOMAIN\user.
	userName := "root"
	if u, err := osuser.Current(); err == nil {
		userName = u.Username[strings.LastIndex(u.Username, `\`)+1:]
	}
	user := f.entry("User", userName, "", nil)
	port := f.entry("SSH port", "22", "", portValidator(false))
	mccPath := "mcc"
	if p, err := exec.LookPath("mcc"); err == nil {
		mccPath = p
	}
	mcc := f.entry("mcc path", mccPath, "", nil)
	preview := qt.NewQPlainTextEdit2()
	preview.SetReadOnly(true)
	preview.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	preview.SetFont(qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont))
	preview.SetFixedHeight(preview.FontMetrics().Height()*5 + 12)
	f.add("", preview.QWidget)

	snippet := func() string {
		bin := mcc.Text()
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
		if port.Text() != "22" {
			cmd += " -p " + port.Text()
		}
		if session.insecure {
			cmd += " -k"
		}
		return fmt.Sprintf("Host %s\n  User %s\n  ProxyCommand %s --proxy\n", host.Text(), user.Text(), cmd)
	}
	update := func(string) { preview.SetPlainText(snippet()) }
	for _, e := range []*qt.QLineEdit{host, user, port, mcc} {
		e.OnTextChanged(update)
	}
	update("")
	f.show(640, func() {
		qt.QGuiApplication_Clipboard().SetText(snippet())
		logf("Copied SSH config for %s, paste it into ~/.ssh/config", deviceName(d))
	})
}

func showAbout() {
	d := qt.NewQDialog(parent())
	d.SetWindowTitle("About")
	d.SetAttribute(qt.WA_DeleteOnClose)
	v := qt.NewQVBoxLayout(d.QWidget)
	title := qt.NewQLabel3("MeshCentral Client")
	tf := title.Font()
	tf.SetBold(true)
	tf.SetPointSizeF(tf.PointSizeF() * 1.3)
	title.SetFont(tf)
	info := qt.NewQLabel3(fmt.Sprintf("Version %s\nCommit %s, built %s\n%s, Qt %s GUI",
		cmp.Or(version, "dev"), cmp.Or(commit, "unknown"), cmp.Or(buildDate, "unknown"), runtime.Version(), qt.QLibraryInfo_Version().ToString()))
	info.SetTextInteractionFlags(qt.TextSelectableByMouse)
	desc := qt.NewQLabel3("Desktop client for MeshCentral: devices, port routes and remote commands,\nsharing profiles and 2FA login with the mcc CLI. Not affiliated with MeshCentral.")
	link := qt.NewQLabel3(`<a href="https://github.com/alexpaval/mesh-central-client-go">github.com/alexpaval/mesh-central-client-go</a>`)
	link.SetOpenExternalLinks(true)
	credits := qt.NewQLabel3("MIT License. Icons by Font Awesome Free (CC BY 4.0), UI by Qt (LGPLv3) through miqt (MIT).")
	credits.SetForegroundRole(qt.QPalette__PlaceholderText)
	for _, w := range []*qt.QWidget{title.QWidget, info.QWidget, desc.QWidget, link.QWidget, credits.QWidget} {
		v.AddWidget(w)
	}
	box := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Close)
	box.OnRejected(d.Reject)
	v.AddWidget(box.QWidget)
	d.Open()
}

// showProfileDialog adds a profile, or edits one when edit is set. The first
// run goes through CreateConfig, which writes the config file and always names
// the profile "default".
func showProfileDialog(firstRun bool, edit *config.Profile) {
	title := "Set up MeshCentral server"
	switch {
	case edit != nil:
		title = "Edit profile"
	case !firstRun:
		title = "Add profile"
	}
	f := newForm(title, "Save")
	var existing []string
	for i := range profileSel.Count() {
		existing = append(existing, profileSel.ItemText(i))
	}
	name := qt.NewQLineEdit2()
	if !firstRun {
		name = f.entry("Name", "", "", func(s string) error {
			if strings.TrimSpace(s) == "" {
				return errors.New("required")
			}
			if slices.Contains(existing, s) && (edit == nil || s != edit.Name) {
				return errors.New("already exists")
			}
			return nil
		})
	}
	required := func(s string) error {
		if strings.TrimSpace(s) == "" {
			return errors.New("required")
		}
		return nil
	}
	server := f.entry("Server", "", "mesh.example.com", required)
	username := f.entry("Username", "", "", nil)
	password := f.entry("Password", "", "", nil)
	password.SetEchoMode(qt.QLineEdit__Password)
	makeDefault := qt.NewQCheckBox3("Use as default profile")
	if !firstRun {
		f.add("", makeDefault.QWidget)
	}
	if edit != nil {
		name.SetText(edit.Name)
		server.SetText(edit.Server)
		username.SetText(edit.Username)
		password.SetPlaceholderText("unchanged")
		makeDefault.SetChecked(config.GetDefaultProfileName() == edit.Name)
	}
	f.show(460, func() {
		var err error
		if firstRun {
			if err = config.CreateConfig(server.Text(), username.Text(), password.Text()); err == nil {
				err = config.LoadConfig()
			}
		} else if edit != nil {
			p := config.Profile{Name: name.Text(), Server: server.Text(), Username: username.Text()}
			if err = config.UpdateProfile(edit.Name, p, password.Text(), makeDefault.IsChecked()); err == nil && p.Name != edit.Name {
				// The GUI's own memory of the profile follows a rename.
				setPref(routesKey(p.Name), pref(routesKey(edit.Name)))
				setPref(routesKey(edit.Name), "")
				if pref("lastProfile") == edit.Name {
					setPref("lastProfile", p.Name)
				}
			}
		} else {
			_, err = config.AddProfile(name.Text(), makeDefault.IsChecked(), server.Text(), username.Text(), password.Text())
		}
		if err != nil {
			showError(err)
			return
		}
		refreshProfiles()
		if !firstRun {
			profileSel.SetCurrentIndex(profileSel.FindText(name.Text()))
		}
	})
}

// promptToken runs on the StartSocket goroutine and blocks it until the user
// answers the dialog.
func promptToken(email2fa, sms2fa, emailSent bool) (string, bool) {
	type answer struct {
		token string
		ok    bool
	}
	ch := make(chan answer, 1)
	// ch holds one answer, so the dialog's own finish after a send button is dropped.
	reply := func(a answer) {
		select {
		case ch <- a:
		default:
		}
	}
	mainthread.Start(func() {
		msg := "Enter your 2FA token."
		if emailSent {
			msg = "A login token was sent by email. " + msg
		}
		d := qt.NewQDialog(parent())
		d.SetWindowTitle("Two-factor authentication")
		d.SetAttribute(qt.WA_DeleteOnClose)
		v := qt.NewQVBoxLayout(d.QWidget)
		v.AddWidget(qt.NewQLabel3(msg).QWidget)
		entry := qt.NewQLineEdit2()
		entry.SetEchoMode(qt.QLineEdit__Password)
		v.AddWidget(entry.QWidget)
		for _, s := range []struct {
			on         bool
			text, kind string
		}{{email2fa, "Send token by email", "email"}, {sms2fa, "Send token by SMS", "sms"}} {
			if s.on {
				b := qt.NewQPushButton3(s.text)
				b.SetAutoDefault(false)
				b.OnClicked(func() { reply(answer{s.kind, true}); d.Accept() })
				v.AddWidget(b.QWidget)
			}
		}
		box := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
		box.Button(qt.QDialogButtonBox__Ok).SetText("Log in")
		box.OnAccepted(d.Accept)
		box.OnRejected(d.Reject)
		v.AddWidget(box.QWidget)
		d.OnFinished(func(r int) {
			token := strings.TrimSpace(entry.Text())
			reply(answer{token, r == int(qt.QDialog__Accepted) && token != ""})
		})
		d.Open()
		entry.SetFocus()
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
		return startCommand(exec.Command(args[0], args[1:]...))
	case "darwin": // the args are plain words, no quoting needed
		script := fmt.Sprintf(`tell application "Terminal" to do script "%s"`, strings.Join(args, " "))
		return startCommand(exec.Command("osascript", "-e", script, "-e", `tell application "Terminal" to activate`))
	}
	for _, t := range terminals {
		if _, err := exec.LookPath(t[0]); err == nil {
			return startCommand(exec.Command(t[0], append(t[1:], args...)...))
		}
	}
	return errors.New("no terminal emulator found, use Copy for the ssh command")
}

func startCommand(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	// Reap external clients without blocking the UI while their windows are open.
	go func() { _ = cmd.Wait() }()
	return nil
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
			return openURL(u)
		}
	case 3389:
		if runtime.GOOS == "windows" { // mstsc doesn't register rdp://
			return func() error { return startCommand(exec.Command("mstsc", "/v:"+localAddr(r))) }
		}
		return func() error {
			if runtime.GOOS == "linux" { // xdg-open fails silently without a handler
				if out, err := exec.Command("xdg-mime", "query", "default", "x-scheme-handler/rdp").Output(); err == nil && len(bytes.TrimSpace(out)) == 0 {
					return errors.New("no application opens rdp:// links, install an RDP client such as Remmina")
				}
			}
			return openURL(rdpURL(r, runtime.GOOS))
		}
	}
	return nil
}

// openURL opens a link in its default application. macOS gets it through
// open, QUrl would reencode Microsoft's rdp:// form.
func openURL(u *url.URL) error {
	if runtime.GOOS == "darwin" {
		return startCommand(exec.Command("open", u.String()))
	}
	q := qt.NewQUrl3(u.String())
	defer q.Delete()
	if !qt.QDesktopServices_OpenUrl(q) {
		return fmt.Errorf("no application opens %s:// links", u.Scheme)
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
		mainthread.Start(flushLog)
	}
}

func flushLog() {
	pendingLog.Lock()
	lines := pendingLog.lines
	pendingLog.lines = nil
	pendingLog.Unlock()
	logLines = append(logLines, lines...)
	if len(logLines) > logMax {
		logLines = slices.Clone(logLines[len(logLines)-logMax:])
	}
	for _, l := range lines {
		logView.AppendPlainText(l)
	}
}

// Preferences of the GUI itself (last profile, routes to reopen), in a JSON
// file next to the CLI's config.
var prefs struct {
	path string
	m    map[string]string
}

func loadPrefs(path string) {
	prefs.path, prefs.m = path, map[string]string{}
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &prefs.m)
	}
}

func pref(key string) string { return prefs.m[key] }

// setPref stores a preference, "" removes it.
func setPref(key, value string) {
	if prefs.m[key] == value {
		return
	}
	if value == "" {
		delete(prefs.m, key)
	} else {
		prefs.m[key] = value
	}
	b, _ := json.MarshalIndent(prefs.m, "", "  ")
	os.MkdirAll(filepath.Dir(prefs.path), 0o700)
	tmp := prefs.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, prefs.path)
	}
}
