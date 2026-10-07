package main

import (
	"math"

	qt "github.com/mappu/miqt/qt6"
)

// rowData is what a device, group or route row shows: an icon beside a bold
// title over a smaller muted line (after it when inline), elided.
type rowData struct {
	title, sub, icon string
	iconOff          bool // the icon greyed out, for offline devices
	muted            bool // the title muted too
	inline           bool
}

// Padding around and between the lines, and the icon size.
const rowPad, rowLineGap, rowIconSize = 3, 1, 18

// rowFonts are the title and subtitle fonts, from the application font.
var rowFonts struct {
	title, sub     *qt.QFont
	fmTitle, fmSub *qt.QFontMetricsF
}

func initRowFonts() {
	if rowFonts.title != nil {
		return
	}
	rowFonts.title = qt.QApplication_Font()
	rowFonts.title.SetBold(true)
	rowFonts.sub = qt.QApplication_Font()
	rowFonts.sub.SetPointSizeF(rowFonts.sub.PointSizeF() * 0.9)
	rowFonts.fmTitle = qt.NewQFontMetricsF(rowFonts.title)
	rowFonts.fmSub = qt.NewQFontMetricsF(rowFonts.sub)
}

func rowHeight(inline bool) int {
	initRowFonts()
	h := rowFonts.fmTitle.Height()
	if !inline {
		h += rowLineGap + rowFonts.fmSub.Height()
	}
	return int(math.Ceil(max(h, rowIconSize) + 2*rowPad))
}

// paintRow draws a row into r, with the selection's colors when selected.
func paintRow(p *qt.QPainter, r *qt.QRect, pal *qt.QPalette, selected bool, d rowData) {
	initRowFonts()
	fg, muted := pal.ColorWithCr(qt.QPalette__Text), pal.ColorWithCr(qt.QPalette__PlaceholderText)
	if selected {
		fg, muted = pal.ColorWithCr(qt.QPalette__HighlightedText), pal.ColorWithCr(qt.QPalette__HighlightedText)
	}
	if d.muted {
		fg = muted
	}
	x, y, w, h := float64(r.X()), float64(r.Y()), float64(r.Width()), float64(r.Height())
	x0 := x + rowPad
	if d.icon != "" {
		mode := qt.QIcon__Normal
		switch {
		case d.iconOff:
			mode = qt.QIcon__Disabled
		case selected:
			mode = qt.QIcon__Selected
		}
		p.DrawPixmap11(int(x0), int(y+(h-rowIconSize)/2), rowIconSize, rowIconSize, rowPixmap(d.icon, mode))
		x0 += rowIconSize + 3*rowPad
	}
	width := x + w - x0 - rowPad
	ft, fs := rowFonts.fmTitle, rowFonts.fmSub
	title := ft.ElidedText(d.title, qt.ElideRight, width)
	var tb, sb float64 // baselines
	if d.inline {
		tb = y + (h+ft.Ascent()-ft.Descent())/2
		sb = tb
	} else {
		top := y + (h-ft.Height()-rowLineGap-fs.Height())/2
		tb = top + ft.Ascent()
		sb = top + ft.Height() + rowLineGap + fs.Ascent()
	}
	p.SetFont(rowFonts.title)
	p.SetPen(fg)
	drawText(p, x0, tb, title)
	sx := x0
	if d.inline {
		tw := ft.HorizontalAdvance(title)
		sx, width = x0+tw, width-tw
	}
	if width > 0 && d.sub != "" {
		p.SetFont(rowFonts.sub)
		p.SetPen(muted)
		drawText(p, sx, sb, fs.ElidedText(d.sub, qt.ElideRight, width))
	}
}

// highlightOK caches whether the palette's highlighted text reads on the
// style's selected-row background, nil until a selected row is painted and
// after palette changes. Native styles may tint selected rows lightly rather
// than fill them with the highlight color, as Windows 11 does in light mode.
var highlightOK *bool

