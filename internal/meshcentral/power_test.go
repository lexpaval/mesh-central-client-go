package meshcentral

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// Power actions go to the server as MeshCentral's web UI sends them, a wake
// returns what the server did and turns its refusals into errors.
func TestPower(t *testing.T) {
	got := make(chan map[string]any, 1)
	controlServer(t, func(ws *websocket.Conn) {
		for {
			var req map[string]any
			if ws.ReadJSON(&req) != nil {
				return
			}
			if req["action"] == "poweraction" {
				got <- req
				continue
			}
			result := map[string]string{
				"node//lan":    "Used 2 device(s) to send wake packets",
				"node//alone":  "Used 0 device(s) to send wake packets",
				"node//nonet":  "No network information for this device",
				"node//denied": "Invalid nodeid",
			}[req["nodeids"].([]any)[0].(string)]
			if result != "" {
				ws.WriteJSON(map[string]any{"action": "wakedevices", "responseid": req["responseid"], "result": result})
			}
		}
	}, handleReply)

	if err := Power("node//a", Restart); err != nil {
		t.Fatal(err)
	}
	if req := <-got; req["actiontype"] != float64(3) || req["nodeids"].([]any)[0] != "node//a" {
		t.Errorf("sent %v", req)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if r, err := Wake(ctx, "node//lan"); err != nil || !strings.HasPrefix(r, "Used 2") {
		t.Errorf("wake: %q, %v", r, err)
	}
	for id, want := range map[string]string{"node//alone": "no online device", "node//nonet": "No network information", "node//denied": "Wake devices right"} {
		if _, err := Wake(ctx, id); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: %v", id, err)
		}
	}
	short, cancel2 := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel2()
	if _, err := Wake(short, "node//pdu"); err == nil {
		t.Error("no error without an answer")
	}
}
