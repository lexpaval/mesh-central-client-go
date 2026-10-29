package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mappu/miqt/qt6/mainthread"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// One Details tab per device, the device's text shown as text, closed with
// the session.
func TestDetailsTab(t *testing.T) {
	var dv *detailsTab
	ui(t, func() {
		setConnected(true)
		setDevices([]meshcentral.Device{{Id: "n", MeshID: "m", Group: "G", Name: "dev", Pwr: 1}})
		openDetails(devices[0])
		openDetails(devices[0])
		if len(detailsTabs) != 1 || tabs.Count() != 3 || tabs.TabText(2) != "dev · Details" {
			t.Errorf("%d details tabs, %d tabs", len(detailsTabs), tabs.Count())
			return
		}
		dv = detailsTabs[0]
	})
	// No server here, the load fails and says so.
	waitFor(t, "the failed load", func() bool { return dv.status.Text() != "Loading…" })
	mainthread.Wait(func() {
		if dv.status.StyleSheet() != errorStyle {
			t.Errorf("status %q isn't an error", dv.status.Text())
		}
		var node map[string]interface{}
		json.Unmarshal([]byte(`{"_id": "n", "name": "dev", "desc": "<b>rack 4</b>", "wsc": {"firewall": "BAD"}}`), &node)
		dv.info = &meshcentral.DeviceInfo{Node: node}
		dv.render()
		text := dv.view.ToPlainText()
		if !strings.Contains(text, "<b>rack 4</b>") || !strings.Contains(text, "Firewall - BAD") {
			t.Errorf("shown %q", text)
		}
		page := dv.view.ToHtml()
		if !strings.Contains(page, "#cf222e") {
			t.Error("the bad firewall isn't red")
		}
		if !strings.Contains(page, `src="icon:circle-info"`) || !strings.Contains(page, `src="icon:shield-halved"`) {
			t.Error("section titles without their icons")
		}
		disconnect()
		if len(detailsTabs) != 0 || tabs.Count() != 2 {
			t.Errorf("disconnect left %d details tabs", len(detailsTabs))
		}
	})
}
