package cmd

import (
	"errors"
	"strconv"
	"strings"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
)

var profileCmd = &cobra.Command{
	Use:     "profile",
	Aliases: []string{"p"},
	Short:   "Manage local profiles",
	Long:    ``,
}

var profileDefaultCmd = &cobra.Command{
	Use:     "default",
	Aliases: []string{"switch", "d"},
	Short:   "Set a new default profile",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {
		err := config.SetDefaultProfile(args[0], true)
		if err != nil {
			pExit("Failed to switch profile:", err)
		}
		pterm.Info.Println("Switched default profile to: ", args[0])
	},
}

var profileListCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List all profiles",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {
		printProfileTable(config.GetProfiles())
	},
}

var profileRmCmd = &cobra.Command{
	Use:     "rm",
	Aliases: []string{"remove", "delete"},
	Short:   "Remove a profile",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {
		config.RemoveProfile(args[0])
		pterm.Info.Println("Removed profile: ", args[0])
	},
}

var profileAddCmd = &cobra.Command{
	Use:     "add",
	Aliases: []string{"a", "create"},
	Short:   "Add a new profile",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {
		name, _ := cmd.Flags().GetString("name")
		server, _ := cmd.Flags().GetString("server")
		username, _ := cmd.Flags().GetString("username")
		password, _ := cmd.Flags().GetString("password")
		isDefault, _ := cmd.Flags().GetBool("default")
		sso, _ := cmd.Flags().GetBool("sso")
		if !sso && (username == "" || password == "") {
			pExit("Failed to add profile:", errors.New("--username and --password are required without --sso"))
		}

		p, err := config.AddProfile(name, isDefault, server, username, password, sso)
		pExit("Failed to add profile:", err)

		printProfileTable([]config.Profile{*p})
	},
}

func init() {
	rootCmd.AddCommand(profileCmd)

	profileCmd.AddCommand(profileDefaultCmd)
	profileCmd.AddCommand(profileListCmd)
	profileCmd.AddCommand(profileAddCmd)
	profileCmd.AddCommand(profileRmCmd)

	profileAddCmd.Flags().StringP("name", "n", "", "The name of the profile to add")
	profileAddCmd.Flags().BoolP("default", "d", false, "Set this profile as the default profile")
	profileAddCmd.Flags().StringP("server", "s", "", "Mesh Central Server URL")
	profileAddCmd.Flags().StringP("username", "u", "", "Mesh Central Username")
	profileAddCmd.Flags().StringP("password", "p", "", "Mesh Central Password")
	profileAddCmd.Flags().Bool("sso", false, "Log in through the web UI's single sign-on (OIDC) instead of a password")
	profileAddCmd.MarkFlagRequired("name")
	profileAddCmd.MarkFlagRequired("server")

}

func printProfileTable(profiles []config.Profile) {
	// print profiles in a table
	profileData := [][]string{}

	// add header
	profileData = append(profileData, []string{"Name", "Server", "Username", "SSO", "IsDefault"})

	// add profile data
	for _, p := range profiles {
		d := config.GetDefaultProfileName()
		/*isDefault := "false"
		if strings.Compare(p.Name, d) == 0 {
			isDefault = "true"
			}*/

		profileData = append(
			profileData,
			[]string{
				p.Name,
				p.Server,
				p.Username,
				strconv.FormatBool(p.SSO),
				strconv.FormatBool((strings.Compare(p.Name, d) == 0)),
			},
		)
	}
	pterm.DefaultTable.WithHasHeader().WithBoxed().WithData(profileData).Render()
}
