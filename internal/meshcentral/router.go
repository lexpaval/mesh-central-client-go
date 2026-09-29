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

func GetLocalPort() int {
	return settings.LocalPort
}

func StartRouter(ready chan struct{}) {
	bindAddress := settings.LocalBindAddress
	if bindAddress == "" {
		bindAddress = "127.0.0.1"
	}
	listener, err := net.Listen("tcp", net.JoinHostPort(bindAddress, strconv.Itoa(settings.LocalPort)))
	if err != nil {
		fmt.Printf("Unable to bind to local TCP port %s:%d: %v\n", bindAddress, settings.LocalPort, err)
		os.Exit(1)
		return
	}
	settings.LocalPort = listener.Addr().(*net.TCPAddr).Port
	defer listener.Close()

	<-settings.WebChannel

	close(ready)
	fmt.Printf("Redirecting %s to remote port %d.\n", listener.Addr(), settings.RemotePort)
	fmt.Println("Press ctrl-c to exit.")

	for {
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println("Error accepting connection:", err)
			continue
		}

		go onTcpClientConnected(conn)
	}
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

func onTcpClientConnected(conn net.Conn) {
	if settings.debug {
		fmt.Println("Client connected")
	}
	defer conn.Close()

	conn.(*net.TCPConn).SetKeepAlive(true)
	conn.(*net.TCPConn).SetKeepAlivePeriod(30 * time.Second)

	options, err := url.Parse(fmt.Sprintf("%s?auth=%s&nodeid=%s&tcpport=%d",
		settings.ServerURL, settings.ACookie, settings.RemoteNodeID, settings.RemotePort))
	if err != nil {
		fmt.Printf("Unable to build tunnel URL: %v\n", err)
		return
	}
	if settings.RemoteTarget != "" {
		options.RawQuery += fmt.Sprintf("&tcpaddr=%s", settings.RemoteTarget)
	}

	wsConn, err := dialWithRetry(options.String(), os.Stdout)
	if err != nil {
		fmt.Printf("Unable to connect to server: %v\n", err)
		return
	}

	onWebSocket(wsConn, conn)
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

func onWebSocket(wsConn *websocket.Conn, tcpConn net.Conn) {
	if settings.debug {
		fmt.Println("Websocket connected")
	}
	cause := pumpBidirectional(wsConn, tcpConn, tcpConn, os.Stdout)
	if errors.Is(cause, errTunnelNotEstablished) || errors.Is(cause, errTunnelNoData) {
		fmt.Printf("Tunnel to remote port %d failed: %v\n", settings.RemotePort, cause)
	}
}

// StartProxyRouter runs the SSH ProxyCommand tunnel: stdin/stdout of this
// process ARE the raw SSH byte stream, so unlike the control socket and
// shell session, a lost tunnel here can never be transparently reconnected
// mid-stream without corrupting the SSH transport. Instead, once connected,
// any tunnel loss terminates the process with a clear stderr message so the
// ssh client (and VSCode Remote-SSH) sees the ProxyCommand exit and reports
// the failure instead of hanging forever.
func StartProxyRouter(ready chan struct{}) {
	options, err := url.Parse(settings.ServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to parse server URL: %v\n", err)
		close(ready)
		os.Exit(1)
	}

	query := url.Values{}
	query.Add("auth", settings.ACookie)
	query.Add("nodeid", settings.RemoteNodeID)
	query.Add("tcpport", fmt.Sprintf("%d", settings.RemotePort))
	if settings.RemoteTarget != "" {
		query.Add("tcpaddr", settings.RemoteTarget)
	}
	options.RawQuery = query.Encode()

	if settings.debug {
		fmt.Fprintf(os.Stderr, "Proxy connecting to: %s\n", options.String())
	}

	wsConn, err := dialWithRetry(options.String(), os.Stderr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to server: %v\n", err)
		close(ready)
		os.Exit(1)
	}

	close(ready) // signal ready AFTER successful connect

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
