package main

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

// TestMain runs Qt on the main thread, offscreen, and the tests beside it.
// They reach the UI through ui, Qt objects belong to its thread.
func TestMain(m *testing.M) {
	os.Setenv("QT_QPA_PLATFORM", "offscreen")
	qt.NewQApplication([]string{"mcc-gui-test"})
	qt.QApplication_SetStyleWithStyle("Fusion")
	qt.QGuiApplication_SetQuitOnLastWindowClosed(false) // screenshots close their windows
	code := 0
	go func() {
		code = m.Run()
		mainthread.Start(qt.QCoreApplication_Quit)
	}()
	qt.QApplication_Exec()
	os.Exit(code)
}

// ui runs f on the Qt thread with a fresh UI, state and preferences.
// Failures inside use t.Errorf, t.Fatal would end the Qt thread.
func ui(t *testing.T, f func()) {
	t.Helper()
	loadPrefs(filepath.Join(t.TempDir(), "prefs.json"))
	mainthread.Wait(func() {
		win, devices, routes, logLines, selectedID, connected = nil, nil, nil, nil, "", false
		session.profile, session.insecure, session.loading = "", false, false
		buildUI()
		setDevices(nil)
		f()
	})
}

// settle runs work queued for the Qt thread, such as logf's.
func settle() { qt.QCoreApplication_ProcessEvents() }

// event delivers a node event the way the socket reader does and flushes it.
func event(action, id string, pwr int) {
	onNodeEvent(meshcentral.NodeEvent{Action: action, NodeID: id, Pwr: pwr})
	flushNodeEvents()
}

func TestNodeEventsNeedConnection(t *testing.T) {
	ui(t, func() {
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1}})
		settle()
		logLines = nil

		// Events the server pushes while connect is still loading are ignored.
		event("nodeconnect", "a", 0)
		event("changenode", "a", 0)
		settle()
		if devices[0].Pwr != 1 || len(logLines) != 0 || reloadTimer != nil {
			t.Errorf("event applied while disconnected: pwr=%d log=%v reload=%v", devices[0].Pwr, logLines, reloadTimer != nil)
			return
		}

		// Connected but the first list is still loading, which reflects them.
		setConnected(true)
		session.loading = true
		event("nodeconnect", "a", 0)
		if devices[0].Pwr != 1 {
			t.Error("event applied while the device list was loading")
			return
		}

		session.loading = false
		event("nodeconnect", "a", 0)
		if devices[0].Pwr != 0 {
			t.Error("event not applied once loaded")
			return
		}

		// A reload scheduled before a disconnect doesn't fire after it.
		event("changenode", "a", 0)
		if reloadTimer == nil {
			t.Error("change event scheduled no reload")
			return
		}
		disconnect()
		if reloadTimer != nil {
			t.Error("disconnect left the reload scheduled")
		}
	})
}

func TestDisconnectDiscardsPendingNodeEvents(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "Old", Name: "old", Pwr: 1}})
		onNodeEvent(meshcentral.NodeEvent{Action: "nodeconnect", NodeID: "a", Pwr: 0})
		onNodeEvent(meshcentral.NodeEvent{Action: "meshchange"})
		disconnect()

		// The old batch can reach the UI after a different profile has connected.
		setConnected(true)
		defer disconnect()
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "New", Name: "new", Pwr: 1}})
		flushNodeEvents()
		if devices[0].Pwr != 1 || reloadTimer != nil {
			t.Error("events from the old session changed the new session or scheduled a reload")
		}

		// Discarding the old batch must leave delivery working for the new session.
		event("nodeconnect", "a", 0)
		if devices[0].Pwr != 0 {
			t.Error("new session's node event was discarded")
		}
	})
}

func TestQueuedReloadDoesNotAffectNewSession(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		defer disconnect()
		scheduleReload()
		reloadTimer.Reset(0)
		// Keep Qt busy while the Go timer queues its callback.
		time.Sleep(100 * time.Millisecond)
		if reloadTimer.Stop() {
			t.Error("reload timer did not fire")
			return
		}
		disconnect()
		setConnected(true)
		reloading = true // an extra refresh would set reloadAgain
		scheduleReload()
		current := reloadTimer
		settle()
		if reloadTimer != current || reloadAgain {
			t.Error("old callback changed the new session's timer or requested a refresh")
		}
	})
}

