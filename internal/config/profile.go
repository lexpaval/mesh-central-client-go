package config

import (
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
}

// GetPassword retrieves password from system keyring
func (p *Profile) GetPassword() (string, error) {
	return keyring.Get(keyringService, p.Name)
}

// SetPassword stores password in system keyring
func (p *Profile) SetPassword(password string) error {
	return keyring.Set(keyringService, p.Name, password)
}

// DeletePassword removes password from system keyring
func (p *Profile) DeletePassword() error {
	return keyring.Delete(keyringService, p.Name)
}

// The 2FA cookie is MeshCentral's "remember this device" cookie, kept in the
// keyring next to the password so later logins skip the token prompt.
func (p *Profile) GetTwoFactorCookie() (string, error) {
	return keyring.Get(keyringService, p.Name+"/2fa")
}

func (p *Profile) SetTwoFactorCookie(cookie string) error {
	return keyring.Set(keyringService, p.Name+"/2fa", cookie)
}

func (p *Profile) DeleteTwoFactorCookie() error {
	return keyring.Delete(keyringService, p.Name+"/2fa")
}

// SSO profiles log in with a MeshCentral login token minted from a browser
// session, stored as "<token user> <token password>" (neither has spaces).
func (p *Profile) GetLoginToken() (user, pass string, err error) {
	t, err := keyring.Get(keyringService, p.Name+"/sso")
	if err != nil {
		return "", "", err
	}
	user, pass, _ = strings.Cut(t, " ")
	return user, pass, nil
}

func (p *Profile) SetLoginToken(user, pass string) error {
	return keyring.Set(keyringService, p.Name+"/sso", user+" "+pass)
}

func (p *Profile) DeleteLoginToken() error {
	return keyring.Delete(keyringService, p.Name+"/sso")
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

func AddProfile(name string, isDefault bool, server string, username string, password string, sso bool) (*Profile, error) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	newProfile := Profile{
		Name:     name,
		Server:   server,
		Username: username,
		SSO:      sso,
	}

	if password != "" {
		if err := newProfile.SetPassword(password); err != nil {
			return nil, err
		}
	}

	profiles = append(profiles, newProfile)

	if isDefault {
		viper.Set("default_profile", name)
	}

	viper.Set("profiles", profiles)
	if err := viper.WriteConfig(); err != nil {
		return nil, err
	}

	newProfile.Password = password // Set for return value
	return &newProfile, nil
}

// UpdateProfile replaces profile oldName with p. An empty password keeps the
// stored one. On a rename the password, 2FA cookie and login token move with
// the profile. The cookie and token are dropped when the server, username or
// SSO setting change, since they belong to that account. The default profile
// follows a rename.
func UpdateProfile(oldName string, p Profile, password string, isDefault bool) error {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)
	i := slices.IndexFunc(profiles, func(x Profile) bool { return x.Name == oldName })
	if i < 0 {
		return &ProfileNotFoundError{oldName}
	}
	old := profiles[i]
	renamed := p.Name != oldName

	if password == "" && renamed {
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
	} else if renamed {
		if c, err := old.GetTwoFactorCookie(); err == nil {
			p.SetTwoFactorCookie(c)
		}
		if u, pw, err := old.GetLoginToken(); err == nil {
			p.SetLoginToken(u, pw)
		}
	}
	if renamed {
		old.DeletePassword()
		old.DeleteTwoFactorCookie()
		old.DeleteLoginToken()
	}

	if isDefault || viper.GetString("default_profile") == oldName {
		viper.Set("default_profile", p.Name)
	}
	profiles[i] = Profile{Name: p.Name, Server: p.Server, Username: p.Username, SSO: p.SSO}
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
