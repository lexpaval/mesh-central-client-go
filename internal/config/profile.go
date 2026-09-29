package config

import (
	"slices"

	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

// Profile is a struct that holds the profile information
type Profile struct {
	Name     string
	Server   string
	Username string
	Password string `json:"-"` // Don't serialize password
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
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	defaultProfile := viper.GetString("default_profile")

	for _, p := range profiles {
		if p.Name == defaultProfile {
			// Load password from keyring
			if pwd, err := p.GetPassword(); err == nil {
				p.Password = pwd
			}
			return p
		}
	}

	return Profile{}
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

func AddProfile(name string, isDefault bool, server string, username string, password string) (*Profile, error) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	newProfile := Profile{
		Name:     name,
		Server:   server,
		Username: username,
	}

	if err := newProfile.SetPassword(password); err != nil {
		return nil, err
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
// stored one. On a rename the password and 2FA cookie move with the profile,
// and the cookie is dropped when the server or username change, since it
// belongs to that account. The default profile follows a rename.
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
	if p.Server != old.Server || p.Username != old.Username {
		old.DeleteTwoFactorCookie()
	} else if c, err := old.GetTwoFactorCookie(); renamed && err == nil {
		p.SetTwoFactorCookie(c)
	}
	if renamed {
		old.DeletePassword()
		old.DeleteTwoFactorCookie()
	}

	if isDefault || viper.GetString("default_profile") == oldName {
		viper.Set("default_profile", p.Name)
	}
	profiles[i] = Profile{Name: p.Name, Server: p.Server, Username: p.Username}
	viper.Set("profiles", profiles)
	return viper.WriteConfig()
}

func RemoveProfile(name string) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	for i, p := range profiles {
		if p.Name == name {
			// Delete password and 2FA cookie from keyring
			p.DeletePassword()
			p.DeleteTwoFactorCookie()
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