func TestNodeEventsUpdateInPlace(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		offlineChk.SetChecked(true)
		setDevices([]meshcentral.Device{
			{Id: "a", MeshID: "m", Group: "G", Name: "b-host", Conn: 1, Pwr: 1},
			{Id: "b", MeshID: "m", Group: "G", Name: "c-host", Conn: 1, Pwr: 1},
		})
		selectDevice("b")
		if selectedID != "b" {
			t.Errorf("selected %q, want b", selectedID)
			return
		}

		// Going offline with offline devices shown keeps the rows.
		item := treeItems["b"]
		event("nodeconnect", "b", 0)
		if devices[deviceIdx["b"]].Pwr != 0 {
			t.Error("offline event not applied")
			return
		}
		if treeItems["b"] != item || !treeRowData("b").muted {
			t.Error("offline device rebuilt the tree or isn't shown offline")
			return
		}
		event("nodeconnect", "b", 1)

		// A rename keeps the live state and moves the device, an add inserts one,
		// neither reloads the list.
		onNodeEvent(meshcentral.NodeEvent{Action: "changenode", NodeID: "a", Device: &meshcentral.Device{Id: "a", MeshID: "m", Group: "G", Name: "d-host"}})
		onNodeEvent(meshcentral.NodeEvent{Action: "addnode", NodeID: "c", Device: &meshcentral.Device{Id: "c", MeshID: "m", Group: "G", Name: "a-host"}})
		flushNodeEvents()
		if got := groupChildren["m"]; !slices.Equal(got, []string{"c", "b", "a"}) {
			t.Errorf("tree order %v after rename and add", got)
			return
		}
		if g := treeItems["m"]; g == nil || g.ChildCount() != 3 || g.Child(0).Data(0, int(qt.UserRole)).ToString() != "c" {
			t.Error("tree rows don't follow the order")
			return
		}
		if d := devices[deviceIdx["a"]]; d.Conn != 1 || d.Pwr != 1 {
			t.Errorf("change event reset the connection state: %+v", d)
			return
		}
		if reloadTimer != nil {
			t.Error("event carrying the device scheduled a reload")
			return
		}
		if groupLabels["m"][1] != "2 of 3 online" {
			t.Errorf("group label %q", groupLabels["m"][1])
			return
		}
		if selectedID != "b" {
			t.Error("update dropped the selection")
			return
		}

		event("removenode", "c", 0)
		if got := groupChildren["m"]; !slices.Equal(got, []string{"b", "a"}) || devices[deviceIdx["a"]].Id != "a" {
			t.Errorf("tree %v after remove", got)
			return
		}
		event("removenode", "b", 0)
		if selectedID != "" || len(deviceTree.SelectedItems()) != 0 {
			t.Error("removed device still selected")
		}
	})
}

func TestLogKeepsNewestLines(t *testing.T) {
	ui(t, func() {
		settle()
		logLines = nil
		logView.SetPlainText("")
		for i := range logMax + 10 {
			logf("line %d", i)
		}
		settle()
		if len(logLines) != logMax || !strings.HasSuffix(logLines[0], "line 10") {
			t.Errorf("%d lines, first %q", len(logLines), logLines[0])
			return
		}
		logf("newest")
		settle()
		shown := strings.Split(logView.ToPlainText(), "\n")
		if len(shown) != logMax || !strings.HasSuffix(shown[len(shown)-1], "newest") || !strings.HasSuffix(shown[0], "line 11") {
			t.Errorf("log view shows %d lines, %q to %q", len(shown), shown[0], shown[len(shown)-1])
		}
	})
}

