package meshcentral

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// detailsServer answers like MeshCentral for the devices in nodes, by ID,
// with sys and net their getsysinfo and getnetworkinfo replies. Others get
// an empty list and noinfo.
func detailsServer(t *testing.T, nodes map[string]string, sys, net map[string]string) {
	t.Helper()
	controlServer(t, func(ws *websocket.Conn) {
		for {
			var req struct{ Action, ID, NodeID, ResponseID string }
			if ws.ReadJSON(&req) != nil {
				return
			}
			reply := map[string]any{"action": req.Action, "nodeid": req.NodeID}
			fill := func(s string) {
				if s == "" {
					reply["noinfo"] = true
				} else {
					json.Unmarshal([]byte(s), &reply)
				}
			}
			switch req.Action {
			case "nodes":
				reply = map[string]any{"action": "nodes", "responseid": req.ResponseID, "nodes": map[string]any{}}
				if n, ok := nodes[req.ID]; ok {
					var node any
					json.Unmarshal([]byte(n), &node)
					reply["nodes"] = map[string]any{"mesh//g": []any{node}}
				}
			case "getsysinfo":
				fill(sys[req.NodeID])
			case "getnetworkinfo":
				fill(net[req.NodeID])
			case "lastconnect":
				fill(map[string]string{"node//win": `{"time": 1793000000000, "addr": "198.51.100.7:51234"}`}[req.NodeID])
			}
			ws.WriteJSON(reply)
		}
	}, handleReply)
}

const (
	winNode = `{"_id": "node//win", "name": "Site A - Office PC", "rname": "DESKTOP-EXAMPLE1", "desc": "Front desk",
		"osdesc": "Microsoft Windows 11 Pro - 24H2/26100", "tags": ["kiosk", "floor-2"], "users": ["EXAMPLE\\alice"],
		"lusers": ["EXAMPLE\\alice"], "conn": 9, "agent": {"id": 4, "ver": 0, "root": true}, "firstconnect": 1700000000000,
		"wsc": {"antiVirus": "OK", "autoUpdate": "OK", "firewall": "BAD"},
		"av": [{"product": "Windows Defender", "enabled": true, "updated": false}], "pr": ["Windows Update"]}`
	winSys = `{"time": 1793000000000, "hardware": {
		"windows": {"osinfo": {"BuildRevision": "2033", "OSArchitecture": "64-bit", "LastBootUpTime": "20261021081400.500000+120", "Domain": "WORKGROUP"},
			"memory": [{"BankLabel": "BANK 1", "Capacity": 8589934592, "Speed": 3200, "PartNumber": "M471A1K43", "Manufacturer": "Example"},
				{"BankLabel": "BANK 0", "Capacity": 8589934592, "Speed": 3200, "Manufacturer": "Undefined", "PartNumber": "M471A1K43"}],
			"volumes": {"C": {"name": "System", "size": 510770802688, "sizeremaining": 1073741824, "type": "NTFS", "dType": 3,
				"volumeStatus": 1, "protectionStatus": 1, "encryptionMethod": 6}, "D": null},
			"drives": [{"Model": "Example NVMe 512GB", "Status": "OK"}]},
		"identifiers": {"bios_vendor": "Example Inc.", "cpu_name": "Intel(R) Core(TM) i5-1240P", "gpu_name": ["Intel(R) Iris(R) Xe Graphics", null],
			"storage_devices": [{"Caption": "Example NVMe 512GB", "Model": "Example NVMe 512GB", "Size": "512105932800"}, {"Caption": "Card reader"}]},
		"tpm": {"SpecVersion": "2.0, 0, 1.38", "IsActivated": true, "IsEnabled": true, "IsOwned": false},
		"network": {"dns": ["192.0.2.53"]}}}`
	winNet = `{"netif2": {
		"Ethernet": [{"mac": "00:11:22:33:44:55", "address": "192.0.2.10", "netmask": "255.255.255.0", "gateway": "192.0.2.1",
			"family": "IPv4", "fqdn": "example.lan", "speed": 1000000000}, {"mac": "00:11:22:33:44:55", "address": "fe80::1", "family": "IPv6"}],
		"Loopback Pseudo-Interface 1": [{"mac": "00:00:00:00:00:00", "address": "127.0.0.1", "family": "IPv4"}],
		"Wi-Fi": [{"mac": "00:11:22:33:44:66", "speed": 9223372036854775807}]}}`
)

// rowsOf are the values of label in a section's group.
func rowsOf(sections []DetailSection, section, group, label string) []string {
	var v []string
	for _, s := range sections {
		for _, g := range s.Groups {
			if s.Title == section && g.Title == group {
				for _, r := range g.Rows {
					if r.Label == label {
						v = append(v, r.Text())
					}
				}
			}
		}
	}
	return v
}

