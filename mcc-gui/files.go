package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// A Files tab browses a device's files. Browsing and edits share one files
// channel, transfers queue on a second one, so browsing never waits for them.

// openFileSession opens a files channel, tests replace it.
var openFileSession = meshcentral.OpenFiles

// fileChannel runs calls on one files channel, one at a time on its own
// goroutine. It opens the channel on first use and again after it broke,
// and closes it once everyone using it released it.
type fileChannel struct {
	nodeID string
	notice func(string) // called from the channel's goroutine
	calls  chan fileCall
	users  int // on the Qt thread
}

type fileCall struct {
	f    func(*meshcentral.FileSession) error
	done func(error)
}

func newFileChannel(nodeID string, notice func(string)) *fileChannel {
	c := &fileChannel{nodeID: nodeID, notice: notice, calls: make(chan fileCall, 256), users: 1}
	go c.run()
	return c
}

func (c *fileChannel) run() {
	var s *meshcentral.FileSession
	for call := range c.calls {
		var err error
		if s == nil {
			s, err = openFileSession(c.nodeID, c.notice)
		}
		if err == nil {
			if err = call.f(s); s.Broken() {
				s.Close()
				s = nil
			}
		}
		if call.done != nil {
			mainthread.Start(func() { call.done(err) })
		}
	}
	if s != nil {
		s.Close()
	}
}

// do queues f, done (may be nil) gets its error on the Qt thread.
func (c *fileChannel) do(f func(*meshcentral.FileSession) error, done func(error)) {
	c.calls <- fileCall{f, done}
}

func (c *fileChannel) retain() { c.users++ }

func (c *fileChannel) release() {
	if c.users--; c.users == 0 {
		close(c.calls)
	}
}

type filesTab struct {
	d     meshcentral.Device
	name  string // the device's
	w     *qt.QWidget
	icons []func() // set again on a palette change

	pathEdit                          *qt.QLineEdit
	upBtn, mkdirBtn, upload, download *qt.QPushButton
	renameBtn, deleteBtn              *qt.QPushButton
	list                              *qt.QTreeWidget
	status                            *qt.QLabel
	progress                          *qt.QProgressBar
	cancelBtn                         *qt.QPushButton
	progressTimer                     *qt.QTimer
	cur                               string // the folder shown, "" for a Windows device's drives
	drives                            bool
	entries                           []meshcentral.FileEntry
	sortCol                           int
	sortDesc                          bool
	loads                             int    // listings asked for, only the latest is shown
	selectAfter                       string // the name to select once listed
	browse, transfers                 *fileChannel
	queue                             []*transfer
	running                           *transfer
	closed                            bool
}

// transfer is one file to download or upload.
type transfer struct {
	upload        bool
	remote, local string
	size          int64
	ctx           context.Context
	cancel        context.CancelFunc
	sent          atomic.Int64
	began         atomic.Int64 // Unix nanoseconds when the data started to flow
	speed         speed        // on the Qt thread
}

// speed is a transfer's rate from its progress samples, smoothed over a
// few seconds so the figure doesn't jump with every block.
type speed struct {
	lastN int64
	lastT time.Time
	rate  float64 // bytes per second, 0 until measured
}

func (sp *speed) sample(n int64, now time.Time) {
	if sp.lastT.IsZero() {
		sp.lastN, sp.lastT = n, now
		return
	}
	dt := now.Sub(sp.lastT).Seconds()
	if dt < 0.5 {
		return
	}
	rate := float64(n-sp.lastN) / dt
	if sp.rate == 0 {
		sp.rate = rate
	} else {
		sp.rate += (1 - math.Exp(-dt/3)) * (rate - sp.rate)
	}
	sp.lastN, sp.lastT = n, now
}

func rateText(bytesPerSec float64) string { return fileSize(int64(bytesPerSec)) + "/s" }

// durationText is a rough duration: "40 s", "3 min", "1 h 5 min".
func durationText(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", max(int(d.Seconds()), 1))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()+0.5))
	}
	return fmt.Sprintf("%d h %d min", int(d.Hours()), int(d.Minutes())%60)
}

