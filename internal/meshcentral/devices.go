package meshcentral

import (
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/gorilla/websocket"
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
	for _, nodeGroup := range nodeGroups {
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

func GetDevices() []Device {
	settings.DeviceQueryState = 1
	settings.deviceChan = make(chan struct{})
	settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(`{"action":"nodes"}`))

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
	return settings.WebSocket.WriteMessage(websocket.TextMessage, payload)
}
