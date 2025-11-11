package cmd

import (
	"fmt"

	"github.com/lexpaval/mesh-central-client-go/internal/config"
	"github.com/spf13/cobra"
)

var configCmd = &cobra.Command{
	Use:     "config",
	Aliases: []string{"c"},
	Short:   "Return Config Path",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {

		fmt.Println(config.GetConfigPath())

	},
}

func init() {
	rootCmd.AddCommand(configCmd)
}
