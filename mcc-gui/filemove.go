package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// Files and folders move or copy within a device by dragging them onto a
// folder, or with cut, copy and paste, in any of the device's Files tabs.
// The agent moves and copies them in place, nothing passes through here.

// fileClip holds the items cut or copied, for a paste.
var fileClip struct {
	nodeID string
	paths  []string
	cut    bool
}

// filesMime carries a drag of a device's items, as dragFiles JSON.
const filesMime = "application/x-mcc-files"

type dragFiles struct {
	Node  string   `json:"node"`
	Paths []string `json:"paths"`
}

func samePath(a, b string) bool { return meshcentral.InPath(a, b) && meshcentral.InPath(b, a) }

func items(n int) string {
	if n == 1 {
		return "1 item"
	}
	return fmt.Sprintf("%d items", n)
}

func (ft *filesTab) paths(sel []meshcentral.FileEntry) []string {
	var paths []string
	for _, e := range sel {
		paths = append(paths, ft.path(e))
	}
	return paths
}

// clip cuts or copies sel, for a paste in another folder.
func (ft *filesTab) clip(sel []meshcentral.FileEntry, cut bool) {
	if len(sel) == 0 || ft.drives {
		return
	}
	fileClip.nodeID, fileClip.paths, fileClip.cut = ft.d.Id, ft.paths(sel), cut
	verb := "Copied"
	if cut {
		verb = "Cut"
	}
	setStatus(ft.status, fmt.Sprintf("%s %s, paste with Ctrl+V", verb, items(len(sel))), false)
}

func (ft *filesTab) canPaste() bool { return fileClip.nodeID == ft.d.Id && len(fileClip.paths) > 0 }

// paste moves or copies the items cut or copied into dir. A cut pastes once.
func (ft *filesTab) paste(dir string) {
	if !ft.canPaste() || dir == "" {
		return
	}
	paths, cut := fileClip.paths, fileClip.cut
	if cut && !slices.ContainsFunc(paths, func(p string) bool { return meshcentral.InPath(dir, p) }) && !placeable(paths, dir, false) {
		setStatus(ft.status, "Already in this folder, paste into another one", true)
		return
	}
	if cut {
		fileClip.paths = nil
	}
	ft.place(paths, dir, !cut)
}

// copyName is a name for a copy of name next to it, that taken doesn't
// have: "a (copy).txt", then "a (copy 2).txt".
func copyName(name string, dir bool, taken []meshcentral.FileEntry) string {
	base, ext := name, ""
	if dot := strings.LastIndexByte(name, '.'); dot > 0 && !dir {
		base, ext = name[:dot], name[dot:]
	}
	for i := 1; ; i++ {
		n := base + " (copy)" + ext
		if i > 1 {
			n = fmt.Sprintf("%s (copy %d)%s", base, i, ext)
		}
		if !slices.ContainsFunc(taken, func(e meshcentral.FileEntry) bool { return strings.EqualFold(e.Name, n) }) {
			return n
		}
	}
}

// placeable is whether paths can move (or copy) into dir: not into
// themselves, and a move not where they are.
func placeable(paths []string, dir string, copying bool) bool {
	if dir == "" || len(paths) == 0 {
		return false
	}
	moves := false
	for _, p := range paths {
		if meshcentral.InPath(dir, p) {
			return false
		}
		if from, _ := meshcentral.SplitPath(p); !samePath(from, dir) {
			moves = true
		}
	}
	return copying || moves
}

