package meshcentral

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type Device struct {
	Id          string
	Name        string // Hostname (rname)
	DisplayName string // Custom name from MeshCentral
	OS          string
	IP          string
	Icon        int
	Conn        int
	Pwr         int
	MeshID      string // device group
	Group       string // device group name
}

type Settings struct {
	ServerURL             string
	Username              string
	Password              string
	Token                 string
	EmailToken            bool
	SMSToken              bool
	AuthCookie            string
	ServerID              string
	LoginKey              string
	WebSocket             *websocket.Conn
	wsMu                  sync.Mutex // gorilla allows one concurrent writer per conn
	WebChannel            chan struct{}
	AuthErrChannel        chan error
	ACookie               string
	RCookie               string
	RenewCookieTimer      *time.Timer
	ServerAuthClientNonce string
	MeshServerTlsHash     string
	ServerHttpsHash       string
	Devices               []Device
	DeviceQueryState      int
	deviceChan            chan struct{}
	groups                map[string]string // mesh ID -> device group name
	groupChan             chan struct{}
	Insecure              bool
	debug                 bool
	closing               bool
	initialAuthDone       bool
}

var settings Settings

func ApplySettings(insecure bool, debug bool) {
	settings.Insecure = insecure
	settings.debug = debug
}

func ApplyAuth(token string, emailToken bool, smsToken bool) {
	settings.Token = token
	settings.EmailToken = emailToken
	settings.SMSToken = smsToken
}

// TokenPrompt is asked for a 2FA token when the server requires one. It
// returns the token, or "email"/"sms" to have one sent; ok=false aborts the
// login. Defaults to prompting on the controlling terminal.
var TokenPrompt func(email2fa, sms2fa, emailSent bool) (token string, ok bool) = promptForToken

// OnConnectionLost is called when an authenticated session can't be kept
// alive (reconnects exhausted, auth revoked). Defaults to exiting the process.
var OnConnectionLost = func(err error) {
	fmt.Fprintf(os.Stderr, "\n%v\n", err)
	os.Exit(1)
}

// OnNodeEvent is called on the control socket reader for device events the
// server pushes: "nodeconnect" carries the new conn/pwr state, the others
// (devices or groups added, removed, changed) mean the device list is stale.
// Nil in the CLI.
var OnNodeEvent func(action, nodeID string, conn, pwr int)

// send writes a text message on the control socket, serialized against the
// reader goroutine, the cookie renew timer and concurrent callers.
func send(msg []byte) error {
	settings.wsMu.Lock()
	defer settings.wsMu.Unlock()
	if settings.WebSocket == nil {
		return errors.New("not connected to server")
	}
	return settings.WebSocket.WriteMessage(websocket.TextMessage, msg)
}
