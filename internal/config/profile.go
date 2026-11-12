package config

import (
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

// Profile is a struct that holds the profile information
type Profile struct {
	Name     string
	Server   string
	Username string
	Password string `json:"-"` // Don't serialize password
	SSO      bool   // log in through the web UI's single sign-on, see GetLoginToken
	// Description is a short free-form note shown next to the name.
	Description string `json:",omitempty"`
}

// account is where p's secret of kind ("", "/2fa" or "/sso") is kept in the
// keyring: with the user and server it belongs to, so a profile of the same
// name in another config file, pointing at another server, doesn't get it.
func (p *Profile) account(kind string) string {
	return p.Name + " (" + p.Username + "@" + p.Server + ")" + kind
}

// GetPassword retrieves password from system keyring
func (p *Profile) GetPassword() (string, error) {
	return keyring.Get(keyringService, p.account(""))
}

// SetPassword stores password in system keyring
func (p *Profile) SetPassword(password string) error {
	return keyring.Set(keyringService, p.account(""), password)
}

// DeletePassword removes password from system keyring
func (p *Profile) DeletePassword() error {
	return keyring.Delete(keyringService, p.account(""))
}

// The 2FA cookie is MeshCentral's "remember this device" cookie, kept in the
// keyring next to the password so later logins skip the token prompt.
func (p *Profile) GetTwoFactorCookie() (string, error) {
	return keyring.Get(keyringService, p.account("/2fa"))
}

func (p *Profile) SetTwoFactorCookie(cookie string) error {
	return keyring.Set(keyringService, p.account("/2fa"), cookie)
}

func (p *Profile) DeleteTwoFactorCookie() error {
	return keyring.Delete(keyringService, p.account("/2fa"))
}

// SSO profiles log in with a MeshCentral login token minted from a browser
// session, stored as "<token user> <token password>" (neither has spaces).
func (p *Profile) GetLoginToken() (user, pass string, err error) {
	t, err := keyring.Get(keyringService, p.account("/sso"))
	if err != nil {
		return "", "", err
	}
	user, pass, _ = strings.Cut(t, " ")
	return user, pass, nil
}

func (p *Profile) SetLoginToken(user, pass string) error {
	return keyring.Set(keyringService, p.account("/sso"), user+" "+pass)
}

func (p *Profile) DeleteLoginToken() error {
	return keyring.Delete(keyringService, p.account("/sso"))
}

// keyringVersion 2 keeps secrets by profile, user and server, 1 by profile
// name alone.
const keyringVersion = 2

// migrateKeyring moves the secrets kept by profile name alone to their
// profile's user and server, once. Only for the default config file, the
// one they were saved from: a file passed with -C could otherwise claim them
// for a profile of the same name on another server.
func migrateKeyring() {
	if viper.GetInt("keyring") >= keyringVersion || filepath.Clean(viper.ConfigFileUsed()) != filepath.Clean(DefaultConfigPath) {
		return
	}
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)
	for _, p := range profiles {
		for _, kind := range []string{"", "/2fa", "/sso"} {
			v, err := keyring.Get(keyringService, p.Name+kind)
			if errors.Is(err, keyring.ErrNotFound) {
				continue
			} else if err != nil {
				return // the keyring is locked or missing, try again next time
			}
			if _, err := keyring.Get(keyringService, p.account(kind)); errors.Is(err, keyring.ErrNotFound) {
				if keyring.Set(keyringService, p.account(kind), v) != nil {
					return
				}
			}
			keyring.Delete(keyringService, p.Name+kind)
		}
	}
	viper.Set("keyring", keyringVersion)
	viper.WriteConfig()
}

func GetProfiles() []Profile {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	// Load passwords from keyring
	for i := range profiles {
		if pwd, err := profiles[i].GetPassword(); err == nil {
			profiles[i].Password = pwd
		}
	}

	return profiles
}

func GetDefaultProfile() Profile {
	p, _ := GetProfile(viper.GetString("default_profile"))
	return p
}

