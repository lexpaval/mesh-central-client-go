package meshcentral

import (
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// Route forwards a local TCP port to a port on a node, or on a host reachable
// from it. Each accepted connection gets its own relay tunnel.
type Route struct {
	NodeID      string
	BindAddress string // empty means 127.0.0.1
	LocalPort   int    // 0 picks a free port, Start writes back the bound one
	Target      string // empty means the node itself
	RemotePort  int
	Out         io.Writer // tunnel errors and debug output, os.Stdout if nil

	listener net.Listener
	mu       sync.Mutex
	conns    map[net.Conn]struct{}
	closed   bool
}

// Start binds the local listener and accepts in the background until Close.
// The control socket must already be authenticated (StartSocket returned).
func (r *Route) Start() error {
	if r.Out == nil {
		r.Out = os.Stdout
	}
	bindAddress := r.BindAddress
	if bindAddress == "" {
		bindAddress = "127.0.0.1"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindAddress, strconv.Itoa(r.LocalPort)))
	if err != nil {
		return fmt.Errorf("unable to bind to local TCP port %s:%d: %w", bindAddress, r.LocalPort, err)
	}
	r.listener = listener
	r.LocalPort = listener.Addr().(*net.TCPAddr).Port
	r.conns = map[net.Conn]struct{}{}

	go func() {
		for {
			conn, err := listener.Accept()
			if errors.Is(err, net.ErrClosed) {
				return
			}
			if err != nil {
				fmt.Fprintln(r.Out, "Error accepting connection:", err)
				continue
			}
			go r.handleConn(conn)
		}
	}()
	return nil
}

// Close stops listening and drops the route's open tunnels.
func (r *Route) Close() {
	if r.listener != nil {
		r.listener.Close()
	}
	r.mu.Lock()
	r.closed = true
	for c := range r.conns {
		c.Close()
	}
	r.mu.Unlock()
}

// Active returns the number of connections currently tunnelled.
func (r *Route) Active() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.conns)
}

func (r *Route) handleConn(conn net.Conn) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		conn.Close()
		return
	}
	r.conns[conn] = struct{}{}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, conn)
		r.mu.Unlock()
		conn.Close()
	}()

	if settings.debug {
		fmt.Fprintln(r.Out, "Client connected")
	}
	conn.(*net.TCPConn).SetKeepAlive(true)
	conn.(*net.TCPConn).SetKeepAlivePeriod(30 * time.Second)

	wsConn, err := dialWithRetry(r.tunnelURL(), r.Out)
	if err != nil {
		fmt.Fprintf(r.Out, "Unable to connect to server: %v\n", err)
		return
	}
	if settings.debug {
		fmt.Fprintln(r.Out, "Websocket connected")
	}
	cause := pumpBidirectional(wsConn, conn, conn, r.Out)
	if errors.Is(cause, errTunnelNotEstablished) || errors.Is(cause, errTunnelNoData) {
		fmt.Fprintf(r.Out, "Tunnel to remote port %d failed: %v\n", r.RemotePort, cause)
	}
}

func (r *Route) tunnelURL() string {
	query := url.Values{}
	query.Add("auth", settings.ACookie)
	query.Add("nodeid", r.NodeID)
	query.Add("tcpport", strconv.Itoa(r.RemotePort))
	if r.Target != "" {
		query.Add("tcpaddr", r.Target)
	}
	return settings.ServerURL + "?" + query.Encode()
}

