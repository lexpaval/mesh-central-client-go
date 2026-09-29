package cmd

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

var runCmd = &cobra.Command{
	Use:   "run [command]",
	Short: "Run a shell command on a node, fire and forget",
	Long:  `Dispatches a command to the node's native shell (cmd.exe on Windows, sh on Linux/macOS). Does not wait for or return output.`,
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {

		nodeID, _ := cmd.Flags().GetString("nodeid")
		debug, _ := cmd.Flags().GetBool("debug")
		insecure, _ := cmd.Flags().GetBool("insecure")
		asUser, _ := cmd.Flags().GetBool("as-user")

		nodeID = resolveNodeID(nodeID, insecure, debug)

		runAsUser := 0
		if asUser {
			runAsUser = 1
		}

		command := strings.Join(args, " ")
		if err := meshcentral.RunCommand(nodeID, command, runAsUser); err != nil {
			fmt.Println("Error sending command:", err)
		} else {
			fmt.Printf("Command sent to %s\n", nodeID)
		}

		meshcentral.StopSocket()
	},
}

func init() {
	rootCmd.AddCommand(runCmd)

	runCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	runCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	runCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
	runCmd.Flags().BoolP("as-user", "", false, "Run as the logged-in user instead of SYSTEM/root")
}
