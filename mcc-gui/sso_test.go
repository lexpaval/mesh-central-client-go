package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// A Router link opened from the browser logs the matching SSO profile in
// without the dialog, and the session's cookie is traded for a login token.
func TestRouterLinkLogin(t *testing.T) {
	keyring.MockInit()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer ws.Close()
		if r.URL.Query().Get("auth") != "cookie1" {
			ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"close","cause":"noauth"}`))
			return
		}
		ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie","cookie":"c2","rcookie":"r"}`))
		for {
			_, msg, err := ws.ReadMessage()
			if err != nil {
				return
			}
			for req, reply := range map[string]string{
				`"createLoginToken"`: `{"action":"createLoginToken","tokenUser":"~t:u","tokenPass":"tp"}`,
				`"meshes"`:           `{"action":"meshes","meshes":[]}`,
				`"nodes"`:            `{"action":"nodes","nodes":{}}`,
			} {
				if strings.Contains(string(msg), req) {
					ws.WriteMessage(websocket.TextMessage, []byte(reply))
				}
			}
		}
	}))
	defer server.Close()
	host := strings.TrimPrefix(server.URL, "https://")
	viper.Set("profiles", []map[string]any{{"name": "pw", "server": host}, {"name": "sso", "server": host, "sso": true}})
	prompt := meshcentral.SSOPrompt
	meshcentral.SSOPrompt = promptSSO
	t.Cleanup(func() { viper.Reset(); meshcentral.SSOPrompt = prompt })

	ui(t, func() {
		for _, w := range qt.QApplication_TopLevelWidgets() {
			if w.WindowTitle() == "Error" { // left by earlier tests
				w.Close()
			}
		}
		refreshProfiles()
		insecureChk.SetChecked(true)
		handleRouterLink("mcrouter://" + host + "/control.ashx?c=cookie1&t=hash")
	})
	defer mainthread.Wait(disconnect)
	deadline := time.Now().Add(5 * time.Second)
	for done := false; !done; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("link login did not finish")
		}
		mainthread.Wait(func() {
			if !connected || session.loading {
				return
			}
			done = true
			if session.profile != "sso" || pendingLink != "" {
				t.Errorf("logged in with profile %q, pending link %q", session.profile, pendingLink)
			}
			for _, w := range qt.QApplication_TopLevelWidgets() {
				if w.IsVisible() && (w.WindowTitle() == "Single sign-on" || w.WindowTitle() == "Error") {
					t.Errorf("link login showed %q", w.WindowTitle())
				}
			}
		})
	}
	p := config.Profile{Name: "sso"}
	if u, pw, _ := p.GetLoginToken(); u != "~t:u" || pw != "tp" {
		t.Errorf("stored login token %q/%q", u, pw)
	}
}