func TestDeviceInfo(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	detailsServer(t, map[string]string{"node//win": winNode, "node//bare": `{"_id": "node//bare", "name": "bare"}`},
		map[string]string{"node//win": winSys}, map[string]string{"node//win": winNet})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	info, err := GetDeviceInfo(ctx, "node//win")
	if err != nil {
		t.Fatal(err)
	}
	s := info.Sections()
	var titles []string
	for _, x := range s {
		titles = append(titles, x.Title)
	}
	want := []string{"General", "Security", "Operating System", "Mesh Agent", "Networking", "BIOS", "Motherboard", "TPM", "Memory", "Storage", "Storage Volumes"}
	if !slices.Equal(titles, want) {
		t.Errorf("sections %q", titles)
	}
	for _, c := range []struct {
		section, group, label string
		want                  []string
	}{
		{"General", "", "Tags", []string{"kiosk, floor-2"}},
		{"General", "", "Active User", []string{`EXAMPLE\alice (locked)`}},
		{"General", "", "Connectivity", []string{"Mesh Agent, Mesh Relay"}},
		{"Security", "", "Windows Security", []string{"AV - OK, Update - OK, Firewall - BAD"}},
		{"Security", "", "Pending Reboot", []string{"Windows Update"}},
		{"Security", "", "Antivirus", []string{"Windows Defender - Out of date"}},
		{"Operating System", "", "Last Boot Up Time", []string{"2026-10-21 08:14:00"}},
		{"Operating System", "", "Workgroup", []string{"WORKGROUP"}},
		{"Mesh Agent", "", "Mesh Agent", []string{"Windows 64bit service"}},
		{"Mesh Agent", "", "Node ID", []string{"win"}},
		{"Mesh Agent", "", "Last agent connection", []string{"Connected now"}},
		{"Mesh Agent", "", "Last agent address", []string{"198.51.100.7"}},
		{"Mesh Agent", "", "Last details update", []string{"2026-10-26 07:33:20"}},
		{"Networking", "Ethernet, example.lan", "IPv4 Layer", []string{"IP: 192.0.2.10, Mask: 255.255.255.0, Gateway: 192.0.2.1"}},
		{"Networking", "Ethernet, example.lan", "IPv6 Layer", []string{"IP: fe80::1"}},
		{"Networking", "Ethernet, example.lan", "Interface Speed", []string{"1.0 Gbps"}},
		{"Networking", "Wi-Fi", "Interface Speed", nil},
		{"Networking", "Loopback Pseudo-Interface 1", "MAC Layer", nil},
		{"Networking", "DNS Servers", "Servers", []string{"192.0.2.53"}},
		{"Motherboard", "", "CPU", []string{"Intel® Core™ i5-1240P"}},
		{"Motherboard", "", "GPU", []string{"Intel® Iris® Xe Graphics"}},
		{"TPM", "", "SpecVersion", []string{"2.0"}},
		{"TPM", "", "Owned", []string{"No"}},
		{"Memory", "BANK 0", "Capacity / Speed", []string{"8192 Mb, 3200 Mhz"}},
		{"Memory", "BANK 0", "Part Number", []string{"M471A1K43"}},
		{"Memory", "BANK 1", "Part Number", []string{"Example, M471A1K43"}},
		{"Storage", "Example NVMe 512GB", "Capacity", []string{"488382 Mb"}},
		{"Storage", "Example NVMe 512GB", "Status", []string{"OK"}},
		{"Storage Volumes", "C: - System", "Capacity", []string{"475.69 GB"}},
		{"Storage Volumes", "C: - System", "Capacity Remaining", []string{"1.00 GB"}},
		{"Storage Volumes", "C: - System", "File System", []string{"Local Disk / NTFS"}},
		{"Storage Volumes", "C: - System", "BitLocker", []string{"Fully Encrypted / XTS-AES-128"}},
	} {
		if got := rowsOf(s, c.section, c.group, c.label); !slices.Equal(got, c.want) {
			t.Errorf("%s / %s / %s: %q, want %q", c.section, c.group, c.label, got, c.want)
		}
	}
	// Each status is colored on its own, like the web UI does.
	var states []State
	for _, r := range s[1].Groups[0].Rows {
		for _, p := range r.Value {
			if p.State != Plain {
				states = append(states, p.State)
			}
		}
	}
	if want := []State{Good, Good, Bad, Attention, Bad}; !slices.Equal(states, want) {
		t.Errorf("security states %v, want %v", states, want)
	}
	if got := s[len(s)-1].Groups; len(got) != 1 {
		t.Errorf("volumes %+v, the null one should be skipped", got)
	}

	// Without the details right, or before the agent sent any, only the
	// record is there.
	info, err = GetDeviceInfo(ctx, "node//bare")
	if err != nil || info.Sys != nil || info.Net != nil || info.Node["lastconnect"] != nil {
		t.Fatalf("bare device: %+v, %v", info, err)
	}
	if s := info.Sections(); len(s) != 1 || s[0].Title != "General" {
		t.Errorf("bare device sections %+v", s)
	}
	if _, err := GetDeviceInfo(ctx, "node//gone"); !errors.Is(err, ErrNoDevice) {
		t.Errorf("unknown device returned %v", err)
	}
}

