package meshcentral

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// agentJoin is the agent's obj.path.join: separators trimmed between the
// parts, joined with '/'.
func agentJoin(parts ...string) string {
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

// fakeFilesAgent dials a fakeFilesHandler on root.
func fakeFilesAgent(t *testing.T, root string, consent ...string) *websocket.Conn {
	srv := httptest.NewServer(fakeFilesHandler(root, consent...))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// fakeFilesHandler plays a Linux agent's files channel (meshcore.js,
// protocol 5) on root: replies as binary or text frames like the agent, no
// reply and swallowed errors for mkdir, rm, rename and copy. consent, when
// set, is sent as console messages before anything else.
func fakeFilesHandler(root string, consent ...string) http.HandlerFunc {
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
						os.RemoveAll(local(agentJoin(cmd.Path, n)))
					} else {
						os.Remove(local(agentJoin(cmd.Path, n)))
					}
				}
			case "rename":
				os.Rename(local(agentJoin(cmd.Path, cmd.Oldname)), local(agentJoin(cmd.Path, cmd.Newname)))
			case "copy":
				for _, n := range cmd.Names {
					sc, ds := agentJoin(cmd.Scpath, n), agentJoin(cmd.Dspath, n)
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

func filesSession(t *testing.T) (*FileSession, string) {
	root := t.TempDir()
	s, err := newFileSession(fakeFilesAgent(t, root), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s, root
}

func TestFilesUploadDownload(t *testing.T) {
	s, root := filesSession(t)
	big := make([]byte, 300_000) // several upload chunks, many download blocks
	rand.Read(big)
	cases := map[string][]byte{
		"empty": {},
		"brace": []byte(`{"action":"ls"} looks like JSON`),
		"zero":  {0, 0, 1, 2},
		"block": bytes.Repeat([]byte("x"), 16380), // ends on a block boundary
		"big":   big,
	}
	for name, want := range cases {
		var sent int64
		if err := s.Upload("/"+name, bytes.NewReader(want), func(n int64) { sent = n }); err != nil {
			t.Fatalf("%s: upload: %v", name, err)
		}
		if got, _ := os.ReadFile(filepath.Join(root, name)); !bytes.Equal(got, want) || sent != int64(len(want)) {
			t.Fatalf("%s: agent has %d bytes, sent %d, want %d", name, len(got), sent, len(want))
		}
		var got bytes.Buffer
		if err := s.Download("/"+name, &got, nil); err != nil {
			t.Fatalf("%s: download: %v", name, err)
		}
		if !bytes.Equal(got.Bytes(), want) {
			t.Fatalf("%s: downloaded %d bytes, want %d", name, got.Len(), len(want))
		}
	}
	// The acks sent ahead of the last download don't confuse what follows.
	if entries, err := s.List("/"); err != nil || len(entries) != len(cases) {
		t.Fatalf("list after downloads: %v %v", entries, err)
	}
	if err := s.Download("/missing", &bytes.Buffer{}, nil); err == nil {
		t.Fatal("downloaded a missing file")
	}
}

func TestFilesManage(t *testing.T) {
	s, root := filesSession(t)
	if err := s.Mkdir("/a"); err != nil {
		t.Fatal(err)
	}
	if err := s.Mkdir("/a"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("mkdir twice: %v", err)
	}
	if err := s.Mkdir("/missing/b"); err == nil {
		t.Fatal("mkdir without parent succeeded")
	}
	os.WriteFile(filepath.Join(root, "a", "f.txt"), []byte("hello"), 0o644)

	e, err := s.Stat("/a/f.txt")
	if err != nil || e.Type != FileFile || e.Size != 5 || time.Since(e.Mod) > time.Minute {
		t.Fatalf("stat: %+v %v", e, err)
	}
	if _, err := s.Stat("/a/nope"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat missing: %v", err)
	}
	if _, err := s.List("/nope"); err == nil {
		t.Fatal("listed a missing folder")
	}

	if err := s.Copy("/a/f.txt", "/a/g.txt"); err != nil { // same folder, new name
		t.Fatal(err)
	}
	if err := s.Mkdir("/b"); err != nil {
		t.Fatal(err)
	}
	if err := s.Copy("/a/f.txt", "/b/f.txt"); err != nil {
		t.Fatal(err)
	}
	if err := s.Copy("/a/f.txt", "/b/f.txt"); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("copy over a file: %v", err)
	}
	if err := s.Copy("/a", "/c"); err == nil {
		t.Fatal("copied a folder")
	}
	if err := s.Rename("/a/g.txt", "/b/h.txt"); err != nil { // across folders
		t.Fatal(err)
	}
	if err := s.Rename("/b", "/c"); err != nil {
		t.Fatal(err)
	}
	ents, _ := os.ReadDir(filepath.Join(root, "a"))
	if len(ents) != 1 || ents[0].Name() != "f.txt" { // no scratch folder left
		t.Fatalf("a holds %v", ents)
	}
	for _, p := range []string{"c/f.txt", "c/h.txt"} {
		if b, _ := os.ReadFile(filepath.Join(root, p)); string(b) != "hello" {
			t.Fatalf("%s holds %q", p, b)
		}
	}

	if err := s.Remove("/c", false); err == nil {
		t.Fatal("removed a full folder without recursive")
	}
	if err := s.Remove("/c", true); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("/a/f.txt", false); err != nil {
		t.Fatal(err)
	}
	if err := s.Remove("/", true); err == nil {
		t.Fatal("removed the root")
	}
}

func TestFilesConsentRefused(t *testing.T) {
	var notices []string
	s, err := newFileSession(fakeFilesAgent(t, t.TempDir(), "Waiting for user to grant access...", "Denied"), func(m string) { notices = append(notices, m) })
	if err != nil {
		t.Fatal(err) // the refusal comes after the handshake
	}
	if _, err := s.List("/"); err == nil || !strings.Contains(err.Error(), "Denied") {
		t.Fatalf("list after refusal: %v", err)
	}
	if len(notices) != 1 || !strings.HasPrefix(notices[0], "Waiting") {
		t.Fatalf("notices %q", notices)
	}
}

func TestRemotePaths(t *testing.T) {
	for _, c := range []struct{ in, dir, name string }{
		{"/etc/hosts", "/etc", "hosts"},
		{"/etc/", "/", "etc"},
		{`C:\Users\x`, `C:\Users`, "x"},
		{`C:\x`, "C:", "x"},
		{"C:/a/b", "C:/a", "b"},
	} {
		if d, n := SplitPath(c.in); d != c.dir || n != c.name {
			t.Errorf("SplitPath(%q) = %q, %q", c.in, d, n)
		}
	}
	for _, c := range []struct{ dir, name, want string }{
		{"/", "a", "/a"},
		{"/etc/", "a", "/etc/a"},
		{"C:", "a", `C:\a`},
		{`C:\Users\`, "a", `C:\Users\a`},
	} {
		if got := JoinPath(c.dir, c.name); got != c.want {
			t.Errorf("JoinPath(%q, %q) = %q", c.dir, c.name, got)
		}
	}
	// A rename names both paths from the root, which the agent's join must
	// turn back into the full paths.
	for _, p := range []string{"/home/u/f", `C:\Users\u\f`, "C:/x"} {
		root, rest := splitRoot(p)
		got := agentJoin(root, rest)
		if isWindowsPath(p) {
			got = strings.ReplaceAll(got, `\`, "/")
			p = strings.ReplaceAll(p, `\`, "/")
		}
		if got != p {
			t.Errorf("splitRoot(%q) rejoins as %q", p, got)
		}
	}
	for p, want := range map[string]bool{"": true, "/": true, "C:": true, `c:\`: true, "/a": false, `C:\a`: false} {
		if isRootPath(p) != want {
			t.Errorf("isRootPath(%q) = %v", p, !want)
		}
	}
}
