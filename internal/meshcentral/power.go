package meshcentral

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// PowerAction is what the agent does to its device, by the server's
// actiontype.
type PowerAction int

const (
	PowerOff PowerAction = 2
	Restart  PowerAction = 3
	Sleep    PowerAction = 4
)

// Power asks nodeID's agent for a power action. Nothing confirms it: the
// server answers the same whether or not the user has the "Reset/Off" right
// and the agent is connected.
func Power(nodeID string, a PowerAction) error {
	b, _ := json.Marshal(map[string]any{"action": "poweraction", "nodeids": []string{nodeID}, "actiontype": int(a)})
	return send(b)
}

// Wake has the server and the online agents in the user's groups send
// wake-on-LAN packets to nodeID's MAC addresses, it returns what the server
// says it did. The server doesn't answer for some devices (power strips), so
// ctx needs a deadline.
func Wake(ctx context.Context, nodeID string) (string, error) {
	b, _ := json.Marshal(map[string]any{"action": "wakedevices", "nodeids": []string{nodeID}, "responseid": nodeID})
	m, err := request(ctx, replyKey("wakedevices", nodeID), b)
	if err != nil {
		return "", err
	}
	result, _ := m["result"].(string)
	switch {
	case result == "Invalid nodeid":
		return "", errors.New("refused, waking needs the Wake devices right")
	case strings.HasPrefix(result, "Used 0 "):
		return "", errors.New("no online device in your groups can send the wake packet")
	case !strings.HasPrefix(result, "Used "):
		return "", fmt.Errorf("not woken: %s", result)
	}
	return result, nil
}