// place moves or copies paths into dir on the device, then lists the
// folders that changed again. A copy into its own folder gets a new name,
// a move there stays.
func (ft *filesTab) place(paths []string, dir string, copying bool) {
	for _, p := range paths {
		if meshcentral.InPath(dir, p) {
			showError(fmt.Errorf("%s: cannot move or copy a folder into itself", p))
			return
		}
	}
	if !placeable(paths, dir, copying) {
		return
	}
	verb := "Moving"
	if copying {
		verb = "Copying"
	}
	setStatus(ft.status, fmt.Sprintf("%s %s…", verb, items(len(paths))), false)
	changed := []string{dir}
	var first string // the name of the first item placed, selected if dir is shown
	ft.browse.do(func(s *meshcentral.FileSession) error {
		var errs []error
		var here []meshcentral.FileEntry // dir's entries, for the names of copies into it
		for _, p := range paths {
			from, name := meshcentral.SplitPath(p)
			if samePath(from, dir) {
				if !copying {
					continue
				}
				if here == nil {
					var err error
					if here, err = s.List(dir); err != nil {
						return err
					}
				}
				i := slices.IndexFunc(here, func(e meshcentral.FileEntry) bool { return e.Name == name })
				name = copyName(name, i >= 0 && here[i].IsDir(), here)
				here = append(here, meshcentral.FileEntry{Name: name})
			} else if !slices.ContainsFunc(changed, func(d string) bool { return samePath(d, from) }) {
				changed = append(changed, from)
			}
			var err error
			if to := meshcentral.JoinPath(dir, name); copying {
				err = s.Copy(p, to)
			} else {
				err = s.Rename(p, to)
			}
			if err == nil && first == "" {
				first = name
			}
			if errs = append(errs, err); s.Broken() {
				break
			}
		}
		return errors.Join(errs...)
	}, func(err error) {
		if err != nil {
			showError(err)
		}
		in := func(dir string) bool {
			return dir != "" && slices.ContainsFunc(changed, func(d string) bool { return samePath(d, dir) })
		}
		for _, other := range filesTabs {
			if other != ft && other.d.Id == ft.d.Id && in(other.cur) {
				other.navigate(other.cur)
			}
		}
		if !ft.closed {
			if samePath(ft.cur, dir) {
				ft.selectAfter = first
			}
			ft.navigate(ft.cur) // also ends the status
		}
	})
}

// dragged is the device's items a drag carries, if it carries this device's.
func (ft *filesTab) dragged(md *qt.QMimeData) ([]string, bool) {
	if !md.HasFormat(filesMime) {
		return nil, false
	}
	var d dragFiles
	if json.Unmarshal(md.Data(filesMime), &d) != nil || d.Node != ft.d.Id || len(d.Paths) == 0 {
		return nil, false
	}
	return d.Paths, true
}

// dropAction is what dropping md into dir does: move or copy the device's
// items, holding Ctrl (Option on macOS) copies; or upload local files.
// IgnoreAction when it can't go there.
func (ft *filesTab) dropAction(md *qt.QMimeData, proposed qt.DropAction, dir string) qt.DropAction {
	if dir == "" {
		return qt.IgnoreAction
	}
	if paths, ok := ft.dragged(md); ok {
		copying := proposed == qt.CopyAction
		if !placeable(paths, dir, copying) {
			return qt.IgnoreAction
		}
		if copying {
			return qt.CopyAction
		}
		return qt.MoveAction
	}
	if md.HasUrls() {
		return qt.CopyAction // never Move: the source would delete its files
	}
	return qt.IgnoreAction
}

// drop moves, copies or uploads what md carries into dir.
func (ft *filesTab) drop(md *qt.QMimeData, action qt.DropAction, dir string) {
	if paths, ok := ft.dragged(md); ok {
		ft.place(paths, dir, action == qt.CopyAction)
		return
	}
	var local []string
	for _, u := range md.Urls() {
		if u.IsLocalFile() {
			local = append(local, u.ToLocalFile())
		}
	}
	ft.uploadTo(local, dir)
}

// dropTarget is the folder a drop at pos goes into: the folder or drive
// under it, unless it is being dragged itself, else the folder shown.
func (ft *filesTab) dropTarget(md *qt.QMimeData, pos *qt.QPointF) (string, *qt.QTreeWidgetItem) {
	p := pos.ToPoint()
	defer p.Delete()
	if item := ft.list.ItemAt(p); item != nil {
		if e, ok := ft.entryOf(item); ok && e.IsDir() {
			dir := ft.path(e)
			paths, _ := ft.dragged(md)
			if !slices.ContainsFunc(paths, func(p string) bool { return samePath(p, dir) }) {
				return dir, item
			}
		}
	}
	if ft.drives {
		return "", nil
	}
	return ft.cur, nil
}

// setDropRow highlights the folder a drop would go into, nil for none.
func (ft *filesTab) setDropRow(item *qt.QTreeWidgetItem) {
	if item == ft.dropRow {
		return
	}
	for col := range ft.list.ColumnCount() {
		if ft.dropRow != nil {
			ft.dropRow.SetData(col, int(qt.BackgroundRole), qt.NewQVariant())
		}
		if item != nil {
			c := qt.NewQColor10(ft.list.Palette().ColorWithCr(qt.QPalette__Highlight))
			c.SetAlphaF(0.3)
			item.SetBackground(col, qt.NewQBrush3(c))
			c.Delete()
		}
	}
	ft.dropRow = item
}

