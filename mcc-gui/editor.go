package main

import (
	"bytes"
	"context"
	"crypto/sha512"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	qt "github.com/mappu/miqt/qt6"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// editMax is the largest file the editor opens.
const editMax = 2 << 20

// editor is a text file of a device open in its own window. It keeps the
// files channel of the tab it came from, which stays open while it does.
// Saving writes the file in place, so it keeps its owner and permissions.
type editor struct {
	nodeID, device, path string
	ch                   *fileChannel
	w                    *qt.QWidget
	text                 *qt.QPlainTextEdit
	status               *qt.QLabel
	saveBtn              *qt.QPushButton
	hash                 string // of the file on the device, when opened or last saved
	crlf                 bool   // the file has Windows line ends, the editor shows \n
	saving, discard      bool
	closed               bool
}

var editors []*editor

var errNotText = errors.New("not a text file")

// openEditor loads path from ft's device into an editor window, or brings
// up the one already open.
func openEditor(ft *filesTab, path string) {
	for _, ed := range editors {
		if ed.nodeID == ft.d.Id && ed.path == path {
			ed.w.ActivateWindow()
			ed.w.Raise()
			return
		}
	}
	_, name := meshcentral.SplitPath(path)
	ch := ft.browse
	ch.retain()
	ft.setStatus("Opening "+name+"…", false)
	var data []byte
	ch.do(func(s *meshcentral.FileSession) error {
		var buf bytes.Buffer
		if err := s.Download(context.Background(), path, &buf, nil); err != nil {
			return err
		}
		data = buf.Bytes()
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			return errNotText
		}
		return nil
	}, func(err error) {
		if !ft.closed {
			ft.setStatus("", false)
		}
		if err != nil {
			ch.release()
			if errors.Is(err, errNotText) && !ft.closed {
				confirm("Not a text file", name+" isn't a text file. Download it instead?", func() {
					if e, ok := ft.entryNamed(name); ok {
						ft.askDownload([]meshcentral.FileEntry{e})
					}
				})
				return
			}
			showError(err)
			return
		}
		newEditor(ft, ch, path, data).w.Show()
	})
}

func (ft *filesTab) entryNamed(name string) (meshcentral.FileEntry, bool) {
	for _, e := range ft.entries {
		if e.Name == name {
			return e, true
		}
	}
	return meshcentral.FileEntry{}, false
}

func newEditor(ft *filesTab, ch *fileChannel, path string, data []byte) *editor {
	sum := sha512.Sum384(data)
	ed := &editor{nodeID: ft.d.Id, device: ft.name, path: path, ch: ch, hash: hex.EncodeToString(sum[:])}
	text := string(data)
	if strings.Contains(text, "\r\n") {
		ed.crlf = true
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}

	ed.w = qt.NewQWidget(parent())
	ed.w.SetWindowFlags(qt.Window)
	ed.w.SetAttribute(qt.WA_DeleteOnClose)
	_, name := meshcentral.SplitPath(path)
	ed.w.SetWindowTitle(fmt.Sprintf("%s[*] · %s · %s", name, path, ed.device))
	ed.saveBtn = qt.NewQPushButton4(icon("floppy-disk"), "Save")
	ed.saveBtn.SetToolTip("Save to the device (Ctrl+S)")
	ed.saveBtn.SetEnabled(false)
	ed.saveBtn.OnClicked(func() { ed.save(false, nil) })
	ed.status = newElidedLabel("Opened " + time.Now().Format("15:04"))
	ed.status.SetForegroundRole(qt.QPalette__PlaceholderText)
	bar := hbox(false, ed.saveBtn.QWidget)
	bar.AddWidget2(ed.status.QWidget, 1)

	ed.text = qt.NewQPlainTextEdit2()
	font := qt.QFontDatabase_SystemFont(qt.QFontDatabase__FixedFont)
	ed.text.SetFont(font)
	ed.text.SetTabStopDistance(qt.NewQFontMetricsF(font).HorizontalAdvance(" ") * 4)
	ed.text.SetLineWrapMode(qt.QPlainTextEdit__NoWrap)
	ed.text.SetPlainText(text)
	ed.text.Document().SetModifiedWithBool(false)
	ed.text.Document().OnModificationChanged(func(changed bool) {
		ed.w.SetWindowModified(changed)
		ed.saveBtn.SetEnabled(changed && !ed.saving)
	})
	qt.NewQShortcut3(qt.QKeySequence__Save, ed.w.QObject).OnActivated(func() { ed.save(false, nil) })

	v := qt.NewQVBoxLayout(ed.w)
	v.AddLayout(bar.QLayout)
	v.AddWidget2(ed.text.QWidget, 1)
	ed.w.Resize(820, 620)

	ed.w.OnCloseEvent(func(super func(*qt.QCloseEvent), e *qt.QCloseEvent) {
		if ed.text.Document().IsModified() && !ed.discard {
			e.Ignore()
			ed.askClose()
			return
		}
		super(e)
		ed.closed = true
		editors = slices.DeleteFunc(editors, func(x *editor) bool { return x == ed })
		ed.ch.release()
		if ed.text.Document().IsModified() {
			logf("%s: closed %s, discarding the changes", ed.device, ed.path)
		} else {
			logf("%s: closed %s", ed.device, ed.path)
		}
	})
	editors = append(editors, ed)
	logf("%s: editing %s", ed.device, path)
	return ed
}

