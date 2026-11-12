package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

func TestUpdateProfile(t *testing.T) {
	keyring.MockInit()
	viper.Reset()
	viper.SetConfigFile(filepath.Join(t.TempDir(), "config.json"))
	if err := CreateConfig(Profile{Server: "mesh.example.com", Username: "alice"}, "pw1"); err != nil {
		t.Fatal(err)
	}
	def := Profile{Name: "default", Server: "mesh.example.com", Username: "alice"}
	def.SetTwoFactorCookie("cookie1")
	def.SetLoginToken("~t:u", "tp")

	// Rename with an empty password keeps the password and cookie, and the
	// default follows.
	if err := UpdateProfile("default", Profile{Name: "work", Server: "mesh.example.com", Username: "alice"}, "", false); err != nil {
		t.Fatal(err)
	}
	work := Profile{Name: "work", Server: "mesh.example.com", Username: "alice"}
	if pw, _ := work.GetPassword(); pw != "pw1" {
		t.Errorf("password after rename = %q, want pw1", pw)
	}
	if c, _ := work.GetTwoFactorCookie(); c != "cookie1" {
		t.Errorf("2FA cookie after rename = %q, want cookie1", c)
	}
	if u, pw, _ := work.GetLoginToken(); u != "~t:u" || pw != "tp" {
		t.Errorf("login token after rename = %q/%q, want ~t:u/tp", u, pw)
	}
	if _, _, err := def.GetLoginToken(); err == nil {
		t.Error("old name still has a login token in the keyring")
	}
	if _, err := def.GetPassword(); err == nil {
		t.Error("old name still has a password in the keyring")
	}
	if got := GetDefaultProfileName(); got != "work" {
		t.Errorf("default profile = %q, want work", got)
	}

	// Changing the server drops the cookie, a new password replaces the old.
	if err := UpdateProfile("work", Profile{Name: "work", Server: "other.example.com", Username: "alice"}, "pw2", false); err != nil {
		t.Fatal(err)
	}
	moved := Profile{Name: "work", Server: "other.example.com", Username: "alice"}
	if _, err := moved.GetTwoFactorCookie(); err == nil {
		t.Error("2FA cookie kept after the server changed")
	}
	if _, _, err := moved.GetLoginToken(); err == nil {
		t.Error("login token kept after the server changed")
	}
	if _, err := work.GetPassword(); err == nil {
		t.Error("the old server's entry still has a password")
	}
	if pw, _ := moved.GetPassword(); pw != "pw2" {
		t.Errorf("password = %q, want pw2", pw)
	}
	if p := GetDefaultProfile(); p.Server != "other.example.com" || p.Password != "pw2" {
		t.Errorf("stored profile = %+v", p)
	}

	if err := UpdateProfile("missing", Profile{Name: "x"}, "", false); err == nil {
		t.Error("updating a missing profile succeeded")
	}
}

// Secrets belong to a server: another config file's profile of the same name
// pointing elsewhere doesn't get them.
func TestProfileSecretsPerServer(t *testing.T) {
	keyring.MockInit()
	viper.Reset()
	viper.SetConfigFile(filepath.Join(t.TempDir(), "config.json"))
	if err := CreateConfig(Profile{Server: "mesh.example.com", Username: "alice"}, "pw1"); err != nil {
		t.Fatal(err)
	}
	a := GetDefaultProfile()
	a.SetTwoFactorCookie("cookie1")

	viper.Reset()
	viper.SetConfigFile(filepath.Join(t.TempDir(), "other.json"))
	viper.SetConfigType("json")
	viper.Set("default_profile", "default")
	viper.Set("profiles", []Profile{{Name: "default", Server: "evil.example.com", Username: "alice"}})
	b := GetDefaultProfile()
	if b.Password != "" {
		t.Errorf("profile on another server got password %q", b.Password)
	}
	if c, err := b.GetTwoFactorCookie(); err == nil {
		t.Errorf("profile on another server got 2FA cookie %q", c)
	}
	if _, err := AddProfile(Profile{Name: "default", Server: "evil.example.com", Username: "bob"}, "pw2", false); err == nil {
		t.Error("added a second profile called default")
	}
}

func TestMigrateKeyring(t *testing.T) {
	write := func(path string) {
		viper.Reset()
		viper.SetConfigFile(path)
		os.MkdirAll(filepath.Dir(path), 0o700)
		os.WriteFile(path, []byte(`{"default_profile":"default","profiles":[{"name":"default","server":"mesh.example.com","username":"alice"}]}`), 0o600)
		keyring.MockInit()
		keyring.Set(keyringService, "default", "pw1")
		keyring.Set(keyringService, "default/2fa", "cookie1")
		if err := LoadConfig(); err != nil {
			t.Fatal(err)
		}
	}

	// A file passed with -C doesn't claim the old entries.
	write(filepath.Join(t.TempDir(), "other.json"))
	if p := GetDefaultProfile(); p.Password != "" {
		t.Errorf("-C config took the old password %q", p.Password)
	}
	if v, _ := keyring.Get(keyringService, "default"); v != "pw1" {
		t.Errorf("-C config moved the old entry, left %q", v)
	}

	defer func(p string) { DefaultConfigPath = p }(DefaultConfigPath)
	DefaultConfigPath = filepath.Join(t.TempDir(), "mcc", "meshcentral-client.json")
	write(DefaultConfigPath)
	p := GetDefaultProfile()
	if c, _ := p.GetTwoFactorCookie(); p.Password != "pw1" || c != "cookie1" {
		t.Errorf("after migration: password %q, cookie %q", p.Password, c)
	}
	if _, err := keyring.Get(keyringService, "default"); err == nil {
		t.Error("the old entry is still there")
	}
	if viper.GetInt("keyring") != keyringVersion {
		t.Error("migration not recorded in the config")
	}
}

// The first profile can have its own name, and a description stays through
// a reload and an edit.
func TestProfileNameAndDescription(t *testing.T) {
	keyring.MockInit()
	viper.Reset()
	viper.SetConfigFile(filepath.Join(t.TempDir(), "config.json"))
	if err := CreateConfig(Profile{Name: "office", Server: "mesh.example.com", Username: "alice", Description: "main site"}, "pw1"); err != nil {
		t.Fatal(err)
	}
	if _, err := AddProfile(Profile{Name: "lab", Server: "lab.example.com", Username: "bob", Description: "test bench"}, "pw2", false); err != nil {
		t.Fatal(err)
	}
	if err := LoadConfig(); err != nil {
		t.Fatal(err)
	}
	if p := GetDefaultProfile(); p.Name != "office" || p.Description != "main site" || p.Password != "pw1" {
		t.Errorf("first profile = %+v", p)
	}
	if err := UpdateProfile("lab", Profile{Name: "lab", Server: "lab.example.com", Username: "bob", Description: "bench 2"}, "", false); err != nil {
		t.Fatal(err)
	}
	if p, _ := GetProfile("lab"); p.Description != "bench 2" || p.Password != "pw2" {
		t.Errorf("edited profile = %+v", p)
	}
}