// GetProfile returns the profile called name, with its password.
func GetProfile(name string) (Profile, bool) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	for _, p := range profiles {
		if p.Name == name {
			// Load password from keyring
			if pwd, err := p.GetPassword(); err == nil {
				p.Password = pwd
			}
			return p, true
		}
	}

	return Profile{}, false
}

func GetDefaultProfileName() string {
	return viper.GetString("default_profile")
}

func SetDefaultProfile(name string, commit bool) error {
	// get profiles from config
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	// make sure profile exists
	for _, p := range profiles {
		if p.Name == name {
			viper.Set("default_profile", name)
			if commit {
				viper.WriteConfig()
			}
			return nil
		}
	}
	return &ProfileNotFoundError{name}
}

// AddProfile saves p as a new profile, its password in the keyring.
func AddProfile(p Profile, password string, isDefault bool) (*Profile, error) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)
	if slices.ContainsFunc(profiles, func(x Profile) bool { return x.Name == p.Name }) {
		return nil, fmt.Errorf("a profile called %s already exists", p.Name)
	}

	newProfile := Profile{
		Name:        p.Name,
		Server:      p.Server,
		Username:    p.Username,
		SSO:         p.SSO,
		Description: p.Description,
	}

	if password != "" {
		if err := newProfile.SetPassword(password); err != nil {
			return nil, err
		}
	}

	profiles = append(profiles, newProfile)

	if isDefault {
		viper.Set("default_profile", p.Name)
	}

	viper.Set("profiles", profiles)
	if err := viper.WriteConfig(); err != nil {
		return nil, err
	}

	newProfile.Password = password // Set for return value
	return &newProfile, nil
}

// UpdateProfile replaces profile oldName with p. An empty password keeps the
// stored one, also when the name, server or username change. On a rename the
// 2FA cookie and login token move with the profile. They are dropped when the
// server, username or SSO setting change, since they belong to that account.
// The default profile follows a rename.
func UpdateProfile(oldName string, p Profile, password string, isDefault bool) error {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)
	i := slices.IndexFunc(profiles, func(x Profile) bool { return x.Name == oldName })
	if i < 0 {
		return &ProfileNotFoundError{oldName}
	}
	old := profiles[i]
	moved := p.account("") != old.account("") // kept under another keyring entry

	if password == "" && moved {
		password, _ = old.GetPassword()
	}
	if password != "" {
		if err := p.SetPassword(password); err != nil {
			return err
		}
	}
	if p.Server != old.Server || p.Username != old.Username || p.SSO != old.SSO {
		old.DeleteTwoFactorCookie()
		old.DeleteLoginToken()
	} else if moved {
		if c, err := old.GetTwoFactorCookie(); err == nil {
			p.SetTwoFactorCookie(c)
		}
		if u, pw, err := old.GetLoginToken(); err == nil {
			p.SetLoginToken(u, pw)
		}
	}
	if moved {
		old.DeletePassword()
		old.DeleteTwoFactorCookie()
		old.DeleteLoginToken()
	}

	if isDefault || viper.GetString("default_profile") == oldName {
		viper.Set("default_profile", p.Name)
	}
	profiles[i] = Profile{Name: p.Name, Server: p.Server, Username: p.Username, SSO: p.SSO, Description: p.Description}
	viper.Set("profiles", profiles)
	return viper.WriteConfig()
}

func RemoveProfile(name string) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	for i, p := range profiles {
		if p.Name == name {
			// Delete password, 2FA cookie and login token from keyring
			p.DeletePassword()
			p.DeleteTwoFactorCookie()
			p.DeleteLoginToken()
			profiles = append(profiles[:i], profiles[i+1:]...)
			break
		}
	}

	viper.Set("profiles", profiles)
	viper.WriteConfig()
}

// profile not found error definition
type ProfileNotFoundError struct {
	Name string
}

func (e *ProfileNotFoundError) Error() string {
	return "Profile not found: " + e.Name
}