// Linux agents report memory and volumes their own way, older ones netif.
func TestDeviceInfoLinux(t *testing.T) {
	local := time.Local
	time.Local = time.UTC
	t.Cleanup(func() { time.Local = local })
	var info DeviceInfo
	for _, j := range []struct {
		s string
		v *map[string]interface{}
	}{
		{`{"_id": "node//lin", "rname": "build-01", "osdesc": "openSUSE Tumbleweed", "conn": 0, "lastconnect": 1793000000000,
			"lastaddr": "[2001:db8::7]:443", "agent": {"id": 6, "ver": 0, "root": false}, "lsc": {"antiVirus": "OK", "firewall": "OK"}}`, &info.Node},
		{`{"hardware": {"linux": {"LastBootUpTime": "2026-10-21 08:14:00", "kernel_release": "6.17.1-1-default", "arch": "x86_64",
			"memory": {"Memory_Device": [{"Locator": "DIMM 1", "Size": "No Module Installed"}, {"Locator": "DIMM 0", "Size": "16 GB", "Speed": "4800 MT/s"}]},
			"volumes": [{"mount_point": "/", "size": 488281250, "available": 0, "type": "btrfs"},
				{"mount_point": "/var/lib/docker/overlay2/x", "size": 1, "type": "overlay"}]}}}`, &info.Sys},
		{`{"netif": [{"name": "eth0", "mac": "00:11:22:33:44:77", "v4addr": "203.0.113.70", "v4mask": "255.255.255.0", "v4gateway": "203.0.113.1"},
			{"name": "eth0", "mac": "00:11:22:33:44:77", "v4addr": "203.0.113.71"}, {"name": "lo"}]}`, &info.Net},
	} {
		if err := json.Unmarshal([]byte(j.s), j.v); err != nil {
			t.Fatal(err)
		}
	}
	s := info.Sections()
	for _, c := range []struct {
		section, group, label string
		want                  []string
	}{
		{"Security", "", "Linux Security", []string{"AV - OK, Firewall - OK"}},
		{"Operating System", "", "Last Boot Up Time", []string{"2026-10-21 08:14:00"}},
		{"Operating System", "", "Architecture", []string{"64-bit"}},
		{"Mesh Agent", "", "Mesh Agent", []string{"Linux 64bit"}},
		{"Mesh Agent", "", "Last agent connection", []string{"2026-10-26 07:33:20"}},
		{"Mesh Agent", "", "Last agent address", []string{"2001:db8::7"}},
		{"Networking", "eth0", "IPv4 Layer", []string{"IP: 203.0.113.70, Mask: 255.255.255.0, Gateway: 203.0.113.1", "IP: 203.0.113.71"}},
		{"Memory", "DIMM 0", "Capacity / Speed", []string{"16 GB, 4800 MT/s"}},
		{"Memory", "DIMM 1", "Capacity", nil},
		{"Storage Volumes", "/", "Capacity", []string{"465.66 GB"}},
		{"Storage Volumes", "/", "Capacity Remaining", []string{"0 KB"}},
		{"Storage Volumes", "/var/lib/docker/overlay2/x", "File System", nil},
	} {
		if got := rowsOf(s, c.section, c.group, c.label); !slices.Equal(got, c.want) {
			t.Errorf("%s / %s / %s: %q, want %q", c.section, c.group, c.label, got, c.want)
		}
	}
	for _, x := range s {
		for _, g := range x.Groups {
			if strings.HasPrefix(g.Title, "lo") || g.Title == "DIMM 1" {
				t.Errorf("%s shown", g.Title)
			}
		}
	}
}

// A "nodes" reply without a responseid is the device list, one with it
// answers GetDeviceInfo and leaves the list alone.
func TestNodesReplyRouting(t *testing.T) {
	settings.deviceMu.Lock()
	saved := settings.Devices
	settings.Devices = nil
	settings.deviceMu.Unlock()
	t.Cleanup(func() {
		settings.deviceMu.Lock()
		settings.Devices = saved
		settings.deviceMu.Unlock()
	})
	nodes := map[string]interface{}{"m": []interface{}{map[string]interface{}{"_id": "node//a", "name": "a"}}}
	handleReply(map[string]interface{}{"action": "nodes", "responseid": "mcc-info-x", "nodes": nodes})
	settings.deviceMu.Lock()
	n := len(settings.Devices)
	settings.deviceMu.Unlock()
	if n != 0 {
		t.Fatal("a reply for GetDeviceInfo replaced the device list")
	}
	handleReply(map[string]interface{}{"action": "nodes", "nodes": nodes})
	settings.deviceMu.Lock()
	n = len(settings.Devices)
	settings.deviceMu.Unlock()
	if n != 1 {
		t.Errorf("the device list has %d devices", n)
	}
}
