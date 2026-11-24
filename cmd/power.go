package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

// powerActions are the agent's, by the name mcc power takes.
var powerActions = map[string]meshcentral.PowerAction{"sleep": meshcentral.Sleep, "restart": meshcentral.Restart, "off": meshcentral.PowerOff}

var powerCmd = &cobra.Command{
	Use:   "power wake|sleep|restart|off",
	Short: "Wake a node, or put it to sleep, restart it or power it off",
	Long: `wake has agents on the node's network send wake-on-LAN packets, it needs the Wake devices right and the node's network details.
sleep, restart and off go to the node's agent, which has to be connected, and need the Reset/Off right. The server doesn't confirm them.`,
	Args:      cobra.ExactArgs(1),
	ValidArgs: []string{"wake", "sleep", "restart", "off"},
	Run: func(cmd *cobra.Command, args []string) {
		action, ok := powerActions[args[0]]
		if args[0] != "wake" && !ok {
			fmt.Fprintf(os.Stderr, "Unknown power action %q, use wake, sleep, restart or off\n", args[0])
			os.Exit(1)
		}
		nodeID, _ := cmd.Flags().GetString("nodeid")
		debug, _ := cmd.Flags().GetBool("debug")
		insecure, _ := cmd.Flags().GetBool("insecure")
		nodeID = resolveNodeID(nodeID, insecure, debug)
		defer meshcentral.StopSocket()

		if args[0] == "wake" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			result, err := meshcentral.Wake(ctx, nodeID)
			if err != nil {
				fmt.Fprintln(os.Stderr, "Wake:", err)
				return
			}
			fmt.Println(result)
			return
		}
		if err := meshcentral.Power(nodeID, action); err != nil {
			fmt.Fprintln(os.Stderr, "Error sending power action:", err)
			return
		}
		fmt.Printf("Asked %s to %s\n", nodeID, map[string]string{"sleep": "sleep", "restart": "restart", "off": "power off"}[args[0]])
	},
}

func init() {
	rootCmd.AddCommand(powerCmd)

	powerCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	powerCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	powerCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
}
