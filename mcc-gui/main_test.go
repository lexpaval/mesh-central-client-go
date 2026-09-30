package main

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/test"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

func TestDeviceRowTaps(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	setConnected(true)
	setDevices([]meshcentral.Device{
		{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1},
		{Id: "b", MeshID: "m", Group: "G", Name: "b", Pwr: 1},
	})
	rowA, rowB := newDeviceRow(), newDeviceRow()
	rowA.id, rowB.id = "a", "b"

	// Quick taps on different rows each select at once and open nothing.
	for _, r := range []*deviceRow{rowA, rowB, rowA, rowB} {
		test.Tap(r)
		if selectedID != r.id {
			t.Fatalf("selected %q after tapping %q", selectedID, r.id)
		}
	}
	if len(tabList) != 1 {
		t.Fatalf("taps on different rows opened %d shell tabs", len(tabList)-1)
	}

	// Two taps on the same row open a shell.
	test.Tap(rowA)
	test.Tap(rowA)
	if len(tabList) != 2 {
		t.Fatalf("double tap opened %d shell tabs, want 1", len(tabList)-1)
	}
	// The test driver runs fyne.Do inline, let the shell, which fails without
	// a server, finish logging before the next test touches the UI.
	time.Sleep(200 * time.Millisecond)

	// A second tap after the double-click delay is a new single tap.
	lastTap.id = ""
	test.Tap(rowB)
	lastTap.at = lastTap.at.Add(-time.Second)
	test.Tap(rowB)
	if len(tabList) != 2 {
		t.Fatal("slow second tap opened a shell")
	}
}

func TestNodeEventsNeedConnection(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1}})
	logLines = nil

	// Events the server pushes while connect is still loading are ignored.
	event("nodeconnect", "a", 0)
	event("changenode", "a", 0)
	if devices[0].Pwr != 1 || len(logLines) != 0 || reloadTimer != nil {
		t.Fatalf("event applied while disconnected: pwr=%d log=%v reload=%v", devices[0].Pwr, logLines, reloadTimer != nil)
	}

	// Connected but the first list is still loading, which reflects them.
	setConnected(true)
	session.loading = true
	event("nodeconnect", "a", 0)
	if devices[0].Pwr != 1 {
		t.Fatal("event applied while the device list was loading")
	}

	session.loading = false
	event("nodeconnect", "a", 0)
	if devices[0].Pwr != 0 {
		t.Fatal("event not applied once loaded")
	}

	// A reload scheduled before a disconnect doesn't fire after it.
	event("changenode", "a", 0)
	if reloadTimer == nil {
		t.Fatal("change event scheduled no reload")
	}
	disconnect()
	if reloadTimer != nil {
		t.Fatal("disconnect left the reload scheduled")
	}
}

// event delivers a node event the way the socket reader does and flushes it.
func event(action, id string, pwr int) {
	onNodeEvent(meshcentral.NodeEvent{Action: action, NodeID: id, Pwr: pwr})
	flushNodeEvents()
}

func TestNodeEventsUpdateInPlace(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	setConnected(true)
	offlineChk.SetChecked(true)
	setDevices([]meshcentral.Device{
		{Id: "a", MeshID: "m", Group: "G", Name: "b-host", Conn: 1, Pwr: 1},
		{Id: "b", MeshID: "m", Group: "G", Name: "c-host", Conn: 1, Pwr: 1},
	})
	deviceTree.Select("b")

	// Going offline with offline devices shown only redraws rows.
	event("nodeconnect", "b", 0)
	if devices[deviceIdx["b"]].Pwr != 0 {
		t.Fatal("offline event not applied")
	}
	event("nodeconnect", "b", 1)

	// A rename keeps the live state and moves the device, an add inserts one,
	// neither reloads the list.
	onNodeEvent(meshcentral.NodeEvent{Action: "changenode", NodeID: "a", Device: &meshcentral.Device{Id: "a", MeshID: "m", Group: "G", Name: "d-host"}})
	onNodeEvent(meshcentral.NodeEvent{Action: "addnode", NodeID: "c", Device: &meshcentral.Device{Id: "c", MeshID: "m", Group: "G", Name: "a-host"}})
	flushNodeEvents()
	if got := groupChildren["m"]; !slices.Equal(got, []string{"c", "b", "a"}) {
		t.Fatalf("tree order %v after rename and add", got)
	}
	if d := devices[deviceIdx["a"]]; d.Conn != 1 || d.Pwr != 1 {
		t.Fatalf("change event reset the connection state: %+v", d)
	}
	if reloadTimer != nil {
		t.Fatal("event carrying the device scheduled a reload")
	}
	if groupLabels["m"][1] != "2 of 3 online" {
		t.Fatalf("group label %q", groupLabels["m"][1])
	}
	if selectedID != "b" {
		t.Fatal("update dropped the selection")
	}

	event("removenode", "c", 0)
	if got := groupChildren["m"]; !slices.Equal(got, []string{"b", "a"}) || devices[deviceIdx["a"]].Id != "a" {
		t.Fatalf("tree %v after remove", got)
	}
	event("removenode", "b", 0)
	if selectedID != "" {
		t.Fatal("removed device still selected")
	}
}

func TestLogKeepsNewestLines(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	win.Resize(fyne.NewSize(1100, 700))
	logLines = nil
	check := func() {
		t.Helper()
		last := false
		for l, id := range logRows {
			if l.Text != logLines[id] {
				t.Fatalf("row %d shows %q, want %q", id, l.Text, logLines[id])
			}
			last = last || id == len(logLines)-1
		}
		if !last {
			t.Fatal("newest line not shown")
		}
	}

	// Past logMax the rows are relabeled in place instead of refreshed.
	for i := range logMax + 10 {
		logf("line %d", i)
	}
	if len(logLines) != logMax || !strings.HasSuffix(logLines[0], "line 10") {
		t.Fatalf("%d lines, first %q", len(logLines), logLines[0])
	}
	check()
	logf("newest")
	check()
}

func TestSearchKeepsCollapsedGroups(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	setConnected(true)
	var devs []meshcentral.Device
	for i := range 60 {
		devs = append(devs, meshcentral.Device{Id: fmt.Sprintf("node//%d", i), MeshID: fmt.Sprintf("mesh//%d", i%2), Group: fmt.Sprintf("G%d", i%2), Name: fmt.Sprintf("host-%02d", i), Pwr: 1})
	}
	offlineChk.SetChecked(true)
	setDevices(slices.Clone(devs))
	searchEntry.SetText("host")
	if !deviceTree.IsBranchOpen("mesh//0") {
		t.Fatal("search didn't open the matching groups")
	}
	deviceTree.CloseBranch("mesh//0")

	// A burst big enough for a full refresh, then a reload, keep it collapsed.
	for i := range 60 {
		onNodeEvent(meshcentral.NodeEvent{Action: "nodeconnect", NodeID: fmt.Sprintf("node//%d", i), Pwr: 0})
	}
	flushNodeEvents()
	if deviceTree.IsBranchOpen("mesh//0") {
		t.Fatal("collapsed group reopened by an event burst")
	}
	setDevices(slices.Clone(devs))
	if deviceTree.IsBranchOpen("mesh//0") {
		t.Fatal("collapsed group reopened by a reload")
	}
}