var filesTabs []*filesTab

// openFilesTab opens a tab browsing d's files, from the root.
func openFilesTab(d meshcentral.Device) {
	ft := &filesTab{d: d, name: deviceName(d)}
	ft.build()
	ft.browse = newFileChannel(d.Id, ft.notice)
	filesTabs = append(filesTabs, ft)
	tabs.SetCurrentIndex(tabs.AddTab2(ft.w, icon("folder"), ft.name+" · Files"))
	ft.list.SetFocus()
	ft.navigate("")
	logf("%s: files opened", ft.name)
	recordRecent(recentAction{Kind: "files", NodeID: d.Id, Device: ft.name})
}

func filesAt(i int) *filesTab {
	w := tabs.Widget(i)
	if w == nil {
		return nil
	}
	for _, ft := range filesTabs {
		if ft.w.UnsafePointer() == w.UnsafePointer() {
			return ft
		}
	}
	return nil
}

// requestClose closes the tab, asking first while transfers are left.
func (ft *filesTab) requestClose() {
	if ft.running == nil {
		ft.close()
		return
	}
	confirm("Transfers running", fmt.Sprintf("Cancel the transfers on %s and close the tab?", ft.name), ft.close)
}

func (ft *filesTab) close() {
	if ft.closed {
		return
	}
	ft.closed = true
	ft.cancelTransfers()
	ft.browse.release()
	if ft.transfers != nil {
		ft.transfers.release()
	}
	filesTabs = slices.DeleteFunc(filesTabs, func(x *filesTab) bool { return x == ft })
	tabs.RemoveTab(tabs.IndexOf(ft.w))
	ft.w.DeleteLater()
}

