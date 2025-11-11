package config

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/adrg/xdg"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

var configPath = xdg.ConfigHome + "/mcc"
var configName = "meshcentral-client"
var configType = "json"

const keyringService = "meshcentral-client"

// DefaultConfigPath constant containing default path to config file
var DefaultConfigPath = configPath + "/" + configName + "." + configType

func CreateConfig(server string, username string, password string) error {
	// Create profile without password in config
	viper.Set("profiles", []map[string]interface{}{
		{
			"name":     "default",
			"server":   server,
			"username": username,
		},
	})

	viper.Set("default_profile", "default")

	// Create directory if it does not exist
	path := filepath.Dir(viper.ConfigFileUsed())
	if _, err := os.Stat(path); os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0700); err != nil {
			return err
		}
	}

	// Write config first
	if err := viper.WriteConfig(); err != nil {
		return err
	}

	// Store password in keyring
	return keyring.Set(keyringService, "default", password)
}

func LoadConfig() error {
	viper.SetConfigType(configType)

	if viper.ConfigFileUsed() == "" {
		viper.SetConfigName(configName)
		viper.AddConfigPath(configPath)
	}

	viper.SetDefault("default_profile", "default")
	viper.SetDefault("profiles", []map[string]interface{}{
		{
			"name":     "default",
			"server":   "",
			"username": "",
		},
	})

	return viper.ReadInConfig()
}

func GetConfigPath() string {
	return viper.ConfigFileUsed()
}

func GetConfigJSON() (*string, error) {
	configJSON, err := json.MarshalIndent(viper.AllSettings(), "", "  ")
	if err != nil {
		return nil, err
	}
	configString := string(configJSON)
	return &configString, nil
}

func SaveConfig() error {
	return viper.WriteConfig()
}
