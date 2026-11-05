package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mappu/miqt/qt6/mainthread"
)

func TestNewerRelease(t *testing.T) {
	for _, c := range []struct {
		tag, version string
		want         bool
	}{
		{"v1.11.0", "v1.10.0", true},
		{"v1.10.1", "v1.10.0", true},
		{"v2.0.0", "v1.10.0", true},
		{"v1.10.0", "v1.10.0", false},
		{"v1.9.9", "v1.10.0", false}, // by number, not as text
		{"v1.11.0", "v1.10.0-2-g7010538", false},
		{"v1.11.0", "", false},
		{"", "v1.10.0", false},
	} {
		if got := newer(c.tag, c.version); got != c.want {
			t.Errorf("newer(%q, %q) = %v", c.tag, c.version, got)
		}
	}
}

func TestUpdateCheck(t *testing.T) {
	var hits atomic.Int32
	var tag, page atomic.Value
	tag.Store("v1.11.0")
	page.Store(releasePages + "tag/v1.11.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if r.Header.Get("User-Agent") == "" {
			http.Error(w, "no user agent", http.StatusForbidden)
			return
		}
		fmt.Fprintf(w, `{"tag_name":%q,"html_url":%q}`, tag.Load(), page.Load())
	}))
	defer srv.Close()
	savedAPI, savedVersion := releaseAPI, version
	t.Cleanup(func() { releaseAPI, version, checking, checkErr = savedAPI, savedVersion, false, "" })
	releaseAPI, version, checking, checkErr = srv.URL, "v1.10.0", false, ""
	ui(t, func() {})

	check := func(now bool) {
		t.Helper()
		mainthread.Wait(func() { checkUpdates(now) })
		waitFor(t, "the check", func() bool { return !checking })
	}
	shown := func() (on bool, text string) {
		mainthread.Wait(func() { on, text = !updateBtn.IsHidden(), updateBtn.Text() })
		return on, text
	}

	check(false)
	if on, text := shown(); !on || text != "Update available: v1.11.0" || hits.Load() != 1 {
		t.Fatalf("newer release: shown %v %q after %d requests", on, text, hits.Load())
	}
	check(false) // checked today already
	if hits.Load() != 1 {
		t.Errorf("checked again within a day")
	}

	tag.Store("v1.10.0")
	check(true)
	if on, _ := shown(); on || hits.Load() != 2 {
		t.Errorf("same release: shown %v after %d requests", on, hits.Load())
	}

	// Off, nothing is shown and only a check asked for is made.
	tag.Store("v1.12.0")
	page.Store(releasePages + "tag/v1.12.0")
	mainthread.Wait(func() { setPref("updateCheck", "off"); setPref("updateChecked", "") })
	check(false)
	if hits.Load() != 2 {
		t.Errorf("checked while off")
	}
	check(true)
	if on, _ := shown(); on || hits.Load() != 3 || !strings.Contains(updateStatus(), "Version v1.12.0 is available") {
		t.Errorf("off: shown %v, status %q", on, updateStatus())
	}

	// A page elsewhere than the project's releases isn't taken.
	page.Store("https://example.com/v1.13.0")
	tag.Store("v1.13.0")
	check(true)
	if latest, _ := latestRelease(); latest != "v1.12.0" || !strings.Contains(updateStatus(), "Couldn't check") {
		t.Errorf("took a foreign page: latest %q, status %q", latest, updateStatus())
	}

	// Builds between releases don't check.
	version = "v1.10.0-2-g7010538"
	check(true)
	if hits.Load() != 4 {
		t.Errorf("a dev build checked, %d requests", hits.Load())
	}
}