func (ft *filesTab) build() {
	ft.w = qt.NewQWidget2()
	button := func(text, iconName, tip string) *qt.QPushButton {
		b := qt.NewQPushButton3(text)
		b.SetToolTip(tip)
		set := func() { b.SetIcon(icon(iconName)) }
		set()
		ft.icons = append(ft.icons, set)
		return b
	}
	ft.upBtn = button("", "arrow-up", "Parent folder (Backspace)")
	ft.upBtn.OnClicked(ft.goUp)
	ft.pathEdit = qt.NewQLineEdit2()
	ft.pathEdit.SetPlaceholderText("Drives")
	ft.pathEdit.OnReturnPressed(func() { ft.navigate(strings.TrimSpace(ft.pathEdit.Text())) })
	reload := button("", "rotate", "Reload the folder (F5)")
	reload.OnClicked(func() { ft.navigate(ft.cur) })
	ft.mkdirBtn = button("", "folder-plus", "New folder")
	ft.mkdirBtn.OnClicked(ft.askNewFolder)
	ft.upload = button("Upload", "upload", "Upload files into this folder, or drop them on the list")
	ft.upload.OnClicked(ft.askUpload)
	ft.download = button("Download", "download", "Download the selected files")
	ft.download.OnClicked(func() { ft.askDownload(ft.selected()) })
	ft.renameBtn = button("", "pen", "Rename (F2)")
	ft.renameBtn.OnClicked(func() {
		if sel := ft.selected(); len(sel) == 1 {
			ft.askRename(sel[0])
		}
	})
	ft.deleteBtn = button("", "trash-can", "Delete (Del)")
	ft.deleteBtn.OnClicked(func() { ft.askDelete(ft.selected()) })
	bar := hbox(false, ft.upBtn.QWidget)
	bar.AddWidget2(ft.pathEdit.QWidget, 1)
	for _, b := range []*qt.QPushButton{reload, ft.mkdirBtn, ft.upload, ft.download, ft.renameBtn, ft.deleteBtn} {
		bar.AddWidget(b.QWidget)
	}

	ft.list = qt.NewQTreeWidget2()
	ft.list.SetColumnCount(3)
	ft.list.SetHeaderLabels([]string{"Name", "Size", "Modified"})
	ft.list.SetRootIsDecorated(false)
	ft.list.SetUniformRowHeights(true)
	ft.list.SetSelectionMode(qt.QAbstractItemView__ExtendedSelection)
	ft.list.SetContextMenuPolicy(qt.CustomContextMenu)
	ft.list.SetAcceptDrops(true)
	ft.list.Viewport().SetAcceptDrops(true)
	h := ft.list.Header()
	h.SetStretchLastSection(false)
	h.SetSectionResizeMode2(0, qt.QHeaderView__Stretch)
	h.SetSectionResizeMode2(1, qt.QHeaderView__ResizeToContents)
	h.SetSectionResizeMode2(2, qt.QHeaderView__ResizeToContents)
	h.SetSectionsClickable(true)
	h.SetSortIndicatorShown(true)
	h.SetSortIndicator(0, qt.AscendingOrder)
	h.OnSectionClicked(func(col int) {
		if col == ft.sortCol {
			ft.sortDesc = !ft.sortDesc
		} else {
			ft.sortCol, ft.sortDesc = col, false
		}
		order := qt.AscendingOrder
		if ft.sortDesc {
			order = qt.DescendingOrder
		}
		h.SetSortIndicator(col, order)
		ft.fill()
	})
	ft.list.OnItemSelectionChanged(ft.updateButtons)
	ft.list.OnItemDoubleClicked(func(item *qt.QTreeWidgetItem, _ int) {
		if e, ok := ft.entryOf(item); ok {
			ft.activate(e)
		}
	})
	ft.list.OnKeyPressEvent(func(super func(*qt.QKeyEvent), e *qt.QKeyEvent) {
		sel := ft.selected()
		switch e.Key() {
		case int(qt.Key_Return), int(qt.Key_Enter):
			if len(sel) == 1 {
				ft.activate(sel[0])
			}
		case int(qt.Key_Backspace):
			ft.goUp()
		case int(qt.Key_Delete):
			ft.askDelete(sel)
		case int(qt.Key_F2):
			if len(sel) == 1 {
				ft.askRename(sel[0])
			}
		case int(qt.Key_F5):
			ft.navigate(ft.cur)
		default:
			super(e)
		}
	})
	ft.list.OnCustomContextMenuRequested(ft.contextMenu)
	ft.list.OnDragEnterEvent(func(_ func(*qt.QDragEnterEvent), e *qt.QDragEnterEvent) {
		if e.MimeData().HasUrls() && !ft.drives {
			e.AcceptProposedAction()
		}
	})
	ft.list.OnDragMoveEvent(func(_ func(*qt.QDragMoveEvent), e *qt.QDragMoveEvent) {
		if e.MimeData().HasUrls() && !ft.drives {
			e.AcceptProposedAction()
		}
	})
	ft.list.OnDropEvent(func(_ func(*qt.QDropEvent), e *qt.QDropEvent) {
		var paths []string
		for _, u := range e.MimeData().Urls() {
			if u.IsLocalFile() {
				paths = append(paths, u.ToLocalFile())
			}
		}
		e.AcceptProposedAction()
		ft.uploadFiles(paths)
	})

	ft.status = newElidedLabel("")
	ft.progress = qt.NewQProgressBar2()
	ft.progress.SetRange(0, 1000)
	ft.progress.SetTextVisible(false)
	ft.progress.SetMaximumWidth(160)
	ft.progress.SetVisible(false)
	ft.cancelBtn = button("Cancel", "xmark", "Cancel the transfers")
	ft.cancelBtn.SetVisible(false)
	ft.cancelBtn.OnClicked(ft.cancelTransfers)
	foot := hbox(false)
	foot.AddWidget2(ft.status.QWidget, 1)
	foot.AddWidget(ft.progress.QWidget)
	foot.AddWidget(ft.cancelBtn.QWidget)
	ft.progressTimer = qt.NewQTimer2(ft.w.QObject)
	ft.progressTimer.OnTimeout(ft.showProgress)

	v := qt.NewQVBoxLayout(ft.w)
	v.AddLayout(bar.QLayout)
	v.AddWidget2(ft.list.QWidget, 1)
	v.AddLayout(foot.QLayout)
	ft.updateButtons()
}

