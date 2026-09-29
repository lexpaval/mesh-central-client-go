package meshcentral

import (
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
	LocalBindAddress      string
	LocalPort             int
	RemotePort            int
	RemoteTarget          string
	RemoteNodeID          string
	WebSocket             *websocket.Conn
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
	Insecure              bool
	debug                 bool
	closing               bool
	initialAuthDone       bool
}

var settings Settings

func ApplySettings(remoteNodeId string, remotePort int, localPort int, remoteTarget string, insecure bool, debug bool) {
	settings.RemoteNodeID = remoteNodeId
	settings.RemotePort = remotePort
	settings.LocalPort = localPort
	settings.RemoteTarget = remoteTarget
	settings.Insecure = insecure
	settings.debug = debug
}

// SetLocalBindAddress sets the local interface the router listens on, empty means 127.0.0.1
func SetLocalBindAddress(addr string) {
	settings.LocalBindAddress = addr
}

func ApplyAuth(token string, emailToken bool, smsToken bool) {
	settings.Token = token
	settings.EmailToken = emailToken
	settings.SMSToken = smsToken
}
