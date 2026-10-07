package meshcentral

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io/fs"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral/fakeagent"
)

// fakeFilesAgent dials a fake agent serving root.
func fakeFilesAgent(t *testing.T, root string, consent ...string) *websocket.Conn {
	srv := httptest.NewServer(fakeagent.Files(root, consent...))
	t.Cleanup(srv.Close)
	ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

func filesSession(t *testing.T) (*FileSession, string) {
	root := t.TempDir()
	s, err := NewFileSession(fakeFilesAgent(t, root), nil)
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
		if err := s.Upload(context.Background(), "/"+name, bytes.NewReader(want), func(n int64) { sent = n }); err != nil {
			t.Fatalf("%s: upload: %v", name, err)
		}
		if got, _ := os.ReadFile(filepath.Join(root, name)); !bytes.Equal(got, want) || sent != int64(len(want)) {
			t.Fatalf("%s: agent has %d bytes, sent %d, want %d", name, len(got), sent, len(want))
		}
		var got bytes.Buffer
		if err := s.Download(context.Background(), "/"+name, &got, nil); err != nil {
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
	if err := s.Download(context.Background(), "/missing", &bytes.Buffer{}, nil); err == nil {
		t.Fatal("downloaded a missing file")
	}
}

func TestFilesCancel(t *testing.T) {
	s, root := filesSession(t)
	big := make([]byte, 2_000_000)
	os.WriteFile(filepath.Join(root, "big"), big, 0o644)
	os.WriteFile(filepath.Join(root, "small"), []byte("small file"), 0o644)

	ctx, cancel := context.WithCancel(context.Background())
	err := s.Upload(ctx, "/up", bytes.NewReader(big), func(n int64) { cancel() })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled upload: %v", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	var got bytes.Buffer
	err = s.Download(ctx, "/big", &got, func(n int64) { cancel() })
	if !errors.Is(err, context.Canceled) || got.Len() >= len(big) {
		t.Fatalf("canceled download: %v after %d bytes", err, got.Len())
	}
	// The blocks still in flight don't leak into the next download.
	got.Reset()
	if err := s.Download(context.Background(), "/small", &got, nil); err != nil || got.String() != "small file" {
		t.Fatalf("download after cancel: %q %v", got.String(), err)
	}
	if _, err := os.Stat(filepath.Join(root, "up")); !os.IsNotExist(err) {
		t.Errorf("partial upload left behind: %v", err)
	}
	if s.Broken() {
		t.Error("canceling broke the session")
	}
}

func TestFilesBroken(t *testing.T) {
	s, _ := filesSession(t)
	if _, err := s.Stat("/missing"); err == nil || s.Broken() {
		t.Fatalf("a failed call broke the session: %v", err)
	}
	s.conn.Close()
	if _, err := s.List("/"); err == nil || !s.Broken() {
		t.Fatalf("closed channel: %v, broken %v", err, s.Broken())
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
	s, err := NewFileSession(fakeFilesAgent(t, t.TempDir(), "Waiting for user to grant access...", "Denied"), func(m string) { notices = append(notices, m) })
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
		got := fakeagent.Join(root, rest)
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
