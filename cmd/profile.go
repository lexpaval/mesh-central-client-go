package cmd

import (
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

		p := config.AddProfile(name, isDefault, server, username, password)

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
	profileAddCmd.MarkFlagRequired("name")
	profileAddCmd.MarkFlagRequired("server")
	profileAddCmd.MarkFlagRequired("username")
	profileAddCmd.MarkFlagRequired("password")

}

func printProfileTable(profiles []config.Profile) {
	// print profiles in a table
	profileData := [][]string{}

	// add header
	profileData = append(profileData, []string{"Name", "Server", "Username", "IsDefault"})

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
				strconv.FormatBool((strings.Compare(p.Name, d) == 0)),
			},
		)
	}
	pterm.DefaultTable.WithHasHeader().WithBoxed().WithData(profileData).Render()
}
