package meshcentral

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// DeviceInfo is what the web UI's General and Details tabs show of a device,
// as the server sends it.
type DeviceInfo struct {
	Node  map[string]interface{} `json:"node"`    // the device record, with lastconnect and lastaddr added
	Group string                 `json:"group"`   // the device group's name
	Sys   map[string]interface{} `json:"sysinfo"` // getsysinfo: hardware and the time it was taken, nil without
	Net   map[string]interface{} `json:"netinfo"` // getnetworkinfo: netif2, or netif from older agents, nil without
}

// ErrNoDevice means the server has no device by that ID the user can see.
var ErrNoDevice = errors.New("the server has no such device")

// infoRequests numbers the "nodes" requests of GetDeviceInfo, their replies
// carry it back.
var infoRequests atomic.Int64

// GetDeviceInfo fetches a device's details, the way meshctrl's deviceinfo
// does: its record, system and network information and last connection.
// Without the "Device details" right Sys and Net stay nil. ctx needs a
// deadline, the server ignores malformed IDs.
func GetDeviceInfo(ctx context.Context, nodeID string) (*DeviceInfo, error) {
	responseID := "mcc-info-" + strconv.FormatInt(infoRequests.Add(1), 10)
	reqs := []struct {
		key string
		msg map[string]string
	}{
		{replyKey("nodes", responseID), map[string]string{"action": "nodes", "id": nodeID, "responseid": responseID}},
		{replyKey("getsysinfo", nodeID), map[string]string{"action": "getsysinfo", "nodeid": nodeID}},
		{replyKey("getnetworkinfo", nodeID), map[string]string{"action": "getnetworkinfo", "nodeid": nodeID}},
		{replyKey("lastconnect", nodeID), map[string]string{"action": "lastconnect", "nodeid": nodeID}},
	}
	replies := make([]map[string]interface{}, len(reqs))
	errs := make([]error, len(reqs))
	var wg sync.WaitGroup
	for i, r := range reqs {
		wg.Go(func() {
			b, _ := json.Marshal(r.msg)
			replies[i], errs[i] = request(ctx, r.key, b)
		})
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	info := &DeviceInfo{}
	groups, _ := replies[0]["nodes"].(map[string]interface{})
	for meshID, nodes := range groups {
		list, _ := nodes.([]interface{})
		for _, n := range list {
			if node, _ := n.(map[string]interface{}); node != nil && node["_id"] == nodeID {
				info.Node, info.Group = node, settings.groups[meshID]
			}
		}
	}
	if info.Node == nil {
		return nil, ErrNoDevice
	}
	if sys := replies[1]; sys["noinfo"] != true {
		info.Sys = sys
	}
	if net := replies[2]; net["noinfo"] != true && (net["netif2"] != nil || net["netif"] != nil) {
		info.Net = net
	}
	if lc := replies[3]; lc["noinfo"] != true {
		info.Node["lastconnect"], info.Node["lastaddr"] = lc["time"], lc["addr"]
	}
	return info, nil
}

// DetailSection is one titled part of a device's details. Its rows come in
// groups, such as one per network interface, a group's title may be empty.
type DetailSection struct {
	Title  string
	Groups []DetailGroup
}

type DetailGroup struct {
	Title string
	Rows  []DetailRow
}

// DetailRow is a labelled value, in parts for the statuses in it.
type DetailRow struct {
	Label string
	Value []DetailPart
}

// Text is the row's value as plain text.
func (r DetailRow) Text() string {
	var b strings.Builder
	for _, p := range r.Value {
		b.WriteString(p.Text)
	}
	return b.String()
}

type DetailPart struct {
	Text  string
	State State
}

// State is how a status reads. The web UI shows Good in green and Bad in
// red, Attention (a pending reboot) is mcc's own.
type State int

const (
	Plain State = iota
	Good
	Attention
	Bad
)

// agentNames are the agent types by ID, as the web UI names them.
var agentNames = []string{"Unknown", "Windows 32bit console", "Windows 64bit console", "Windows 32bit service", "Windows 64bit service",
	"Linux 32bit", "Linux 64bit", "MIPS", "XENx86", "Android", "Linux ARM", "macOS x86-32bit", "Android x86", "PogoPlug ARM", "Android",
	"Linux Poky x86-32bit", "macOS x86-64bit", "ChromeOS", "Linux Poky x86-64bit", "Linux NoKVM x86-32bit", "Linux NoKVM x86-64bit",
	"Windows MinCore console", "Windows MinCore service", "NodeJS", "ARM-Linaro", "ARMv6l / ARMv7l", "ARMv8 64bit", "ARMv6l / ARMv7l / NoKVM",
	"MIPS24KC (OpenWRT)", "Apple Silicon", "FreeBSD x86-64", "Unknown", "Linux ARM 64 bit", "Alpine Linux x86 64 Bit (MUSL)",
	"Assistant (Windows)", "Armada370 - ARM32/HF (libc/2.26)", "OpenWRT x86-64", "OpenBSD x86-64", "Unknown", "Unknown",
	"MIPSEL24KC (OpenWRT)", "ARMADA/CORTEX-A53/MUSL (OpenWRT)", "Windows ARM 64bit console", "Windows ARM 64bit service",
	"ARMVIRT32 (OpenWRT)", "RISC-V x86-64"}

// Sections lays the details out like the web UI: the General tab's
// attributes, then the Details tab's sections, without Intel AMT and mobile
// devices. Empty sections are left out.
func (info *DeviceInfo) Sections() []DetailSection {
	node := info.Node
	hw := obj(info.Sys["hardware"])
	var out []DetailSection
	add := func(title string, groups ...DetailGroup) {
		groups = slices.DeleteFunc(groups, func(g DetailGroup) bool { return len(g.Rows) == 0 })
		if len(groups) > 0 {
			out = append(out, DetailSection{title, groups})
		}
	}

	var g rows
	g.add("Name", str(node["name"]))
	g.add("Group", info.Group)
	g.add("Hostname", str(node["host"]))
	g.add("Description", str(node["desc"]))
	g.add("Tags", strings.Join(strs(node["tags"]), ", "))
	if users := strs(node["users"]); len(users) > 0 {
		locked := strs(node["lusers"])
		for i, u := range users {
			if slices.Contains(locked, u) {
				users[i] += " (locked)"
			}
		}
		g.add(map[bool]string{false: "Active User", true: "Active Users"}[len(users) > 1], strings.Join(users, ", "))
	}
	if conn := int(numOr(node["conn"])); conn > 1 { // more than the agent
		var c []string
		for _, f := range []struct {
			bit  int
			name string
		}{{1, "Mesh Agent"}, {2, "Intel AMT CIRA"}, {4, "Intel AMT"}, {8, "Mesh Relay"}, {16, "MQTT"}} {
			if conn&f.bit != 0 && (f.bit != 4 || conn&2 == 0) {
				c = append(c, f.name)
			}
		}
		g.add("Connectivity", strings.Join(c, ", "))
	}
	g.add("Agent Tag", str(get(node, "agent", "tag")))
	add("General", DetailGroup{Rows: g})

	// The security attributes of the General tab, as "AV - OK, Firewall - BAD".
	g = nil
	type item struct {
		name  string
		state DetailPart
	}
	judge := func(good bool, yes, no string) DetailPart {
		if good {
			return DetailPart{yes, Good}
		}
		return DetailPart{no, Bad}
	}
	joined := func(items []item) []DetailPart {
		var p []DetailPart
		for i, it := range items {
			if i > 0 {
				p = append(p, DetailPart{Text: ", "})
			}
			p = append(p, DetailPart{Text: it.name + " - "}, it.state)
		}
		return p
	}
	status := func(label string, m map[string]interface{}, checks ...[2]string) {
		var items []item
		for _, c := range checks {
			if v := m[c[0]]; v != nil {
				items = append(items, item{c[1], judge(v == "OK", "OK", "BAD")})
			}
		}
		g.parts(label, joined(items)...)
	}
	status("Windows Security", obj(node["wsc"]), [2]string{"antiVirus", "AV"}, [2]string{"autoUpdate", "Update"}, [2]string{"firewall", "Firewall"})
	if def := obj(node["defender"]); def != nil {
		var items []item
		for _, c := range [][2]string{{"RealTimeProtection", "RealTimeProtection"}, {"TamperProtected", "TamperProtection"}} {
			if on, ok := def[c[0]].(bool); ok {
				items = append(items, item{c[1], judge(on, "On", "Off")})
			}
		}
		if v := str(def["AntivirusSignatureVersion"]); v != "" {
			items = append(items, item{"SignatureVersion", DetailPart{v, Good}})
		}
		g.parts("Windows Defender", joined(items)...)
	}
	if pr := strs(node["pr"]); len(pr) > 0 {
		g.parts("Pending Reboot", DetailPart{strings.Join(pr, ", "), Attention})
	}
	status("Linux Security", obj(node["lsc"]), [2]string{"antiVirus", "AV"}, [2]string{"firewall", "Firewall"})
	for _, a := range list(node["av"]) {
		av := obj(a)
		product := str(av["product"])
		if product == "" {
			continue
		}
		enabled, updated := av["enabled"] == true, av["updated"] == true
		p := []DetailPart{{Text: product}}
		if !enabled {
			p = append(p, DetailPart{Text: " - "}, DetailPart{"Disabled", Bad})
		}
		if !updated {
			p = append(p, DetailPart{Text: " - "}, DetailPart{"Out of date", Bad})
		}
		if enabled && updated {
			p = append(p, DetailPart{Text: " - "}, DetailPart{"OK", Good})
		}
		g.parts("Antivirus", p...)
	}
	add("Security", DetailGroup{Rows: g})

	g = nil
	g.add("Name", str(node["rname"]))
	win, linux, darwin := obj(hw["windows"]), obj(hw["linux"]), obj(hw["darwin"])
	osinfo := obj(win["osinfo"])
	g.add("Description", str(osinfo["Description"]))
	g.add("Version", str(node["osdesc"]))
	g.add("Build Number", str(osinfo["BuildRevision"]))
	if a := str(osinfo["OSArchitecture"]); strings.HasPrefix(a, "32") {
		g.add("Architecture", "32-bit")
	} else if strings.HasPrefix(a, "64") {
		g.add("Architecture", "64-bit")
	} else {
		g.add("Architecture", a)
	}
	if s := str(osinfo["LastBootUpTime"]); len(s) >= 14 { // WMI's yyyymmddHHMMSS, local time
		if t, err := time.ParseInLocation("20060102150405", s[:14], time.Local); err == nil {
			g.add("Last Boot Up Time", formatTime(t))
		}
	}
	if d := str(osinfo["Domain"]); d != "" {
		g.add(map[bool]string{false: "Workgroup", true: "Domain"}[osinfo["PartOfDomain"] == true], d)
	}
	if s := int(numOr(osinfo["DomainState"])); s > 0 {
		g.add("Domain State", pick([]string{"None", "Azure AD", "On-Premises AD", "Hybrid AD", "Microsoft Account"}, s, "Unknown"))
	}
	g.add("Last Boot Up Time", bootTime(linux["LastBootUpTime"], time.Millisecond))
	g.add("Last Boot Up Time", bootTime(darwin["LastBootUpTime"], time.Second))
	g.add("Kernel Release", str(linux["kernel_release"]))
	g.add("Kernel Build", str(linux["kernel_build"]))
	switch a := str(linux["arch"]); a {
	case "x86_64", "amd64":
		g.add("Architecture", "64-bit")
	case "i686", "x86", "i586":
		g.add("Architecture", "32-bit")
	default:
		g.add("Architecture", a)
	}
	add("Operating System", DetailGroup{Rows: g})

	if agent := obj(node["agent"]); agent != nil {
		g = nil
		if id, ok := num(agent["id"]); ok && agent["ver"] != nil {
			s := pick(agentNames, int(id), agentNames[0])
			if v := str(agent["ver"]); v != "0" {
				s += " v" + v
			}
			if id == 14 {
				s = str(agent["core"])
			}
			if agent["root"] == false && int(numOr(node["conn"]))&1 != 0 {
				s += ", Restricted"
			}
			g.add("Mesh Agent", s)
		}
		if id := str(node["_id"]); strings.Count(id, "/") == 2 {
			g.add("Node ID", strings.Split(id, "/")[2])
		}
		g.add("First agent connection", msTime(node["firstconnect"]))
		if int(numOr(node["conn"]))&1 != 0 {
			g.add("Last agent connection", "Connected now")
		} else {
			g.add("Last agent connection", msTime(node["lastconnect"]))
		}
		if addr := str(node["lastaddr"]); strings.Count(addr, ":") > 1 { // [v6]:port or v6:port
			g.add("Last agent address", strings.Trim(addr[:strings.LastIndex(addr, ":")], "[]"))
		} else {
			g.add("Last agent address", strings.Split(addr, ":")[0])
		}
		if ct := str(get(hw, "agentvers", "compileTime")); ct != "" {
			g.add("Compile time", ct)
		}
		g.add("Last details update", msTime(info.Sys["time"]))
		add("Mesh Agent", DetailGroup{Rows: g})
	}

	add("Networking", networking(info.Net, hw)...)

	ident := obj(hw["identifiers"])
	g = nil
	g.add("Vendor", str(ident["bios_vendor"]))
	g.add("Version", str(ident["bios_version"]))
	g.add("Serial", str(ident["bios_serial"]))
	g.add("Mode", str(ident["bios_mode"]))
	add("BIOS", DetailGroup{Rows: g})
	g = nil
	g.add("Vendor", str(ident["board_vendor"]))
	g.add("Name", str(ident["board_name"]))
	g.add("Serial", str(ident["board_serial"]))
	g.add("Version", str(ident["board_version"]))
	g.add("Identifier", str(ident["product_uuid"]))
	g.add("CPU", trademarks(str(ident["cpu_name"])))
	gpus := strs(ident["gpu_name"])
	if s := str(ident["gpu_name"]); s != "" {
		gpus = []string{s}
	}
	for _, gpu := range gpus {
		g.add("GPU", trademarks(gpu))
	}
	add("Motherboard", DetailGroup{Rows: g})
	g = nil
	g.add("Manufacturer", str(ident["chassis_manufacturer"]))
	g.add("Product Name", str(ident["product_name"]))
	g.add("Serial", str(ident["chassis_serial"]))
	g.add("Asset Tag", str(ident["chassis_assettag"]))
	add("System", DetailGroup{Rows: g})

	if tpm := obj(hw["tpm"]); tpm != nil {
		g = nil
		if v := str(tpm["SpecVersion"]); v != "" {
			// parseFloat("2.0, 0, 1.38").toFixed(1)
			end := strings.IndexFunc(v, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
			if end < 0 {
				end = len(v)
			}
			if f, err := strconv.ParseFloat(v[:end], 64); err == nil {
				g.add("SpecVersion", strconv.FormatFloat(f, 'f', 1, 64))
			}
		}
		g.add("Identifier", str(tpm["ManufacturerId"]))
		g.add("Version", str(tpm["ManufacturerVersion"]))
		g.add("Activated", yesNo(tpm["IsActivated"]))
		g.add("Enabled", yesNo(tpm["IsEnabled"]))
		g.add("Owned", yesNo(tpm["IsOwned"]))
		add("TPM", DetailGroup{Rows: g})
	}

	var groups []DetailGroup
	for _, e := range entries(hw["battery"]) {
		b := obj(e.v)
		if b == nil {
			continue
		}
		var g rows
		unit := func(label, key, format string, scale float64) {
			if f, ok := num(b[key]); ok && f != 0 {
				g.add(label, fmt.Sprintf(format, f/scale))
			}
		}
		g.add("Cycle Count", nonZero(b["CycleCount"]))
		unit("Full Charged Capacity", "FullChargedCapacity", "%g mWh", 1)
		if f, ok := num(b["EstimatedRuntime"]); ok && f != 0 {
			g.add("Estimated Runtime", fmt.Sprintf("%d minutes", int(f/60)))
		}
		g.add("Chemistry", str(b["Chemistry"]))
		unit("Design Capacity", "DesignedCapacity", "%g mWh", 1)
		g.add("Manufacture Date", str(b["ManufactureDate"]))
		g.add("Manufacture Name", str(b["ManufactureName"]))
		g.add("Serial Number", str(b["SerialNumber"]))
		unit("Charge Rate", "ChargeRate", "%g mW", 1)
		g.add("Charging", yesNo(b["Charging"]))
		unit("Discharge Rate", "DischargeRate", "%g mW", 1)
		g.add("Discharging", yesNo(b["Discharging"]))
		unit("Remaining Capacity", "RemainingCapacity", "%g mWh", 1)
		unit("Voltage", "Voltage", "%g V", 1000)
		unit("Health", "Health", "%g %%", 1)
		unit("Battery Charge", "BatteryCharge", "%g %%", 1)
		groups = append(groups, DetailGroup{cmp.Or(str(b["DeviceName"]), "Unknown"), g})
	}
	add("Battery", groups...)

	add("Memory", memory(win, linux, darwin)...)

	groups = nil
	drives := list(win["drives"])
	disks := list(ident["storage_devices"])
	slices.SortStableFunc(disks, func(a, b interface{}) int { return strings.Compare(str(obj(a)["Caption"]), str(obj(b)["Caption"])) })
	for _, d := range disks {
		m := obj(d)
		if !truthy(m["Size"]) {
			continue
		}
		var g rows
		caption, model := str(m["Caption"]), str(m["Model"])
		if model != caption {
			g.add("Model", model)
		}
		if f, err := strconv.ParseFloat(str(m["Size"]), 64); err == nil {
			g.add("Capacity", fmt.Sprintf("%d Mb", int64(f/1024/1024)))
		} else {
			g.add("Capacity", str(m["Size"]))
		}
		if model != "" {
			for _, dr := range drives {
				if str(obj(dr)["Model"]) == model {
					g.add("Status", str(obj(dr)["Status"]))
					break
				}
			}
		}
		groups = append(groups, DetailGroup{caption, g})
	}
	add("Storage", groups...)

	add("Storage Volumes", volumes(win, linux, darwin)...)
	return out
}

// networking is the Networking section: one group per interface, from
// netif2 or the older netif, and the DNS servers.
func networking(net, hw map[string]interface{}) []DetailGroup {
	var groups []DetailGroup
	if netif2 := obj(net["netif2"]); netif2 != nil {
		for _, e := range entries(netif2) {
			addrs := list(e.v)
			if len(addrs) == 0 {
				continue
			}
			first := obj(addrs[0])
			mac := str(first["mac"])
			if first == nil || strings.HasPrefix(mac, "00:00:00:00") { // loopback
				continue
			}
			title := e.k
			if fqdn := str(first["fqdn"]); fqdn != "" {
				title += ", " + fqdn
			}
			var g rows
			if mac != "" {
				g.add("MAC Layer", macLayer(mac, str(first["gatewaymac"])))
			}
			// Unknown speeds are math.MaxInt64.
			if f, ok := num(first["speed"]); ok && f > 0 && f < math.MaxInt64 {
				g.add("Interface Speed", networkSpeed(f))
			}
			for _, a := range addrs {
				l := obj(a)
				var items []string
				for _, f := range [][2]string{{"address", "IP"}, {"netmask", "Mask"}, {"gateway", "Gateway"}} {
					if v := str(l[f[0]]); v != "" {
						items = append(items, f[1]+": "+v)
					}
				}
				if fam := str(l["family"]); len(items) > 0 && (fam == "IPv4" || fam == "IPv6") {
					g.add(fam+" Layer", strings.Join(items, ", "))
				}
			}
			groups = append(groups, DetailGroup{title, g})
		}
		if dns := strs(get(hw, "network", "dns")); len(dns) > 0 {
			var g rows
			g.add("Servers", strings.Join(dns, ", "))
			groups = append(groups, DetailGroup{"DNS Servers", g})
		}
		return groups
	}
	// Older agents: one entry per address, shown per MAC address.
	byMAC := map[string]int{} // index in groups
	for _, n := range list(net["netif"]) {
		m := obj(n)
		mac, ok := m["mac"].(string)
		if !ok {
			continue
		}
		i, ok := byMAC[mac]
		if !ok {
			var r rows
			r.add("Description", trademarks(str(m["desc"])))
			r.add("MAC Layer", macLayer(mac, str(m["gatewaymac"])))
			title := str(m["name"])
			if s := str(m["dnssuffix"]); s != "" {
				title += ", " + s
			}
			i = len(groups)
			byMAC[mac] = i
			groups = append(groups, DetailGroup{title, r})
		}
		r := rows(groups[i].Rows)
		if addr, mask, gw := str(m["v4addr"]), str(m["v4mask"]), str(m["v4gateway"]); gw != "" && mask != "" {
			r.add("IPv4 Layer", fmt.Sprintf("IP: %s, Mask: %s, Gateway: %s", addr, mask, gw))
		} else if addr != "" {
			r.add("IPv4 Layer", "IP: "+addr)
		}
		groups[i].Rows = r
	}
	return groups
}

func macLayer(mac, gateway string) string {
	if gateway != "" {
		return fmt.Sprintf("MAC: %s, Gateway: %s", mac, gateway)
	}
	return "MAC: " + mac
}

// networkSpeed is getNetworkSpeed of the web UI.
func networkSpeed(bps float64) string {
	switch {
	case bps < 1e3:
		return fmt.Sprintf("%g bps", bps)
	case bps < 1e6:
		return fmt.Sprintf("%d Kbps", int64(math.Round(bps/1e3)))
	case bps < 1e9:
		return fmt.Sprintf("%d Mbps", int64(math.Round(bps/1e6)))
	}
	return fmt.Sprintf("%.1f Gbps", bps/1e9)
}

// memory is the Memory section, one group per module.
func memory(win, linux, darwin map[string]interface{}) []DetailGroup {
	var groups []DetailGroup
	module := func(title string, capacity, speed string, m map[string]interface{}, undefined string) {
		var g rows
		if capacity != "" && speed != "" {
			g.add("Capacity / Speed", capacity+", "+speed)
		} else {
			g.add("Capacity", capacity)
		}
		if pn := str(m["PartNumber"]); pn != "" {
			if mf := str(m["Manufacturer"]); mf != "" && mf != undefined {
				pn = mf + ", " + pn
			}
			g.add("Part Number", pn)
		}
		groups = append(groups, DetailGroup{title, g})
	}
	sorted := func(v interface{}, key string) []interface{} {
		l := list(v)
		slices.SortStableFunc(l, func(a, b interface{}) int { return strings.Compare(str(obj(a)[key]), str(obj(b)[key])) })
		return l
	}
	for _, x := range sorted(win["memory"], "BankLabel") {
		m := obj(x)
		capacity, speed := "", ""
		if f, ok := num(m["Capacity"]); ok && f != 0 {
			capacity = fmt.Sprintf("%g Mb", f/1024/1024)
			if s := nonZero(m["Speed"]); s != "" {
				speed = s + " Mhz"
			}
		}
		module(cmp.Or(str(m["BankLabel"]), str(m["DeviceLocator"]), "Unknown"), capacity, speed, m, "Undefined")
	}
	for _, x := range sorted(get(linux, "memory", "Memory_Device"), "Locator") {
		m := obj(x)
		if m["Size"] == "No Module Installed" {
			continue
		}
		module(cmp.Or(str(m["Locator"]), "Unknown"), str(m["Size"]), str(m["Speed"]), m, "Undefined")
	}
	for _, x := range list(darwin["memory"]) {
		m := obj(x)
		if m["Size"] == "No Module Installed" {
			continue
		}
		module(cmp.Or(str(m["DeviceLocator"]), "Unknown"), str(m["Size"]), str(m["Speed"]), m, "")
	}
	return groups
}

// volumes is the Storage Volumes section, one group per volume.
func volumes(win, linux, darwin map[string]interface{}) []DetailGroup {
	var groups []DetailGroup
	fileSystem := func(g *rows, m map[string]interface{}, kind string) {
		if t := str(m["type"]); t != "" {
			if kind != "" {
				t = kind + " / " + t
			}
			g.add("File System", t)
		}
	}
	removable := func(m map[string]interface{}) string {
		if m["removable"] == true {
			return "Removable"
		}
		if m["cdrom"] == true {
			return "CD-ROM"
		}
		return ""
	}
	encMethods := []string{"", "AES-128 with diffuser", "AES-256 with diffuser", "AES-128", "AES-256", "Hardware encryption", "XTS-AES-128", "XTS-AES-256"}
	conversion := []string{"Unknown", "Fully Encrypted", "Encryption In Progress", "Decryption In Progress", "Encryption Paused", "Decryption Paused"}
	driveTypes := []string{"Unknown", "No Root Directory", "Removable Disk", "Local Disk", "Network Drive", "Compact Disc", "RAM Disk"}
	for _, e := range entries(win["volumes"]) {
		m := obj(e.v)
		if m == nil {
			continue
		}
		var g rows
		g.add("Capacity", byteSize(m["size"], "Bytes", "KB", "MB", "GB", "TB"))
		g.add("Capacity Remaining", byteSize(m["sizeremaining"], "Bytes", "KB", "MB", "GB", "TB"))
		kind := removable(m)
		if t, ok := num(m["dType"]); ok {
			kind = pick(driveTypes, int(t), "Unknown")
		}
		fileSystem(&g, m, kind)
		if truthy(m["protectionStatus"]) || truthy(m["volumeStatus"]) {
			state, isText := m["volumeStatus"].(string) // older servers store the text
			if !isText {
				if numOr(m["protectionStatus"]) == 2 {
					state = "Locked"
				} else {
					state = pick(conversion, int(numOr(m["volumeStatus"])), "Unknown")
				}
			}
			if f, ok := num(m["encryptionMethod"]); ok && int(f) >= 0 && int(f) < len(encMethods) {
				state += " / " + encMethods[int(f)]
			} else {
				state += " / Unknown"
			}
			g.add("BitLocker", state)
		}
		title := e.k + ":"
		if n := str(m["name"]); n != "" {
			title += " - " + n
		}
		groups = append(groups, DetailGroup{title, g})
	}
	for _, src := range []struct {
		m     map[string]interface{}
		units []string // size and available, nil if they come as text
	}{{linux, []string{"KB", "MB", "GB", "TB"}}, {darwin, nil}} {
		for _, e := range entries(src.m["volumes"]) {
			m := obj(e.v)
			mount := str(m["mount_point"])
			if m == nil || strings.HasPrefix(mount, "/var/lib/docker/overlay2") {
				continue
			}
			var g rows
			if src.units != nil {
				g.add("Capacity", byteSize(m["size"], src.units...))
				if f, ok := num(m["available"]); ok && f == 0 {
					g.add("Capacity Remaining", "0 KB")
				} else {
					g.add("Capacity Remaining", byteSize(m["available"], src.units...))
				}
			} else {
				g.add("Capacity", str(m["size"]))
				g.add("Capacity Remaining", str(m["available"]))
			}
			fileSystem(&g, m, removable(m))
			groups = append(groups, DetailGroup{mount, g})
		}
	}
	return groups
}

// byteSize shows a size in the largest of units it reaches, units[0] being
// its own, 1024 apart. "" for none or 0.
func byteSize(v interface{}, units ...string) string {
	f, ok := num(v)
	if !ok || f == 0 {
		return ""
	}
	i := min(int(math.Floor(math.Log(math.Abs(f))/math.Log(1024))), len(units)-1)
	if i <= 0 {
		return fmt.Sprintf("%g %s", f, units[0])
	}
	return fmt.Sprintf("%.2f %s", f/math.Pow(1024, float64(i)), units[i])
}

// rows collects a group's rows, leaving out empty values.
type rows []DetailRow

func (r *rows) add(label, value string) {
	if value != "" {
		*r = append(*r, DetailRow{label, []DetailPart{{Text: value}}})
	}
}

func (r *rows) parts(label string, parts ...DetailPart) {
	if len(parts) > 0 {
		*r = append(*r, DetailRow{label, parts})
	}
}

// The server's JSON, read leniently: a missing or mistyped value reads as
// empty.

func obj(v interface{}) map[string]interface{} { m, _ := v.(map[string]interface{}); return m }

func list(v interface{}) []interface{} {
	l, _ := v.([]interface{})
	return slices.DeleteFunc(slices.Clone(l), func(x interface{}) bool { return x == nil })
}

// get follows keys through nested objects.
func get(v interface{}, keys ...string) interface{} {
	for _, k := range keys {
		v = obj(v)[k]
	}
	return v
}

// str is a string or number as text.
func str(v interface{}) string {
	switch v := v.(type) {
	case string:
		return v
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	}
	return ""
}

// strs is a list's strings and numbers as text.
func strs(v interface{}) []string {
	var s []string
	for _, x := range list(v) {
		if x := str(x); x != "" {
			s = append(s, x)
		}
	}
	return s
}

func num(v interface{}) (float64, bool) {
	switch v := v.(type) {
	case float64:
		return v, true
	case string:
		f, err := strconv.ParseFloat(v, 64)
		return f, err == nil
	}
	return 0, false
}

func numOr(v interface{}) float64 { f, _ := num(v); return f }

// truthy is whether JavaScript would take v as true.
func truthy(v interface{}) bool { return v != nil && v != false && v != 0.0 && v != "" }

// nonZero is str, but "" for zero, which the web UI leaves out.
func nonZero(v interface{}) string {
	if f, ok := num(v); ok && f == 0 {
		return ""
	}
	return str(v)
}

func yesNo(v interface{}) string {
	if b, ok := v.(bool); ok {
		return map[bool]string{false: "No", true: "Yes"}[b]
	}
	return ""
}

func pick(names []string, i int, def string) string {
	if i >= 0 && i < len(names) {
		return names[i]
	}
	return def
}

func trademarks(s string) string {
	return strings.NewReplacer("(TM)", "™", "(R)", "®").Replace(s)
}

type entry struct {
	k string
	v interface{}
}

// entries are an object's keys and values sorted by key, or a list's by
// index.
func entries(v interface{}) []entry {
	var e []entry
	switch v := v.(type) {
	case map[string]interface{}:
		for k, x := range v {
			e = append(e, entry{k, x})
		}
		slices.SortFunc(e, func(a, b entry) int { return strings.Compare(a.k, b.k) })
	case []interface{}:
		for i, x := range v {
			e = append(e, entry{strconv.Itoa(i), x})
		}
	}
	return e
}

// formatTime shows a time in the local zone.
func formatTime(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") }

// msTime formats Unix milliseconds, "" for none.
func msTime(v interface{}) string {
	if f, ok := v.(float64); ok && f > 0 {
		return formatTime(time.UnixMilli(int64(f)))
	}
	return ""
}

// bootTime formats a boot time given in unit since the epoch, or as text
// the web UI's Date would parse.
func bootTime(v interface{}, unit time.Duration) string {
	switch v := v.(type) {
	case float64:
		if v > 0 {
			return formatTime(time.Unix(0, int64(v)*int64(unit)))
		}
	case string:
		for _, layout := range []string{time.RFC3339, "2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.ParseInLocation(layout, v, time.Local); err == nil {
				return formatTime(t)
			}
		}
		return v
	}
	return ""
}
