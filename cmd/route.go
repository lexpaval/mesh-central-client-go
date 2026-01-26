package cmd

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

var routeCmd = &cobra.Command{
	Use:     "route",
	Aliases: []string{"r"},
	Short:   "Forward TCP traffic to specified Node",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {

		bindAddress, _ := cmd.Flags().GetString("bind-address")
		nodeID, _ := cmd.Flags().GetString("nodeid")
		debug, _ := cmd.Flags().GetBool("debug")
		insecure, _ := cmd.Flags().GetBool("insecure")

		localport, target, remoteport, err := parseBindAddress(bindAddress)
		if err != nil {
			fmt.Println("Error parsing bind address: ", err)
			return
		}

		meshcentral.ApplySettings(
			nodeID,
			remoteport,
			localport,
			target,
			insecure,
			debug,
		)

		meshcentral.StartSocket()

		if nodeID == "" {
			devices := meshcentral.GetDevices()
			filterAndSortDevices(&devices)
			nodeID = searchDevices(&devices)

			meshcentral.ApplySettings(
				nodeID,
				remoteport,
				localport,
				target,
				insecure,
				debug,
			)
		}

		ready := make(chan struct{})
		meshcentral.StartRouter(ready)
	},
}

func init() {
	rootCmd.AddCommand(routeCmd)

	routeCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	routeCmd.Flags().StringP("bind-address", "L", "", "localport:[target:]remoteport")
	routeCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	routeCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
}

// parseBindAddress parses a bind address string in the format:
// "localport:target:remoteport" or "localport:remoteport" or just "remoteport"
func parseBindAddress(s string) (localPort int, target string, remotePort int, err error) {
	parts := strings.Split(s, ":")
	switch len(parts) {
	case 1:
		remotePort, err = strconv.Atoi(parts[0])
		if err != nil {
			return 0, "", 0, errors.New("invalid bind address format")
		}
	case 2:
		if isDigits(parts[0]) {
			localPort, _ = strconv.Atoi(parts[0])
		} else {
			target = parts[0]
		}
		remotePort, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, "", 0, errors.New("invalid bind address format")
		}
	case 3:
		if !isDigits(parts[0]) {
			return 0, "", 0, errors.New("invalid bind address format")
		}
		localPort, _ = strconv.Atoi(parts[0])
		target = parts[1]
		remotePort, err = strconv.Atoi(parts[2])
		if err != nil {
			return 0, "", 0, errors.New("invalid bind address format")
		}
	default:
		return 0, "", 0, errors.New("invalid bind address format")
	}

	// If target is "127.0.0.1", set to nothing
	if target == "127.0.0.1" {
		target = ""
	}

	return localPort, target, remotePort, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
