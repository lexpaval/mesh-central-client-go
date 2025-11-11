package meshcentral

import (
	"time"

	"github.com/gorilla/websocket"
)

type Device struct {
	Id   string
	Name string
	OS   string
	IP   string
	Icon int
	Conn int
	Pwr  int
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
	LocalPort             int
	RemotePort            int
	RemoteTarget          string
	RemoteNodeID          string
	WebSocket             *websocket.Conn
	WebChannel            chan struct{}
	ACookie               string
	RCookie               string
	RenewCookieTimer      *time.Timer
	ServerAuthClientNonce string
	MeshServerTlsHash     string
	ServerHttpsHash       string
	Devices               []Device
	DeviceQueryState      int
	Insecure              bool
	debug                 bool
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