func TestSearchKeepsCollapsedGroups(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		var devs []meshcentral.Device
		for i := range 60 {
			devs = append(devs, meshcentral.Device{Id: fmt.Sprintf("node//%d", i), MeshID: fmt.Sprintf("mesh//%d", i%2), Group: fmt.Sprintf("G%d", i%2), Name: fmt.Sprintf("host-%02d", i), Pwr: 1})
		}
		offlineChk.SetChecked(true)
		collapsed["mesh//0"] = true
		setDevices(slices.Clone(devs))
		searchEntry.SetText("host")
		if !treeItems["mesh//0"].IsExpanded() {
			t.Error("search didn't open the matching groups")
			return
		}
		treeItems["mesh//0"].SetExpanded(false)

		// An event burst, then a reload, keep it collapsed.
		for i := range 60 {
			onNodeEvent(meshcentral.NodeEvent{Action: "nodeconnect", NodeID: fmt.Sprintf("node//%d", i), Pwr: 0})
		}
		flushNodeEvents()
		if treeItems["mesh//0"].IsExpanded() {
			t.Error("collapsed group reopened by an event burst")
			return
		}
		setDevices(slices.Clone(devs))
		if treeItems["mesh//0"].IsExpanded() || !treeItems["mesh//1"].IsExpanded() {
			t.Error("reload changed which groups are open")
		}
	})
}

func TestProfileSelection(t *testing.T) {
	keyring.MockInit()
	viper.Set("profiles", []map[string]any{{"name": "cli"}, {"name": "gui"}})
	viper.Set("default_profile", "cli")
	t.Cleanup(viper.Reset)
	ui(t, func() {
		refreshProfiles()
		if got := profileSel.CurrentText(); got != "cli" {
			t.Errorf("selected %q with nothing remembered, want the CLI default", got)
		}
		setPref("lastProfile", "gui")
		refreshProfiles()
		if got := profileSel.CurrentText(); got != "gui" {
			t.Errorf("selected %q, want the last connected profile", got)
		}
		setPref("lastProfile", "removed")
		refreshProfiles()
		if got := profileSel.CurrentText(); got != "cli" {
			t.Errorf("selected %q for a removed last profile, want the CLI default", got)
		}
	})
	if p, ok := config.GetProfile("gui"); !ok || p.Name != "gui" || config.GetDefaultProfileName() != "cli" {
		t.Fatalf("GetProfile = %+v %v, default %q", p, ok, config.GetDefaultProfileName())
	}
}

func TestPrefsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "prefs.json")
	loadPrefs(path)
	setPref("lastProfile", "work")
	setPref("routes/work", `[{"Device":"d"}]`)
	setPref("routes/work", "")
	loadPrefs(path)
	if pref("lastProfile") != "work" || pref("routes/work") != "" || len(prefs.m) != 1 {
		t.Fatalf("reloaded %v", prefs.m)
	}
}

func TestRefreshFailureKeepsDevices(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		reloading, reloadAgain = false, false
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1}})
		selectDevice("a")
		settle()
		logLines = nil
		// No control socket: the request fails instead of returning an empty list.
		refreshDevices()
	})
	defer mainthread.Wait(func() {
		for _, w := range qt.QApplication_TopLevelWidgets() {
			if w.WindowTitle() == "Error" {
				w.Close()
			}
		}
		setConnected(false)
	})
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		mainthread.Wait(func() {
			if reloading {
				return
			}
			done = true
			flushLog()
			if len(devices) != 1 || devices[0].Id != "a" || selectedID != "a" || treeItems["a"] == nil {
				t.Errorf("failed refresh changed devices or selection: %v, %q", devices, selectedID)
			}
			if !strings.Contains(strings.Join(logLines, "\n"), "Device refresh failed:") {
				t.Error("refresh failure was not logged")
			}
			shownError := false
			for _, w := range qt.QApplication_TopLevelWidgets() {
				if w.WindowTitle() == "Error" && w.IsVisible() {
					shownError = true
				}
			}
			if !shownError {
				t.Error("refresh failure was not shown")
			}
		})
		if done {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("device refresh did not finish")
}

func TestRoutesReopen(t *testing.T) {
	ui(t, func() {
		session.profile = "p"
		closeAll := func() {
			for _, ar := range routes {
				ar.route.Close()
			}
			routes = nil
		}
		defer closeAll()

		// An automatic local port is remembered as the one it got.
		if err := startRoute("dev", &meshcentral.Route{NodeID: "node//1", RemotePort: 22}); err != nil {
			t.Error(err)
			return
		}
		saveRoutes()
		port := routes[0].route.LocalPort
		closeAll() // as disconnect does, without saving
		restoreRoutes()
		if len(routes) != 1 || routes[0].route.LocalPort != port || routes[0].device != "dev" || routes[0].route.NodeID != "node//1" || len(routeRows) != 1 {
			t.Errorf("reopened %+v, %d rows", routes, len(routeRows))
			return
		}

		// A port taken meanwhile fails that route, which is then forgotten.
		closeAll()
		l, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
		if err != nil {
			t.Error(err)
			return
		}
		defer l.Close()
		restoreRoutes()
		if len(routes) != 0 || pref(routesKey("p")) != "" {
			t.Errorf("routes %d, saved %q", len(routes), pref(routesKey("p")))
		}
	})
}

