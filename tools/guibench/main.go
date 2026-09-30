// guibench compares the CPU and memory use of GUI builds under the same
// load: it serves a fake MeshCentral server (devices, a stream of node
// events, shells redrawing a full screen like top), starts each GUI against
// it with a profile in a scratch config folder, and samples /proc (Linux).
//
//	go run ./tools/guibench [flags] <gui binary>...
//
// The GUIs connect and open shells on their own when MCC_GUI_BENCH is set to
// the number of shells, see bench.go in mcc-gui and mcc-qt.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/big"
	mrand "math/rand/v2"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/gorilla/websocket"
)

var (
	nDevices = flag.Int("devices", 2000, "devices on the fake server")
	nGroups  = flag.Int("groups", 40, "device groups")
	evRate   = flag.Float64("events", 20, "node events per second, one in ten takes a device on/offline")
	fps      = flag.Float64("fps", 10, "screen redraws per second in each shell")
	warmup   = flag.Duration("warmup", 15*time.Second, "time to connect and settle before measuring")
	measure  = flag.Duration("measure", 30*time.Second, "measuring time per run")
	shellSet = flag.String("shells", "0,2", "shell counts to run each GUI with")
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: guibench [flags] <gui binary>...\n")
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() == 0 {
		flag.Usage()
		os.Exit(2)
	}
	addr := serve()
	log.Printf("fake server on %s: %d devices in %d groups, %.0f events/s, shells at %.0f fps", addr, *nDevices, *nGroups, *evRate, *fps)

	var results []result
	for _, s := range strings.Split(*shellSet, ",") {
		shells, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			log.Fatalf("bad -shells %q", s)
		}
		for _, bin := range flag.Args() {
			r, err := run(bin, addr, shells)
			if err != nil {
				log.Fatalf("%s: %v", bin, err)
			}
			results = append(results, r)
		}
	}

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "GUI\tshells (up)\tCPU avg %\tCPU max %\tRSS MB\tRSS max MB\tPSS MB\tthreads\t")
	for _, r := range results {
		fmt.Fprintf(tw, "%s\t%d (%d)\t%.1f\t%.1f\t%.0f\t%.0f\t%.0f\t%d\t\n", r.name, r.shells, r.shellsUp, r.cpuAvg, r.cpuMax, r.rss, r.rssMax, r.pss, r.threads)
	}
	tw.Flush()
	fmt.Printf("\nCPU %% is of one core, sampled every second over %s after %s warmup. RSS counts shared libraries (Qt) in\nfull, PSS splits them between the processes using them.\n", *measure, *warmup)
}

type result struct {
	name                        string
	shells, threads             int
	lists, shellsUp             int64
	cpuAvg, cpuMax, rss, rssMax float64
	pss                         float64
}

// run starts a GUI connected to the fake server and samples it.
func run(bin, addr string, shells int) (result, error) {
	r := result{name: filepath.Base(bin), shells: shells}
	cfg, err := os.MkdirTemp("", "guibench")
	if err != nil {
		return r, err
	}
	defer os.RemoveAll(cfg)
	conf := map[string]any{
		"default_profile": "guibench",
		"profiles":        []map[string]string{{"name": "guibench", "server": addr, "username": "bench"}},
	}
	b, _ := json.Marshal(conf)
	os.MkdirAll(filepath.Join(cfg, "mcc"), 0o700)
	if err := os.WriteFile(filepath.Join(cfg, "mcc", "meshcentral-client.json"), b, 0o600); err != nil {
		return r, err
	}

	cmd := exec.Command(bin)
	cmd.Env = append(os.Environ(), "XDG_CONFIG_HOME="+cfg, "MCC_GUI_BENCH="+strconv.Itoa(shells))
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Start(); err != nil {
		return r, err
	}
	defer func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
			<-done
		}
	}()
	log.Printf("%s with %d shells: warming up", r.name, shells)
	lists, up := served.lists.Load(), served.shells.Load()
	time.Sleep(*warmup)
	r.lists, r.shellsUp = served.lists.Load()-lists, served.shells.Load()-up
	if r.lists == 0 || r.shellsUp < int64(shells) {
		log.Printf("%s: only %d device lists and %d of %d shells served, the numbers don't show the full load", r.name, r.lists, r.shellsUp, shells)
	}
	if cmd.ProcessState != nil || syscall.Kill(cmd.Process.Pid, 0) != nil {
		return r, fmt.Errorf("exited during warmup:\n%s", out.String())
	}

	pid := cmd.Process.Pid
	prev, _ := cpuTicks(pid)
	start := prev
	var rssSum float64
	n := int(*measure / time.Second)
	for i := 0; i < n; i++ {
		time.Sleep(time.Second)
		t, err := cpuTicks(pid)
		if err != nil {
			return r, fmt.Errorf("exited while measuring:\n%s", out.String())
		}
		r.cpuMax = max(r.cpuMax, float64(t-prev)) // 100 ticks a second, so ticks are %
		prev = t
		rss := statusMB(pid, "VmRSS:")
		rssSum += rss
		r.rssMax = max(r.rssMax, rss)
	}
	r.cpuAvg = float64(prev-start) / float64(n)
	r.rss = rssSum / float64(n)
	r.pss = rollupMB(pid, "Pss:")
	r.threads = int(statusMB(pid, "Threads:") * 1024)
	log.Printf("%s with %d shells: CPU %.1f%%, RSS %.0f MB", r.name, shells, r.cpuAvg, r.rss)
	return r, nil
}

