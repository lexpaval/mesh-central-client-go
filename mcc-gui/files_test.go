package main

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral/fakeagent"
)

// fakeFiles has files channels open on a fake agent serving a new folder,
// which it returns.
func fakeFiles(t *testing.T) string {
	root := t.TempDir()
	srv := httptest.NewServer(fakeagent.Files(root))
	t.Cleanup(srv.Close)
	open := openFileSession
	openFileSession = func(_ string, notice func(string)) (*meshcentral.FileSession, error) {
		ws, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http"), nil)
		if err != nil {
			return nil, err
		}
		return meshcentral.NewFileSession(ws, notice)
	}
	t.Cleanup(func() { openFileSession = open })
	return root
}

// waitFor runs f on the Qt thread until it holds, failing after a while.
func waitFor(t *testing.T, what string, f func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		ok := false
		mainthread.Wait(func() { ok = f() })
		if ok {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func shownNames(ft *filesTab) []string {
	var names []string
	for i := range ft.list.TopLevelItemCount() {
		names = append(names, ft.list.TopLevelItem(i).Text(0))
	}
	return names
}

func TestFilesTab(t *testing.T) {
	root := fakeFiles(t)
	os.Mkdir(filepath.Join(root, "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("bee"), 0o644)
	os.WriteFile(filepath.Join(root, "A.txt"), []byte("ay"), 0o644)
	var ft *filesTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openFilesTab(devices[0])
		ft = filesTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { ft.close() }) })
	waitFor(t, "the root listing", func() bool { return slices.Equal(shownNames(ft), []string{"docs", "A.txt", "b.txt"}) })
	mainthread.Wait(func() {
		if ft.cur != "/" || ft.upBtn.IsEnabled() || tabs.TabText(tabs.IndexOf(ft.w)) != "dev · Files" {
			t.Errorf("at %q, up enabled %v, tab %q", ft.cur, ft.upBtn.IsEnabled(), tabs.TabText(tabs.IndexOf(ft.w)))
		}
		ft.list.Header().SectionClicked(1) // by size, folders still first
	})
	waitFor(t, "sorting by size", func() bool { return slices.Equal(shownNames(ft), []string{"docs", "A.txt", "b.txt"}) })
	mainthread.Wait(func() {
		ft.list.Header().SectionClicked(1)
		if got := shownNames(ft); !slices.Equal(got, []string{"docs", "b.txt", "A.txt"}) {
			t.Errorf("by size, descending: %v", got)
		}
		e, _ := ft.entryNamed("docs")
		ft.activate(e)
	})
	waitFor(t, "docs", func() bool { return ft.cur == "/docs" && ft.status.Text() == "Empty folder" })

	mainthread.Wait(func() { ft.newFolder("sub") })
	waitFor(t, "the new folder", func() bool { return slices.Equal(shownNames(ft), []string{"sub"}) })
	local := t.TempDir()
	os.WriteFile(filepath.Join(local, "up.txt"), []byte("uploaded"), 0o644)
	mainthread.Wait(func() { ft.uploadFiles([]string{filepath.Join(local, "up.txt")}) })
	waitFor(t, "the upload", func() bool { return slices.Equal(shownNames(ft), []string{"sub", "up.txt"}) && ft.running == nil })
	if b, _ := os.ReadFile(filepath.Join(root, "docs", "up.txt")); string(b) != "uploaded" {
		t.Fatalf("uploaded %q", b)
	}
	mainthread.Wait(func() {
		e, _ := ft.entryNamed("up.txt")
		ft.rename(e, "moved.txt")
	})
	waitFor(t, "the rename", func() bool { return slices.Equal(shownNames(ft), []string{"sub", "moved.txt"}) })
	mainthread.Wait(func() {
		e, _ := ft.entryNamed("moved.txt")
		ft.downloadTo([]meshcentral.FileEntry{e}, local, "")
	})
	waitFor(t, "the download", func() bool { return ft.running == nil && len(ft.queue) == 0 })
	if b, _ := os.ReadFile(filepath.Join(local, "moved.txt")); string(b) != "uploaded" {
		t.Fatalf("downloaded %q", b)
	}
	mainthread.Wait(func() { ft.remove(ft.entries) })
	waitFor(t, "the delete", func() bool { return ft.status.Text() == "Empty folder" })
	if ents, _ := os.ReadDir(filepath.Join(root, "docs")); len(ents) != 0 {
		t.Fatalf("left %v", ents)
	}
	mainthread.Wait(ft.goUp)
	waitFor(t, "going up", func() bool {
		return ft.cur == "/" && ft.list.CurrentItem() != nil && ft.list.CurrentItem().Text(0) == "docs"
	})
}

// retype replaces the editor's text the way typing does, marking it modified.
func retype(ed *editor, text string) {
	ed.text.SelectAll()
	ed.text.InsertPlainText(text)
}

func TestFilesEditor(t *testing.T) {
	root := fakeFiles(t)
	os.WriteFile(filepath.Join(root, "win.txt"), []byte("one\r\ntwo\r\n"), 0o644)
	os.WriteFile(filepath.Join(root, "bin"), []byte{1, 0, 2}, 0o644)
	var ft *filesTab
	ui(t, func() {
		openFilesTab(meshcentral.Device{Id: "n", Name: "dev"})
		ft = filesTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { ft.close(); closeEditors() }) })
	waitFor(t, "the listing", func() bool { return len(ft.entries) == 2 })

	mainthread.Wait(func() { openEditor(ft, "/bin") })
	waitFor(t, "the binary file refused", func() bool { return ft.status.Text() == "" })
	mainthread.Wait(func() { openEditor(ft, "/win.txt") })
	waitFor(t, "the editor", func() bool { return len(editors) == 1 })
	ed := editors[0]
	mainthread.Wait(func() {
		if got := ed.text.ToPlainText(); got != "one\ntwo\n" || !ed.crlf {
			t.Errorf("editor shows %q, crlf %v", got, ed.crlf)
		}
		openEditor(ft, "/win.txt") // brings up the same window
		retype(ed, "one\ntwo\nthree\n")
		if !ed.w.IsWindowModified() || !ed.saveBtn.IsEnabled() {
			t.Error("an edit doesn't mark the window modified")
		}
		ed.save(false, nil)
	})
	waitFor(t, "the save", func() bool { return !ed.saving })
	if b, _ := os.ReadFile(filepath.Join(root, "win.txt")); string(b) != "one\r\ntwo\r\nthree\r\n" {
		t.Fatalf("saved %q", b)
	}
	mainthread.Wait(func() {
		if len(editors) != 1 || ed.w.IsWindowModified() || unsavedEditors() != 0 {
			t.Errorf("%d editors, modified %v after saving", len(editors), ed.w.IsWindowModified())
		}
	})

	// Changed on the device since: the save stops and asks.
	os.WriteFile(filepath.Join(root, "win.txt"), []byte("theirs"), 0o644)
	mainthread.Wait(func() {
		retype(ed, "mine\n")
		ed.save(false, nil)
	})
	waitFor(t, "the conflict", func() bool { return !ed.saving })
	mainthread.Wait(func() {
		if ed.status.Text() != "Not saved" || unsavedEditors() != 1 {
			t.Errorf("status %q, %d unsaved", ed.status.Text(), unsavedEditors())
		}
		ed.save(true, nil)
	})
	waitFor(t, "the forced save", func() bool { return !ed.saving })
	if b, _ := os.ReadFile(filepath.Join(root, "win.txt")); string(b) != "mine\r\n" {
		t.Fatalf("forced save wrote %q", b)
	}

	// The editor keeps the channel after its tab closed.
	mainthread.Wait(func() {
		ft.close()
		retype(ed, "after\n")
		ed.save(false, nil)
	})
	waitFor(t, "saving without the tab", func() bool { return !ed.saving })
	if b, _ := os.ReadFile(filepath.Join(root, "win.txt")); string(b) != "after\r\n" {
		t.Fatalf("saved %q without the tab", b)
	}
	mainthread.Wait(func() {
		closeEditors()
		if len(editors) != 0 || ft.browse.users != 0 {
			t.Errorf("%d editors left, channel users %d", len(editors), ft.browse.users)
		}
	})
}

