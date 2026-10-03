package cmd

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/zalando/go-keyring"
)

// Run the real command in a subprocess: proxy mode calls os.Exit, and its
// stdin/stdout must stay separate from the test runner and the user's keyring.
func TestSSHProxyProcess(t *testing.T) {
	if os.Getenv("MCC_TEST_SSH_PROXY") != "1" {
		return
	}
	keyring.MockInit()
	if err := keyring.Set("meshcentral-client", "test", "test-password"); err != nil {
		t.Fatal(err)
	}
	for i, arg := range os.Args {
		if arg == "--" {
			rootCmd.SetArgs(os.Args[i+1:])
			Execute()
			os.Exit(0)
		}
	}
	t.Fatal("missing CLI arguments")
}

func TestSSHProxy(t *testing.T) {
	input := append([]byte("SSH-2.0-test-client\r\n"), bytes.Repeat([]byte{0, 1, 127, 128, 255}, 100)...)
	output := append([]byte("SSH-2.0-test-server\r\n"), bytes.Repeat([]byte{255, 128, 127, 1, 0}, 100)...)
	for _, tc := range []struct {
		name, nodeArg, target, join, stderr string
		debug, malformed, offline, reject   bool
		noJoin, noData, drop, unknown       bool
	}{
		{name: "bidirectional bytes", nodeArg: "test", join: "c"},
		{name: "quoted node and recorded relay", nodeArg: "'node//test'", join: "cr"},
		{name: "IPv6 target", nodeArg: `"test"`, target: "2001:db8::10", join: "c"},
		{name: "debug output", nodeArg: "test", join: "c", debug: true, stderr: "Connected to server."},
		{name: "malformed control message", nodeArg: "test", join: "c", malformed: true, stderr: "Error parsing command:"},
		{name: "offline warning", nodeArg: "test", join: "c", offline: true, stderr: "appears offline"},
		{name: "login rejected", nodeArg: "test", reject: true, stderr: "invalid username/password"},
		{name: "unknown node", nodeArg: "missing", unknown: true, stderr: "not found"},
		{name: "relay never joined", nodeArg: "test", noJoin: true, stderr: "before the device connected"},
		{name: "relay closed without data", nodeArg: "test", join: "c", noData: true, stderr: "without sending data"},
		{name: "relay dropped after data", nodeArg: "test", join: "c", drop: true, stderr: "Proxy tunnel to MeshCentral lost:"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var handlers sync.WaitGroup
			var relays atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				handlers.Add(1)
				defer handlers.Done()
				ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					t.Error(err)
					return
				}
				defer ws.Close()
				ws.SetReadDeadline(time.Now().Add(10 * time.Second))
				ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
				switch r.URL.Path {
				case "/control.ashx":
					wantAuth := base64.StdEncoding.EncodeToString([]byte("test-user")) + "," + base64.StdEncoding.EncodeToString([]byte("test-password"))
					if r.Header.Get("x-meshauth") != wantAuth {
						t.Error("control socket did not use the selected profile credentials")
					}
					if tc.reject {
						ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"close","cause":"noauth","msg":"badcredentials"}`))
					} else {
						if tc.malformed {
							ws.WriteMessage(websocket.TextMessage, []byte(`{invalid`))
						}
						ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"serverinfo"}`))
					}
					for {
						var message struct{ Action string }
						if err := ws.ReadJSON(&message); err != nil {
							return
						}
						switch message.Action {
						case "authcookie":
							ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"authcookie","cookie":"relay-cookie","rcookie":"renew-cookie"}`))
						case "meshes":
							ws.WriteMessage(websocket.TextMessage, []byte(`{"action":"meshes","meshes":[]}`))
						case "nodes":
							power := 1
							if tc.offline {
								power = 0
							}
							ws.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"action":"nodes","nodes":{"mesh//test":[{"_id":"node//test","rname":"test-node","pwr":%d}]}}`, power))
						default:
							t.Errorf("unexpected control action %q", message.Action)
						}
					}
				case "/meshrelay.ashx":
					relays.Add(1)
					query := r.URL.Query()
					if query.Get("auth") != "relay-cookie" || query.Get("nodeid") != "node//test" || query.Get("tcpport") != "2222" || query.Get("tcpaddr") != tc.target {
						t.Errorf("incorrect relay parameters: %v", query)
					}
					if !tc.noJoin {
						ws.WriteMessage(websocket.TextMessage, []byte(tc.join))
					}
					if !tc.noJoin && !tc.noData {
						var received []byte
						for len(received) < len(input) {
							kind, data, err := ws.ReadMessage()
							if err != nil {
								t.Error(err)
								return
							}
							if kind != websocket.BinaryMessage {
								t.Errorf("stdin sent as WebSocket message type %d", kind)
								return
							}
							received = append(received, data...)
						}
						if !bytes.Equal(received, input) {
							t.Errorf("relay received %q, want %q", received, input)
						}
						ws.WriteMessage(websocket.BinaryMessage, output[:20])
						ws.WriteMessage(websocket.BinaryMessage, output[20:])
					}
					if !tc.drop {
						ws.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
					}
				default:
					t.Errorf("unexpected WebSocket path %q", r.URL.Path)
				}
			}))
			t.Cleanup(func() { server.Close(); handlers.Wait() })

			configPath := filepath.Join(t.TempDir(), "config.json")
			configData := fmt.Appendf(nil, `{"default_profile":"unused","profiles":[{"name":"unused","server":"unused.invalid","username":"unused"},{"name":"test","server":%q,"username":"test-user"}]}`, strings.TrimPrefix(server.URL, "https://"))
			if err := os.WriteFile(configPath, configData, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			args := []string{"-test.run=^TestSSHProxyProcess$", "--", "ssh", "--proxy", "-k", "-C", configPath, "-P", "test", "-i", tc.nodeArg, "-p", "2222"}
			if tc.debug {
				args = append(args, "--debug")
			}
			if tc.target != "" {
				args = append(args, "root@"+tc.target)
			}
			process := exec.CommandContext(ctx, executable, args...)
			process.Env = append(os.Environ(), "MCC_TEST_SSH_PROXY=1")
			var stdout, stderr bytes.Buffer
			process.Stdout, process.Stderr = &stdout, &stderr
			stdin, err := process.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			// Leave stdin open: a server close must end the proxy without a local EOF.
			if _, err := stdin.Write(input); err != nil {
				t.Error(err)
			}
			err = process.Wait()
			if ctx.Err() != nil {
				t.Fatalf("proxy hung: %v; stderr: %s", ctx.Err(), &stderr)
			}
			wantCode := 0
			if tc.reject || tc.unknown || tc.noJoin || tc.noData || tc.drop {
				wantCode = 1
			}
			if process.ProcessState.ExitCode() != wantCode {
				t.Errorf("exit = %v, want code %d; stderr: %s", err, wantCode, &stderr)
			}
			wantOutput, wantRelays := output, int32(1)
			if tc.reject || tc.unknown || tc.noJoin || tc.noData {
				wantOutput = nil
			}
			if tc.reject || tc.unknown {
				wantRelays = 0
			}
			if !bytes.Equal(stdout.Bytes(), wantOutput) {
				t.Errorf("stdout = %q, want only relay bytes %q", stdout.Bytes(), wantOutput)
			}
			if relays.Load() != wantRelays {
				t.Errorf("relay connections = %d, want %d", relays.Load(), wantRelays)
			}
			if tc.stderr == "" && stderr.Len() != 0 || tc.stderr != "" && !strings.Contains(stderr.String(), tc.stderr) {
				t.Errorf("stderr = %q, want diagnostic %q", stderr.String(), tc.stderr)
			}
			if got, err := os.ReadFile(configPath); err != nil || !bytes.Equal(got, configData) {
				t.Errorf("temporary profile selection changed the config: %s, %v", got, err)
			}
		})
	}
}
