package meshcentral

import (
	"encoding/json"
	"fmt"
	"os"
	"time"
)

// deviceQueryTimeout bounds how long GetDevices waits for the server's
// "nodes" response, so a request that never answers (dropped connection,
// permission error) doesn't hang the CLI forever.
const deviceQueryTimeout = 15 * time.Second

func handleNodesCommand(command map[string]interface{}) {
	if settings.debug {
		fmt.Println("Received nodes command")
	}
	var devices []Device
	nodeGroups := command["nodes"].(map[string]interface{})
	for meshID, nodeGroup := range nodeGroups {
		nodes := nodeGroup.([]interface{})
		for _, node := range nodes {
			nodeMap := node.(map[string]interface{})

			// Check for nil values and set defaults
			if nodeMap["name"] == nil {
				nodeMap["name"] = ""
			}
			if nodeMap["rname"] == nil {
				nodeMap["rname"] = ""
			}
			if nodeMap["osdesc"] == nil {
				nodeMap["osdesc"] = ""
			}
			if nodeMap["ip"] == nil {
				nodeMap["ip"] = ""
			}
			if nodeMap["pwr"] == nil {
				nodeMap["pwr"] = 0.0
			}
			if nodeMap["conn"] == nil {
				nodeMap["conn"] = 0.0
			}

			device := Device{
				Id:          nodeMap["_id"].(string),
				Name:        nodeMap["rname"].(string),
				DisplayName: nodeMap["name"].(string),
				OS:          nodeMap["osdesc"].(string),
				IP:          nodeMap["ip"].(string),
				Icon:        int(nodeMap["icon"].(float64)),
				Conn:        int(nodeMap["conn"].(float64)),
				Pwr:         int(nodeMap["pwr"].(float64)),
				MeshID:      meshID,
				Group:       settings.groups[meshID],
			}
			devices = append(devices, device)
		}
	}

	settings.Devices = devices
	settings.DeviceQueryState = 0
	if settings.deviceChan != nil {
		close(settings.deviceChan)
		settings.deviceChan = nil
	}
}

func handleMeshesCommand(command map[string]interface{}) {
	groups := map[string]string{}
	meshes, _ := command["meshes"].([]interface{})
	for _, m := range meshes {
		mesh, _ := m.(map[string]interface{})
		id, _ := mesh["_id"].(string)
		name, _ := mesh["name"].(string)
		groups[id] = name
	}
	settings.groups = groups
	if settings.groupChan != nil {
		close(settings.groupChan)
		settings.groupChan = nil
	}
}

// handleEventCommand forwards device events, which the server sends to every
// session that can see the device, to OnNodeEvent.
func handleEventCommand(command map[string]interface{}) {
	ev, ok := command["event"].(map[string]interface{})
	if !ok || OnNodeEvent == nil {
		return
	}
	action, _ := ev["action"].(string)
	switch action {
	case "nodeconnect", "addnode", "removenode", "changenode", "createmesh", "deletemesh", "meshchange":
		nodeID, _ := ev["nodeid"].(string)
		conn, _ := ev["conn"].(float64)
		pwr, _ := ev["pwr"].(float64)
		OnNodeEvent(action, nodeID, int(conn), int(pwr))
	}
}

func GetDevices() []Device {
	// Group names come from "meshes", fetched first so nodes can be labelled.
	// A failure only leaves devices without group names.
	settings.groupChan = make(chan struct{})
	if err := send([]byte(`{"action":"meshes"}`)); err == nil {
		select {
		case <-settings.groupChan:
		case <-time.After(deviceQueryTimeout):
			fmt.Fprintln(os.Stderr, "Timed out waiting for device groups from server.")
		}
	}

	settings.DeviceQueryState = 1
	settings.deviceChan = make(chan struct{})
	if err := send([]byte(`{"action":"nodes"}`)); err != nil {
		fmt.Fprintln(os.Stderr, "Unable to request device list:", err)
		return nil
	}

	select {
	case <-settings.deviceChan:
	case <-time.After(deviceQueryTimeout):
		fmt.Fprintln(os.Stderr, "Timed out waiting for device list from server.")
	}

	return settings.Devices
}

// RunCommand dispatches a shell command to nodeID and returns as soon as the
// server has accepted it, without waiting for the agent to run it or for any
// output (fire and forget).
func RunCommand(nodeID, command string, runAsUser int) error {
	payload, err := json.Marshal(map[string]interface{}{
		"action":     "runcommands",
		"type":       0, // 0 = cmd/shell (native shell on both Windows and Linux agents)
		"nodeids":    []string{nodeID},
		"cmds":       command,
		"runAsUser":  runAsUser,
		"responseid": "meshctrl",
	})
	if err != nil {
		return err
	}
	return send(payload)
}
