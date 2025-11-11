package cmd

import (
	"os"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"github.com/spf13/viper"
	"github.com/zalando/go-keyring"
)

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "meshagent",
	Short: "Simple tool to interact with the MeshCentral API",
	Long:  ``,
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		c, _ := cmd.Flags().GetString("config")
		if c != "" {
			viper.SetConfigFile(c)
		} else {
			viper.SetConfigFile(config.DefaultConfigPath)
		}

		// create config file if necessary
		initializeSetup()

		// Load the config file
		config.LoadConfig()

		// Migrate plaintext passwords to keyring
		if err := migratePasswords(); err != nil {
			pterm.Warning.Println("Password migration warning:", err)
		}

		p, _ := cmd.Flags().GetString("profile")
		if p != "" {
			config.SetDefaultProfile(p, false)
		}

	},
	// Uncomment the following line if your bare application
	// has an action associated with it:
	// Run: func(cmd *cobra.Command, args []string) { },
}

func migratePasswords() error {
	var profiles []map[string]interface{}
	viper.UnmarshalKey("profiles", &profiles)

	modified := false
	for i, p := range profiles {
		if pwd, ok := p["password"].(string); ok && pwd != "" {
			// Store in keyring
			name := p["name"].(string)
			if err := keyring.Set("meshcentral-client", name, pwd); err != nil {
				return err
			}
			// Remove from config
			delete(profiles[i], "password")
			modified = true
		}
	}

	if modified {
		viper.Set("profiles", profiles)
		return viper.WriteConfig()
	}
	return nil
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {
	// Here you will define your flags and configuration settings.
	// Cobra supports persistent flags, which, if defined here,
	// will be global for your application.

	rootCmd.PersistentFlags().StringP("config", "C", "", "Alternate configuration file to use")
	rootCmd.PersistentFlags().StringP("profile", "P", "", "Override the active profile")
}

func pExit(s string, err error) {
	if err != nil {
		pterm.Error.Println(s, err)
		os.Exit(1)
	}
}

func initializeSetup() {
	// Check if the config file exists
	_, err := os.Stat(viper.ConfigFileUsed())
	if os.IsNotExist(err) {
		result, _ := pterm.DefaultInteractiveConfirm.Show("Config file does not exist. Would you like to create it?")
		if !result {
			pterm.Info.Println("Exiting...")
			os.Exit(0)
		}

		// Create the config file
		server, _ := pterm.DefaultInteractiveTextInput.Show("Enter the MeshCentral server hostname or IP (ex: mesh.example.com)")
		username, _ := pterm.DefaultInteractiveTextInput.Show("Enter the MeshCentral username")
		password, _ := pterm.DefaultInteractiveTextInput.WithMask("*").Show("Enter the MeshCentral password")

		err := config.CreateConfig(server, username, password)
		if err != nil {
			pterm.Error.Println("Error creating config file:", err)
			os.Exit(1)
		}
	}
}