// retheme sets the tab's icons again after a palette change.
func (ft *filesTab) retheme() {
	for _, set := range ft.icons {
		set()
	}
	ft.fill()
}

// notice shows the agent's status messages, such as waiting for consent.
func (ft *filesTab) notice(msg string) {
	mainthread.Start(func() {
		if !ft.closed {
			ft.setStatus(msg, false)
		}
	})
}

func (ft *filesTab) setStatus(text string, isErr bool) {
	ft.status.SetText(text)
	ft.status.SetToolTip(text)
	if isErr {
		ft.status.SetStyleSheet("color: #e5534b")
	} else {
		ft.status.SetStyleSheet("")
	}
	ft.status.Update()
}

// navigate lists path, "" for the root, and shows it once listed.
func (ft *filesTab) navigate(path string) {
	if ft.closed {
		return
	}
	ft.loads++
	load := ft.loads
	var entries []meshcentral.FileEntry
	ft.setStatus("Loading…", false)
	ft.browse.do(func(s *meshcentral.FileSession) (err error) {
		entries, err = s.List(path)
		return err
	}, func(err error) {
		if ft.closed || load != ft.loads {
			return
		}
		if err != nil {
			ft.setStatus(err.Error(), true)
			ft.pathEdit.SetText(ft.cur)
			return
		}
		ft.drives = len(entries) > 0 && entries[0].Type == meshcentral.FileDrive
		if path == "" && !ft.drives {
			path = "/"
		}
		ft.cur, ft.entries = path, entries
		ft.pathEdit.SetText(path)
		ft.fill()
		dirs := 0
		for _, e := range entries {
			if e.IsDir() {
				dirs++
			}
		}
		switch {
		case ft.drives:
			ft.setStatus(fmt.Sprintf("%d drives", len(entries)), false)
		case len(entries) == 0:
			ft.setStatus("Empty folder", false)
		default:
			ft.setStatus(fmt.Sprintf("%d folders, %d files", dirs, len(entries)-dirs), false)
		}
	})
}

func (ft *filesTab) atRoot() bool { return ft.drives || ft.cur == "/" }

func (ft *filesTab) goUp() {
	if ft.atRoot() {
		return
	}
	parent, name := meshcentral.SplitPath(ft.cur)
	ft.selectAfter = name
	ft.navigate(parent)
}

