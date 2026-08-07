package meshcentral

import (
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

func GetLocalPort() int {
	return settings.LocalPort
}

func StartRouter(ready chan struct{}) {
	listener, err := net.Listen("tcp", fmt.Sprintf(":%d", settings.LocalPort))
	if err != nil {
		fmt.Printf("Unable to bind to local TCP port %d: %v\n", settings.LocalPort, err)
		os.Exit(1)
		return
	}
	settings.LocalPort = listener.Addr().(*net.TCPAddr).Port
	defer listener.Close()

	<-settings.WebChannel

	close(ready)
	fmt.Printf("Redirecting local port %d to remote port %d.\n", listener.Addr().(*net.TCPAddr).Port, settings.RemotePort)
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

func onWebSocket(wsConn *websocket.Conn, tcpConn net.Conn) {
	if settings.debug {
		fmt.Println("Websocket connected")
	}

	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			wsConn.Close()
			tcpConn.Close()
			close(done)
		})
	}

	// Create pipes for each direction
	wsToTcpReader, wsToTcpWriter := io.Pipe()
	tcpToWsReader, tcpToWsWriter := io.Pipe()

	// WebSocket reader -> pipe writer (for WS -> TCP)
	go func() {
		defer wsToTcpWriter.Close()
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				if settings.debug && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					fmt.Println("WebSocket read error:", err)
				}
				wsToTcpWriter.CloseWithError(err)
				return
			}
			if messageType == websocket.BinaryMessage && len(message) > 0 {
				_, err = wsToTcpWriter.Write(message)
				if err != nil {
					if settings.debug {
						fmt.Println("Pipe write error (WS -> TCP):", err)
					}
					return
				}
			}
		}
	}()

	// Pipe reader -> TCP writer (WS -> TCP)
	go func() {
		defer closeAll()
		_, err := io.Copy(tcpConn, wsToTcpReader)
		if err != nil && settings.debug {
			fmt.Println("io.Copy error (WS -> TCP):", err)
		}
	}()

	// TCP reader -> pipe writer (TCP -> WS)
	go func() {
		defer tcpToWsWriter.Close()
		_, err := io.Copy(tcpToWsWriter, tcpConn)
		if err != nil && settings.debug {
			fmt.Println("io.Copy error (TCP -> WS pipe):", err)
		}
	}()

	// Pipe reader -> WebSocket writer (TCP -> WS)
	go func() {
		defer closeAll()
		buf := make([]byte, 32768) // Reuse buffer for chunked writes to WS
		for {
			n, err := tcpToWsReader.Read(buf)
			if err != nil {
				if err != io.EOF && settings.debug {
					fmt.Println("Pipe read error (TCP -> WS):", err)
				}
				return
			}
			if n > 0 {
				err = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
				if err != nil {
					if settings.debug {
						fmt.Println("WebSocket write error:", err)
					}
					return
				}
			}
		}
	}()

	<-done
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

	done := make(chan struct{})
	var once sync.Once
	var closeCause error
	closeAll := func(cause error) {
		once.Do(func() {
			closeCause = cause
			wsConn.Close()
			close(done)
		})
	}

	// Create pipes for each direction
	wsToStdoutReader, wsToStdoutWriter := io.Pipe()
	stdinToWsReader, stdinToWsWriter := io.Pipe()

	// WebSocket reader -> pipe writer (WS -> stdout)
	go func() {
		defer wsToStdoutWriter.Close()
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					return // graceful close: deferred Close() yields a plain EOF downstream
				}
				if settings.debug {
					fmt.Fprintf(os.Stderr, "WebSocket read error: %v\n", err)
				}
				wsToStdoutWriter.CloseWithError(err)
				return
			}
			if messageType == websocket.BinaryMessage && len(message) > 0 {
				_, err = wsToStdoutWriter.Write(message)
				if err != nil {
					if settings.debug {
						fmt.Fprintf(os.Stderr, "Pipe write error (WS -> stdout): %v\n", err)
					}
					return
				}
			}
		}
	}()

	// Pipe reader -> stdout writer (WS -> stdout)
	go func() {
		_, err := io.Copy(os.Stdout, wsToStdoutReader)
		if err != nil && settings.debug {
			fmt.Fprintf(os.Stderr, "io.Copy error (WS -> stdout): %v\n", err)
		}
		closeAll(err)
	}()

	// stdin reader -> pipe writer (stdin -> WS)
	go func() {
		defer stdinToWsWriter.Close()
		_, err := io.Copy(stdinToWsWriter, os.Stdin)
		if err != nil && settings.debug {
			fmt.Fprintf(os.Stderr, "io.Copy error (stdin -> WS pipe): %v\n", err)
		}
	}()

	// Pipe reader -> WebSocket writer (stdin -> WS)
	go func() {
		buf := make([]byte, 32768) // Reuse buffer for chunked writes to WS
		for {
			n, err := stdinToWsReader.Read(buf)
			if err != nil {
				if err == io.EOF {
					closeAll(nil) // ssh client closed stdin: session ended normally
				} else {
					if settings.debug {
						fmt.Fprintf(os.Stderr, "Pipe read error (stdin -> WS): %v\n", err)
					}
					closeAll(err)
				}
				return
			}
			if n > 0 {
				err = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
				if err != nil {
					if settings.debug {
						fmt.Fprintf(os.Stderr, "WebSocket write error: %v\n", err)
					}
					closeAll(err)
					return
				}
			}
		}
	}()

	<-done

	if closeCause != nil {
		fmt.Fprintf(os.Stderr, "\nProxy tunnel to MeshCentral lost: %v\n", closeCause)
		os.Exit(1)
	}
	os.Exit(0)
}
