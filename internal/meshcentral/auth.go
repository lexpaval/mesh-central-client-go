package meshcentral

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/pterm/pterm"
	"golang.org/x/term"
)

type authError struct {
	code      string
	message   string
	email2fa  bool
	sms2fa    bool
	emailSent bool
}

func (e authError) Error() string {
	return e.message
}

func StartSocket() {
	p := config.GetDefaultProfile()

	settings.Username = p.Username
	settings.Password = p.Password
	settings.ServerURL = "wss://" + p.Server + "/meshrelay.ashx"

	for {
		// Reset cookie state before each attempt so handleAuthCookieCommand
		// always takes the first-time branch and closes WebChannel
		settings.ACookie = ""
		settings.RCookie = ""

		if err := startSocketOnce(); err != nil {
			if ae, ok := err.(authError); ok && ae.code == "tokenrequired" {
				printTokenRequired(ae)
				if !promptForToken(ae) {
					os.Exit(1)
				}
				continue
			}
			fmt.Fprintln(os.Stderr, err.Error())
			os.Exit(1)
		}
		return
	}
}

func startSocketOnce() error {
	var options *url.URL
	var err error

	options, err = url.Parse(settings.ServerURL)
	if err != nil {
		return fmt.Errorf("unable to parse server URL")
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
		return fmt.Errorf("unable to connect to server: %v", err)
	}

	if settings.debug {
		fmt.Println("Connected to server.")
	}

	settings.WebChannel = make(chan struct{})
	settings.AuthErrChannel = make(chan error, 1)
	settings.WebSocket = conn
	go onServerWebSocket(conn)

	select {
	case <-settings.WebChannel:
		return nil
	case err := <-settings.AuthErrChannel:
		StopSocket()
		return err
	}
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
		// Don't sleep - if server closed us (tokenrequired), the write
		// may already fail and sleeping just adds latency
		settings.WebSocket.Close()
		settings.WebSocket = nil
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
			sendAuthError(authError{
				code:      "tokenrequired",
				message:   "login token required",
				email2fa:  getBool(command, "email2fa"),
				sms2fa:    getBool(command, "sms2fa"),
				emailSent: getBool(command, "email2fasent"),
			})
		case "badtlscert":
			sendAuthError(authError{code: "badtlscert", message: "invalid TLS certificate detected"})
		case "badargs":
			sendAuthError(authError{code: "badargs", message: "invalid protocol arguments"})
		default:
			sendAuthError(authError{code: "badcredentials", message: "invalid username/password"})
		}
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

func sendAuthError(err error) {
	if settings.AuthErrChannel == nil {
		return
	}
	select {
	case settings.AuthErrChannel <- err:
	default:
	}
}

func getBool(command map[string]interface{}, key string) bool {
	val, ok := command[key]
	if !ok {
		return false
	}
	b, ok := val.(bool)
	return ok && b
}

func printTokenRequired(ae authError) {
	if ae.emailSent {
		pterm.Info.Println("Login token email sent.")
	}
	if ae.email2fa && ae.sms2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'email'/'sms' to request one.")
	} else if ae.sms2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'sms' to request one.")
	} else if ae.email2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'email' to request one.")
	} else {
		pterm.Warning.Println("2FA required.")
	}
}

func openConsole() (*os.File, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile("CONIN$", os.O_RDWR, 0)
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func promptForToken(ae authError) bool {
	console, err := openConsole()
	if err != nil {
		fmt.Fprintf(os.Stderr, "2FA required but no console available (%v). Use --token flag.\n", err)
		return false
	}
	defer console.Close()

	for {
		fmt.Fprint(console, "Enter 2FA token: ")
		tokenBytes, err := term.ReadPassword(int(console.Fd()))
		fmt.Fprintln(console)
		if err != nil || len(tokenBytes) == 0 {
			fmt.Fprintln(console, "No token entered, aborting.")
			return false
		}
		token := strings.TrimSpace(string(tokenBytes))

		switch strings.ToLower(token) {
		case "email":
			if !ae.email2fa {
				fmt.Fprintln(console, "Email token not available for this account.")
				continue
			}
			settings.EmailToken = true
			settings.SMSToken = false
			settings.Token = ""
			return true
		case "sms":
			if !ae.sms2fa {
				fmt.Fprintln(console, "SMS token not available for this account.")
				continue
			}
			settings.SMSToken = true
			settings.EmailToken = false
			settings.Token = ""
			return true
		default:
			settings.Token = token
			settings.EmailToken = false
			settings.SMSToken = false
			return true
		}
	}
}
