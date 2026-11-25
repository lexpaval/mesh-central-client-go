package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

func TestFilesMove(t *testing.T) {
	root := fakeFiles(t)
	os.Mkdir(filepath.Join(root, "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(root, "b.txt"), []byte("b"), 0o644)
	var ft *filesTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openFilesTab(devices[0])
		ft = filesTabs[0]
	})
	t.Cleanup(func() { mainthread.Wait(func() { ft.close() }) })
	waitFor(t, "the listing", func() bool { return slices.Equal(shownNames(ft), []string{"docs", "a.txt", "b.txt"}) })
	entry := func(name string) meshcentral.FileEntry {
		e, _ := ft.entryNamed(name)
		return e
	}
	has := func(p, want string) bool {
		b, err := os.ReadFile(filepath.Join(root, p))
		return err == nil && string(b) == want
	}

	// Copy and paste into the same folder makes a copy next to it.
	mainthread.Wait(func() {
		ft.clip([]meshcentral.FileEntry{entry("a.txt")}, false)
		ft.paste(ft.cur)
	})
	waitFor(t, "the copy", func() bool { return slices.Contains(shownNames(ft), "a (copy).txt") })
	if !has("a (copy).txt", "a") {
		t.Fatal("copy holds the wrong data")
	}

	// A cut pastes once, and not back where it is.
	mainthread.Wait(func() {
		ft.clip([]meshcentral.FileEntry{entry("a (copy).txt")}, true)
		ft.paste(ft.cur)
		if !ft.canPaste() || ft.status.Text() != "Already in this folder, paste into another one" {
			t.Errorf("a cut pasted where it is: can paste %v, status %q", ft.canPaste(), ft.status.Text())
		}
		ft.paste("/docs")
		if ft.canPaste() {
			t.Error("a cut can paste twice")
		}
	})
	waitFor(t, "the paste", func() bool { return !slices.Contains(shownNames(ft), "a (copy).txt") })
	if !has("docs/a (copy).txt", "a") {
		t.Fatal("cut and paste didn't move the file")
	}

	// Drops: onto a folder moves, Ctrl copies; not into itself or where it is.
	drag := func(names ...string) *qt.QMimeData {
		var paths []string
		for _, n := range names {
			paths = append(paths, ft.path(entry(n)))
		}
		b, _ := json.Marshal(dragFiles{Node: "n", Paths: paths})
		md := qt.NewQMimeData()
		md.SetData(filesMime, b)
		return md
	}
	mainthread.Wait(func() {
		for _, c := range []struct {
			md       *qt.QMimeData
			proposed qt.DropAction
			dir      string
			want     qt.DropAction
		}{
			{drag("b.txt"), qt.MoveAction, "/docs", qt.MoveAction},
			{drag("b.txt"), qt.CopyAction, "/docs", qt.CopyAction},
			{drag("b.txt"), qt.MoveAction, "/", qt.IgnoreAction},
			{drag("b.txt"), qt.CopyAction, "/", qt.CopyAction},
			{drag("docs"), qt.MoveAction, "/docs", qt.IgnoreAction},
			{drag("docs"), qt.CopyAction, "/docs/sub", qt.IgnoreAction},
			{drag("b.txt"), qt.MoveAction, "", qt.IgnoreAction},
		} {
			if got := ft.dropAction(c.md, c.proposed, c.dir); got != c.want {
				t.Errorf("dropping %s (%v) into %q: %v, want %v", c.md.Data(filesMime), c.proposed, c.dir, got, c.want)
			}
		}
		other := qt.NewQMimeData()
		b, _ := json.Marshal(dragFiles{Node: "other", Paths: []string{"/b.txt"}})
		other.SetData(filesMime, b)
		if got := ft.dropAction(other, qt.MoveAction, "/docs"); got != qt.IgnoreAction {
			t.Errorf("took another device's items: %v", got)
		}
		ft.drop(drag("b.txt"), qt.MoveAction, "/docs")
	})
	waitFor(t, "the drop", func() bool { return slices.Equal(shownNames(ft), []string{"docs", "a.txt"}) })
	if !has("docs/b.txt", "b") {
		t.Fatal("the drop didn't move the file")
	}

	// Folders copy with what they hold.
	mainthread.Wait(func() { ft.drop(drag("docs"), qt.CopyAction, "/") })
	waitFor(t, "the folder copy", func() bool { return slices.Contains(shownNames(ft), "docs (copy)") })
	if !has("docs (copy)/b.txt", "b") || !has("docs (copy)/a (copy).txt", "a") {
		t.Fatal("the folder copy is missing files")
	}
}

func TestCopyName(t *testing.T) {
	taken := []meshcentral.FileEntry{{Name: "a.txt"}, {Name: "a (copy).txt"}, {Name: "v1.2"}}
	for _, c := range []struct {
		name string
		dir  bool
		want string
	}{
		{"a.txt", false, "a (copy 2).txt"},
		{"b.txt", false, "b (copy).txt"},
		{"v1.2", true, "v1.2 (copy)"},
		{".bashrc", false, ".bashrc (copy)"},
	} {
		if got := copyName(c.name, c.dir, taken); got != c.want {
			t.Errorf("copyName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}
