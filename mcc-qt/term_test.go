package main

import (
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
)

// newTestTerm makes an 80x24 terminal, closed when the test ends.
func newTestTerm(t *testing.T) *term {
	var tm *term
	mainthread.Wait(func() {
		tm = newTerm()
		tm.w.Resize(int(80*tm.cw)+1, int(24*tm.ch)+1)
		tm.relayout()
	})
	t.Cleanup(func() { mainthread.Wait(func() { tm.close(); tm.w.DeleteLater() }) })
	return tm
}

// sent waits for the input the terminal queued for the device and takes it.
func (tm *term) sent(want int) string {
	deadline := time.Now().Add(time.Second)
	for {
		tm.in.mu.Lock()
		s := string(tm.in.buf)
		if len(s) >= want || time.Now().After(deadline) {
			tm.in.buf = nil
			tm.in.mu.Unlock()
			return s
		}
		tm.in.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
}

func TestTermOutput(t *testing.T) {
	tm := newTestTerm(t)
	if c, r := tm.Size(); c != 80 || r != 24 {
		t.Fatalf("size %dx%d", c, r)
	}
	tm.Write([]byte("hello \x1b[1;31mred\x1b[0m wide:世界\r\n\x1b[7mrev"))
	mainthread.Wait(func() {
		tm.mu.Lock()
		c := tm.cellAt(0, 6)
		w := tm.cellAt(0, 15)
		tm.mu.Unlock()
		if c.Content != "r" || c.Style.Fg != ansi.BasicColor(1) {
			t.Errorf("cell %q fg %v", c.Content, c.Style.Fg)
		}
		if w.Content != "世" || w.Width != 2 {
			t.Errorf("wide cell %q width %d", w.Content, w.Width)
		}
		tm.w.Grab() // paints, colors, wide characters and the cursor included

		tm.sel.on, tm.sel.a, tm.sel.b = true, cellPos{0, 6}, cellPos{1, 2}
		if got := tm.selectionText(); got != "red wide:世界\nre" {
			t.Errorf("selection %q", got)
		}
	})
}

func TestTermKeys(t *testing.T) {
	tm := newTestTerm(t)
	press := func(key qt.Key, mods qt.KeyboardModifier, text string) string {
		var s string
		mainthread.Wait(func() {
			e := qt.NewQKeyEvent3(qt.QEvent__KeyPress, int(key), mods, text)
			defer e.Delete()
			tm.key(e)
		})
		s = tm.sent(1)
		return s
	}
	for _, c := range []struct {
		key  qt.Key
		mods qt.KeyboardModifier
		text string
		want string
	}{
		{qt.Key_A, 0, "a", "a"},
		{qt.Key_Return, 0, "\r", "\r"},
		{qt.Key_Backspace, 0, "\x08", "\x7f"},
		{qt.Key_Up, 0, "", "\x1b[A"},
		{qt.Key_C, ctrlModifier, "\x03", "\x03"},
		{qt.Key_X, qt.AltModifier, "x", "\x1bx"},
		{qt.Key_Backtab, qt.ShiftModifier, "", "\x1b[Z"},
		{qt.Key_Tab, 0, "\t", "\t"},
	} {
		if got := press(c.key, c.mods, c.text); got != c.want {
			t.Errorf("key %x mods %x: sent %q, want %q", c.key, c.mods, got, c.want)
		}
	}

	// Application cursor keys, as vim and less ask for.
	tm.Write([]byte("\x1b[?1h"))
	if got := press(qt.Key_Up, 0, ""); got != "\x1bOA" {
		t.Errorf("application mode up: %q", got)
	}
}

func TestTermReplies(t *testing.T) {
	tm := newTestTerm(t)
	// A cursor position query is answered through the emulator's pipe, which
	// must not block the output.
	done := make(chan struct{})
	go func() {
		tm.Write([]byte("abc\x1b[6n"))
		tm.Write([]byte("\x1b[6n"))
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("output blocked on the reply")
	}
	if got := tm.sent(12); got != "\x1b[1;4R\x1b[1;4R" {
		t.Fatalf("replies %q", got)
	}
}

func TestTermHistory(t *testing.T) {
	tm := newTestTerm(t)
	var b strings.Builder
	for i := range 100 {
		b.WriteString("line\r\n")
		_ = i
	}
	tm.Write([]byte(b.String()))
	mainthread.Wait(func() {
		settle()
		e := qt.NewQKeyEvent3(qt.QEvent__KeyPress, int(qt.Key_PageUp), qt.ShiftModifier, "")
		tm.key(e)
		e.Delete()
		if tm.scroll != 23 {
			t.Errorf("scrolled %d lines, want a page of 23", tm.scroll)
		}
		tm.w.Grab()
		// Typing returns to the live screen.
		e = qt.NewQKeyEvent3(qt.QEvent__KeyPress, int(qt.Key_A), 0, "a")
		tm.key(e)
		e.Delete()
		if tm.scroll != 0 {
			t.Errorf("still scrolled %d after typing", tm.scroll)
		}
	})
}

func TestTermFontChangeAndClose(t *testing.T) {
	tm := newTestTerm(t)
	tm.Write([]byte("\x1b[1;4;9;31;44mstyled output\x1b[0m"))
	mainthread.Wait(func() {
		font := qt.NewQFont5(tm.font)
		defer font.Delete()
		for range 3 {
			font.SetPointSize(max(1, font.PointSize()) + 1)
			tm.w.SetFont(font)
			tm.w.Grab()
		}
		tm.close()
		tm.close()
		// Deferred widget events must not recreate or use resources after close.
		font.SetPointSize(max(1, font.PointSize()) + 1)
		tm.w.SetFont(font)
		tm.w.Grab()
	})
}