// dragAndDrop lets the list's items be dragged onto its folders and the up
// button, and local files dropped to upload them.
func (ft *filesTab) dragAndDrop() {
	ft.list.SetDragEnabled(true)
	ft.list.SetAcceptDrops(true)
	ft.list.Viewport().SetAcceptDrops(true)
	ft.list.OnStartDrag(func(_ func(qt.DropAction), _ qt.DropAction) {
		sel := ft.selected()
		if len(sel) == 0 || ft.drives {
			return
		}
		b, _ := json.Marshal(dragFiles{Node: ft.d.Id, Paths: ft.paths(sel)})
		md := qt.NewQMimeData()
		md.SetData(filesMime, b)
		d := qt.NewQDrag(ft.list.QObject)
		d.SetMimeData(md)
		name := "file"
		if !onlyFiles(sel) {
			name = "folder"
		}
		d.SetPixmap(icon(name).PixmapWithExtent(32))
		d.Exec2(qt.MoveAction|qt.CopyAction, qt.MoveAction)
		d.DeleteLater()
		ft.setDropRow(nil)
	})
	acceptable := func(md *qt.QMimeData) bool {
		_, ok := ft.dragged(md)
		return ok || md.HasUrls()
	}
	ft.list.OnDragEnterEvent(func(_ func(*qt.QDragEnterEvent), e *qt.QDragEnterEvent) {
		if acceptable(e.MimeData()) {
			e.AcceptProposedAction()
		}
	})
	ft.list.OnDragMoveEvent(func(_ func(*qt.QDragMoveEvent), e *qt.QDragMoveEvent) {
		dir, item := ft.dropTarget(e.MimeData(), e.Position())
		action := ft.dropAction(e.MimeData(), e.ProposedAction(), dir)
		if action == qt.IgnoreAction {
			ft.setDropRow(nil)
			e.Ignore()
			return
		}
		ft.setDropRow(item)
		e.SetDropAction(action)
		e.Accept()
	})
	ft.list.OnDragLeaveEvent(func(_ func(*qt.QDragLeaveEvent), _ *qt.QDragLeaveEvent) { ft.setDropRow(nil) })
	ft.list.OnDropEvent(func(_ func(*qt.QDropEvent), e *qt.QDropEvent) {
		ft.setDropRow(nil)
		dir, _ := ft.dropTarget(e.MimeData(), e.Position())
		action := ft.dropAction(e.MimeData(), e.ProposedAction(), dir)
		if action == qt.IgnoreAction {
			e.Ignore()
			return
		}
		e.SetDropAction(action)
		e.Accept()
		ft.drop(e.MimeData(), action, dir)
	})

	// The up button takes drops into the parent folder.
	parentDir := func() string {
		if ft.atRoot() {
			return ""
		}
		dir, _ := meshcentral.SplitPath(ft.cur)
		return dir
	}
	ft.upBtn.SetAcceptDrops(true)
	ft.upBtn.OnDragEnterEvent(func(_ func(*qt.QDragEnterEvent), e *qt.QDragEnterEvent) {
		if action := ft.dropAction(e.MimeData(), e.ProposedAction(), parentDir()); action != qt.IgnoreAction {
			e.SetDropAction(action)
			e.Accept()
		}
	})
	ft.upBtn.OnDragMoveEvent(func(_ func(*qt.QDragMoveEvent), e *qt.QDragMoveEvent) {
		if action := ft.dropAction(e.MimeData(), e.ProposedAction(), parentDir()); action != qt.IgnoreAction {
			e.SetDropAction(action)
			e.Accept()
		} else {
			e.Ignore()
		}
	})
	ft.upBtn.OnDropEvent(func(_ func(*qt.QDropEvent), e *qt.QDropEvent) {
		dir := parentDir()
		if action := ft.dropAction(e.MimeData(), e.ProposedAction(), dir); action != qt.IgnoreAction {
			e.SetDropAction(action)
			e.Accept()
			ft.drop(e.MimeData(), action, dir)
		}
	})
}
