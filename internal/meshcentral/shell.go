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

func dialShellTunnel() (*websocket.Conn, error) {
	id, _ := randomHex()

	settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(
		`{"action":"msg","nodeid":"%s","type":"tunnel","usage":1,"value":"*/meshrelay.ashx?p=1&nodeid=%s&id=%s&rauth=%s","responseid":"meshctrl"}`,
		settings.RemoteNodeID, settings.RemoteNodeID, id, settings.RCookie)))

	wsUrl, err := url.Parse(fmt.Sprintf("%s?browser=1&p=1&nodeid=%s&id=%s&auth=%s",
		settings.ServerURL, settings.RemoteNodeID, id, settings.ACookie))
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: settings.Insecure},
	}
	conn, _, err := dialer.Dial(wsUrl.String(), http.Header{})
	return conn, err
}

// runShellSession pipes one tunnel session. Returns (userExited, err).
// userExited=true means Ctrl-] was pressed; err!=nil on unexpected loss.
func runShellSession(wsConn *websocket.Conn, protocol int) (bool, error) {
	if settings.debug {
		fmt.Println("Websocket connected")
	}

	quit := make(chan struct{})
	var sessErr error
	var userExited bool
	var wg sync.WaitGroup

	wg.Go(func() {
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-quit:
				return
			case <-ticker.C:
				epoch := time.Now().UnixNano() / int64(time.Millisecond)
				if err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"ctrlChannel":102938,"type":"rtt","time":%d}`, epoch))); err != nil {
					return
				}
			}
		}
	})

	wg.Go(func() {
		for {
			msgType, msg, err := wsConn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					sessErr = err
				}
				close(quit)
				return
			}
			if msgType != websocket.BinaryMessage {
				if string(msg) == "c" {
					sendOptionsUpdate(wsConn, protocol)
					if err := wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf("%d", protocol))); err != nil {
						sessErr = err
						close(quit)
						return
					}
					continue
				}
			} else {
				os.Stdout.Write(msg)
			}
		}
	})

	reader := bufio.NewReader(os.Stdin)
readLoop:
	for {
		select {
		case <-quit:
			break readLoop
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
			userExited = true
			close(quit)
			break
		}

		buf := make([]byte, utf8.RuneLen(r))
		utf8.EncodeRune(buf, r)
		if err := wsConn.WriteMessage(websocket.BinaryMessage, buf); err != nil {
			close(quit)
			break
		}
	}

	wg.Wait()
	return userExited, sessErr
}

// maxShellReconnectAttempts bounds how many times a dropped shell session
// will be redialed before giving up, so a permanently unreachable server
// produces a clear final message instead of retrying silently forever.
const maxShellReconnectAttempts = 8

func StartShell(protocol int) {
	<-settings.WebChannel

	oldState, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err != nil {
		fmt.Println("Failed to set raw mode:", err)
		return
	}
	defer term.Restore(int(os.Stdin.Fd()), oldState)

	backoff := time.Second
	for attempt := 1; ; attempt++ {
		wsConn, err := dialShellTunnel()
		if err != nil {
			fmt.Printf("Unable to connect to server: %v\n", err)
			return
		}

		userExited, connErr := runShellSession(wsConn, protocol)
		wsConn.Close()

		if userExited || connErr == nil {
			return
		}

		if attempt >= maxShellReconnectAttempts {
			fmt.Fprintf(os.Stderr, "\n[reconnect] Session lost (%v). Giving up after %d attempts.\n", connErr, maxShellReconnectAttempts)
			return
		}

		fmt.Fprintf(os.Stderr, "\n[reconnect] Session lost (%v), retrying...\n", connErr)
		time.Sleep(backoff)
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

func sendOptionsUpdate(wsConn *websocket.Conn, protocol int) {
	fd := int(os.Stdout.Fd())
	cols, rows, _ := term.GetSize(fd)

	wsConn.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"protocol":%d,"cols":%d,"rows":%d,"xterm":true,"type":"options"}`, protocol, cols, rows)))
}
