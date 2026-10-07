// Package fakeagent plays a MeshCentral agent's side of relay tunnels, for
// tests and the GUI benchmark's dummy server.
package fakeagent

import (
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/gorilla/websocket"
)

// Join is the agent's obj.path.join: separators trimmed between the
// parts, joined with '/'.
func Join(parts ...string) string {
	var x []string
	for i, w := range parts {
		w = strings.TrimRight(w, `/\`)
		if i != 0 {
			w = strings.TrimLeft(w, `/\`)
		}
		x = append(x, w)
	}
	return strings.Join(x, "/")
}

// Files serves the device side of a relay tunnel as a Linux agent's files
// channel (meshcore.js, protocol 5) on root: it joins with "c", replies in
// binary or text frames like the agent, and sends no reply and swallows the
// errors of mkdir, rm, rename and copy. consent messages, when given, are
// sent as the agent's console messages before anything else; two or more
// end with a refusal, like a local user denying access.
func Files(root string, consent ...string) http.HandlerFunc {
	up := websocket.Upgrader{}
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		local := func(p string) string { return filepath.Join(root, filepath.FromSlash(p)) }
		reply := func(v any) { b, _ := json.Marshal(v); c.WriteMessage(websocket.BinaryMessage, b) }

		c.WriteMessage(websocket.TextMessage, []byte("c"))
		if _, m, err := c.ReadMessage(); err != nil || string(m) != "5" {
			return
		}
		for i, msg := range consent {
			c.WriteMessage(websocket.TextMessage, []byte(`{"ctrlChannel":"102938","type":"console","msg":"`+msg+`","msgid":`+string(rune('1'+i))+`}`))
		}
		if len(consent) > 1 {
			return
		}

		var down *os.File
		var downID any
		var upFile *os.File
		var upID any
		sendBlock := func() {
			buf := make([]byte, 16384)
			n, _ := down.Read(buf[4:])
			flag := uint32(0x01000000)
			if n < 16380 {
				flag |= 1
				down.Close()
				down = nil
			}
			binary.BigEndian.PutUint32(buf, flag)
			c.WriteMessage(websocket.BinaryMessage, buf[:n+4])
		}
		for {
			mt, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if upFile != nil && mt == websocket.BinaryMessage && data[0] != '{' {
				if data[0] == 0 {
					data = data[1:]
				}
				upFile.Write(data)
				reply(map[string]any{"action": "uploadack", "reqid": upID})
				continue
			}
			var cmd struct {
				Action, Sub, Path, Name, Oldname, Newname, Scpath, Dspath string
				Reqid, ID                                                 any
				Delfiles, Names                                           []string
				Rec                                                       bool
			}
			if json.Unmarshal(data, &cmd) != nil {
				continue
			}
			switch cmd.Action {
			case "ls":
				resp := map[string]any{"path": cmd.Path, "reqid": cmd.Reqid}
				ents, err := os.ReadDir(local(cmd.Path))
				if err != nil {
					resp["dir"] = nil
				} else {
					dir := []map[string]any{}
					for _, e := range ents {
						info, _ := e.Info()
						d := map[string]any{"n": e.Name(), "t": 3, "s": info.Size(), "d": info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")}
						if e.IsDir() {
							d = map[string]any{"n": e.Name(), "t": 2, "d": info.ModTime().UTC().Format("2006-01-02T15:04:05.000Z")}
						}
						dir = append(dir, d)
					}
					resp["dir"] = dir
				}
				reply(resp)
			case "mkdir":
				os.Mkdir(local(cmd.Path), 0o755)
			case "rm":
				for _, n := range cmd.Delfiles {
					if cmd.Rec {
						os.RemoveAll(local(Join(cmd.Path, n)))
					} else {
						os.Remove(local(Join(cmd.Path, n)))
					}
				}
			case "rename":
				os.Rename(local(Join(cmd.Path, cmd.Oldname)), local(Join(cmd.Path, cmd.Newname)))
			case "copy":
				for _, n := range cmd.Names {
					sc, ds := Join(cmd.Scpath, n), Join(cmd.Dspath, n)
					if sc != ds {
						if b, err := os.ReadFile(local(sc)); err == nil {
							os.WriteFile(local(ds), b, 0o644)
						}
					}
				}
			case "download":
				switch cmd.Sub {
				case "start":
					f, err := os.Open(local(cmd.Path))
					if err != nil {
						c.WriteMessage(websocket.TextMessage, []byte(`{"action":"download","sub":"cancel","id":"`+cmd.ID.(string)+`"}`))
						continue
					}
					down, downID = f, cmd.ID
					c.WriteMessage(websocket.TextMessage, []byte(`{"action":"download","sub":"start","id":"`+cmd.ID.(string)+`"}`))
				case "startack", "ack":
					if down != nil && cmd.ID == downID {
						sendBlock()
					}
				case "stop":
					if down != nil && cmd.ID == downID {
						down.Close()
						down = nil
					}
				}
			case "upload":
				f, err := os.Create(local(cmd.Path))
				if err != nil {
					reply(map[string]any{"action": "uploaderror", "reqid": cmd.Reqid})
					continue
				}
				upFile, upID = f, cmd.Reqid
				reply(map[string]any{"action": "uploadstart", "reqid": upID})
			case "uploaddone":
				if upFile != nil {
					upFile.Close()
					upFile = nil
					reply(map[string]any{"action": "uploaddone", "reqid": upID})
				}
			case "uploadcancel":
				if upFile != nil {
					upFile.Close()
					os.Remove(upFile.Name())
					upFile = nil
					reply(map[string]any{"action": "uploadcancel", "reqid": upID})
				}
			case "uploadhash":
				var hash any
				if b, err := os.ReadFile(local(cmd.Path)); err == nil {
					sum := sha512.Sum384(b)
					hash = hex.EncodeToString(sum[:])
				}
				reply(map[string]any{"action": "uploadhash", "reqid": cmd.Reqid, "path": cmd.Path, "hash": hash})
			}
		}
	}
}
