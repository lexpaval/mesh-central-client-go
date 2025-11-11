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
		fmt.Println("Unable to parse server URL:", err)
		return
	}

	if settings.RemoteTarget != "" {
		options.RawQuery += fmt.Sprintf("&tcpaddr=%s", settings.RemoteTarget)
	}

	headers := http.Header{}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: settings.Insecure,
		},
	}

	wsConn, _, err := dialer.Dial(options.String(), headers)
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

	// WebSocket -> TCP
	go func() {
		defer closeAll()
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				if settings.debug && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					fmt.Println("WebSocket read error:", err)
				}
				return
			}
			if messageType == websocket.BinaryMessage && len(message) > 0 {
				_, err = tcpConn.Write(message)
				if err != nil {
					if settings.debug {
						fmt.Println("TCP write error:", err)
					}
					return
				}
			}
		}
	}()

	// TCP -> WebSocket
	go func() {
		defer closeAll()
		buf := make([]byte, 32768)
		for {
			n, err := tcpConn.Read(buf)
			if err != nil {
				if err != io.EOF && settings.debug {
					fmt.Println("TCP read error:", err)
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

func StartProxyRouter(ready chan struct{}) {
	defer close(ready)

	options, err := url.Parse(settings.ServerURL)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to parse server URL: %v\n", err)
		os.Exit(1)
		return
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

	headers := http.Header{}
	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: settings.Insecure,
		},
	}

	wsConn, _, err := dialer.Dial(options.String(), headers)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Unable to connect to server: %v\n", err)
		os.Exit(1)
		return
	}

	if settings.debug {
		fmt.Fprintf(os.Stderr, "Proxy WebSocket connected\n")
	}

	defer wsConn.Close()

	done := make(chan struct{})
	var once sync.Once
	closeAll := func() {
		once.Do(func() {
			wsConn.Close()
			close(done)
		})
	}

	// WebSocket -> stdout
	go func() {
		defer closeAll()
		for {
			messageType, message, err := wsConn.ReadMessage()
			if err != nil {
				if settings.debug && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					fmt.Fprintf(os.Stderr, "WebSocket read error: %v\n", err)
				}
				return
			}
			if messageType == websocket.BinaryMessage && len(message) > 0 {
				_, err = os.Stdout.Write(message)
				if err != nil {
					if settings.debug {
						fmt.Fprintf(os.Stderr, "Stdout write error: %v\n", err)
					}
					return
				}
			}
		}
	}()

	// stdin -> WebSocket
	go func() {
		defer closeAll()
		buf := make([]byte, 32768)
		for {
			n, err := os.Stdin.Read(buf)
			if err != nil {
				if err != io.EOF && settings.debug {
					fmt.Fprintf(os.Stderr, "Stdin read error: %v\n", err)
				}
				return
			}
			if n > 0 {
				err = wsConn.WriteMessage(websocket.BinaryMessage, buf[:n])
				if err != nil {
					if settings.debug {
						fmt.Fprintf(os.Stderr, "WebSocket write error: %v\n", err)
					}
					return
				}
			}
		}
	}()

	<-done
}
