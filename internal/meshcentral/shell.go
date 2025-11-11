package meshcentral

import (
	"bufio"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const exitKey = 0x1D // Ctrl-]

func randomHex() (string, error) {
	bytes := make([]byte, 5)
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

func StartShell(protocol int) {
	<-settings.WebChannel

	id, _ := randomHex()

	settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"action":"msg","nodeid":"%s","type":"tunnel","usage":1,"value":"*/meshrelay.ashx?p=1&nodeid=%s&id=%s&rauth=%s","responseid":"meshctrl"}`,
		settings.RemoteNodeID, settings.RemoteNodeID, id, settings.RCookie)))

	wsUrl, err := url.Parse(fmt.Sprintf("%s?browser=1&p=1&nodeid=%s&id=%s&auth=%s",
		settings.ServerURL, settings.RemoteNodeID, id, settings.ACookie))
	if err != nil {
		fmt.Println("Unable to parse server URL:", err)
		return
	}

	headers := http.Header{}
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: settings.Insecure,
		},
	}

	wsConn, _, err := dialer.Dial(wsUrl.String(), headers)
	if err != nil {
		fmt.Printf("Unable to connect to server: %v\n", err)
		return
	}

	done := make(chan struct{})
	go onShellWebSocket(wsConn, protocol, done)
	<-done

	if settings.debug {
		fmt.Println("Websocket closed")
	}
}

func onShellWebSocket(wsConn *websocket.Conn, protocol int, done chan struct{}) {
	if settings.debug {
		fmt.Println("Websocket connected")
	}
	defer wsConn.Close()

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Failed to set raw mode:", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	quit := make(chan struct{})
	var wg sync.WaitGroup

	// Send RTT every 5 seconds
	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				epoch := time.Now().UnixNano() / int64(time.Millisecond)
				err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"ctrlChannel":102938,"type":"rtt","time":%d}`, epoch)))
				if err != nil {
					return
				}
			}
		}
	}()

	// Read from WebSocket
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			msgType, msg, err := wsConn.ReadMessage()
			if err != nil {
				if settings.debug && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					fmt.Println("Error reading message:", err)
				}
				term.Restore(int(os.Stdin.Fd()), oldState)
				close(quit)
				close(done)
				return
			}

			if msgType != websocket.BinaryMessage {
				if string(msg) == "c" {
					if settings.debug {
						fmt.Println("Received 'c' message")
					}
					sendOptionsUpdate(wsConn, protocol)
					if err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("%d", protocol))); err != nil {
						close(quit)
						close(done)
						return
					}
					continue
				}
			} else {
				os.Stdout.Write(msg)
			}
		}
	}()

	// Read from stdin
	reader := bufio.NewReader(os.Stdin)
	for {
		select {
		case <-quit:
			wg.Wait()
			return
		default:
		}

		r, size, err := reader.ReadRune()
		if err != nil {
			close(quit)
			break
		}

		if r == rune(exitKey) && size == 1 {
			fmt.Fprintln(os.Stderr, "\n[exit] Detected Ctrl-]")
			wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, `{"ctrlChannel":"102938","type":"close"}`))
			close(quit)
			break
		}

		buf := make([]byte, utf8.RuneLen(r))
		utf8.EncodeRune(buf, r)

		err = wsConn.WriteMessage(websocket.BinaryMessage, buf)
		if err != nil {
			close(quit)
			break
		}
	}

	wg.Wait()
}

func sendOptionsUpdate(wsConn *websocket.Conn, protocol int) {
	fd := int(os.Stdout.Fd())
	cols, rows, _ := term.GetSize(fd)

	wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"protocol":%d,"cols":%d,"rows":%d,"xterm":true,"type":"options"}`, protocol, cols, rows)))
}
