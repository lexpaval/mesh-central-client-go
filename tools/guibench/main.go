// guibench compares the CPU and memory use of GUI builds under the same
// load: it serves a fake MeshCentral server (devices, a stream of node
// events, shells redrawing a full screen like top), starts each GUI against
// it with a profile in a scratch config folder, and samples /proc (Linux).
//
//	go run ./tools/guibench [flags] <gui binary>...
//
// With -soak it runs the GUIs side by side for that long instead, sampling
// their memory to show growth, typically with -headless (each in its own
// headless mutter, nothing shows on the desktop) and -hide (the window hidden
// as if minimized, which gets no frame callbacks on Wayland).
//
// The GUIs connect and open shells on their own when MCC_GUI_BENCH is set to
// the number of shells, see bench.go in mcc-gui.
package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
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
	shellSet = flag.String("shells", "0,2", "shell counts to run each GUI with (-soak: the first)")
	soak     = flag.Duration("soak", 0, "run the GUIs side by side this long, sampling memory growth")
	interval = flag.Duration("sample", 10*time.Second, "sampling interval with -soak")
	headless = flag.Bool("headless", false, "run each GUI in its own headless mutter (Wayland, Xwayland for X11 GUIs)")
	hide     = flag.Bool("hide", false, "have the GUIs hide their window once connected, as minimized")
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
	var shellCounts []int
	for _, s := range strings.Split(*shellSet, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(s))
		if err != nil {
			log.Fatalf("bad -shells %q", s)
		}
		shellCounts = append(shellCounts, n)
	}
	if *soak > 0 {
		soakAll(addr, shellCounts[0])
		return
	}

	var results []result
	for _, shells := range shellCounts {
		for i, bin := range flag.Args() {
			r, err := bench(bin, addr, shells, fmt.Sprintf("run%d-%d-%d", os.Getpid(), shells, i))
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

// proc is a running GUI, pid is the GUI itself (not mutter's) once found.
type proc struct {
	name, id string
	cmd      *exec.Cmd
	pid      int
	out      bytes.Buffer
	cfg      string
}

// start launches a GUI connected to the fake server as user id, which the
// server counts its device lists and shells under.
func start(bin, addr string, shells int, id string) (*proc, error) {
	p := &proc{name: filepath.Base(bin), id: id}
	abs, err := filepath.Abs(bin)
	if err != nil {
		return nil, err
	}
	if p.cfg, err = os.MkdirTemp("", "guibench"); err != nil {
		return nil, err
	}
	conf := map[string]any{
		"default_profile": "guibench",
		"profiles":        []map[string]string{{"name": "guibench", "server": addr, "username": id}},
	}
	b, _ := json.Marshal(conf)
	os.MkdirAll(filepath.Join(p.cfg, "mcc"), 0o700)
	if err := os.WriteFile(filepath.Join(p.cfg, "mcc", "meshcentral-client.json"), b, 0o600); err != nil {
		return nil, err
	}

	env := append(os.Environ(), "XDG_CONFIG_HOME="+p.cfg, "MCC_GUI_BENCH="+strconv.Itoa(shells), "GUIBENCH_RUN="+id)
	if *hide {
		env = append(env, "MCC_GUI_BENCH_HIDE=1")
	}
	if *headless {
		// A session bus of its own that starts no services: the keyring would
		// wait for an unlock prompt nobody sees, without one the password
		// lookup fails at once (the fake server takes any).
		busConf := filepath.Join(p.cfg, "bus.conf")
		os.WriteFile(busConf, []byte(`<busconfig><type>session</type><listen>unix:tmpdir=/tmp</listen><auth>EXTERNAL</auth>`+
			`<policy context="default"><allow send_destination="*" eavesdrop="true"/><allow eavesdrop="true"/><allow own="*"/></policy></busconfig>`), 0o600)
		p.cmd = exec.Command("dbus-run-session", "--config-file="+busConf, "--", "mutter", "--headless", "--wayland",
			"--wayland-display", "guibench-"+id, "--virtual-monitor", "1280x800", "--", abs)
		env = slicesDelete(env, "DISPLAY=", "WAYLAND_DISPLAY=")
	} else {
		p.cmd = exec.Command(abs)
	}
	p.cmd.Env = env
	p.cmd.Stdout, p.cmd.Stderr = &p.out, &p.out
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := p.cmd.Start(); err != nil {
		return nil, err
	}
	// The GUI is a child of mutter when headless, found by its binary and run ID.
	for range 100 {
		if p.pid = findPID(abs, id); p.pid != 0 {
			return p, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	p.stop()
	return nil, fmt.Errorf("didn't start:\n%s", p.out.String())
}

func slicesDelete(env []string, prefixes ...string) []string {
	var out []string
	for _, e := range env {
		keep := true
		for _, p := range prefixes {
			keep = keep && !strings.HasPrefix(e, p)
		}
		if keep {
			out = append(out, e)
		}
	}
	return out
}

func findPID(exe, id string) int {
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		if e, _ := os.Readlink(fmt.Sprintf("/proc/%d/exe", pid)); e != exe {
			continue
		}
		env, _ := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if bytes.Contains(env, []byte("\x00GUIBENCH_RUN="+id+"\x00")) {
			return pid
		}
	}
	return 0
}

func (p *proc) alive() bool { return p.pid != 0 && syscall.Kill(p.pid, 0) == nil }

// stop ends the GUI and whatever runs it (mutter, dbus).
func (p *proc) stop() {
	pgid := p.cmd.Process.Pid
	syscall.Kill(-pgid, syscall.SIGTERM)
	done := make(chan struct{})
	go func() { p.cmd.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		syscall.Kill(-pgid, syscall.SIGKILL)
		<-done
	}
	os.RemoveAll(p.cfg)
}

// warm waits out the warmup and checks the GUI got the full load.
func (p *proc) warm(shells int) (lists, up int64, err error) {
	time.Sleep(*warmup)
	c := counters(p.id)
	lists, up = c.lists.Load(), c.shells.Load()
	if !p.alive() {
		return lists, up, fmt.Errorf("exited during warmup:\n%s", p.out.String())
	}
	if lists == 0 || up < int64(shells) {
		log.Printf("%s: only %d device lists and %d of %d shells served, the numbers don't show the full load", p.name, lists, up, shells)
	}
	return lists, up, nil
}

// bench starts a GUI and samples its CPU and memory for -measure.
func bench(bin, addr string, shells int, id string) (result, error) {
	r := result{name: filepath.Base(bin), shells: shells}
	p, err := start(bin, addr, shells, id)
	if err != nil {
		return r, err
	}
	defer p.stop()
	log.Printf("%s with %d shells: warming up", r.name, shells)
	if r.lists, r.shellsUp, err = p.warm(shells); err != nil {
		return r, err
	}

	prev, _ := cpuTicks(p.pid)
	start := prev
	var rssSum float64
	n := int(*measure / time.Second)
	for i := 0; i < n; i++ {
		time.Sleep(time.Second)
		t, err := cpuTicks(p.pid)
		if err != nil {
			return r, fmt.Errorf("exited while measuring:\n%s", p.out.String())
		}
		r.cpuMax = max(r.cpuMax, float64(t-prev)) // 100 ticks a second, so ticks are %
		prev = t
		rss := statusMB(p.pid, "VmRSS:")
		rssSum += rss
		r.rssMax = max(r.rssMax, rss)
	}
	r.cpuAvg = float64(prev-start) / float64(n)
	r.rss = rssSum / float64(n)
	r.pss = rollupMB(p.pid, "Pss:")
	r.threads = int(statusMB(p.pid, "Threads:") * 1024)
	log.Printf("%s with %d shells: CPU %.1f%%, RSS %.0f MB", r.name, shells, r.cpuAvg, r.rss)
	return r, nil
}

// soakAll runs every GUI at once for -soak, sampling RSS every -sample, and
// reports how it grew: a least squares fit over all samples, and a series.
func soakAll(addr string, shells int) {
	type series struct {
		p       *proc
		t, rss  []float64 // minutes since measuring started, MB
		ticks   int64
		up      int64
		err     error
		elapsed float64
	}
	runs := make([]*series, flag.NArg())
	var wg sync.WaitGroup
	for i, bin := range flag.Args() {
		s := &series{}
		runs[i] = s
		wg.Go(func() {
			p, err := start(bin, addr, shells, fmt.Sprintf("soak%d-%d", os.Getpid(), i))
			if err != nil {
				s.err = err
				return
			}
			s.p = p
			defer p.stop()
			if _, s.up, s.err = p.warm(shells); s.err != nil {
				return
			}
			log.Printf("%s: soaking for %s", p.name, *soak)
			t0, c0 := time.Now(), must(cpuTicks(p.pid))
			for time.Since(t0) < *soak {
				time.Sleep(*interval)
				if !p.alive() {
					s.err = fmt.Errorf("exited after %s:\n%s", time.Since(t0).Round(time.Second), p.out.String())
					return
				}
				s.t = append(s.t, time.Since(t0).Minutes())
				s.rss = append(s.rss, statusMB(p.pid, "VmRSS:"))
			}
			s.elapsed = time.Since(t0).Seconds()
			s.ticks = must(cpuTicks(p.pid)) - c0
		})
	}
	wg.Wait()

	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', tabwriter.AlignRight)
	fmt.Fprintln(tw, "GUI\tshells (up)\tCPU avg %\tRSS start MB\tRSS end MB\tRSS max MB\tgrowth MB/h\t2nd half MB/h\t")
	for _, s := range runs {
		if s.err != nil || len(s.rss) < 2 {
			name := "?"
			if s.p != nil {
				name = s.p.name
			}
			fmt.Fprintf(tw, "%s\tfailed: %v\t\t\t\t\t\t\t\n", name, s.err)
			continue
		}
		n := len(s.rss)
		end := mean(s.rss[max(0, n-6):])
		fmt.Fprintf(tw, "%s\t%d (%d)\t%.1f\t%.0f\t%.0f\t%.0f\t%+.1f\t%+.1f\t\n", s.p.name, shells, s.up, float64(s.ticks)/s.elapsed,
			s.rss[0], end, maxOf(s.rss), slope(s.t, s.rss)*60, slope(s.t[n/2:], s.rss[n/2:])*60)
	}
	tw.Flush()
	fmt.Printf("\nRSS every %s over %s after %s warmup, growth is a least squares fit over all samples, 2nd half over\nthe second half only (past start-up), end the mean of the last minute.", *interval, *soak, *warmup)
	if *hide {
		fmt.Print(" Windows hidden once connected.")
	}
	fmt.Println("\n\nRSS by minute (MB):")
	for _, s := range runs {
		if s.err != nil || len(s.rss) == 0 {
			continue
		}
		var b strings.Builder
		next := 0.0
		for i, t := range s.t {
			if t >= next {
				fmt.Fprintf(&b, " %.0f", s.rss[i])
				next = float64(int(t)) + 1
			}
		}
		fmt.Printf("  %s:%s\n", s.p.name, b.String())
	}
}

func must[T any](v T, err error) T {
	if err != nil {
		log.Fatal(err)
	}
	return v
}

func mean(v []float64) float64 {
	var s float64
	for _, x := range v {
		s += x
	}
	return s / float64(len(v))
}

func maxOf(v []float64) float64 {
	m := v[0]
	for _, x := range v {
		m = max(m, x)
	}
	return m
}

// slope is the least squares slope of y over x.
func slope(x, y []float64) float64 {
	mx, my := mean(x), mean(y)
	var num, den float64
	for i := range x {
		num += (x[i] - mx) * (y[i] - my)
		den += (x[i] - mx) * (x[i] - mx)
	}
	if den == 0 {
		return 0
	}
	return num / den
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

// What the server did per user (run), to check each GUI loaded the devices
// and got its shells.
type served struct{ lists, shells atomic.Int64 }

var (
	servedMu sync.Mutex
	servedBy = map[string]*served{}
)

func counters(user string) *served {
	servedMu.Lock()
	defer servedMu.Unlock()
	c := servedBy[user]
	if c == nil {
		c = &served{}
		servedBy[user] = c
	}
	return c
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

var oses = []string{"Fedora Linux 44 (Server Edition)", "Microsoft Windows 11 Pro - 24H2/26100", "Ubuntu 24.04.1 LTS",
	"Debian GNU/Linux 12 (bookworm)", "Raspbian GNU/Linux 12 (bookworm)", "macOS 15.3", "openSUSE Tumbleweed"}

func deviceID(i int) string { return fmt.Sprintf("node//bench%05d", i) }

// control is the session's control socket: login, device list, events.
func control(w http.ResponseWriter, r *http.Request) {
	// The user names the run, and the auth cookie the relay sees.
	user, _, _ := strings.Cut(r.Header.Get("X-Meshauth"), ",")
	if b, err := base64.StdEncoding.DecodeString(user); err == nil {
		user = string(b)
	}
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
			send(map[string]string{"action": "authcookie", "cookie": user, "rcookie": user})
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
			counters(user).lists.Add(1)
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
	counters(r.URL.Query().Get("auth")).shells.Add(1)
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
