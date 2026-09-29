package cmd

import (
	"cmp"
	"fmt"
	"os"
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

		bindaddr, localport, target, remoteport, err := parseBindAddress(bindAddress)
		if err != nil {
			fmt.Println("Error parsing bind address:", err)
			return
		}
		route := meshcentral.Route{
			NodeID:      resolveNodeID(nodeID, insecure, debug),
			BindAddress: bindaddr,
			LocalPort:   localport,
			Target:      target,
			RemotePort:  remoteport,
		}
		if err := route.Start(); err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Printf("Redirecting %s:%d to remote port %d.\n", cmp.Or(bindaddr, "127.0.0.1"), route.LocalPort, remoteport)
		fmt.Println("Press ctrl-c to exit.")
		select {}
	},
}

func init() {
	rootCmd.AddCommand(routeCmd)

	routeCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	routeCmd.Flags().StringP("bind-address", "L", "", "[bind_address:]localport:target:remoteport, localport:remoteport or remoteport")
	routeCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	routeCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
}

// parseBindAddress parses a bind address string in the format:
// "bindaddress:localport:target:remoteport", "localport:target:remoteport",
// "localport:remoteport", "target:remoteport" or just "remoteport"
func parseBindAddress(s string) (bindAddress string, localPort int, target string, remotePort int, err error) {
	errFormat := fmt.Errorf("invalid bind address %q, expected [bind_address:]localport:target:remoteport, localport:remoteport or remoteport", s)

	parts := strings.Split(s, ":")
	if len(parts) == 4 {
		bindAddress, parts = parts[0], parts[1:]
		if bindAddress == "" {
			return "", 0, "", 0, errFormat
		}
	}

	switch len(parts) {
	case 1:
		remotePort, err = strconv.Atoi(parts[0])
		if err != nil {
			return "", 0, "", 0, errFormat
		}
	case 2:
		if isDigits(parts[0]) {
			localPort, _ = strconv.Atoi(parts[0])
		} else {
			target = parts[0]
		}
		remotePort, err = strconv.Atoi(parts[1])
		if err != nil {
			return "", 0, "", 0, errFormat
		}
	case 3:
		if !isDigits(parts[0]) {
			return "", 0, "", 0, errFormat
		}
		localPort, _ = strconv.Atoi(parts[0])
		target = parts[1]
		remotePort, err = strconv.Atoi(parts[2])
		if err != nil {
			return "", 0, "", 0, errFormat
		}
	default:
		return "", 0, "", 0, errFormat
	}

	// If target is "127.0.0.1", set to nothing
	if target == "127.0.0.1" {
		target = ""
	}

	return bindAddress, localPort, target, remotePort, nil
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
