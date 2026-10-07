package main

import (
	"cmp"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"time"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// The Recent tab lists the shells, files, routes and commands used before, per
// profile, most used and most recent first, to do again in one click. On a
// large server most devices are someone else's, this keeps one's own close.

// recentAction is a shell, files tab, route or command used on a device.
type recentAction struct {
	Kind        string // "shell", "files", "route" or "command"
	NodeID      string
	Device      string // its name when last used, for devices not loaded
	Protocol    int    `json:",omitempty"` // shell: 1 default, 6 PowerShell
	BindAddress string `json:",omitempty"` // route, as entered
	Target      string `json:",omitempty"`
	LocalPort   int    `json:",omitempty"` // route, 0 for automatic
	RemotePort  int    `json:",omitempty"`
	Command     string `json:",omitempty"`
	AsUser      bool   `json:",omitempty"`
	Uses        int
	Last        int64 // Unix time
	Pinned      bool  `json:",omitempty"` // kept on top and never dropped
}

// id is the action without its bookkeeping, equal for the same action.
func (a recentAction) id() recentAction {
	a.Device, a.Uses, a.Last, a.Pinned = "", 0, 0, false
	return a
}

// score ranks by use, each use counting half as much a week later.
func (a *recentAction) score(now time.Time) float64 {
	age := now.Sub(time.Unix(a.Last, 0))
	return float64(a.Uses) * math.Exp2(-age.Hours()/recentHalfLife.Hours())
}

const (
	recentHalfLife = 7 * 24 * time.Hour
	recentKeep     = 30 // unpinned actions kept, the lowest scored go first
)

var (
	recent     []*recentAction // pinned first, then by score
	recentBox  *qt.QVBoxLayout
	recentHint *qt.QLabel
	recentRows []*recentRow
	timeNow    = time.Now // tests age actions
)

func recentKey(profile string) string { return "recent/" + profile }

func loadRecent() {
	recent = nil
	json.Unmarshal([]byte(pref(recentKey(session.profile))), &recent)
	sortRecent()
	rebuildRecent()
}

func saveRecent() {
	if len(recent) == 0 {
		setPref(recentKey(session.profile), "")
		return
	}
	b, _ := json.Marshal(recent)
	setPref(recentKey(session.profile), string(b))
}

func sortRecent() {
	now := timeNow()
	slices.SortStableFunc(recent, func(a, b *recentAction) int {
		if a.Pinned != b.Pinned {
			if a.Pinned {
				return -1
			}
			return 1
		}
		return cmp.Compare(b.score(now), a.score(now))
	})
}

// recordRecent counts a use of a, adding it if new. The lowest scored
// unpinned actions past recentKeep are dropped, never the one just used.
func recordRecent(a recentAction) {
	if session.profile == "" {
		return
	}
	a.Last = timeNow().Unix()
	used := &a
	if i := slices.IndexFunc(recent, func(x *recentAction) bool { return x.id() == a.id() }); i >= 0 {
		used = recent[i]
		used.Uses++
		used.Last, used.Device = a.Last, a.Device
	} else {
		a.Uses = 1
		recent = append(recent, used)
	}
	sortRecent()
	excess := -recentKeep
	for _, x := range recent {
		if !x.Pinned {
			excess++
		}
	}
	for i := len(recent) - 1; i >= 0 && excess > 0; i-- {
		if x := recent[i]; !x.Pinned && x != used {
			recent = slices.Delete(recent, i, i+1)
			excess--
		}
	}
	saveRecent()
	rebuildRecent()
}

// recentRow is an action in the Recent tab.
type recentRow struct {
	a    *recentAction
	w    *qt.QWidget
	text *rowText
}

// rebuildRecent recreates the rows after the actions changed.
func rebuildRecent() {
	for _, rr := range recentRows {
		rr.w.Hide()
		rr.w.DeleteLater()
	}
	recentRows = nil
	for i, a := range recent {
		rr := &recentRow{a: a, w: qt.NewQWidget2(), text: newRowText(false)}
		do := qt.NewQPushButton4(icon(recentDoIcon(a)), recentDoText(a))
		pin := qt.NewQPushButton2()
		pin.SetFlat(true)
		if a.Pinned {
			pin.SetIcon(icon("star"))
			pin.SetToolTip("Unpin")
		} else {
			pin.SetIcon(icon("star-regular"))
			pin.SetToolTip("Pin to the top")
		}
		forget := qt.NewQPushButton2()
		forget.SetFlat(true)
		forget.SetIcon(icon("xmark"))
		forget.SetToolTip("Remove from Recent")
		row := hbox(false, pin.QWidget, rr.text.w, do.QWidget, forget.QWidget)
		row.SetStretch(1, 1)
		rr.w.SetLayout(row.QLayout)
		do.OnClicked(func() { replayRecent(a) })
		pin.OnClicked(func() {
			a.Pinned = !a.Pinned
			sortRecent()
			saveRecent()
			rebuildRecent()
		})
		forget.OnClicked(func() {
			recent = slices.DeleteFunc(recent, func(x *recentAction) bool { return x == a })
			saveRecent()
			rebuildRecent()
		})
		recentBox.InsertWidget(i, rr.w)
		recentRows = append(recentRows, rr)
		rr.text.set(recentRowData(a))
	}
	if connected {
		recentHint.SetText("Nothing yet. Shells, files, routes and commands you use show up here, most used first.")
	} else {
		recentHint.SetText("Connect to see the shells, files, routes and commands you use most.")
	}
	recentHint.SetVisible(len(recent) == 0)
}

// refreshRecentRows redraws the rows, for device names, state and ages.
func refreshRecentRows() {
	for _, rr := range recentRows {
		rr.text.set(recentRowData(rr.a))
	}
}

func recentDoText(a *recentAction) string {
	switch {
	case a.Kind == "command":
		return "Run…"
	case a.Kind == "route" && openCmd(&meshcentral.Route{RemotePort: a.RemotePort}) == nil:
		return "Start"
	}
	return "Open"
}

func recentDoIcon(a *recentAction) string {
	if a.Kind == "command" || recentDoText(a) == "Start" {
		return "play"
	}
	return "arrow-up-right-from-square"
}

func recentRowData(a *recentAction) rowData {
	name, off := a.Device, false
	if i, ok := deviceIdx[a.NodeID]; ok {
		name, off = deviceName(devices[i]), devices[i].Pwr == 0
	}
	var what, detail, ic string
	switch a.Kind {
	case "shell":
		what, ic = "Shell", "terminal"
		if a.Protocol == 6 {
			what = "PowerShell"
		}
	case "files":
		what, ic = "Files", "folder"
	case "route":
		what, ic = service(a.RemotePort)
		detail = fmt.Sprintf("port %d", a.RemotePort)
		if a.Target != "" {
			detail = fmt.Sprintf("%s:%d", a.Target, a.RemotePort)
		}
	case "command":
		what, ic, detail = "Run command", "play", a.Command
	}
	d := rowData{title: name + " · " + what, icon: ic, iconOff: off, muted: off}
	d.sub = fmt.Sprintf("%d× · %s", a.Uses, ago(timeNow().Sub(time.Unix(a.Last, 0))))
	if detail != "" {
		d.sub = detail + " · " + d.sub
	}
	if off {
		d.sub += " · device offline"
	}
	return d
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%d h ago", int(d.Hours()))
	case d < 48*time.Hour:
		return "yesterday"
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

// replayRecent does a again: reopens the shell, starts and opens the
// route, or opens the command filled in, never running it unasked.
func replayRecent(a *recentAction) {
	i, ok := deviceIdx[a.NodeID]
	if !ok {
		if session.loading {
			showInfo("Loading", "The device list is still loading.")
		} else {
			showInfo("Device not found", a.Device+" isn't in this server's device list any more.")
		}
		return
	}
	d := devices[i]
	switch a.Kind {
	case "shell":
		openShell(d, a.Protocol)
	case "files":
		openFilesTab(d)
	case "route":
		useRoute(deviceName(d), &meshcentral.Route{NodeID: d.Id, BindAddress: a.BindAddress, LocalPort: a.LocalPort, Target: a.Target, RemotePort: a.RemotePort}, true)
	case "command":
		showRunCommandFor(d, a.Command, a.AsUser)
	}
}
