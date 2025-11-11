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
	"os/signal"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/gorilla/websocket"
	"golang.org/x/term"
)

const exitKey = 0x1D // Ctrl-]

func randomHex() (string, error) {
	bytes := make([]byte, 5) // n bytes = 2n hex characters
	_, err := rand.Read(bytes)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(bytes), nil
}

// test setting up websocket
func StartShell(protocol int) {
	// wait for server to be authenticated
	<-settings.WebChannel

	id, _ := randomHex()

	settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"action":"msg","nodeid":"%s","type":"tunnel","usage":1,"value":"*/meshrelay.ashx?p=1&nodeid=%s&id=%s&rauth=%s","responseid":"meshctrl"}`,
		settings.RemoteNodeID, settings.RemoteNodeID, id, settings.RCookie)))

	// build url
	wsUrl, err := url.Parse(fmt.Sprintf("%s?browser=1&p=1&nodeid=%s&id=%s&auth=%s",
		settings.ServerURL, settings.RemoteNodeID, id, settings.ACookie))
	if err != nil {
		fmt.Println("Unable to parse server URL:", err)
		return
	}

	// set up headers
	headers := http.Header{}

	// set up websocket dialer
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: settings.Insecure,
		},
	}

	// connect to websocket
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

	// Set raw terminal mode
	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Failed to set raw mode:", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	// Goroutine to send rtt every 5 seconds
	go func() {
		for {
			epoch := time.Now().UnixNano() / int64(time.Millisecond)
			err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"ctrlChannel":102938,"type":"rtt","time":%d}`, epoch)))
			if err != nil {
				fmt.Println("RTT error:", err)
				return
			}
			time.Sleep(5 * time.Second)
		}
	}()

	// Handle terminal resize signals
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)

	go func() {
		for range winch {
			sendOptionsUpdate(wsConn, protocol)
		}
	}()

	// Goroutine to read messages from the websocket
	go func() {
		for {
			// read message from websocket
			msgType, msg, err := wsConn.ReadMessage()
			if err != nil {
				// check if the error is a close message
				if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					if settings.debug {
						fmt.Println("Server closed connection")
					}
				} else {
					fmt.Println("Error reading message:", err)
				}
				term.Restore(int(os.Stdin.Fd()), oldState)
				close(done)
				return
			}

			// if json, handle it
			if msgType != websocket.BinaryMessage {
				// if message is just the letter 'c', send a 1 in response
				if string(msg) == "c" {
					if settings.debug {
						fmt.Println("Received 'c' message")
					}
					sendOptionsUpdate(wsConn, protocol)

					if err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("%d", protocol))); err != nil {
						fmt.Println("Error sending 'c' response:", err)
						return
					}
					continue
				}
				// ignore rtt messages
			} else {
				// print the binary message
				os.Stdout.Write(msg)
				// write the message to stdout as hex
				//fmt.Printf("Received message: %x\n", msg)
			}

		}
	}()

	// handle input from stdin
	// Read from stdin → WebSocket (with UTF-8 rune support)
	reader := bufio.NewReader(os.Stdin)
	for {
		r, size, err := reader.ReadRune()
		if err != nil {
			fmt.Println("stdin read error:", err)
			break
		}

		// Check for exit key (Ctrl-])
		if r == rune(exitKey) && size == 1 {
			fmt.Fprintln(os.Stderr, "\n[exit] Detected Ctrl-]")
			wsConn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, `{"ctrlChannel":"102938","type":"close"}`))
			break
		}

		buf := make([]byte, utf8.RuneLen(r))
		utf8.EncodeRune(buf, r)

		err = wsConn.WriteMessage(websocket.BinaryMessage, buf)
		if err != nil {
			fmt.Println("WebSocket write error:", err)
			break
		}
	}

}

func sendOptionsUpdate(wsConn *websocket.Conn, protocol int) {
	fd := int(os.Stdout.Fd())
	cols, rows, _ := term.GetSize(fd)

	if err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"protocol":%d,"cols":%d,"rows":%d,"xterm":true,"type":"options"}`, protocol, cols, rows))); err != nil {
		fmt.Println("Error sending options message:", err)
		return
	}
}
