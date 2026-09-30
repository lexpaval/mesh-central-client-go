package meshcentral

import (
	"bytes"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sync"
	"sync/atomic"
	"time"

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

func dialShellTunnel(nodeID string) (*websocket.Conn, error) {
	id, _ := randomHex()

	if err := send([]byte(fmt.Sprintf(
		`{"action":"msg","nodeid":"%s","type":"tunnel","usage":1,"value":"*/meshrelay.ashx?p=1&nodeid=%s&id=%s&rauth=%s","responseid":"meshctrl"}`,
		nodeID, nodeID, id, settings.RCookie))); err != nil {
		return nil, err
	}

	wsUrl, err := url.Parse(fmt.Sprintf("%s?browser=1&p=1&nodeid=%s&id=%s&auth=%s",
		settings.ServerURL, nodeID, id, settings.ACookie))
	if err != nil {
		return nil, err
	}

	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: settings.Insecure},
	}
	conn, _, err := dialer.Dial(wsUrl.String(), http.Header{})
	return conn, err
}

// runShellSession pipes one tunnel session between input and out. Returns
// (inputClosed, err): inputClosed means the user ended it, err is set when
// the tunnel dropped unexpectedly.
func runShellSession(wsConn *websocket.Conn, protocol int, input <-chan []byte, out io.Writer, size func() (int, int), resize <-chan struct{}, recorded func()) (bool, error) {
	if settings.debug {
		fmt.Fprintln(os.Stderr, "Websocket connected")
	}

	// The keepalive, the reader's handshake reply and the input loop all write.
	var wmu sync.Mutex
	write := func(msgType int, b []byte) error {
		wmu.Lock()
		defer wmu.Unlock()
		return wsConn.WriteMessage(msgType, b)
	}
	sendOptions := func() error {
		cols, rows := size()
		if cols <= 0 || rows <= 0 {
			cols, rows = 80, 24 // terminal not laid out yet, a resize follows
		}
		return write(websocket.TextMessage, []byte(fmt.Sprintf(`{"protocol":%d,"cols":%d,"rows":%d,"xterm":true,"type":"options"}`, protocol, cols, rows)))
	}
	// The options only size the session as it starts, the agent resizes its
	// terminal on termsize. Until it has started the options send the latest size.
	var started atomic.Bool
	sendSize := func() error {
		cols, rows := size()
		if !started.Load() || cols <= 0 || rows <= 0 {
			return nil
		}
		return write(websocket.TextMessage, []byte(fmt.Sprintf(`{"ctrlChannel":"102938","type":"termsize","cols":%d,"rows":%d}`, cols, rows)))
	}

	quit := make(chan struct{})
	var sessErr error
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
				if err := write(websocket.TextMessage, []byte(fmt.Sprintf(`{"ctrlChannel":102938,"type":"rtt","time":%d}`, epoch))); err != nil {
					return
				}
			}
		}
	})

	wg.Go(func() {
		defer close(quit)
		for {
			msgType, msg, err := wsConn.ReadMessage()
			if err != nil {
				if !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
					sessErr = err
				}
				return
			}
			if msgType == websocket.BinaryMessage {
				out.Write(msg)
			} else if string(msg) == "c" || string(msg) == "cr" { // "cr" when the session is recorded
				if string(msg) == "cr" && recorded != nil {
					recorded()
				}
				sendOptions()
				if err := write(websocket.TextMessage, []byte(fmt.Sprintf("%d", protocol))); err != nil {
					sessErr = err
					return
				}
				started.Store(true)
				sendSize() // in case it changed since the options
			}
		}
	})

	inputClosed := false
loop:
	for {
		select {
		case <-quit:
			break loop
		case <-resize:
			sendSize()
		case b, ok := <-input:
			if !ok {
				write(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, `{"ctrlChannel":"102938","type":"close"}`))
				inputClosed = true
				break loop
			}
			if err := write(websocket.BinaryMessage, b); err != nil {
				break loop
			}
		}
	}
	wsConn.Close() // unblocks the reader
	wg.Wait()
	return inputClosed, sessErr
}

// maxShellReconnectAttempts bounds how many times a dropped shell session
// will be redialed before giving up, so a permanently unreachable server
// produces a clear final message instead of retrying silently forever.
const maxShellReconnectAttempts = 8

// RunShell runs an interactive shell on nodeID (protocol 1 is the device's
// default shell, 6 PowerShell on Windows agents) between in and out,
// redialing if the tunnel drops. It returns once in hits EOF, the remote shell
// exits, or the session can't be restored. size reports the terminal size,
// resize (may be nil) signals that it changed. recorded (may be nil) is
// called when the server says it records the session, again on reconnects.
func RunShell(nodeID string, protocol int, in io.Reader, out io.Writer, size func() (cols, rows int), resize <-chan struct{}, recorded func()) error {
	done := make(chan struct{})
	defer close(done)

	// One reader across reconnects, so a dropped session doesn't leave a
	// blocked Read behind that swallows the next session's keystrokes.
	input := make(chan []byte)
	go func() {
		defer close(input)
		buf := make([]byte, 4096)
		for {
			n, err := in.Read(buf)
			if n > 0 {
				select {
				case input <- append([]byte(nil), buf[:n]...):
				case <-done:
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	backoff := time.Second
	for attempt := 1; ; attempt++ {
		wsConn, err := dialShellTunnel(nodeID)
		if err != nil {
			return fmt.Errorf("unable to connect to server: %w", err)
		}

		inputClosed, err := runShellSession(wsConn, protocol, input, out, size, resize, recorded)
		if inputClosed || err == nil {
			return nil
		}
		if attempt >= maxShellReconnectAttempts {
			return fmt.Errorf("session lost (%v), giving up after %d attempts", err, maxShellReconnectAttempts)
		}

		fmt.Fprintf(out, "\r\n[reconnect] Session lost (%v), retrying...\r\n", err)
		time.Sleep(backoff)
		if backoff < 15*time.Second {
			backoff *= 2
		}
	}
}

// StartShell runs a shell on the controlling terminal in raw mode, Ctrl-]
// ends the session.
func StartShell(nodeID string, protocol int) {
	fd := int(os.Stdin.Fd())
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		fmt.Println("Failed to set raw mode:", err)
		return
	}
	defer term.Restore(fd, oldState)

	in := readerFunc(func(p []byte) (int, error) {
		n, err := os.Stdin.Read(p)
		if i := bytes.IndexByte(p[:n], exitKey); i >= 0 {
			fmt.Fprint(os.Stderr, "\r\n[exit] Detected Ctrl-]\r\n")
			return i, io.EOF
		}
		return n, err
	})
	size := func() (int, int) {
		cols, rows, _ := term.GetSize(int(os.Stdout.Fd()))
		return cols, rows
	}
	var once sync.Once
	recorded := func() {
		once.Do(func() { fmt.Fprint(os.Stderr, "\r\n[recorded] The server records this session\r\n") })
	}
	if err := RunShell(nodeID, protocol, in, os.Stdout, size, nil, recorded); err != nil {
		fmt.Fprintf(os.Stderr, "\r\n%v\r\n", err)
	}
}

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }
