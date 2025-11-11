package meshcentral

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lexpaval/mesh-central-client-go/internal/config"
)

func StartSocket() {
	p := config.GetDefaultProfile()

	settings.Username = p.Username
	settings.Password = p.Password
	settings.ServerURL = "wss://" + p.Server + "/meshrelay.ashx"

	var options *url.URL
	var err error

	options, err = url.Parse(settings.ServerURL)
	if err != nil {
		fmt.Println("Unable to parse server URL.")
		os.Exit(1)
		return
	}

	xtoken := ""
	if settings.EmailToken {
		xtoken = "**email**"
	} else if settings.SMSToken {
		xtoken = "**sms**"
	} else if settings.Token != "" {
		xtoken = settings.Token
	}

	headers := http.Header{}
	if settings.ServerID == "" {
		if settings.AuthCookie != "" {
			options.RawQuery = fmt.Sprintf("auth=%s", settings.AuthCookie)
			if xtoken != "" {
				options.RawQuery += fmt.Sprintf("&token=%s", xtoken)
			}
		} else {
			auth := base64.StdEncoding.EncodeToString([]byte(settings.Username)) + "," +
				base64.StdEncoding.EncodeToString([]byte(settings.Password))
			if xtoken != "" {
				auth += "," + base64.StdEncoding.EncodeToString([]byte(xtoken))
			}
			headers.Add("x-meshauth", auth)
		}
	} else {
		headers.Add("x-meshauth", "*")
	}

	urlStr := strings.Replace(settings.ServerURL, "meshrelay.ashx", "control.ashx", 1)

	dialer := websocket.Dialer{
		HandshakeTimeout: 10 * time.Second,
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: settings.Insecure,
		},
	}
	conn, _, err := dialer.Dial(urlStr, headers)
	if err != nil {
		fmt.Printf("Unable to connect to server: %v\n", err)
		os.Exit(1)
		return
	}

	if settings.debug {
		fmt.Println("Connected to server.")
	}

	settings.WebChannel = make(chan struct{})
	settings.WebSocket = conn
	go onServerWebSocket(conn)

	<-settings.WebChannel
}

func StopSocket() {
	// Stop timer before closing connection
	if settings.RenewCookieTimer != nil {
		settings.RenewCookieTimer.Stop()
		settings.RenewCookieTimer = nil
	}

	if settings.WebSocket != nil {
		settings.WebSocket.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(1000, "all done"))
		time.Sleep(100 * time.Millisecond)
		settings.WebSocket.Close()
	}
}

func onServerWebSocket(conn *websocket.Conn) {
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				if settings.debug {
					fmt.Println("Server closed connection")
				}
				return
			}
			fmt.Println("Server connection error:", err)
			return
		}

		var command map[string]interface{}
		if err := json.Unmarshal(message, &command); err != nil {
			fmt.Println("Error parsing command:", err)
			continue
		}

		switch command["action"] {
		case "close":
			handleCloseCommand(command)
		case "serverinfo":
			conn.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie"}`))
		case "authcookie":
			handleAuthCookieCommand(command)
		case "serverAuth":
			handleServerAuthCommand(command)
		case "nodes":
			handleNodesCommand(command)
		}
	}
}

func handleCloseCommand(command map[string]interface{}) {
	if command["cause"] == "noauth" {
		switch command["msg"] {
		case "tokenrequired":
			if command["email2fasent"] == true {
				fmt.Println("Login token email sent.")
			} else if command["email2fa"] == true && command["sms2fa"] == true {
				fmt.Println("Login token required, use --token [token], or --emailtoken, --smstoken get a token.")
			} else if command["sms2fa"] == true {
				fmt.Println("Login token required, use --token [token], or --smstoken get a token.")
			} else if command["email2fa"] == true {
				fmt.Println("Login token required, use --token [token], or --emailtoken get a token.")
			} else {
				fmt.Println("Login token required, use --token [token].")
			}
		case "badtlscert":
			fmt.Println("Invalid TLS certificate detected.")
		case "badargs":
			fmt.Println("Invalid protocol arguments.")
		default:
			fmt.Println("Invalid username/password.")
		}
		os.Exit(1)
	} else {
		if settings.debug {
			fmt.Println("Server disconnected:", command["msg"])
		}
	}
}

func handleAuthCookieCommand(command map[string]interface{}) {
	if settings.ACookie == "" {
		settings.ACookie = command["cookie"].(string)
		settings.RCookie = command["rcookie"].(string)
		settings.RenewCookieTimer = time.AfterFunc(10*time.Minute, func() {
			if settings.WebSocket != nil {
				settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie"}`))
			}
		})
		close(settings.WebChannel)
	} else {
		// Stop old timer before creating new one
		if settings.RenewCookieTimer != nil {
			settings.RenewCookieTimer.Stop()
		}
		settings.ACookie = command["cookie"].(string)
		settings.RCookie = command["rcookie"].(string)
		settings.RenewCookieTimer = time.AfterFunc(10*time.Minute, func() {
			if settings.WebSocket != nil {
				settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie"}`))
			}
		})
	}
}

func handleServerAuthCommand(command map[string]interface{}) {
	settings.ServerID = ""
	settings.ServerHttpsHash = settings.MeshServerTlsHash
	settings.MeshServerTlsHash = ""

	xtoken := ""
	if settings.EmailToken {
		xtoken = "**email**"
	} else if settings.SMSToken {
		xtoken = "**sms**"
	} else if settings.Token != "" {
		xtoken = settings.Token
	}

	auth := ""
	if settings.AuthCookie != "" {
		auth = fmt.Sprintf(`{"action":"userAuth","auth":"%s"`, settings.AuthCookie)
		if xtoken != "" {
			auth += fmt.Sprintf(`,"token":"%s"`, xtoken)
		}
		auth += "}"
	} else {
		auth = fmt.Sprintf(`{"action":"userAuth","username":"%s","password":"%s"`,
			base64.StdEncoding.EncodeToString([]byte(settings.Username)),
			base64.StdEncoding.EncodeToString([]byte(settings.Password)))
		if xtoken != "" {
			auth += fmt.Sprintf(`,"token":"%s"`, xtoken)
		}
		auth += "}"
	}

	settings.WebSocket.WriteMessage(websocket.TextMessage, []byte(auth))
}
