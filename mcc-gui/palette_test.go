package main

import (
	"slices"
	"strings"
	"testing"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// press hands the palette a key as its search field gets it.
func press(t *testing.T, key qt.Key) {
	t.Helper()
	if quick == nil {
		t.Fatal("no palette")
	}
	e := qt.NewQKeyEvent(qt.QEvent__KeyPress, int(key), qt.NoModifier)
	defer e.Delete()
	if !quick.key(e) {
		quick.edit.Event(e.QEvent)
	}
}

func titles(rows []paletteRow) []string {
	var s []string
	for _, r := range rows {
		s = append(s, r.title)
	}
	return s
}

// Ctrl+K lists Recent, then the devices a search finds, the most used and
// online ones first, and a device's actions on Tab. Enter does the row.
func TestPalette(t *testing.T) {
	ui(t, func() {
		openPalette()
		if quick.hint.Text() != "Connect to search the devices" || len(quick.rows) != 0 {
			t.Errorf("disconnected: %q, %d rows", quick.hint.Text(), len(quick.rows))
		}
		quick.w.Close()

		session.profile = "p"
		setConnected(true)
		setDevices([]meshcentral.Device{
			{Id: "a", MeshID: "m", Group: "Lab", Name: "lab-a", Pwr: 1, Conn: 1, OS: "Ubuntu"},
			{Id: "b", MeshID: "m", Group: "Lab", Name: "lab-b", Pwr: 0, OS: "Ubuntu"},
			{Id: "c", MeshID: "m", Group: "Lab", Name: "lab-c", Pwr: 1, Conn: 1, OS: "Windows 11"},
			{Id: "d", MeshID: "m", Group: "Office", Name: "desk", Pwr: 1, Conn: 1},
		})
		recordRecent(recentAction{Kind: "files", NodeID: "c", Device: "lab-c"})

		openPalette()
		if len(quick.rows) != 1 || !strings.HasPrefix(quick.rows[0].title, "lab-c · Files") {
			t.Errorf("empty search lists %q", titles(quick.rows))
		}
		quick.edit.SetText("lab")
		if got := titles(quick.rows); !slices.Equal(got, []string{"lab-c", "lab-a", "lab-b"}) {
			t.Errorf("search lists %q", got)
		}
		quick.edit.SetText("g:office")
		if got := titles(quick.rows); !slices.Equal(got, []string{"desk"}) {
			t.Errorf("g:office lists %q", got)
		}

		// Tab lists the Windows device's actions, typing filters them,
		// Left goes back to the search as it was.
		quick.edit.SetText("lab")
		press(t, qt.Key_Tab)
		got := titles(quick.rows)
		for _, want := range []string{"Command prompt", "PowerShell", "Files", "Desktop", "SSH route", "RDP route", "Wake up", "Restart…"} {
			if !slices.Contains(got, want) {
				t.Errorf("actions %q lack %q", got, want)
			}
		}
		quick.edit.SetText("desk")
		if got := titles(quick.rows); !slices.Equal(got, []string{"Desktop"}) {
			t.Errorf("desk in the actions lists %q", got)
		}
		quick.edit.SetText("")
		press(t, qt.Key_Left)
		if quick.device != nil || quick.edit.Text() != "lab" || len(quick.rows) != 3 {
			t.Errorf("back: device %v, text %q, %d rows", quick.device, quick.edit.Text(), len(quick.rows))
		}

		// The offline device's agent can't sleep, restart or power off.
		press(t, qt.Key_Down)
		press(t, qt.Key_Down)
		press(t, qt.Key_Tab)
		if got := titles(quick.rows); slices.Contains(got, "Restart…") || !slices.Contains(got, "Wake up") || got[0] != "Shell" {
			t.Errorf("offline device's actions %q", got)
		}

		// Enter runs it and closes the palette.
		n := tabs.Count()
		press(t, qt.Key_Return)
		if quick != nil || tabs.Count() != n+1 {
			t.Errorf("palette open %v, %d tabs (was %d)", quick != nil, tabs.Count(), n)
		}
		openPalette()
		press(t, qt.Key_Escape)
		if quick != nil {
			t.Error("Esc left it open")
		}
	})
}

// A shell keeps Ctrl+K, the search fields let it open the palette.
func TestPaletteKey(t *testing.T) {
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "a", MeshID: "m", Name: "a", Pwr: 1}})
		openShell(devices[0], 1)
		override := func(w *qt.QObject, mods qt.KeyboardModifier) bool {
			e := qt.NewQKeyEvent(qt.QEvent__ShortcutOverride, int(qt.Key_K), mods)
			defer e.Delete()
			e.Ignore()
			qt.QCoreApplication_SendEvent(w, e.QEvent)
			return e.IsAccepted()
		}
		if !override(shells[0].t.w.QObject, ctrlModifier) {
			t.Error("the shell gave up Ctrl+K")
		}
		if override(shells[0].t.w.QObject, ctrlModifier|qt.ShiftModifier) {
			t.Error("the shell kept Ctrl+Shift+K")
		}
		if override(searchEntry.QObject, qt.ControlModifier) {
			t.Error("the device search kept Ctrl+K")
		}
	})
}