// selectionColors reports whether a selected row in r is painted in the
// highlighted text color, sampling once the background paintBg draws for it.
func selectionColors(pal *qt.QPalette, r *qt.QRect, paintBg func(*qt.QPainter)) bool {
	if highlightOK != nil || r.Width() <= 0 || r.Height() <= 0 {
		return highlightOK == nil || *highlightOK
	}
	img := qt.NewQImage3(r.Width(), r.Height(), qt.QImage__Format_ARGB32_Premultiplied)
	defer img.Delete()
	img.FillWithColor(pal.ColorWithCr(qt.QPalette__Base))
	p := qt.NewQPainter2(img.QPaintDevice)
	p.Translate2(float64(-r.X()), float64(-r.Y()))
	paintBg(p)
	p.End()
	p.Delete()
	bg := img.PixelColor(r.Width()/2, r.Height()/2)
	hl, text := contrast(bg, pal.ColorWithCr(qt.QPalette__HighlightedText)), contrast(bg, pal.ColorWithCr(qt.QPalette__Text))
	ok := hl >= min(text, 3) // readable, or at least not worse than the text color
	highlightOK = &ok
	return ok
}

// contrast is the WCAG contrast ratio of two colors, from 1 to 21.
func contrast(a, b *qt.QColor) float64 {
	la, lb := luminance(a), luminance(b)
	return (max(la, lb) + 0.05) / (min(la, lb) + 0.05)
}

func luminance(c *qt.QColor) float64 {
	lin := func(v float32) float64 {
		if v <= 0.04045 {
			return float64(v) / 12.92
		}
		return math.Pow((float64(v)+0.055)/1.055, 2.4)
	}
	return 0.2126*lin(c.RedF()) + 0.7152*lin(c.GreenF()) + 0.0722*lin(c.BlueF())
}

func drawText(p *qt.QPainter, x, y float64, s string) {
	pt := qt.NewQPointF3(x, y)
	p.DrawText(pt, s)
	pt.Delete()
}

// rowText is a widget showing a row, for the route list.
type rowText struct {
	w *qt.QWidget
	rowData
}

func newRowText(inline bool) *rowText {
	t := &rowText{w: qt.NewQWidget2()}
	t.inline = inline
	t.w.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Fixed)
	t.w.SetFixedHeight(rowHeight(inline))
	t.w.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) {
		p := qt.NewQPainter2(t.w.QPaintDevice)
		defer p.Delete()
		paintRow(p, t.w.Rect(), t.w.Palette(), false, t.rowData)
	})
	return t
}

// set changes what the row shows, repainting only if that changed it.
func (t *rowText) set(d rowData) {
	d.inline = t.inline
	if d != t.rowData {
		t.rowData = d
		t.w.Update()
	}
}

// newElidedLabel is a label that shortens its text with "…" instead of
// growing the window.
func newElidedLabel(text string) *qt.QLabel {
	l := qt.NewQLabel3(text)
	l.SetSizePolicy2(qt.QSizePolicy__Ignored, qt.QSizePolicy__Preferred)
	l.OnPaintEvent(func(_ func(*qt.QPaintEvent), _ *qt.QPaintEvent) {
		p := qt.NewQPainter2(l.QPaintDevice)
		defer p.Delete()
		r := l.ContentsRect()
		p.SetPen(l.Palette().ColorWithCr(l.ForegroundRole()))
		p.DrawText6(r, int(l.Alignment()), l.FontMetrics().ElidedText(l.Text(), qt.ElideRight, r.Width()))
	})
	return l
}

// parent is the window dialogs belong to, nil before it exists (tests).
func parent() *qt.QWidget {
	if win == nil {
		return nil
	}
	return win.QWidget
}

func msgBox(ic qt.QMessageBox__Icon, title, text string, buttons qt.QMessageBox__StandardButton) *qt.QMessageBox {
	b := qt.NewQMessageBox6(ic, title, text, buttons, parent())
	b.SetAttribute(qt.WA_DeleteOnClose)
	b.Open()
	return b
}

