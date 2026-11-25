package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

type powerAction struct {
	action           meshcentral.PowerAction
	label, icon, ask string
}

// powerActions are the agent's, in the order the Power menu offers them.
var powerActions = []powerAction{
	{meshcentral.Sleep, "Sleep", "moon", "Put %s to sleep? Its agent is offline until it wakes."},
	{meshcentral.Restart, "Restart", "arrow-rotate-right", "Restart %s? Unsaved work on it is lost."},
	{meshcentral.PowerOff, "Power off", "power-off", "Power off %s? It can only be woken again if it supports wake-on-LAN."},
}

// addPowerMenu adds a Power submenu to parent, to wake d or have its agent
// put it to sleep, restart it or power it off, which need the agent
// connected and ask first. The client doesn't know the user's rights: a
// refused wake says so, a refused power action does nothing.
func addPowerMenu(parent *qt.QMenu, d meshcentral.Device) *qt.QMenu {
	m := parent.AddMenu2(icon("power-off"), "Power")
	name := deviceName(d)
	m.AddAction2(icon("sun"), "Wake up").OnTriggered(func() { wake(d.Id, name) })
	for _, p := range powerActions {
		a := m.AddAction2(icon(p.icon), p.label+"…")
		a.SetEnabled(d.Conn&1 != 0)
		a.OnTriggered(func() { askPower(d, p) })
	}
	return m
}

// askPower confirms a power action on d, then asks its agent for it.
func askPower(d meshcentral.Device, p powerAction) {
	name := deviceName(d)
	confirm(p.label, fmt.Sprintf(p.ask, name), func() {
		if err := meshcentral.Power(d.Id, p.action); err != nil {
			showError(err)
			return
		}
		logf("%s: asked to %s", name, strings.ToLower(p.label))
	})
}

// wake logs what the server did to wake the device, or shows why it didn't.
func wake(nodeID, name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		result, err := meshcentral.Wake(ctx, nodeID)
		mainthread.Start(func() {
			if err != nil {
				showError(fmt.Errorf("wake %s: %w", name, err))
				return
			}
			logf("%s: %s", name, result)
		})
	}()
}