func TestTransferSpeed(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	clock := timeNow
	timeNow = func() time.Time { return now }
	t.Cleanup(func() { timeNow = clock })
	ui(t, func() {
		ft := &filesTab{}
		ft.build()
		defer ft.w.DeleteLater()
		tr := &transfer{local: "/tmp/big.iso", size: 100 << 20}
		ft.running = tr
		ft.showProgress()
		if got := ft.status.Text(); got != "Downloading big.iso · 0 B of 100.0 MiB" {
			t.Errorf("before the data flows: %q", got)
		}
		tr.began.Store(now.UnixNano())
		for i := range 5 {
			tr.sent.Store(int64(i) * 2 << 20)
			tr.at.Store(now.UnixNano())
			ft.showProgress()
			now = now.Add(time.Second)
		}
		if got := ft.status.Text(); got != "Downloading big.iso · 8.0 MiB of 100.0 MiB · 2.0 MiB/s, 46 s left" {
			t.Errorf("at 2 MiB/s: %q", got)
		}
		if got := tr.summary(); got != " (8.0 MiB in 4 s, 2.0 MiB/s)" { // up to the last bytes
			t.Errorf("summary %q", got)
		}
	})
}

func TestFilesFilter(t *testing.T) {
	root := fakeFiles(t)
	os.Mkdir(filepath.Join(root, "logs"), 0o755)
	for _, n := range []string{"app.log", "SYS.LOG", "app.conf", "notes.txt"} {
		os.WriteFile(filepath.Join(root, n), []byte(n), 0o644)
	}
	var ft *filesTab
	ui(t, func() {
		openFilesTab(meshcentral.Device{Id: "n", Name: "dev"})
		ft = filesTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { ft.close() }) })
	waitFor(t, "the listing", func() bool { return len(ft.entries) == 5 })
	shown := func() []string {
		var names []string
		for i := range ft.list.TopLevelItemCount() {
			if item := ft.list.TopLevelItem(i); !item.IsHidden() {
				names = append(names, item.Text(0))
			}
		}
		return names
	}
	mainthread.Wait(func() {
		ft.list.SelectAll()
		ft.filter.SetText("LOG")
		if got := shown(); !slices.Equal(got, []string{"logs", "app.log", "SYS.LOG"}) || ft.status.Text() != "3 of 5 match" {
			t.Errorf("filtered by log: %v, %q", got, ft.status.Text())
		}
		if n := len(ft.list.SelectedItems()); n != 3 || len(ft.selected()) != 3 {
			t.Errorf("%d selected after filtering a selection of all, want only the 3 shown", n)
		}
		ft.filter.SetText("*.conf")
		if got := shown(); !slices.Equal(got, []string{"app.conf"}) {
			t.Errorf("filtered by *.conf: %v", got)
		}
		ft.filter.SetText("zzz")
		if len(shown()) != 0 || ft.status.Text() != "Nothing here matches zzz" {
			t.Errorf("no match: %v, %q", shown(), ft.status.Text())
		}
		ft.filter.Clear()
		if len(shown()) != 5 || ft.status.Text() != "1 folders, 4 files" {
			t.Errorf("cleared: %v, %q", shown(), ft.status.Text())
		}

		// Typing in the list starts filtering.
		ft.list.SetFocus()
		ev := qt.NewQKeyEvent3(qt.QEvent__KeyPress, int(qt.Key_N), qt.NoModifier, "n")
		qt.QCoreApplication_SendEvent(ft.list.QObject, ev.QEvent)
		if ft.filter.Text() != "n" || !slices.Equal(shown(), []string{"app.conf", "notes.txt"}) {
			t.Errorf("typing n: filter %q, shown %v", ft.filter.Text(), shown())
		}
		ft.filter.SetText("log")
		ft.navigate(ft.cur) // a reload keeps the filter
	})
	waitFor(t, "the reload", func() bool { return ft.status.Text() == "3 of 5 match" })
	mainthread.Wait(func() {
		e, _ := ft.entryNamed("logs")
		ft.activate(e) // another folder clears it
	})
	waitFor(t, "logs", func() bool { return ft.cur == "/logs" && ft.filter.Text() == "" && ft.status.Text() == "Empty folder" })
}
