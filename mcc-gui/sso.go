package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/adrg/xdg"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// Single sign-on hands the browser's session over through the web UI's
// "Launch MeshCentral Router" link (mcrouter://), pasted into the SSO dialog
// or, once this app is registered for the scheme, opened by the browser.
var (
	// pendingLink is a Router link that arrived before its login asked for
	// it, the next SSO prompt takes it instead of showing the dialog.
	pendingLink string
	// ssoDialogLink fills and accepts the open SSO dialog, nil without one.
	ssoDialogLink func(link string)
)

// linkSocket forwards links between instances: the browser starts a new one
// per link, which hands it to the running one and exits.
var linkSocket = filepath.Join(filepath.Dir(config.DefaultConfigPath), "mcc-gui.sock")

// routerLinkArg returns the mcrouter:// link the app was started with.
func routerLinkArg() string {
	for _, a := range os.Args[1:] {
		if strings.HasPrefix(a, "mcrouter:") {
			return a
		}
	}
	return ""
}

// forwardLink hands link to a running instance, false if there's none.
func forwardLink(link string) bool {
	c, err := net.Dial("unix", linkSocket)
	if err != nil {
		return false
	}
	defer c.Close()
	_, err = fmt.Fprintln(c, link)
	return err == nil
}

// listenLinks accepts links forwarded by later instances. A socket that
// doesn't answer was left by one that crashed.
func listenLinks() {
	if c, err := net.Dial("unix", linkSocket); err == nil {
		c.Close()
		return // another instance gets them
	}
	os.Remove(linkSocket)
	l, err := net.Listen("unix", linkSocket)
	if err != nil {
		logf("Router links from the browser won't reach this window: %v", err)
		return
	}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			link, _ := bufio.NewReader(io.LimitReader(c, 4096)).ReadString('\n')
			c.Close()
			link = strings.TrimSpace(link)
			mainthread.Start(func() {
				win.ActivateWindow()
				win.Raise()
				handleRouterLink(link)
			})
		}
	}()
}

// handleRouterLink logs in with a link opened from the web UI: it answers
// the SSO dialog, or connects the SSO profile for its server, offering to add
// one if there's none.
func handleRouterLink(link string) {
	server, _, err := meshcentral.ParseRouterLink(link)
	if err != nil {
		showError(err)
		return
	}
	if ssoDialogLink != nil {
		ssoDialogLink(link)
		return
	}
	if !connectBtn.IsEnabled() { // connecting, with another login method
		return
	}
	if connected {
		if p, _ := config.GetProfile(session.profile); !meshcentral.SameServer(server, p.Server) {
			showInfo("Single sign-on", "Disconnect first to log in to "+server+".")
		}
		return
	}
	login := func(name string) {
		refreshProfiles()
		profileSel.SetCurrentIndex(profileSel.FindText(name))
		pendingLink = link
		connect(insecureChk.IsChecked())
	}
	for _, p := range config.GetProfiles() {
		if p.SSO && meshcentral.SameServer(p.Server, server) {
			login(p.Name)
			return
		}
	}
	confirm("Single sign-on", "There is no single sign-on profile for "+server+". Add one and log in?", func() {
		name := server
		var err error
		if _, statErr := os.Stat(config.DefaultConfigPath); errors.Is(statErr, os.ErrNotExist) {
			name = "default"
			if err = config.CreateConfig(server, "", "", true); err == nil {
				err = config.LoadConfig()
			}
		} else {
			for i := 2; profileSel.FindText(name) >= 0; i++ {
				name = fmt.Sprintf("%s (%d)", server, i)
			}
			_, err = config.AddProfile(name, false, server, "", "", true)
		}
		if err != nil {
			showError(err)
			return
		}
		login(name)
	})
}