// cpuTicks is the process's user+system time in clock ticks (1/100 s).
func cpuTicks(pid int) (int64, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	f := strings.Fields(string(b[bytes.LastIndexByte(b, ')')+2:]))
	u, _ := strconv.ParseInt(f[11], 10, 64)
	s, _ := strconv.ParseInt(f[12], 10, 64)
	return u + s, nil
}

// statusMB reads a kB field of /proc/pid/status in MB (Threads: in 1/1024).
func statusMB(pid int, field string) float64 {
	return procField(fmt.Sprintf("/proc/%d/status", pid), field)
}

func rollupMB(pid int, field string) float64 {
	return procField(fmt.Sprintf("/proc/%d/smaps_rollup", pid), field)
}

func procField(path, field string) float64 {
	b, _ := os.ReadFile(path)
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, field) {
			v, _ := strconv.ParseFloat(strings.Fields(l[len(field):])[0], 64)
			return v / 1024
		}
	}
	return 0
}

// serve starts the fake server on a local port with a self-signed
// certificate, the GUIs connect skipping verification.
func serve() string {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/control.ashx", control)
	mux.HandleFunc("/meshrelay.ashx", relay)
	srv := &http.Server{Handler: mux, TLSConfig: &tls.Config{Certificates: []tls.Certificate{selfSigned()}}}
	go srv.ServeTLS(l, "", "")
	return l.Addr().String()
}

func selfSigned() tls.Certificate {
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}}
	der, _ := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// What the server did, to check each GUI loaded the devices and got its shells.
var served struct{ lists, shells atomic.Int64 }

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

var oses = []string{"Fedora Linux 44 (Server Edition)", "Microsoft Windows 11 Pro - 24H2/26100", "Ubuntu 24.04.1 LTS",
	"Debian GNU/Linux 12 (bookworm)", "Raspbian GNU/Linux 12 (bookworm)", "macOS 15.3", "openSUSE Tumbleweed"}

func deviceID(i int) string { return fmt.Sprintf("node//bench%05d", i) }

// control is the session's control socket: login, device list, events.
func control(w http.ResponseWriter, r *http.Request) {
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	var wmu sync.Mutex
	send := func(v any) error {
		b, _ := json.Marshal(v)
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteMessage(websocket.TextMessage, b)
	}
	send(map[string]string{"action": "serverinfo"})

	// Node events once the device list went out.
	done := make(chan struct{})
	defer close(done)
	listed := make(chan struct{})
	go func() {
		select {
		case <-listed:
		case <-done:
			return
		}
		tick := time.NewTicker(time.Duration(float64(time.Second) / *evRate))
		defer tick.Stop()
		pwr := make([]int, *nDevices)
		for i := range pwr {
			pwr[i] = 1
		}
		for {
			select {
			case <-done:
				return
			case <-tick.C:
			}
			i := mrand.IntN(*nDevices)
			if mrand.IntN(10) == 0 {
				pwr[i] ^= 1
			}
			ev := map[string]any{"action": "nodeconnect", "nodeid": deviceID(i), "conn": 1 + mrand.IntN(4), "pwr": pwr[i]}
			if send(map[string]any{"action": "event", "event": ev}) != nil {
				return
			}
		}
	}()

	var once sync.Once
	for {
		_, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		var cmd struct{ Action string }
		json.Unmarshal(msg, &cmd)
		switch cmd.Action {
		case "authcookie":
			send(map[string]string{"action": "authcookie", "cookie": "bench", "rcookie": "bench"})
		case "meshes":
			var meshes []map[string]string
			for g := range *nGroups {
				meshes = append(meshes, map[string]string{"_id": fmt.Sprintf("mesh//bench%03d", g), "name": fmt.Sprintf("Group %03d", g)})
			}
			send(map[string]any{"action": "meshes", "meshes": meshes})
		case "nodes":
			nodes := map[string][]map[string]any{}
			for i := range *nDevices {
				mesh := fmt.Sprintf("mesh//bench%03d", i%*nGroups)
				nodes[mesh] = append(nodes[mesh], map[string]any{
					"_id": deviceID(i), "rname": fmt.Sprintf("host-%05d", i), "name": fmt.Sprintf("Device %05d", i),
					"osdesc": oses[i%len(oses)], "ip": fmt.Sprintf("198.51.%d.%d", i/250%256, i%250+1), "conn": 1, "pwr": 1,
				})
			}
			send(map[string]any{"action": "nodes", "nodes": nodes})
			served.lists.Add(1)
			once.Do(func() { close(listed) })
		}
	}
}

