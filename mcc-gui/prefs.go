package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// Preferences of the GUI itself, in a JSON file next to the CLI's config.
// Each profile has its own, settings included; the app keeps only the last
// profile and what the release check found. The current profile is the one
// picked in the top bar, the connected one while connected (the bar hides).
var prefs struct {
	path     string
	m        map[string]string            // the app's
	profiles map[string]map[string]string // by profile name
	// legacy are a file's preferences from before profiles had their own,
	// refreshProfiles copies them to every profile once it knows them.
	legacy map[string]string
}

// appPrefs are the app's rather than a profile's.
var appPrefs = []string{"lastProfile", "updateChecked", "updateLatest", "updateURL"}

// The file's layout: the app's preferences, then each profile's.
//
//	{"lastProfile": "work", "profiles": {"work": {"theme": "dark", "routes": "[...]"}}}
//
// Older files had every preference at the top, a profile's routes and Recent
// as "routes/<profile>" and "recent/<profile>". Those move to their profile on
// load, the rest stays at the top until copied (legacy).
func loadPrefs(path string) {
	prefs.path, prefs.m, prefs.profiles, prefs.legacy = path, map[string]string{}, map[string]map[string]string{}, map[string]string{}
	var raw map[string]json.RawMessage
	if b, err := os.ReadFile(path); err != nil || json.Unmarshal(b, &raw) != nil {
		return
	}
	json.Unmarshal(raw["profiles"], &prefs.profiles)
	delete(raw, "profiles")
	for k, v := range raw {
		var s string
		if json.Unmarshal(v, &s) != nil {
			continue
		}
		if slices.Contains(appPrefs, k) {
			prefs.m[k] = s
			continue
		}
		moved := false
		for _, kind := range []string{"routes", "recent"} {
			if name, ok := strings.CutPrefix(k, kind+"/"); ok {
				if profilePref(name, kind) == "" {
					setProfilePref(name, kind, s)
				}
				moved = true
			}
		}
		if !moved {
			prefs.legacy[k] = s
		}
	}
}

// savePrefs writes the file, through a temporary one so it's never half
// written.
func savePrefs() {
	out := map[string]any{}
	for k, v := range prefs.legacy {
		out[k] = v
	}
	for k, v := range prefs.m {
		out[k] = v
	}
	if len(prefs.profiles) > 0 {
		out["profiles"] = prefs.profiles
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	os.MkdirAll(filepath.Dir(prefs.path), 0o700)
	tmp := prefs.path + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, prefs.path)
	}
}

// currentProfile is the profile whose preferences pref and setPref use.
// Before the window exists, the last one connected to.
func currentProfile() string {
	if profileSel != nil {
		return profileSel.CurrentText()
	}
	return prefs.m["lastProfile"]
}

// pref is a preference of the app or the current profile.
func pref(key string) string {
	if slices.Contains(appPrefs, key) {
		return prefs.m[key]
	}
	return profilePref(currentProfile(), key)
}

// setPref stores a preference of the app or the current profile, ""
// removes it.
func setPref(key, value string) {
	if !slices.Contains(appPrefs, key) {
		setProfilePref(currentProfile(), key, value)
		return
	}
	if prefs.m[key] == value {
		return
	}
	if value == "" {
		delete(prefs.m, key)
	} else {
		prefs.m[key] = value
	}
	savePrefs()
}

func profilePref(profile, key string) string { return prefs.profiles[profile][key] }

// setProfilePref stores a preference of profile, "" removes it.
func setProfilePref(profile, key, value string) {
	m := prefs.profiles[profile]
	if m[key] == value {
		return
	}
	if value == "" {
		delete(m, key)
		if len(m) == 0 {
			delete(prefs.profiles, profile)
		}
	} else {
		if m == nil {
			m = map[string]string{}
			prefs.profiles[profile] = m
		}
		m[key] = value
	}
	savePrefs()
}

// renameProfilePrefs moves a profile's preferences along with its rename.
func renameProfilePrefs(from, to string) {
	if m, ok := prefs.profiles[from]; ok {
		delete(prefs.profiles, from)
		prefs.profiles[to] = m
	}
	if prefs.m["lastProfile"] == from {
		prefs.m["lastProfile"] = to
	}
	savePrefs()
}

// removeProfilePrefs forgets a removed profile's preferences.
func removeProfilePrefs(name string) {
	delete(prefs.profiles, name)
	savePrefs()
}

// migrateLegacyPrefs gives each profile the preferences of an older file,
// where they were the app's, unless it has its own already. Profiles added
// later start with the defaults.
func migrateLegacyPrefs(names []string) {
	if len(prefs.legacy) == 0 {
		return
	}
	for _, name := range names {
		for k, v := range prefs.legacy {
			if profilePref(name, k) == "" {
				if prefs.profiles[name] == nil {
					prefs.profiles[name] = map[string]string{}
				}
				prefs.profiles[name][k] = v
			}
		}
	}
	prefs.legacy = map[string]string{}
	savePrefs()
}