// askClose offers to save the changes before the window closes.
func (ed *editor) askClose() {
	_, name := meshcentral.SplitPath(ed.path)
	b := qt.NewQMessageBox6(qt.QMessageBox__Question, "Unsaved changes", "Save the changes to "+name+" before closing?",
		qt.QMessageBox__Save|qt.QMessageBox__Discard|qt.QMessageBox__Cancel, ed.w)
	b.SetAttribute(qt.WA_DeleteOnClose)
	b.OnFinished(func(r int) {
		switch r {
		case int(qt.QMessageBox__Save):
			ed.save(false, func() { ed.w.Close() })
		case int(qt.QMessageBox__Discard):
			ed.discard = true
			ed.w.Close()
		}
	})
	b.Open()
}

// save writes the text to the device, then runs then (may be nil). Unless
// force, it first checks the file is still the one opened, and asks before
// overwriting changes made on the device since.
func (ed *editor) save(force bool, then func()) {
	if ed.saving || ed.closed {
		return
	}
	ed.saving = true
	ed.saveBtn.SetEnabled(false)
	ed.setStatus("Saving…", false)
	plain := ed.text.ToPlainText()
	data := []byte(plain)
	if ed.crlf {
		data = []byte(strings.ReplaceAll(plain, "\n", "\r\n"))
	}
	changed := false
	ed.ch.do(func(s *meshcentral.FileSession) error {
		if !force {
			// A file removed or unreadable counts as changed too.
			if h, err := s.Hash(ed.path); err != nil || !strings.EqualFold(h, ed.hash) {
				if s.Broken() {
					return err
				}
				changed = true
				return nil
			}
		}
		return s.Upload(context.Background(), ed.path, bytes.NewReader(data), nil)
	}, func(err error) {
		ed.saving = false
		if ed.closed {
			return
		}
		ed.saveBtn.SetEnabled(ed.text.Document().IsModified())
		_, name := meshcentral.SplitPath(ed.path)
		switch {
		case err != nil:
			ed.setStatus("Not saved: "+err.Error(), true)
			showError(err)
		case changed:
			ed.setStatus("Not saved", true)
			b := qt.NewQMessageBox6(qt.QMessageBox__Warning, "Changed on the device",
				name+" changed on "+ed.device+" since you opened it. Overwrite it with your version?",
				qt.QMessageBox__Yes|qt.QMessageBox__No, ed.w)
			b.SetAttribute(qt.WA_DeleteOnClose)
			b.OnFinished(func(r int) {
				if r == int(qt.QMessageBox__Yes) {
					ed.save(true, then)
				}
			})
			b.Open()
		default:
			sum := sha512.Sum384(data)
			ed.hash = hex.EncodeToString(sum[:])
			// Edits made while saving stay unsaved.
			if ed.text.ToPlainText() == plain {
				ed.text.Document().SetModifiedWithBool(false)
			}
			ed.setStatus("Saved "+time.Now().Format("15:04:05"), false)
			logf("%s: saved %s", ed.device, ed.path)
			dir, _ := meshcentral.SplitPath(ed.path)
			refreshFolder(ed.nodeID, dir)
			if then != nil {
				then()
			}
		}
	})
}

func (ed *editor) setStatus(text string, isErr bool) {
	ed.status.SetText(text)
	if isErr {
		ed.status.SetStyleSheet("color: #e5534b")
	} else {
		ed.status.SetStyleSheet("")
	}
	ed.status.Update()
}

// unsavedEditors counts the editors with changes not saved.
func unsavedEditors() int {
	n := 0
	for _, ed := range editors {
		if ed.text.Document().IsModified() {
			n++
		}
	}
	return n
}

// closeEditors closes every editor window, dropping unsaved changes.
func closeEditors() {
	for _, ed := range slices.Clone(editors) {
		ed.discard = true
		ed.w.Close()
	}
}