// path is where entry e of the folder shown lives.
func (ft *filesTab) path(e meshcentral.FileEntry) string {
	if ft.drives {
		return strings.TrimRight(e.Name, `\`) + `\`
	}
	return meshcentral.JoinPath(ft.cur, e.Name)
}

// fill shows the entries, sorted, folders first.
func (ft *filesTab) fill() {
	slices.SortStableFunc(ft.entries, func(a, b meshcentral.FileEntry) int {
		if a.IsDir() != b.IsDir() {
			if a.IsDir() {
				return -1
			}
			return 1
		}
		c := 0
		switch ft.sortCol {
		case 1:
			c = cmp.Compare(a.Size, b.Size)
		case 2:
			c = a.Mod.Compare(b.Mod)
		}
		if c == 0 {
			c = cmp.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name))
		}
		if ft.sortDesc {
			c = -c
		}
		return c
	})
	ft.list.Clear()
	for _, e := range ft.entries {
		item := qt.NewQTreeWidgetItem3(ft.list)
		item.SetText(0, e.Name)
		item.SetData(0, int(qt.UserRole), qt.NewQVariant11(e.Name))
		item.SetTextAlignment(1, int(qt.AlignRight|qt.AlignVCenter))
		switch {
		case e.Type == meshcentral.FileDrive:
			item.SetIcon(0, icon("server"))
			if e.Size > 0 {
				item.SetText(1, fmt.Sprintf("%s free of %s", fileSize(e.Free), fileSize(e.Size)))
			}
		case e.IsDir():
			item.SetIcon(0, icon("folder"))
		default:
			item.SetIcon(0, icon("file"))
			item.SetText(1, fileSize(e.Size))
		}
		if !e.Mod.IsZero() {
			item.SetText(2, e.Mod.Local().Format("2006-01-02 15:04"))
		}
		if e.Name == ft.selectAfter {
			ft.list.SetCurrentItem(item)
		}
	}
	ft.selectAfter = ""
	ft.updateButtons()
}

func fileSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

func (ft *filesTab) entryOf(item *qt.QTreeWidgetItem) (meshcentral.FileEntry, bool) {
	name := item.Data(0, int(qt.UserRole)).ToString()
	for _, e := range ft.entries {
		if e.Name == name {
			return e, true
		}
	}
	return meshcentral.FileEntry{}, false
}

func (ft *filesTab) selected() []meshcentral.FileEntry {
	var sel []meshcentral.FileEntry
	for _, item := range ft.list.SelectedItems() {
		if e, ok := ft.entryOf(item); ok {
			sel = append(sel, e)
		}
	}
	return sel
}

func onlyFiles(es []meshcentral.FileEntry) bool {
	for _, e := range es {
		if e.IsDir() {
			return false
		}
	}
	return len(es) > 0
}

func (ft *filesTab) updateButtons() {
	sel := ft.selected()
	ft.upBtn.SetEnabled(!ft.atRoot())
	ft.mkdirBtn.SetEnabled(!ft.drives)
	ft.upload.SetEnabled(!ft.drives)
	ft.download.SetEnabled(onlyFiles(sel))
	ft.renameBtn.SetEnabled(len(sel) == 1 && !ft.drives)
	ft.deleteBtn.SetEnabled(len(sel) > 0 && !ft.drives)
}

// activate opens a folder, or a file in the editor.
func (ft *filesTab) activate(e meshcentral.FileEntry) {
	if e.IsDir() {
		ft.navigate(ft.path(e))
		return
	}
	if e.Size > editMax {
		confirm("Too large to edit", fmt.Sprintf("%s is %s, the editor opens files up to %s. Download it instead?",
			e.Name, fileSize(e.Size), fileSize(editMax)), func() { ft.askDownload([]meshcentral.FileEntry{e}) })
		return
	}
	openEditor(ft, ft.path(e))
}

func (ft *filesTab) contextMenu(pos *qt.QPoint) {
	item := ft.list.ItemAt(pos)
	if item == nil {
		ft.list.ClearSelection()
	} else if !item.IsSelected() {
		ft.list.SetCurrentItem(item)
	}
	sel := ft.selected()
	m := qt.NewQMenu(ft.list.QWidget)
	m.SetAttribute(qt.WA_DeleteOnClose)
	if len(sel) == 1 {
		e := sel[0]
		if e.IsDir() {
			m.AddAction2(icon("folder"), "Open").OnTriggered(func() { ft.activate(e) })
		} else {
			m.AddAction2(icon("pen-to-square"), "Edit").OnTriggered(func() { ft.activate(e) })
		}
	}
	if onlyFiles(sel) {
		m.AddAction2(icon("download"), "Download…").OnTriggered(func() { ft.askDownload(sel) })
	}
	if len(sel) > 0 && !ft.drives {
		if len(sel) == 1 {
			m.AddAction2(icon("pen"), "Rename…").OnTriggered(func() { ft.askRename(sel[0]) })
		}
		m.AddAction2(icon("copy"), "Copy path").OnTriggered(func() {
			var paths []string
			for _, e := range sel {
				paths = append(paths, ft.path(e))
			}
			qt.QGuiApplication_Clipboard().SetText(strings.Join(paths, "\n"))
		})
		m.AddSeparator()
		m.AddAction2(icon("trash-can"), "Delete…").OnTriggered(func() { ft.askDelete(sel) })
	}
	if len(sel) == 0 && !ft.drives {
		m.AddAction2(icon("folder-plus"), "New folder…").OnTriggered(ft.askNewFolder)
		m.AddAction2(icon("upload"), "Upload…").OnTriggered(ft.askUpload)
	}
	m.AddAction2(icon("rotate"), "Reload").OnTriggered(func() { ft.navigate(ft.cur) })
	m.Popup(ft.list.Viewport().MapToGlobalWithQPoint(pos))
}

// change runs f on the browse channel, then lists the folder again,
// selecting selectName. Errors show in a dialog.
func (ft *filesTab) change(f func(*meshcentral.FileSession) error, selectName string) {
	dir := ft.cur
	ft.setStatus("Working…", false)
	ft.browse.do(f, func(err error) {
		if err != nil {
			showError(err)
		}
		if ft.closed || ft.cur != dir {
			return
		}
		ft.selectAfter = selectName
		ft.navigate(dir)
	})
}

func validName(s string) error {
	switch {
	case strings.TrimSpace(s) == "":
		return errors.New("required")
	case strings.ContainsAny(s, `/\`):
		return errors.New("no / or \\")
	case s == "." || s == "..":
		return errors.New("not a name")
	}
	return nil
}

func (ft *filesTab) askNewFolder() {
	if ft.drives {
		return
	}
	f := newForm("New folder in "+ft.cur, "Create")
	name := f.entry("Name", "", "", validName)
	name.SetMinimumWidth(260)
	f.show(0, func() { ft.newFolder(name.Text()) })
}

func (ft *filesTab) newFolder(name string) {
	p := meshcentral.JoinPath(ft.cur, name)
	ft.change(func(s *meshcentral.FileSession) error { return s.Mkdir(p) }, name)
}

func (ft *filesTab) askRename(e meshcentral.FileEntry) {
	if ft.drives {
		return
	}
	f := newForm("Rename "+e.Name, "Rename")
	name := f.entry("New name", e.Name, "", func(s string) error {
		if s == e.Name {
			return errors.New("unchanged")
		}
		return validName(s)
	})
	name.SetMinimumWidth(260)
	if dot := strings.LastIndexByte(e.Name, '.'); dot > 0 && !e.IsDir() {
		name.SetSelection(0, dot) // the name, without its extension
	} else {
		name.SelectAll()
	}
	f.show(0, func() { ft.rename(e, name.Text()) })
}

func (ft *filesTab) rename(e meshcentral.FileEntry, name string) {
	from, to := ft.path(e), meshcentral.JoinPath(ft.cur, name)
	ft.change(func(s *meshcentral.FileSession) error { return s.Rename(from, to) }, name)
}

func (ft *filesTab) askDelete(sel []meshcentral.FileEntry) {
	if len(sel) == 0 || ft.drives {
		return
	}
	what := fmt.Sprintf("%d items", len(sel))
	if len(sel) == 1 {
		what = sel[0].Name
	}
	text := fmt.Sprintf("Delete %s from %s?", what, ft.name)
	if !onlyFiles(sel) {
		text = fmt.Sprintf("Delete %s from %s? Folders are deleted with everything in them.", what, ft.name)
	}
	confirm("Delete", text, func() { ft.remove(sel) })
}

func (ft *filesTab) remove(sel []meshcentral.FileEntry) {
	var paths []string
	for _, e := range sel {
		paths = append(paths, ft.path(e))
	}
	ft.change(func(s *meshcentral.FileSession) error {
		var errs []error
		for _, p := range paths {
			errs = append(errs, s.Remove(p, true))
		}
		return errors.Join(errs...)
	}, "")
}

func (ft *filesTab) askUpload() {
	if ft.drives {
		return
	}
	dir := pref("files/uploadDir")
	if dir == "" {
		dir = qt.QStandardPaths_WritableLocation(qt.QStandardPaths__HomeLocation)
	}
	paths := qt.QFileDialog_GetOpenFileNames3(parent(), "Upload to "+ft.cur+" on "+ft.name, dir)
	if len(paths) > 0 {
		setPref("files/uploadDir", filepath.Dir(paths[0]))
		ft.uploadFiles(paths)
	}
}

// uploadFiles queues local files for the folder shown, asking before
// replacing files there.
func (ft *filesTab) uploadFiles(paths []string) {
	if ft.drives || ft.closed {
		return
	}
	var queue []*transfer
	var folders, replaced []string
	for _, p := range paths {
		st, err := os.Stat(p)
		if err != nil {
			showError(err)
			return
		}
		name := filepath.Base(p)
		if st.IsDir() {
			folders = append(folders, name)
			continue
		}
		if slices.ContainsFunc(ft.entries, func(e meshcentral.FileEntry) bool { return e.Name == name }) {
			replaced = append(replaced, name)
		}
		queue = append(queue, &transfer{upload: true, local: p, remote: meshcentral.JoinPath(ft.cur, name), size: st.Size()})
	}
	if len(folders) > 0 {
		showInfo("Folders skipped", "Folders can't be uploaded, only files: "+strings.Join(folders, ", "))
	}
	if len(queue) == 0 {
		return
	}
	if len(replaced) == 0 {
		ft.enqueue(queue)
		return
	}
	confirm("Replace files", fmt.Sprintf("Replace %s in %s?", strings.Join(replaced, ", "), ft.cur), func() { ft.enqueue(queue) })
}

func (ft *filesTab) askDownload(sel []meshcentral.FileEntry) {
	if !onlyFiles(sel) {
		return
	}
	dir := pref("files/downloadDir")
	if dir == "" {
		dir = qt.QStandardPaths_WritableLocation(qt.QStandardPaths__DownloadLocation)
	}
	if len(sel) == 1 {
		// The dialog asks before replacing the file itself.
		local := qt.QFileDialog_GetSaveFileName3(parent(), "Download "+sel[0].Name, filepath.Join(dir, sel[0].Name))
		if local != "" {
			setPref("files/downloadDir", filepath.Dir(local))
			ft.downloadTo(sel, filepath.Dir(local), filepath.Base(local))
		}
		return
	}
	if dir = qt.QFileDialog_GetExistingDirectory3(parent(), fmt.Sprintf("Download %d files to", len(sel)), dir); dir == "" {
		return
	}
	setPref("files/downloadDir", dir)
	var replaced []string
	for _, e := range sel {
		if _, err := os.Stat(filepath.Join(dir, e.Name)); err == nil {
			replaced = append(replaced, e.Name)
		}
	}
	if len(replaced) == 0 {
		ft.downloadTo(sel, dir, "")
		return
	}
	confirm("Replace files", fmt.Sprintf("Replace %s in %s?", strings.Join(replaced, ", "), dir), func() { ft.downloadTo(sel, dir, "") })
}

// downloadTo queues sel for the local folder dir, a single file under name
// when given.
func (ft *filesTab) downloadTo(sel []meshcentral.FileEntry, dir, name string) {
	var queue []*transfer
	for _, e := range sel {
		local := filepath.Join(dir, e.Name)
		if name != "" {
			local = filepath.Join(dir, name)
		}
		queue = append(queue, &transfer{remote: ft.path(e), local: local, size: e.Size})
	}
	ft.enqueue(queue)
}

func (ft *filesTab) enqueue(queue []*transfer) {
	if ft.closed {
		return
	}
	for _, t := range queue {
		t.ctx, t.cancel = context.WithCancel(context.Background())
	}
	ft.queue = append(ft.queue, queue...)
	ft.startNext()
}

// startNext runs the next queued transfer once the one running is done.
func (ft *filesTab) startNext() {
	if ft.closed || ft.running != nil {
		return
	}
	if len(ft.queue) == 0 {
		ft.progressTimer.Stop()
		ft.progress.SetVisible(false)
		ft.cancelBtn.SetVisible(false)
		return
	}
	t := ft.queue[0]
	ft.queue = ft.queue[1:]
	ft.running = t
	if ft.transfers == nil {
		ft.transfers = newFileChannel(ft.d.Id, ft.notice)
	}
	ft.progress.SetValue(0)
	ft.progress.SetVisible(true)
	ft.cancelBtn.SetVisible(true)
	ft.showProgress()
	ft.progressTimer.Start(200)
	ft.transfers.do(t.run, func(err error) {
		ft.running = nil
		_, name := meshcentral.SplitPath(t.remote)
		switch {
		case errors.Is(err, context.Canceled):
			logf("%s: canceled the transfer of %s", ft.name, name)
		case err != nil:
			logf("%s: %v", ft.name, err)
			if !ft.closed {
				ft.setStatus(err.Error(), true)
				showError(err)
			}
		case t.upload:
			logf("%s: uploaded %s to %s%s", ft.name, filepath.Base(t.local), t.remote, t.summary())
		default:
			logf("%s: downloaded %s to %s%s", ft.name, name, t.local, t.summary())
		}
		if ft.closed {
			return
		}
		if dir, _ := meshcentral.SplitPath(t.remote); t.upload && dir == ft.cur && err == nil {
			ft.selectAfter = name
			ft.navigate(ft.cur)
		} else if err == nil {
			ft.setStatus("Done", false)
		}
		ft.startNext()
	})
}

// summary is the size, time and average speed of a finished transfer.
func (t *transfer) summary() string {
	n, began := t.sent.Load(), t.began.Load()
	if began == 0 || n == 0 {
		return ""
	}
	d := timeNow().Sub(time.Unix(0, began))
	return fmt.Sprintf(" (%s in %s, %s)", fileSize(n), durationText(d), rateText(float64(n)/max(d.Seconds(), 0.001)))
}

// run does the transfer, on the transfer channel's goroutine.
func (t *transfer) run(s *meshcentral.FileSession) error {
	t.began.Store(timeNow().UnixNano()) // the channel is open, waiting for consent is over
	progress := func(n int64) { t.sent.Store(n) }
	if t.upload {
		f, err := os.Open(t.local)
		if err != nil {
			return err
		}
		defer f.Close()
		return s.Upload(t.ctx, t.remote, f, progress)
	}
	f, err := os.Create(t.local)
	if err != nil {
		return err
	}
	err = s.Download(t.ctx, t.remote, f, progress)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(t.local)
	}
	return err
}

func (ft *filesTab) showProgress() {
	t := ft.running
	if t == nil {
		return
	}
	verb, name := "Downloading", filepath.Base(t.local)
	if t.upload {
		verb = "Uploading"
	}
	n := t.sent.Load()
	text := fmt.Sprintf("%s %s · %s of %s", verb, name, fileSize(n), fileSize(t.size))
	if t.began.Load() != 0 {
		t.speed.sample(n, timeNow())
	}
	if r := t.speed.rate; r > 0 {
		text += " · " + rateText(r)
		if left := t.size - n; left > 0 {
			text += ", " + durationText(time.Duration(float64(left)/r*float64(time.Second))) + " left"
		}
	}
	if len(ft.queue) > 0 {
		text += fmt.Sprintf(" · %d more queued", len(ft.queue))
	}
	ft.setStatus(text, false)
	if t.size > 0 {
		ft.progress.SetValue(int(min(n*1000/t.size, 1000)))
	}
}

func (ft *filesTab) cancelTransfers() {
	for _, t := range ft.queue {
		t.cancel()
	}
	ft.queue = nil
	if ft.running != nil {
		ft.running.cancel()
	}
}

// refreshFolder lists dir again in the device's tabs showing it.
func refreshFolder(nodeID, dir string) {
	for _, ft := range filesTabs {
		if ft.d.Id == nodeID && ft.cur == dir {
			ft.navigate(dir)
		}
	}
}