// relay is a shell tunnel: after the handshake it redraws a top-like screen
// fps times a second at the size the client asked for, echoing input.
func relay(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("p") != "1" { // port routes aren't benchmarked
		http.NotFound(w, r)
		return
	}
	c, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	var wmu sync.Mutex
	write := func(t int, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return c.WriteMessage(t, b)
	}
	write(websocket.TextMessage, []byte("c"))

	var mu sync.Mutex
	cols, rows := 80, 24
	started := make(chan struct{})
	done := make(chan struct{})
	defer close(done)
	go func() {
		var once sync.Once
		for {
			t, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			var o struct {
				Type       string
				Cols, Rows int
			}
			if t == websocket.TextMessage && json.Unmarshal(msg, &o) == nil && (o.Type == "options" || o.Type == "termsize") {
				mu.Lock()
				cols, rows = max(o.Cols, 20), max(o.Rows, 5)
				mu.Unlock()
				continue
			}
			if t == websocket.TextMessage && len(msg) == 1 { // the protocol number
				once.Do(func() { close(started) })
				continue
			}
			if t == websocket.BinaryMessage {
				write(websocket.BinaryMessage, msg)
			}
		}
	}()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		return
	}
	served.shells.Add(1)
	write(websocket.BinaryMessage, []byte("\x1b[?1049h\x1b[?25l"))
	tick := time.NewTicker(time.Duration(float64(time.Second) / *fps))
	defer tick.Stop()
	for frame := 0; ; frame++ {
		select {
		case <-done:
			return
		case <-tick.C:
		}
		mu.Lock()
		cc, rr := cols, rows
		mu.Unlock()
		if write(websocket.BinaryMessage, topFrame(frame, cc, rr)) != nil {
			return
		}
	}
}

// topFrame draws a screen like top: colored meters over a process table,
// every line rewritten.
func topFrame(frame, cols, rows int) []byte {
	var b bytes.Buffer
	b.WriteString("\x1b[H")
	line := func(s string, visible int) {
		b.WriteString(s)
		if pad := cols - visible; pad > 0 {
			b.WriteString(strings.Repeat(" ", pad))
		}
		b.WriteString("\r\n")
	}
	for cpu := range 4 {
		n := mrand.IntN(cols - 12)
		bar := strings.Repeat("|", n)
		line(fmt.Sprintf("\x1b[1;36m%3d\x1b[0m[\x1b[32m%s\x1b[0m%s]", cpu, bar, strings.Repeat(" ", cols-12-n)), cols-6)
	}
	line(fmt.Sprintf("\x1b[1mTasks:\x1b[0m %d, frame %d", 180+mrand.IntN(20), frame), 20+len(strconv.Itoa(frame)))
	line("\x1b[30;42m  PID USER       CPU%  MEM%  COMMAND\x1b[0m", 38)
	for i := 0; i < rows-7; i++ {
		s := fmt.Sprintf("%5d %-9s %5.1f %5.1f  %s", 1000+i*7, "bench", mrand.Float64()*100, mrand.Float64()*10, []string{"postgres", "nginx", "java", "node", "sshd"}[i%5])
		if len(s) > cols {
			s = s[:cols]
		}
		color := "\x1b[0m"
		if i%3 == 0 {
			color = "\x1b[33m"
		}
		b.WriteString(color)
		line(s+"\x1b[0m", len(s))
	}
	return b.Bytes()
}
