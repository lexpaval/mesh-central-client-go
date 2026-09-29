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
