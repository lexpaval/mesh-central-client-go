package meshcentral

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
)

func deviceQueryServer(t *testing.T, empty bool) *atomic.Bool {
	t.Helper()
	answerNodes := new(atomic.Bool)
	answerNodes.Store(true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		for {
			var request map[string]interface{}
			if ws.ReadJSON(&request) != nil {
				return
			}
			switch request["action"] {
			case "meshes":
				ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"meshes","meshes":[{"_id":"m","name":"Lab"}]}`))
			case "nodes":
				if answerNodes.Load() {
					if empty {
						ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"nodes","nodes":{}}`))
					} else {
						ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"nodes","nodes":{"m":[{"_id":"n","name":"Original"}]}}`))
					}
				}
			}
		}
	}))
	t.Cleanup(server.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	settings.wsMu.Lock()
	settings.WebSocket = ws
	settings.wsMu.Unlock()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			var response map[string]interface{}
			if ws.ReadJSON(&response) != nil {
				return
			}
			switch response["action"] {
			case "meshes":
				handleMeshesCommand(response)
			case "nodes":
				handleNodesCommand(response)
			}
		}
	}()
	t.Cleanup(func() {
		ws.Close()
		<-done
		settings.wsMu.Lock()
		settings.WebSocket = nil
		settings.wsMu.Unlock()
		settings.deviceMu.Lock()
		settings.Devices, settings.groups = nil, nil
		settings.deviceMu.Unlock()
	})
	return answerNodes
}

func TestConcurrentDeviceQueries(t *testing.T) {
	deviceQueryServer(t, false)
	var callers sync.WaitGroup
	for range 4 {
		callers.Go(func() {
			for range 20 {
				devices := GetDevices()
				if len(devices) != 1 || devices[0].Id != "n" || devices[0].Group != "Lab" {
					t.Errorf("query returned %v", devices)
					return
				}
			}
		})
	}
	callers.Wait()
}

func TestDeviceQueryReturnsOwnedSnapshot(t *testing.T) {
	deviceQueryServer(t, false)
	devices := GetDevices()
	if len(devices) != 1 {
		t.Fatalf("query returned %v", devices)
	}
	devices[0].DisplayName = "Changed by caller"
	settings.deviceMu.Lock()
	defer settings.deviceMu.Unlock()
	if settings.Devices[0].DisplayName != "Original" {
		t.Error("caller changed the shared device snapshot")
	}
}

func TestDeviceQueryTimeoutReturnsNoCachedDevices(t *testing.T) {
	answerNodes := deviceQueryServer(t, false)
	if devices := GetDevices(); len(devices) != 1 {
		t.Fatalf("initial query returned %v", devices)
	}
	answerNodes.Store(false)
	if devices, err := QueryDevices(); devices != nil || err == nil {
		t.Fatalf("timed-out query returned %v, %v; want no devices and an error", devices, err)
	}
	answerNodes.Store(true)
	if devices := GetDevices(); len(devices) != 1 {
		t.Fatalf("query after timeout returned %v", devices)
	}
}

func TestDeviceQueryEmptyList(t *testing.T) {
	deviceQueryServer(t, true)
	if devices, err := QueryDevices(); len(devices) != 0 || err != nil {
		t.Fatalf("empty list returned %v, %v; want no devices and no error", devices, err)
	}
}

func TestDeviceQueryWriteFailure(t *testing.T) {
	deviceQueryServer(t, false)
	if devices := GetDevices(); len(devices) != 1 {
		t.Fatalf("initial query returned %v", devices)
	}
	settings.WebSocket.Close()
	if devices, err := QueryDevices(); devices != nil || err == nil {
		t.Fatalf("failed query returned %v, %v; want no devices and an error", devices, err)
	}
}

func TestHandleEventCommand(t *testing.T) {
	settings.groups = map[string]string{"mesh//1": "Lab"}
	var got []NodeEvent
	OnNodeEvent = func(e NodeEvent) { got = append(got, e) }
	defer func() { OnNodeEvent, settings.groups = nil, nil }()

	for _, msg := range []string{
		`{"action":"event","event":{"action":"nodeconnect","nodeid":"node//1","conn":1,"pwr":1}}`,
		// Edits from the web UI only name the device inside node.
		`{"action":"event","event":{"action":"changenode","node":{"_id":"node//2","meshid":"mesh//1","name":"Bench","rname":"bench","osdesc":"Fedora","ip":"192.0.2.1"}}}`,
		`{"action":"event","event":{"action":"changenode","nodeid":"node//3"}}`,
		`{"action":"event","event":{"action":"addnode","node":{"_id":"node//4","name":"No group"}}}`,
		`{"action":"event","event":{"action":"servertimelinestats"}}`,
	} {
		var cmd map[string]interface{}
		if err := json.Unmarshal([]byte(msg), &cmd); err != nil {
			t.Fatal(err)
		}
		handleEventCommand(cmd)
	}

	if len(got) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(got), got)
	}
	if e := got[0]; e.Action != "nodeconnect" || e.NodeID != "node//1" || e.Conn != 1 || e.Pwr != 1 || e.Device != nil {
		t.Errorf("nodeconnect: %+v", e)
	}
	want := Device{Id: "node//2", Name: "bench", DisplayName: "Bench", OS: "Fedora", IP: "192.0.2.1", MeshID: "mesh//1", Group: "Lab"}
	if e := got[1]; e.NodeID != "node//2" || e.Device == nil || *e.Device != want {
		t.Errorf("changenode with node: %+v %+v", e, e.Device)
	}
	if e := got[2]; e.NodeID != "node//3" || e.Device != nil {
		t.Errorf("changenode without node: %+v", e)
	}
	if e := got[3]; e.Device != nil {
		t.Errorf("addnode without a group: %+v", e.Device)
	}
}
