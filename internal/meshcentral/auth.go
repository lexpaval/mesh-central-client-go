package meshcentral

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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

// maxControlReconnectAttempts bounds how many times the control socket will
// try to redial after an unexpected drop before giving up and exiting, so a
// permanently unreachable server produces a visible error instead of an
// infinite silent retry loop.
const maxControlReconnectAttempts = 8

const controlAuthTimeout = 15 * time.Second

// loginTokenExpiry bounds how long an SSO login lasts before the browser
// sign-in is needed again, in minutes as createLoginToken takes it.
const loginTokenExpiry = 30 * 24 * 60

// StartSocket logs in with the default profile, see StartSocketAs.
func StartSocket() error { return StartSocketAs(config.GetDefaultProfile()) }

// StartSocketAs logs in with p and keeps the control socket open, reconnecting
// after drops.
func StartSocketAs(p config.Profile) error {
	settings.profileName = p.Name

	// SSO profiles log in with a stored login token. Without one (first login,
	// expired, revoked) the user signs in in the browser and hands over the
	// session's auth cookie through a Router link, which is traded for a new
	// token once connected. The cookie is reset so one left by an earlier
	// session (GUI reconnecting with another profile) can't log in.
	settings.AuthCookie = ""
	if p.SSO {
		p.Username, p.Password, _ = p.GetLoginToken()
		if p.Username == "" {
			if err := promptSSO(p.Server); err != nil {
				return err
			}
		}
	}

	// A remembered 2FA cookie stands in for the token until the server
	// rejects it (expired, revoked), then it's dropped and the user prompted.
	usingCookie := false
	if !p.SSO && settings.Token == "" && !settings.EmailToken && !settings.SMSToken {
		if c, err := p.GetTwoFactorCookie(); err == nil && c != "" {
			ApplyAuth("cookie="+c, false, false)
			usingCookie = true
		}
	}

	settings.Username = p.Username
	settings.Password = p.Password
	settings.ServerURL = "wss://" + p.Server + "/meshrelay.ashx"
	settings.initialAuthDone = false

	urlStr := strings.Replace(settings.ServerURL, "meshrelay.ashx", "control.ashx", 1)

	dial := func() (*websocket.Conn, error) {
		options, err := url.Parse(urlStr)
		if err != nil {
			return nil, err
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
				options.RawQuery = "auth=" + url.QueryEscape(settings.AuthCookie)
				if xtoken != "" {
					options.RawQuery += "&token=" + url.QueryEscape(xtoken)
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

		dialer := websocket.Dialer{
			HandshakeTimeout: 10 * time.Second,
			TLSClientConfig:  &tls.Config{InsecureSkipVerify: settings.Insecure},
		}
		conn, _, err := dialer.Dial(options.String(), headers)
		return conn, err
	}

	for {
		// Reset cookie state before each attempt so handleAuthCookieCommand
		// always takes the first-time branch and closes WebChannel
		settings.ACookie = ""
		settings.RCookie = ""
		settings.closing = false
		settings.AuthErrChannel = make(chan error, 1)

		conn, err := dial()
		if err != nil {
			return fmt.Errorf("unable to connect to server: %w", err)
		}
		conn.SetReadDeadline(time.Now().Add(controlAuthTimeout))

		if settings.debug {
			fmt.Fprintln(os.Stderr, "Connected to server.")
		}

		settings.WebChannel = make(chan struct{})
		settings.wsMu.Lock()
		settings.WebSocket = conn
		settings.wsMu.Unlock()
		go onServerWebSocket(conn, dial)

		select {
		case <-settings.WebChannel:
			// A token typed this session is single use, trade it for a 2FA
			// cookie so reconnects and later logins don't prompt again. An SSO
			// session's auth cookie is likewise traded for a login token.
			// Waits for the reply so short CLI commands don't close the socket
			// before it's stored. Servers with remembering disabled never reply.
			var req []byte
			if p.SSO && settings.AuthCookie != "" {
				host, _ := os.Hostname()
				req, _ = json.Marshal(map[string]any{"action": "createLoginToken", "name": "MeshCentral Client on " + host,
					"expire": loginTokenExpiry, "responseid": "mcc"})
			} else if settings.EmailToken || settings.SMSToken || (settings.Token != "" && !strings.HasPrefix(settings.Token, "cookie=")) {
				req = []byte(`{"action":"twoFactorCookie"}`)
			}
			if req != nil {
				settings.replyChan = make(chan struct{})
				if send(req) == nil {
					select {
					case <-settings.replyChan:
					case <-time.After(5 * time.Second):
					}
				}
			}
			return nil
		case err := <-settings.AuthErrChannel:
			StopSocket()
			ae, ok := err.(authError)
			if ok && p.SSO && ae.code == "badcredentials" {
				if settings.AuthCookie != "" {
					settings.AuthCookie = ""
					return errors.New("the Router link was not accepted: it expires after an hour and works only from the IP address of the browser it was copied from")
				}
				// The login token expired or was revoked, sign in again.
				p.DeleteLoginToken()
				settings.Username, settings.Password = "", ""
				if err := promptSSO(p.Server); err != nil {
					return err
				}
				continue
			}
			if !ok || ae.code != "tokenrequired" {
				return err
			}
			if usingCookie {
				p.DeleteTwoFactorCookie()
				ApplyAuth("", false, false)
				usingCookie = false
			}
			token, ok := TokenPrompt(ae.email2fa, ae.sms2fa, ae.emailSent)
			if !ok {
				return err
			}
			switch strings.ToLower(token) {
			case "email":
				ApplyAuth("", true, false)
			case "sms":
				ApplyAuth("", false, true)
			default:
				ApplyAuth(token, false, false)
			}
		}
	}
}

func StopSocket() {
	settings.closing = true

	if settings.RenewCookieTimer != nil {
		settings.RenewCookieTimer.Stop()
		settings.RenewCookieTimer = nil
	}

	settings.wsMu.Lock()
	defer settings.wsMu.Unlock()
	if settings.WebSocket != nil {
		settings.WebSocket.WriteMessage(websocket.CloseMessage,
			websocket.FormatCloseMessage(1000, "all done"))
		time.Sleep(100 * time.Millisecond)
		settings.WebSocket.Close()
		settings.WebSocket = nil
	}
}

// sleepUnlessClosing waits for d, polling settings.closing so a shutdown
// requested mid-backoff (StopSocket) interrupts the wait promptly instead of
// after up to the full backoff duration.
func sleepUnlessClosing(d time.Duration) bool {
	const tick = 200 * time.Millisecond
	for remaining := d; remaining > 0; remaining -= tick {
		if settings.closing {
			return false
		}
		step := tick
		if remaining < step {
			step = remaining
		}
		time.Sleep(step)
	}
	return !settings.closing
}

// reconnectControlSocket redials the control socket with capped exponential
// backoff, printing progress so a network blip is visible on the terminal.
// It gives up (OnConnectionLost) after maxControlReconnectAttempts so a server that
// is permanently unreachable doesn't retry forever in silence.
func reconnectControlSocket(cause error, dial func() (*websocket.Conn, error)) (*websocket.Conn, bool) {
	fmt.Fprintf(os.Stderr, "\nControl connection lost: %v, reconnecting...\n", cause)

	backoff := time.Second
	for attempt := 1; attempt <= maxControlReconnectAttempts; attempt++ {
		if !sleepUnlessClosing(backoff) {
			return nil, false
		}

		conn, err := dial()
		if err == nil {
			settings.wsMu.Lock()
			settings.WebSocket = conn
			settings.wsMu.Unlock()
			fmt.Fprintln(os.Stderr, "Control connection restored.")
			return conn, true
		}

		fmt.Fprintf(os.Stderr, "Reconnect attempt %d/%d failed: %v\n", attempt, maxControlReconnectAttempts, err)
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}

	settings.closing = true
	OnConnectionLost(errors.New("unable to restore connection to MeshCentral server, giving up"))
	return nil, false
}

func onServerWebSocket(conn *websocket.Conn, dial func() (*websocket.Conn, error)) {
	for {
		_, message, err := conn.ReadMessage()
		if err != nil {
			conn.Close()
			if settings.closing {
				return
			}
			if !settings.initialAuthDone {
				sendAuthError(fmt.Errorf("authentication failed: %w", err))
				return
			}
			if websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				if settings.debug {
					fmt.Fprintln(os.Stderr, "Server closed connection")
				}
				settings.closing = true
				OnConnectionLost(fmt.Errorf("server closed the control connection: %w", err))
				return
			}

			newConn, ok := reconnectControlSocket(err, dial)
			if !ok {
				return
			}
			conn = newConn
			continue
		}

		var command map[string]interface{}
		if err := json.Unmarshal(message, &command); err != nil {
			fmt.Fprintln(os.Stderr, "Error parsing command:", err)
			continue
		}

		switch command["action"] {
		case "close":
			handleCloseCommand(command)
		case "serverinfo":
			send([]byte(`{"action":"authcookie"}`))
		case "authcookie":
			// Clear the login deadline before releasing StartSocketAs.
			conn.SetReadDeadline(time.Time{})
			handleAuthCookieCommand(command)
		case "serverAuth":
			handleServerAuthCommand(command)
		case "twoFactorCookie":
			if c, _ := command["cookie"].(string); c != "" {
				ApplyAuth("cookie="+c, false, false)
				p := config.Profile{Name: settings.profileName}
				if err := p.SetTwoFactorCookie(c); err != nil && settings.debug {
					fmt.Fprintln(os.Stderr, "Unable to store 2FA cookie:", err)
				}
			}
			replied()
		case "createLoginToken":
			user, _ := command["tokenUser"].(string)
			pass, _ := command["tokenPass"].(string)
			p := config.Profile{Name: settings.profileName}
			if user == "" || pass == "" {
				// Disabled on the server or for this user. The renewed auth
				// cookie keeps this session going, the next one signs in again.
				fmt.Fprintf(os.Stderr, "Unable to create a login token (%v), the next login will need the browser again.\n", command["result"])
			} else if err := p.SetLoginToken(user, pass); err != nil {
				fmt.Fprintln(os.Stderr, "Unable to store login token:", err)
			} else {
				// Reconnects use the token, the auth cookie expires.
				settings.Username, settings.Password, settings.AuthCookie = user, pass, ""
			}
			replied()
		case "meshes":
			handleMeshesCommand(command)
		case "nodes":
			handleNodesCommand(command)
		case "event":
			handleEventCommand(command)
		case "msg":
			handleClipboardCommand(command)
		}
	}
}

func handleCloseCommand(command map[string]interface{}) {
	if command["cause"] == "noauth" {
		var ae authError
		switch command["msg"] {
		case "tokenrequired":
			ae = authError{
				code:      "tokenrequired",
				message:   "login token required",
				email2fa:  getBool(command, "email2fa"),
				sms2fa:    getBool(command, "sms2fa"),
				emailSent: getBool(command, "email2fasent"),
			}
		case "badtlscert":
			ae = authError{code: "badtlscert", message: "invalid TLS certificate detected"}
		case "badargs":
			ae = authError{code: "badargs", message: "invalid protocol arguments"}
		default:
			ae = authError{code: "badcredentials", message: "invalid username/password"}
		}

		if !settings.initialAuthDone {
			// Still inside StartSocket's connect loop - let it prompt for a
			// token or report the error and exit.
			sendAuthError(ae)
			return
		}

		// Auth was rejected on a reconnect (e.g. a one-time 2FA token was
		// already consumed, or credentials were revoked). Retrying with the
		// same credentials would just loop forever, so fail loudly instead.
		settings.closing = true
		OnConnectionLost(fmt.Errorf("lost authentication with MeshCentral server: %s", ae.message))
	} else {
		if settings.debug {
			fmt.Fprintln(os.Stderr, "Server disconnected:", command["msg"])
		}
	}
}

// replied releases StartSocketAs waiting for a twoFactorCookie or
// createLoginToken reply.
func replied() {
	if settings.replyChan != nil {
		close(settings.replyChan)
		settings.replyChan = nil
	}
}

func handleAuthCookieCommand(command map[string]interface{}) {
	// A session logged in with an auth cookie reconnects with the renewed one,
	// the original expires after an hour.
	if settings.AuthCookie != "" {
		settings.AuthCookie = command["cookie"].(string)
	}
	if settings.ACookie == "" {
		settings.ACookie = command["cookie"].(string)
		settings.RCookie = command["rcookie"].(string)
		settings.RenewCookieTimer = time.AfterFunc(10*time.Minute, func() {
			send([]byte(`{"action":"authcookie"}`))
		})
		settings.initialAuthDone = true
		close(settings.WebChannel)
	} else {
		// Stop old timer before creating new one
		if settings.RenewCookieTimer != nil {
			settings.RenewCookieTimer.Stop()
		}
		settings.ACookie = command["cookie"].(string)
		settings.RCookie = command["rcookie"].(string)
		settings.RenewCookieTimer = time.AfterFunc(10*time.Minute, func() {
			send([]byte(`{"action":"authcookie"}`))
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
		auth = fmt.Sprintf(`{"action":"userAuth","auth":%q`, settings.AuthCookie)
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

	send([]byte(auth))
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

func openConsole() (*os.File, error) {
	if runtime.GOOS == "windows" {
		return os.OpenFile("CONIN$", os.O_RDWR, 0)
	}
	return os.OpenFile("/dev/tty", os.O_RDWR, 0)
}

func promptForToken(email2fa, sms2fa, emailSent bool) (string, bool) {
	if emailSent {
		pterm.Info.Println("Login token email sent.")
	}
	if email2fa && sms2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'email'/'sms' to request one.")
	} else if sms2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'sms' to request one.")
	} else if email2fa {
		pterm.Warning.Println("2FA required. Enter a token or type 'email' to request one.")
	} else {
		pterm.Warning.Println("2FA required.")
	}

	console, err := openConsole()
	if err != nil {
		fmt.Fprintf(os.Stderr, "2FA required but no console available (%v). Use --token flag.\n", err)
		return "", false
	}
	defer console.Close()

	for {
		fmt.Fprint(console, "Enter 2FA token: ")
		tokenBytes, err := term.ReadPassword(int(console.Fd()))
		fmt.Fprintln(console)
		if err != nil || len(tokenBytes) == 0 {
			fmt.Fprintln(console, "No token entered, aborting.")
			return "", false
		}
		token := strings.TrimSpace(string(tokenBytes))

		switch strings.ToLower(token) {
		case "email":
			if !email2fa {
				fmt.Fprintln(console, "Email token not available for this account.")
				continue
			}
		case "sms":
			if !sms2fa {
				fmt.Fprintln(console, "SMS token not available for this account.")
				continue
			}
		}
		return token, true
	}
}

// ParseRouterLink returns the server (host and domain path, as in a profile)
// and auth cookie of the link behind the web UI's "Launch MeshCentral Router"
// button: mcrouter://host[:port]/[domain/]control.ashx?c=<cookie>&t=<tls hash>.
func ParseRouterLink(link string) (server, cookie string, err error) {
	u, err := url.Parse(strings.TrimSpace(link))
	if err != nil || u.Scheme != "mcrouter" || u.Host == "" || u.Query().Get("c") == "" {
		return "", "", errors.New("not a MeshCentral Router link (mcrouter://...)")
	}
	return u.Host + strings.TrimSuffix(strings.TrimSuffix(u.Path, "control.ashx"), "/"), u.Query().Get("c"), nil
}

// SameServer reports whether two profile server strings name the same host,
// ignoring case, the default port and a trailing slash.
func SameServer(a, b string) bool {
	norm := func(s string) string {
		s = strings.TrimSuffix(strings.ToLower(s), "/")
		host, path, _ := strings.Cut(s, "/")
		return strings.TrimSuffix(host, ":443") + "/" + path
	}
	return norm(a) == norm(b)
}

func promptSSO(server string) error {
	link, ok := SSOPrompt(server)
	if !ok {
		return errors.New("single sign-on cancelled")
	}
	linkServer, cookie, err := ParseRouterLink(link)
	if err != nil {
		return err
	}
	if !SameServer(linkServer, server) {
		return fmt.Errorf("the Router link is for %s, the profile's server is %s", linkServer, server)
	}
	settings.AuthCookie = cookie
	return nil
}

// promptForSSOLink opens the server's web UI in the browser and reads the
// Router link the user copies from it.
func promptForSSOLink(server string) (string, bool) {
	web := "https://" + server + "/"
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", web)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", web)
	default:
		cmd = exec.Command("xdg-open", web)
	}
	cmd.Start()

	pterm.Info.Printfln("Single sign-on: sign in at %s (opened in your browser), click Router "+
		"on the devices page and copy the link of the Launch MeshCentral Router button.", web)
	console, err := openConsole()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Single sign-on needs a console (%v).\n", err)
		return "", false
	}
	defer console.Close()
	// Not echoed, the link holds a login cookie that would stay in scrollback.
	fmt.Fprint(console, "Router link (hidden): ")
	b, _ := term.ReadPassword(int(console.Fd()))
	fmt.Fprintln(console)
	line := strings.TrimSpace(string(b))
	return line, line != ""
}
