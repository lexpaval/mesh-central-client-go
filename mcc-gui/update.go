package main

import (
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	qt "github.com/mappu/miqt/qt6"
	"github.com/mappu/miqt/qt6/mainthread"
)

// Release builds ask GitHub for the latest release once a day, unless
// turned off in Settings, and offer a newer one beside the Settings button.
// The request carries nothing about the user or their devices.

// releaseAPI answers with the latest release, tests replace it.
var releaseAPI = "https://api.github.com/repos/lexpaval/mesh-central-client-go/releases/latest"

const (
	releasePages   = "https://github.com/lexpaval/mesh-central-client-go/releases/"
	updateInterval = 24 * time.Hour
)

var (
	updateBtn   *qt.QPushButton
	checking    bool
	checkErr    string
	aboutUpdate func() // About's update status, while it's open
)

// updatesOn is whether the daily check is on, the default.
func updatesOn() bool { return pref("updateCheck") != "off" }

// latestRelease is the newest release the last check found, kept across
// runs: its tag and its page.
func latestRelease() (tag, page string) { return pref("updateLatest"), pref("updateURL") }

// newer reports whether release tag is later than version, both vX.Y.Z.
func newer(tag, version string) bool {
	parse := func(s string) ([]int, bool) {
		if !releaseTag.MatchString(s) {
			return nil, false
		}
		var n []int
		for _, p := range strings.Split(s[1:], ".") {
			i, _ := strconv.Atoi(p)
			n = append(n, i)
		}
		return n, true
	}
	a, ok := parse(tag)
	b, ok2 := parse(version)
	return ok && ok2 && slices.Compare(a, b) > 0
}

// checkUpdates asks for the latest release when the last check is a day
// old, or right away when asked to.
func checkUpdates(now bool) {
	last, _ := strconv.ParseInt(pref("updateChecked"), 10, 64)
	due := updatesOn() && time.Since(time.Unix(last, 0)) >= updateInterval
	if checking || !releaseTag.MatchString(version) || !now && !due {
		showUpdate()
		return
	}
	checking, checkErr = true, ""
	showUpdate()
	go func() {
		tag, page, err := fetchLatestRelease()
		mainthread.Start(func() {
			checking = false
			if err != nil {
				checkErr = err.Error()
				if now {
					logf("Update check failed: %v", err)
				}
			} else {
				setPref("updateChecked", strconv.FormatInt(time.Now().Unix(), 10))
				setPref("updateLatest", tag)
				setPref("updateURL", page)
				if newer(tag, version) {
					logf("MeshCentral Client %s is available: %s", tag, page)
				}
			}
			showUpdate()
		})
	}()
}

func fetchLatestRelease() (tag, page string, err error) {
	req, err := http.NewRequest("GET", releaseAPI, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "mcc-gui/"+version) // GitHub refuses requests without one
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", fmt.Errorf("GitHub answered %s", resp.Status)
	}
	var r struct {
		Tag  string `json:"tag_name"`
		Page string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return "", "", err
	}
	// Only this project's release pages get opened.
	if !releaseTag.MatchString(r.Tag) || !strings.HasPrefix(r.Page, releasePages) {
		return "", "", fmt.Errorf("unexpected release %q at %q", r.Tag, r.Page)
	}
	return r.Tag, r.Page, nil
}

// showUpdate shows a newer release beside the Settings button, while the
// check is on, and in About.
func showUpdate() {
	tag, _ := latestRelease()
	if updateBtn != nil {
		on := updatesOn() && newer(tag, version)
		updateBtn.SetVisible(on)
		updateBtn.SetText("Update available: " + tag)
	}
	if aboutUpdate != nil {
		aboutUpdate()
	}
}

// updateStatus is About's line on updates, as rich text.
func updateStatus() string {
	tag, page := latestRelease()
	switch {
	case checking:
		return "Checking for updates…"
	case checkErr != "":
		return "Couldn't check for updates: " + html.EscapeString(checkErr)
	case newer(tag, version):
		return fmt.Sprintf(`<a href="%s">Version %s is available</a>`, page, tag)
	case tag != "":
		return "You have the latest version"
	case !updatesOn():
		return "Update checks are off in Settings"
	}
	return ""
}

func openLatestRelease() {
	_, page := latestRelease()
	if u, err := url.Parse(page); err == nil && strings.HasPrefix(page, releasePages) {
		if err := openURL(u); err != nil {
			showError(err)
		}
	}
}
