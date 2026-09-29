package config

import (
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

func RemoveProfile(name string) {
	var profiles []Profile
	viper.UnmarshalKey("profiles", &profiles)

	for i, p := range profiles {
		if p.Name == name {
			// Delete password from keyring
			p.DeletePassword()
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
