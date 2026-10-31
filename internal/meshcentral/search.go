package meshcentral

import (
	"slices"
	"strings"
)

// Device searches use the web UI's filter syntax: terms joined by "," or
// " or " match any, by " and " all, a leading "!" excludes. A term is
// "prefix:text" for one field, as SearchPrefixes lists, or plain text,
// which matches the names, IP, OS or group. Matching is by substring, case
// aside; an empty text matches the devices that have the field.

// SearchPrefixes are the web UI's search fields, with what they match as
// listed for the user. The first name is the one shown.
var SearchPrefixes = []struct {
	Names []string
	Help  string
}{
	{[]string{"user", "u"}, "Logged-in user"},
	{[]string{"ip"}, "IP address"},
	{[]string{"group", "g"}, "Device group"},
	{[]string{"tag", "t"}, "Tag"},
	{[]string{"atag", "a"}, "Agent tag"},
	{[]string{"os"}, "Operating system"},
	{[]string{"amt"}, "Intel AMT state, empty for any"},
	{[]string{"desc"}, "Description, empty for any"},
	{[]string{"wsc"}, "Windows Security: ok, noav, noupdate, nofirewall, any"},
	{[]string{"connectivity", "c"}, "Connection: agent, cira, amt, relay, mqtt"},
}

var wscWords = []string{"ok", "noav", "noupdate", "nofirewall", "any"}

type searchTerm struct {
	field string // a SearchPrefixes first name, "" for plain text
	text  string // lower case
	not   bool
}

// DeviceSearch is a parsed search: any of its term lists, each matching
// all its terms. The zero value matches every device.
type DeviceSearch [][]searchTerm

// ParseSearch parses q.
func ParseSearch(q string) DeviceSearch {
	var s DeviceSearch
	q = strings.ToLower(strings.TrimSpace(q))
	for _, part := range strings.Split(q, ",") {
		for _, alt := range strings.Split(part, " or ") {
			if alt = strings.TrimSpace(alt); alt == "" {
				continue
			}
			var all []searchTerm
			for _, x := range strings.Split(alt, " and ") {
				if x = strings.TrimSpace(x); x != "" {
					all = append(all, parseTerm(x))
				}
			}
			if all != nil {
				s = append(s, all)
			}
		}
	}
	return s
}

func parseTerm(x string) searchTerm {
	t := searchTerm{text: x}
	if strings.HasPrefix(x, "!") {
		t.not, x = true, x[1:]
		t.text = x
	}
	k, v, ok := strings.Cut(x, ":")
	if !ok {
		return t
	}
	for _, p := range SearchPrefixes {
		// wsc takes only its words, anything else is a name.
		if slices.Contains(p.Names, k) && (k != "wsc" || slices.Contains(wscWords, v)) {
			t.field, t.text = p.Names[0], v
			break
		}
	}
	return t
}

// Match reports whether d matches the search.
func (s DeviceSearch) Match(d *Device) bool {
	if len(s) == 0 {
		return true
	}
	for _, all := range s {
		if !slices.ContainsFunc(all, func(t searchTerm) bool { return t.match(d) == t.not }) {
			return true
		}
	}
	return false
}

func (t searchTerm) match(d *Device) bool {
	has := func(fields ...string) bool {
		return slices.ContainsFunc(fields, func(f string) bool { return strings.Contains(strings.ToLower(f), t.text) })
	}
	// set is a field the device has, matching.
	set := func(f string) bool { return f != "" && has(f) }
	switch t.field {
	case "user":
		return len(d.Users) > 0 && (has(d.Users...) || has(d.UPNUsers...))
	case "ip":
		return set(d.IP)
	case "group":
		return has(d.Group)
	case "tag":
		return len(d.Tags) == 0 && t.text == "" || has(d.Tags...)
	case "atag":
		return d.Agent && has(d.AgentTag)
	case "os":
		return set(d.OS)
	case "amt":
		return d.AMT && (t.text == "" || d.AMTState == t.text)
	case "desc":
		return set(d.Desc)
	case "wsc":
		return d.WSC.match(t.text)
	case "connectivity":
		bit := map[string]int{"agent": 1, "cira": 2, "amt": 4, "relay": 8, "mqtt": 16}[t.text]
		return d.Conn&bit != 0
	}
	return has(d.DisplayName, d.Name, d.IP, d.OS, d.Group)
}

func (w *WSC) match(word string) bool {
	if w == nil {
		return false
	}
	switch word {
	case "ok":
		return w.AV == "OK" && w.Update == "OK" && w.Firewall == "OK"
	case "noav":
		return w.AV != "OK"
	case "noupdate":
		return w.Update != "OK"
	case "nofirewall":
		return w.Firewall != "OK"
	}
	return w.AV != "OK" || w.Update != "OK" || w.Firewall != "OK" // any
}