// promptSSO runs on the StartSocket goroutine and blocks it until the dialog
// gets a Router link, pasted or opened from the browser.
func promptSSO(server string) (string, bool) {
	type answer struct {
		link string
		ok   bool
	}
	ch := make(chan answer, 1)
	reply := func(a answer) {
		select {
		case ch <- a:
		default:
		}
	}
	mainthread.Start(func() {
		if pendingLink != "" {
			reply(answer{pendingLink, true})
			pendingLink = ""
			return
		}
		web := &url.URL{Scheme: "https", Host: server, Path: "/"}
		if host, path, ok := strings.Cut(server, "/"); ok {
			web.Host, web.Path = host, "/"+path+"/"
		}
		d := qt.NewQDialog(parent())
		d.SetWindowTitle("Single sign-on")
		d.SetAttribute(qt.WA_DeleteOnClose)
		v := qt.NewQVBoxLayout(d.QWidget)
		msg := qt.NewQLabel3("Sign in to " + server + " in your browser, click Router on the devices page, then " +
			"Launch MeshCentral Router. If this app doesn't open by itself, copy that button's link and paste it here.")
		msg.SetWordWrap(true)
		v.AddWidget(msg.QWidget)
		entry := qt.NewQLineEdit2()
		entry.SetPlaceholderText("mcrouter://" + server + "/control.ashx?c=...")
		v.AddWidget(entry.QWidget)
		errLbl := qt.NewQLabel2()
		errLbl.SetStyleSheet(errorStyle)
		errLbl.SetVisible(false)
		v.AddWidget(errLbl.QWidget)
		openBtn := qt.NewQPushButton3("Open sign-in page")
		openBtn.SetAutoDefault(false)
		openBtn.OnClicked(func() {
			if err := openURL(web); err != nil {
				showError(err)
			}
		})
		v.AddWidget(openBtn.QWidget)
		if runtime.GOOS != "darwin" { // the bundle's Info.plist registers it
			regBtn := qt.NewQPushButton3("Open Router links with this app")
			regBtn.SetAutoDefault(false)
			regBtn.SetToolTip("Lets the browser hand the sign-in over in one click. Replaces MeshCentral Router as the handler of mcrouter:// links.")
			regBtn.OnClicked(func() {
				if err := registerLinkHandler(); err != nil {
					showError(err)
					return
				}
				regBtn.SetEnabled(false)
				regBtn.SetText("Router links open with this app")
				logf("Registered for mcrouter:// links")
			})
			v.AddWidget(regBtn.QWidget)
		}
		box := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
		ok := box.Button(qt.QDialogButtonBox__Ok)
		ok.SetText("Log in")
		ok.SetEnabled(false)
		box.OnAccepted(d.Accept)
		box.OnRejected(d.Reject)
		v.AddWidget(box.QWidget)
		entry.OnTextChanged(func(s string) {
			_, _, err := meshcentral.ParseRouterLink(s)
			ok.SetEnabled(err == nil)
			errLbl.SetVisible(err != nil && s != "")
			if err != nil {
				errLbl.SetText(err.Error())
			}
		})
		ssoDialogLink = func(link string) {
			entry.SetText(link)
			if ok.IsEnabled() {
				d.Accept()
			}
		}
		d.OnFinished(func(r int) {
			ssoDialogLink = nil
			reply(answer{strings.TrimSpace(entry.Text()), r == int(qt.QDialog__Accepted)})
		})
		d.Resize(520, d.SizeHint().Height())
		d.Open()
		entry.SetFocus()
		if err := openURL(web); err != nil {
			logf("Unable to open the browser: %v", err)
		}
	})
	a := <-ch
	return a.link, a.ok
}

// registerLinkHandler makes this executable the handler of mcrouter:// links
// for the current user, the way MeshCentral Router's install button does.
func registerLinkHandler() error {
	exe := os.Getenv("APPIMAGE") // the AppImage itself, not its temporary mount
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	switch runtime.GOOS {
	case "windows":
		key := `HKCU\Software\Classes\mcrouter`
		for _, args := range [][]string{
			{key, "/ve", "/d", "URL:MeshCentral Router"},
			{key, "/v", "URL Protocol", "/d", ""},
			{key + `\shell\open\command`, "/ve", "/d", `"` + exe + `" "%1"`},
		} {
			if out, err := exec.Command("reg", append(append([]string{"add"}, args...), "/f")...).CombinedOutput(); err != nil {
				return fmt.Errorf("reg add %s: %v %s", args[0], err, out)
			}
		}
		return nil
	default:
		dir := filepath.Join(xdg.DataHome, "applications")
		name := appID + "-mcrouter.desktop"
		// Exec quoting, then the string escape applied on top of it, and %
		// doubled so it isn't a field code.
		quoted := `"` + strings.NewReplacer(`\`, `\\\\`, `"`, `\\"`, "`", "\\\\`", `$`, `\\$`, `%`, `%%`).Replace(exe) + `"`
		entry := "[Desktop Entry]\nType=Application\nName=MeshCentral Client\nIcon=" + appID +
			"\nExec=" + quoted + " %u\nNoDisplay=true\nMimeType=x-scheme-handler/mcrouter;\n"
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(entry), 0o644); err != nil {
			return err
		}
		exec.Command("update-desktop-database", dir).Run()
		if out, err := exec.Command("xdg-mime", "default", name, "x-scheme-handler/mcrouter").CombinedOutput(); err != nil {
			return fmt.Errorf("xdg-mime: %v %s", err, out)
		}
		return nil
	}
}
