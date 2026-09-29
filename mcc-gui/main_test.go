package main

import (
	"testing"
	"time"

	"fyne.io/fyne/v2/test"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

func TestDeviceRowTaps(t *testing.T) {
	test.NewTempApp(t)
	win = test.NewTempWindow(t, buildUI())
	setConnected(true)
	devices = []meshcentral.Device{
		{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1},
		{Id: "b", MeshID: "m", Group: "G", Name: "b", Pwr: 1},
	}
	applyFilter()
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
	devices = []meshcentral.Device{{Id: "a", MeshID: "m", Group: "G", Name: "a", Pwr: 1}}
	logLines = nil

	// Events the server pushes while connect is still loading are ignored.
	onNodeEvent("nodeconnect", "a", 0, 0)
	onNodeEvent("changenode", "a", 0, 0)
	if devices[0].Pwr != 1 || len(logLines) != 0 || reloadTimer != nil {
		t.Fatalf("event applied while disconnected: pwr=%d log=%v reload=%v", devices[0].Pwr, logLines, reloadTimer != nil)
	}

	// Connected but the first list is still loading, which reflects them.
	setConnected(true)
	session.loading = true
	onNodeEvent("nodeconnect", "a", 0, 0)
	if devices[0].Pwr != 1 {
		t.Fatal("event applied while the device list was loading")
	}

	session.loading = false
	onNodeEvent("nodeconnect", "a", 0, 0)
	if devices[0].Pwr != 0 {
		t.Fatal("event not applied once loaded")
	}

	// A reload scheduled before a disconnect doesn't fire after it.
	onNodeEvent("changenode", "a", 0, 0)
	if reloadTimer == nil {
		t.Fatal("change event scheduled no reload")
	}
	disconnect()
	if reloadTimer != nil {
		t.Fatal("disconnect left the reload scheduled")
	}
}
