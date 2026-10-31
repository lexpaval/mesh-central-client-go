package meshcentral

import "testing"

func TestDeviceSearch(t *testing.T) {
	devices := []Device{
		{Id: "a", DisplayName: "Reception PC", Name: "recep-01", OS: "Windows 11 Pro", IP: "192.0.2.10", Group: "Site A", Conn: 1,
			Tags: []string{"front desk"}, Users: []string{`OFFICE\alice`}, UPNUsers: []string{"alice@example.com"},
			Agent: true, AgentTag: "kiosk-fleet", WSC: &WSC{"OK", "OK", "OK"}},
		{Id: "b", Name: "build", OS: "Fedora 42", IP: "192.0.2.20", Group: "Lab", Desc: "CI runner", Tags: []string{"ci"},
			Conn: 1 | 8, Agent: true, AMT: true, AMTState: "2"},
		{Id: "c", Name: "kiosk", OS: "Windows 10", Group: "Site A", Users: []string{"bob"},
			WSC: &WSC{AV: "OK", Firewall: "OK"}},
	}
	for _, c := range []struct{ q, want string }{
		{"", "abc"},
		{"  ", "abc"},
		{"site a", "ac"}, // the group, spaces are part of the text
		{"site b", ""},
		{"kiosk", "c"}, // names, not the agent tag
		{"user:bob", "c"},
		{"U:ALICE", "a"},
		{"u:example.com", "a"}, // principal names too
		{"u:", "ac"},           // anyone logged in
		{"!u:alice", "bc"},
		{"tag:ci", "b"},
		{"t:front desk", "a"},
		{"t:", "abc"}, // as in the web UI, not untagged
		{"a:fleet", "a"},
		{"a:", "ab"}, // has an agent, tagged or not
		{"g:lab", "b"},
		{"os:windows and !os:11", "c"},
		{"ip:192.0.2.", "ab"},
		{"ip:", "ab"},
		{"desc:runner", "b"},
		{"desc:", "b"},
		{"amt:", "b"},
		{"amt:2", "b"},
		{"amt:1", ""},
		{"wsc:ok", "a"},
		{"wsc:noupdate", "c"}, // unreported is not OK
		{"wsc:any", "c"},
		{"wsc:noav", ""},
		{"wsc:bad", ""}, // not a wsc word, a name
		{"c:relay", "b"},
		{"connectivity:agent", "ab"},
		{"c:local", ""},
		{"u:bob, g:lab", "bc"},
		{"u:bob or g:lab", "bc"},
		{"u:alice and t:front, desc:ci", "ab"},
		{"g:site a and !u:alice", "c"},
		{"windows and u:bob", "c"},
	} {
		got := ""
		s := ParseSearch(c.q)
		for i := range devices {
			if s.Match(&devices[i]) {
				got += devices[i].Id
			}
		}
		if got != c.want {
			t.Errorf("%q matched %q, want %q", c.q, got, c.want)
		}
	}
}
