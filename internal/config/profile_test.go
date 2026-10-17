package config

import (
	"path/filepath"
	"testing"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

func TestUpdateProfile(t *testing.T) {
	keyring.MockInit()
	viper.Reset()
	viper.SetConfigFile(filepath.Join(t.TempDir(), "config.json"))
	if err := CreateConfig("mesh.example.com", "alice", "pw1", false); err != nil {
		t.Fatal(err)
	}
	def := Profile{Name: "default"}
	def.SetTwoFactorCookie("cookie1")
	def.SetLoginToken("~t:u", "tp")

	// Rename with an empty password keeps the password and cookie, and the
	// default follows.
	if err := UpdateProfile("default", Profile{Name: "work", Server: "mesh.example.com", Username: "alice"}, "", false); err != nil {
		t.Fatal(err)
	}
	work := Profile{Name: "work"}
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
	if _, err := work.GetTwoFactorCookie(); err == nil {
		t.Error("2FA cookie kept after the server changed")
	}
	if _, _, err := work.GetLoginToken(); err == nil {
		t.Error("login token kept after the server changed")
	}
	if pw, _ := work.GetPassword(); pw != "pw2" {
		t.Errorf("password = %q, want pw2", pw)
	}
	if p := GetDefaultProfile(); p.Server != "other.example.com" || p.Password != "pw2" {
		t.Errorf("stored profile = %+v", p)
	}

	if err := UpdateProfile("missing", Profile{Name: "x"}, "", false); err == nil {
		t.Error("updating a missing profile succeeded")
	}
}