func showError(err error) { msgBox(qt.QMessageBox__Critical, "Error", err.Error(), qt.QMessageBox__Ok) }

func showInfo(title, text string) {
	msgBox(qt.QMessageBox__Information, title, text, qt.QMessageBox__Ok)
}

// confirm asks a yes/no question, yes runs on yes.
func confirm(title, text string, yes func()) {
	b := msgBox(qt.QMessageBox__Question, title, text, qt.QMessageBox__Yes|qt.QMessageBox__No)
	b.OnFinished(func(r int) {
		if r == int(qt.QMessageBox__Yes) {
			yes()
		}
	})
}

// form is a dialog of labeled fields over OK and Cancel, OK enabled only
// while every field is valid. Errors show once a field was edited.
type form struct {
	d      *qt.QDialog
	fl     *qt.QFormLayout
	ok     *qt.QPushButton
	foot   *qt.QHBoxLayout // the buttons' row
	errLbl *qt.QLabel
	checks []func() (edited bool, err error)
}

func newForm(title, okText string) *form {
	f := &form{d: qt.NewQDialog(parent())}
	f.d.SetWindowTitle(title)
	f.d.SetAttribute(qt.WA_DeleteOnClose)
	v := qt.NewQVBoxLayout(f.d.QWidget)
	f.fl = qt.NewQFormLayout2()
	f.fl.SetFieldGrowthPolicy(qt.QFormLayout__AllNonFixedFieldsGrow)
	v.AddLayout(f.fl.QLayout)
	f.errLbl = qt.NewQLabel2()
	f.errLbl.SetStyleSheet("color: #e5534b")
	f.errLbl.SetVisible(false)
	v.AddWidget(f.errLbl.QWidget)
	box := qt.NewQDialogButtonBox4(qt.QDialogButtonBox__Ok | qt.QDialogButtonBox__Cancel)
	f.ok = box.Button(qt.QDialogButtonBox__Ok)
	f.ok.SetText(okText)
	box.OnAccepted(f.d.Accept)
	box.OnRejected(f.d.Reject)
	f.foot = qt.NewQHBoxLayout2()
	f.foot.AddWidget(box.QWidget)
	v.AddLayout(f.foot.QLayout)
	return f
}

// addFooter puts w left of the dialog's buttons.
func (f *form) addFooter(w *qt.QWidget) { f.foot.InsertWidget(0, w) }

func (f *form) add(label string, w *qt.QWidget) {
	if label == "" {
		f.fl.AddRowWithWidget(w)
	} else {
		f.fl.AddRow3(label, w)
	}
}

// entry adds a line edit, validate (may be nil) checks its text.
func (f *form) entry(label, text, placeholder string, validate func(string) error) *qt.QLineEdit {
	e := qt.NewQLineEdit3(text)
	e.SetPlaceholderText(placeholder)
	edited := false
	if validate != nil {
		f.checks = append(f.checks, func() (bool, error) {
			err := validate(e.Text())
			if err != nil && label != "" {
				err = &fieldError{label, err}
			}
			return edited, err
		})
	}
	e.OnTextEdited(func(string) { edited = true })
	e.OnTextChanged(func(string) { f.validate() })
	f.add(label, e.QWidget)
	return e
}

type fieldError struct {
	field string
	err   error
}

func (e *fieldError) Error() string { return e.field + ": " + e.err.Error() }

func (f *form) validate() {
	ok, msg := true, ""
	for _, c := range f.checks {
		if edited, err := c(); err != nil {
			ok = false
			if edited && msg == "" {
				msg = err.Error()
			}
		}
	}
	f.ok.SetEnabled(ok)
	f.errLbl.SetText(msg)
	f.errLbl.SetVisible(msg != "")
}

// show opens the dialog, width 0 for its natural width. onOK runs when
// it's accepted.
func (f *form) show(width int, onOK func()) {
	f.validate()
	f.d.OnAccepted(onOK)
	if width > 0 {
		f.d.Resize(width, f.d.SizeHint().Height())
	}
	f.d.Open()
}