// dialWithRetry dials a websocket URL with a few retries on transient
// failures (e.g. brief network blip). It does not retry mid-session drops.
// Retry progress (when debug logging is enabled) is written to out.
func dialWithRetry(urlStr string, out io.Writer) (*websocket.Conn, error) {
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig:  &tls.Config{InsecureSkipVerify: settings.Insecure},
	}

	var lastErr error
	backoff := 500 * time.Millisecond
	for attempt := range 4 {
		if attempt > 0 {
			time.Sleep(backoff)
			if backoff < 4*time.Second {
				backoff *= 2
			}
			if settings.debug {
				fmt.Fprintf(out, "Retrying tunnel dial (attempt %d)...\n", attempt+1)
			}
		}
		conn, _, err := dialer.Dial(urlStr, http.Header{})
		if err == nil {
			return conn, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

// Tunnel failures the relay reports only by closing the WebSocket, turned
// into errors so callers can tell the user what went wrong.
var (
	errTunnelNotEstablished = errors.New("server closed the tunnel before the device connected (device offline, wrong node ID, or no access)")
	errTunnelNoData         = errors.New("device accepted the tunnel but closed it without sending data (is a service listening on the remote port?)")
)

// pumpBidirectional relays bytes between wsConn and a local stream (src/dst
// may be the same net.Conn, or split streams like stdin/stdout). It blocks
// until either side closes, then returns the error that caused the shutdown
// (nil for a graceful WebSocket close or a clean EOF on src, or one of the
// errTunnel* errors if the relay closes before any data flowed). It only closes
// wsConn itself; closing src/dst is the caller's responsibility, since some
// callers (stdin/stdout) must not be closed.
func pumpBidirectional(wsConn *websocket.Conn, src io.Reader, dst io.Writer, debugOut io.Writer) error {
	done := make(chan struct{})
	var once sync.Once
	var cause error
	closeAll := func(err error) {
		once.Do(func() {
			cause = err
			wsConn.Close()
			close(done)
		})
	}

	// WS -> dst: each WS message is already a complete chunk, write it
	// straight to dst with no intermediate buffering. The relay sends "c"
	// (or "cr" when recorded) once the device side joins the tunnel.
	go func() {
		established, gotData := false, false
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				if !established {
					closeAll(errTunnelNotEstablished)
				} else if !gotData {
					closeAll(errTunnelNoData)
				} else if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					closeAll(nil)
				} else {
					if settings.debug {
						fmt.Fprintln(debugOut, "WebSocket read error:", err)
					}
					closeAll(err)
				}
				return
			}
			if messageType == websocket.TextMessage && (string(message) == "c" || string(message) == "cr") {
				established = true
			}
			if messageType == websocket.BinaryMessage && len(message) > 0 {
				established, gotData = true, true
				if _, err := dst.Write(message); err != nil {
					if settings.debug {
						fmt.Fprintln(debugOut, "Write error (WS -> dst):", err)
					}
					closeAll(err)
					return
				}
			}
		}
	}()

	// src -> WS: read a chunk, forward it as one WS message.
	go func() {
		buf := make([]byte, 32768) // Reused across reads
		for {
			n, err := src.Read(buf)
			if n > 0 {
				if werr := wsConn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					if settings.debug {
						fmt.Fprintln(debugOut, "WebSocket write error:", werr)
					}
					closeAll(werr)
					return
				}
			}
			if err != nil {
				if err == io.EOF {
					closeAll(nil) // src closed: session ended normally
				} else {
					if settings.debug {
						fmt.Fprintln(debugOut, "Read error (src -> WS):", err)
					}
					closeAll(err)
				}
				return
			}
		}
	}()

	<-done
	return cause
}

// StartProxyRouter runs the SSH ProxyCommand tunnel: stdin/stdout of this
// process ARE the raw SSH byte stream, so unlike the control socket and
// shell session, a lost tunnel here can never be transparently reconnected
// mid-stream without corrupting the SSH transport. Instead, once connected,
// any tunnel loss terminates the process with a clear stderr message so the
// ssh client (and VSCode Remote-SSH) sees the ProxyCommand exit and reports
// the failure instead of hanging forever.
func StartProxyRouter(r *Route) {
	if settings.debug {
		fmt.Fprintf(os.Stderr, "Proxy connecting to: %s\n", r.tunnelURL())
	}

	wsConn, err := dialWithRetry(r.tunnelURL(), os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to server: %v\n", err)
		os.Exit(1)
	}

	if settings.debug {
		fmt.Fprintf(os.Stderr, "Proxy WebSocket connected\n")
	}

	cause := pumpBidirectional(wsConn, os.Stdin, os.Stdout, os.Stderr)

	if cause != nil {
		fmt.Fprintf(os.Stderr, "\nProxy tunnel to MeshCentral lost: %v\n", cause)
		os.Exit(1)
	}
	os.Exit(0)
}
