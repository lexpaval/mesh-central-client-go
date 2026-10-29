package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"slices"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// detailsTimeout bounds loading a device's details, the server ignores
// requests it can't make sense of.
const detailsTimeout = 15 * time.Second

// detailsTab shows what the web UI's General and Details tabs show of a
// device, read once and on Refresh.
type detailsTab struct {
	d      meshcentral.Device
	name   string
	w      *qt.QWidget
	icons  []func() // set again on a palette change
	view   *qt.QTextBrowser
	status *qt.QLabel
	info   *meshcentral.DeviceInfo
	loads  int // loads started, only the latest is shown
	closed bool
}

var detailsTabs []*detailsTab

// openDetails shows d's details in its tab, opening one if it has none and
// reloading it otherwise.
func openDetails(d meshcentral.Device) {
	for _, dv := range detailsTabs {
		if dv.d.Id == d.Id {
			tabs.SetCurrentWidget(dv.w)
			dv.load()
			return
		}
	}
	dv := &detailsTab{d: d, name: deviceName(d)}
	dv.build()
	detailsTabs = append(detailsTabs, dv)
	tabs.SetCurrentIndex(tabs.AddTab2(dv.w, icon("circle-info"), dv.name+" · Details"))
	dv.load()
}

func detailsAt(i int) *detailsTab {
	w := tabs.Widget(i)
	if w == nil {
		return nil
	}
	for _, dv := range detailsTabs {
		if dv.w.UnsafePointer() == w.UnsafePointer() {
			return dv
		}
	}
	return nil
}

func (dv *detailsTab) close() {
	if dv.closed {
		return
	}
	dv.closed = true
	detailsTabs = slices.DeleteFunc(detailsTabs, func(x *detailsTab) bool { return x == dv })
	tabs.RemoveTab(tabs.IndexOf(dv.w))
	dv.w.DeleteLater()
}

func (dv *detailsTab) build() {
	dv.w = qt.NewQWidget2()
	reload := tabButton(&dv.icons, "Refresh", "rotate", "Read the details again (F5)")
	reload.OnClicked(dv.load)
	f5 := qt.NewQShortcut2(qt.NewQKeySequence2("F5"), dv.w.QObject)
	f5.SetContext(qt.WidgetWithChildrenShortcut)
	f5.OnActivated(dv.load)
	dv.status = newElidedLabel("")
	bar := hbox(false, reload.QWidget)
	bar.AddWidget2(dv.status.QWidget, 1)
	dv.view = qt.NewQTextBrowser2()
	dv.view.SetOpenLinks(false)
	dv.view.SetFrameShape(qt.QFrame__NoFrame)
	l := qt.NewQVBoxLayout(dv.w)
	l.AddLayout(bar.QLayout)
	l.AddWidget2(dv.view.QWidget, 1)
}

func (dv *detailsTab) load() {
	dv.loads++
	n := dv.loads
	setStatus(dv.status, "Loading…", false)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), detailsTimeout)
		defer cancel()
		info, err := meshcentral.GetDeviceInfo(ctx, dv.d.Id)
		mainthread.Start(func() {
			if dv.closed || n != dv.loads {
				return
			}
			if err != nil {
				logf("%s: unable to read the details: %v", dv.name, err)
			}
			switch {
			case errors.Is(err, context.DeadlineExceeded):
				setStatus(dv.status, "The server didn't answer, it may not support device details", true)
			case err != nil:
				setStatus(dv.status, err.Error(), true)
			default:
				dv.info = info
				dv.render()
				msg := "Read at " + time.Now().Format("15:04:05")
				if info.Sys == nil {
					msg += ". No hardware details: the agent sent none yet, or the account lacks the Device details right"
				}
				setStatus(dv.status, msg, false)
			}
		})
	}()
}

// sectionIcons go before the section titles. The operating system gets
// the device's, as in the device list.
var sectionIcons = map[string]string{
	"General": "circle-info", "Security": "shield-halved", "Mesh Agent": "satellite-dish", "Networking": "network-wired",
	"BIOS": "gears", "Motherboard": "microchip", "System": "computer", "TPM": "key", "Battery": "battery-three-quarters",
	"Memory": "memory", "Storage": "hard-drive", "Storage Volumes": "database",
}

// render shows the details as rich text, labels muted like the device
// rows' second line. Rerun on a palette change.
func (dv *detailsTab) render() {
	if dv.info == nil {
		return
	}
	muted := dv.view.Palette().ColorWithCr(qt.QPalette__PlaceholderText).Name()
	esc := html.EscapeString
	// Status colors that read on a light or a dark base.
	colors := map[meshcentral.State]string{meshcentral.Good: "#1a7f37", meshcentral.Attention: "#9a6700", meshcentral.Bad: "#cf222e"}
	if isDark() {
		colors = map[meshcentral.State]string{meshcentral.Good: "#3fb950", meshcentral.Attention: "#d29922", meshcentral.Bad: "#f85149"}
	}
	// The section icons, as images of the document, tinted like the rest.
	doc, size := dv.view.Document(), dv.view.FontMetrics().Height()*5/4
	qsize := qt.NewQSize2(size, size)
	defer qsize.Delete()
	img := func(name string) string {
		u := qt.NewQUrl3("icon:" + name)
		defer u.Delete()
		doc.AddResource(int(qt.QTextDocument__ImageResource), u, icon(name).Pixmap3(qsize, dv.view.DevicePixelRatioF()).ToQVariant())
		return fmt.Sprintf(`<img src="icon:%s" width="%d" height="%d" style="vertical-align:middle">&nbsp;&nbsp;`, name, size, size)
	}
	// One table, so the values line up through all sections.
	var b strings.Builder
	for _, s := range dv.info.Sections() {
		name := sectionIcons[s.Title]
		if s.Title == "Operating System" {
			os, _ := dv.info.Node["osdesc"].(string)
			name = osIconName(os)
		}
		heading := esc(s.Title)
		if name != "" {
			heading = img(name) + heading
		}
		b.WriteString(`<tr><td colspan="2" style="padding-top:14px; padding-bottom:4px; font-size:large; font-weight:bold">` + heading + `</td></tr>`)
		for _, g := range s.Groups {
			indent := "12px"
			if g.Title != "" {
				b.WriteString(`<tr><td colspan="2" style="padding-top:8px; padding-left:12px; font-weight:bold">` + esc(g.Title) + `</td></tr>`)
				indent = "24px"
			}
			for _, r := range g.Rows {
				var value string
				for _, p := range r.Value {
					if c, ok := colors[p.State]; ok {
						value += `<span style="color:` + c + `">` + esc(p.Text) + `</span>`
					} else {
						value += esc(p.Text)
					}
				}
				b.WriteString(`<tr><td style="padding-left:` + indent + `; padding-right:18px; color:` + muted + `">` + esc(r.Label) + `</td><td>` + value + `</td></tr>`)
			}
		}
	}
	page := `<p style="color:` + muted + `">No information for this device.</p>`
	if b.Len() > 0 {
		page = `<table cellspacing="0" cellpadding="2">` + b.String() + `</table>`
	}
	// Keep the place on a refresh.
	pos := dv.view.VerticalScrollBar().Value()
	dv.view.SetHtml(page)
	dv.view.VerticalScrollBar().SetValue(pos)
}

// retheme sets the tab's icons and label colors again after a palette
// change.
func (dv *detailsTab) retheme() {
	for _, set := range dv.icons {
		set()
	}
	dv.render()
}
