package meshcentral

import (
	"encoding/json"
	"testing"
)

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