func TestRDPURL(t *testing.T) {
	r := &meshcentral.Route{LocalPort: 40123, RemotePort: 3389}
	if got := rdpURL(r, "linux").String(); got != "rdp://127.0.0.1:40123" {
		t.Errorf("linux: %s", got)
	}
	if got := rdpURL(r, "darwin").String(); got != "rdp://full%20address=s:127.0.0.1:40123" {
		t.Errorf("darwin: %s", got)
	}
	if openCmd(r) == nil {
		t.Error("RDP route has no Open action")
	}
}

func TestRouteClientAddresses(t *testing.T) {
	for _, tc := range []struct {
		bind, host, addr string
	}{
		{"", "127.0.0.1", "127.0.0.1:40123"},
		{"0.0.0.0", "127.0.0.1", "127.0.0.1:40123"},
		{"127.0.0.2", "127.0.0.2", "127.0.0.2:40123"},
		{"localhost", "localhost", "localhost:40123"},
		{"::", "::1", "[::1]:40123"},
		{"0:0:0:0:0:0:0:0", "::1", "[::1]:40123"},
		{"::ffff:0.0.0.0", "127.0.0.1", "127.0.0.1:40123"},
		{"::1", "::1", "[::1]:40123"},
		{"2001:db8::1", "2001:db8::1", "[2001:db8::1]:40123"},
		{"fe80::1%eth0", "fe80::1%eth0", "[fe80::1%eth0]:40123"},
	} {
		t.Run(tc.bind, func(t *testing.T) {
			r := &meshcentral.Route{NodeID: "address-test", BindAddress: tc.bind, LocalPort: 40123, RemotePort: 22}
			if got := localAddr(r); got != tc.addr {
				t.Errorf("local address = %q, want %q", got, tc.addr)
			}
			wantSSH := "ssh -p 40123 " + tc.host
			if got := copyText(r); got != wantSSH {
				t.Errorf("SSH command = %q, want %q", got, wantSSH)
			}
			for _, port := range []int{80, 443, 3389} {
				r.RemotePort = port
				link := copyText(r)
				scheme := "http"
				if port == 443 {
					scheme = "https"
				} else if port == 3389 {
					scheme = "rdp"
					link = rdpURL(r, "linux").String()
				}
				u, err := url.Parse(link)
				if err != nil {
					t.Errorf("invalid %s URL %q: %v", scheme, link, err)
				} else if u.Scheme != scheme || u.Hostname() != tc.host || u.Port() != "40123" {
					t.Errorf("%s URL = %q, want host %q and port 40123", scheme, link, tc.host)
				}
			}
		})
	}
}

