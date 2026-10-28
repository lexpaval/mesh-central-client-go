package main

import (
	"context"
	"errors"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// notesTimeout bounds loading and saving notes, the server stays silent when
// it refuses either.
const notesTimeout = 10 * time.Second

// showNotes edits the MeshCentral notes of a device or group, id being its
// node or mesh ID. They load after the dialog opens, and it stays open when
// saving fails so nothing typed is lost.
func showNotes(id, name string, group bool) {
	d := qt.NewQDialog(parent())
	d.SetWindowTitle("Notes · " + name)
	d.SetAttribute(qt.WA_DeleteOnClose)
	v := qt.NewQVBoxLayout(d.QWidget)
	edit := qt.NewQPlainTextEdit2()
	edit.SetPlaceholderText("Loading…")
	edit.SetReadOnly(true)
	v.AddWidget(edit.QWidget)
	errLbl := qt.NewQLabel2()
	errLbl.SetStyleSheet("color: #e5534b")
	errLbl.SetWordWrap(true)
	errLbl.SetVisible(false)
	v.AddWidget(errLbl.QWidget)
	box := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Save | qt.QDialogButtonBox__Cancel)
	save := box.Button(qt.QDialogButtonBox__Save)
	save.SetEnabled(false)
	v.AddWidget(box.QWidget)

	open := true
	d.OnFinished(func(int) { open = false })
	editable := func(on bool, err error) {
		edit.SetReadOnly(!on)
		save.SetEnabled(on)
		errLbl.SetVisible(err != nil)
		if err != nil {
			errLbl.SetText(notesError(err, group))
		}
	}
	box.OnRejected(d.Reject)
	box.OnAccepted(func() {
		text := edit.ToPlainText()
		editable(false, nil)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), notesTimeout)
			defer cancel()
			err := meshcentral.SetNotes(ctx, id, text)
			mainthread.Start(func() {
				switch {
				case err == nil:
					logf("Saved the notes of %s", name)
					if open {
						d.Accept()
					}
				case open:
					editable(true, err)
				default:
					logf("Unable to save the notes of %s: %s", name, notesError(err, group))
				}
			})
		}()
	})

	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), notesTimeout)
		defer cancel()
		notes, err := meshcentral.GetNotes(ctx, id)
		mainthread.Start(func() {
			if !open {
				return
			}
			edit.SetPlaceholderText("No notes")
			if err != nil {
				editable(false, err)
				return
			}
			edit.SetPlainText(notes)
			editable(true, nil)
			edit.SetFocus()
		})
	}()
	d.Resize(480, 320)
	d.Open()
}

// notesError explains the server's silence, it means a missing right.
func notesError(err error, group bool) string {
	switch {
	case errors.Is(err, context.DeadlineExceeded) && group:
		return "The server didn't answer. Group notes need the Edit group right."
	case errors.Is(err, context.DeadlineExceeded):
		return "The server didn't answer."
	case errors.Is(err, meshcentral.ErrNotesNotSaved) && group:
		return "The server didn't save the notes. Group notes need the Edit group right."
	case errors.Is(err, meshcentral.ErrNotesNotSaved):
		return "The server didn't save the notes. Device notes need the Edit notes right."
	}
	return err.Error()
}
