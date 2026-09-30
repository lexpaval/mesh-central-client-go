package meshcentral

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"
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
	nodeGroups, _ := command["nodes"].(map[string]interface{})
	for meshID, nodeGroup := range nodeGroups {
		nodes, _ := nodeGroup.([]interface{})
		for _, node := range nodes {
			if nodeMap, ok := node.(map[string]interface{}); ok {
				if d, ok := parseNode(meshID, nodeMap); ok {
					devices = append(devices, d)
				}
			}
		}
	}

	settings.Devices = devices
	settings.DeviceQueryState = 0
	if settings.deviceChan != nil {
		close(settings.deviceChan)
		settings.deviceChan = nil
	}
}

// parseNode reads a node as the server sends it in "nodes" and in events,
// missing fields are left empty. ok is false without a node ID.
func parseNode(meshID string, node map[string]interface{}) (d Device, ok bool) {
	str := func(k string) string { s, _ := node[k].(string); return s }
	num := func(k string) int { f, _ := node[k].(float64); return int(f) }
	d = Device{
		Id:          str("_id"),
		Name:        str("rname"),
		DisplayName: str("name"),
		OS:          str("osdesc"),
		IP:          str("ip"),
		Icon:        num("icon"),
		Conn:        num("conn"),
		Pwr:         num("pwr"),
		MeshID:      meshID,
		Group:       settings.groups[meshID],
	}
	return d, d.Id != ""
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
	e := NodeEvent{}
	e.Action, _ = ev["action"].(string)
	switch e.Action {
	case "nodeconnect", "addnode", "removenode", "changenode", "createmesh", "deletemesh", "meshchange":
	default:
		return
	}
	e.NodeID, _ = ev["nodeid"].(string)
	conn, _ := ev["conn"].(float64)
	pwr, _ := ev["pwr"].(float64)
	e.Conn, e.Pwr = int(conn), int(pwr)
	if node, ok := ev["node"].(map[string]interface{}); ok && (e.Action == "addnode" || e.Action == "changenode") {
		// Without its group the device can't be placed, the list gets reloaded.
		meshID, _ := node["meshid"].(string)
		if d, ok := parseNode(meshID, node); ok && meshID != "" {
			e.Device = &d
			e.NodeID = d.Id // device edits from the web UI only set it in node
		}
	}
	OnNodeEvent(e)
}

// devicesMu serializes GetDevices, the replies carry no request ID so only
// one query can be waiting at a time.
var devicesMu sync.Mutex

func GetDevices() []Device {
	devicesMu.Lock()
	defer devicesMu.Unlock()

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