func TestSSHOpen(t *testing.T) {
	ui(t, func() {
		setDevices([]meshcentral.Device{{Id: "node//1", MeshID: "m", Name: "Lab-Bench_1", Pwr: 1}})
	})
	r := &meshcentral.Route{NodeID: "node//1", LocalPort: 40123, RemotePort: 22}
	want := "ssh -p 40123 -o HostKeyAlias=lab-bench-1 127.0.0.1"
	if got := copyText(r); got != want {
		t.Fatalf("copy %q, want %q", got, want)
	}
	if runtime.GOOS != "linux" {
		return
	}
	// The first terminal found runs ssh with the same arguments.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ptyxis"), []byte("#!/bin/sh\necho \"$@\" > \"$0.args\"\n"), 0o755)
	t.Setenv("PATH", dir)
	if err := openCmd(r)(); err != nil {
		t.Fatal(err)
	}
	var got []byte
	for range 50 {
		if got, _ = os.ReadFile(filepath.Join(dir, "ptyxis.args")); len(got) > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if string(got) != "-- "+want+"\n" {
		t.Fatalf("terminal got %q", got)
	}
	t.Setenv("PATH", t.TempDir())
	if err := openCmd(r)(); err == nil {
		t.Fatal("no error without a terminal")
	}
}

func TestShellTabs(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1}, {Id: "b", MeshID: "m", Group: "G", Name: "b", Pwr: 1}})
		// No server, the sessions fail and say so in their tabs.
		openShell(devices[0], 1)
		openShell(devices[1], 6)
		if tabs.Count() != 3 || tabs.TabText(2) != "b · PowerShell" || tabs.CurrentIndex() != 2 {
			t.Errorf("%d tabs, last %q, current %d", tabs.Count(), tabs.TabText(2), tabs.CurrentIndex())
			return
		}
		if shellAt(0) != nil || shellAt(1) != shells[0] {
			t.Error("shellAt maps tabs wrong")
			return
		}
		closeShell(shells[0])
		if tabs.Count() != 2 || len(shells) != 1 || tabs.TabText(1) != "b · PowerShell" {
			t.Errorf("after close: %d tabs, %d shells", tabs.Count(), len(shells))
			return
		}
		disconnect()
		if tabs.Count() != 1 || len(shells) != 0 {
			t.Errorf("disconnect left %d tabs", tabs.Count()-1)
		}
	})
}

func TestFailedShellMarksTabClosed(t *testing.T) {
	var st *shellTab
	ui(t, func() {
		// No control socket is connected, so the shell fails immediately.
		openShell(meshcentral.Device{Id: "n", Name: "unavailable"}, 1)
		st = shells[len(shells)-1]
	})
	t.Cleanup(func() { mainthread.Wait(func() { closeShell(st) }) })
	deadline := time.Now().Add(time.Second)
	for {
		var title string
		mainthread.Wait(func() { title = tabs.TabText(tabs.IndexOf(st.t.w)) })
		if title == "unavailable · closed" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed shell still looks active: %q", title)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTreeRetainsItemsAcrossMoves(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		offlineChk.SetChecked(true)
		setDevices([]meshcentral.Device{
			{Id: "a", MeshID: "one", Group: "One", Name: "a", Pwr: 1},
			{Id: "b", MeshID: "two", Group: "Two", Name: "b", Pwr: 1},
			{Id: "c", MeshID: "two", Group: "Two", Name: "c", Pwr: 1},
		})
		a, b, c, group := treeItems["a"], treeItems["b"], treeItems["c"], treeItems["two"]
		selectDevice("a")
		// Remove the old group, insert into an existing group, and reorder siblings.
		setDevices([]meshcentral.Device{
			{Id: "a", MeshID: "two", Group: "Two", Name: "z", Pwr: 1},
			{Id: "b", MeshID: "two", Group: "Two", Name: "b", Pwr: 1},
			{Id: "c", MeshID: "two", Group: "Two", Name: "a", Pwr: 1},
		})
		if treeItems["a"] != a || treeItems["b"] != b || treeItems["c"] != c || treeItems["two"] != group {
			t.Error("moving devices replaced native items")
		}
		if deviceTree.TopLevelItemCount() != 1 || group.ChildCount() != 3 {
			t.Error("incorrect group or child count")
		}
		for i, id := range []string{"c", "b", "a"} {
			if got := group.Child(i).Data(0, int(qt.UserRole)).ToString(); got != id {
				t.Errorf("child %d = %q, want %q", i, got, id)
			}
		}
		if selectedID != "a" || !a.IsSelected() {
			t.Error("group move lost selection")
		}
	})
}

func TestHiddenTreeDefersChanges(t *testing.T) {
	ui(t, func() {
		win = qt.NewQMainWindow2()
		defer func() { win.DeleteLater(); win = nil }()
		win.Hide()
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Name: "a", Pwr: 1}})
		setDevices([]meshcentral.Device{{Id: "b", MeshID: "m", Name: "b", Pwr: 1}})
		if !treeDirty || treeItems["b"] != nil {
			t.Error("hidden tree was updated eagerly")
		}
		win.Show()
		rebuildTree() // the main window show handler flushes deferred changes
		if treeDirty || treeItems["a"] != nil || treeItems["b"] == nil {
			t.Error("show did not reconcile the latest device list")
		}
		win.Hide()
	})
}
