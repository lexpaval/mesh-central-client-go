package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

var sshCmd = &cobra.Command{
	Use:   "ssh [user][@target]",
	Short: "Shortcut to ssh into a node",
	Long:  `Opens SSH connection with the OpenSSH Client to a node via the local proxy`,
	Run: func(cmd *cobra.Command, args []string) {

		user := "root"
		target := ""

		if len(args) == 1 {
			// parse user@target
			parts := strings.Split(args[0], "@")
			user = parts[0]
			if len(parts) == 2 {
				target = parts[1]
			}
		}

		remoteport, _ := cmd.Flags().GetInt("port")

		nodeID, _ := cmd.Flags().GetString("nodeid")
		debug, _ := cmd.Flags().GetBool("debug")
		proxyMode, _ := cmd.Flags().GetBool("proxy")
		insecure, _ := cmd.Flags().GetBool("insecure")

		route := meshcentral.Route{
			NodeID:     resolveNodeID(nodeID, insecure, debug),
			Target:     target,
			RemotePort: remoteport,
		}

		if proxyMode {
			// Proxy mode: pipe stdin/stdout directly through WebSocket
			meshcentral.StartProxyRouter(&route)
		} else {
			// Interactive mode: start proxy on a random local port and launch SSH client
			if err := route.Start(); err != nil {
				fmt.Println(err)
				os.Exit(1)
			}
			sshPort := route.LocalPort
			fmt.Printf("SSH into %s:%d via 127.0.0.1:%d\n", target, remoteport, sshPort)
			sshCmd := exec.Command("ssh", "-o", "ServerAliveInterval=60",
				"-o", "ServerAliveCountMax=3",
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				fmt.Sprintf("-p%d", sshPort), fmt.Sprintf("%s@127.0.0.1", user),
			)
			sshCmd.Stdout = os.Stdout
			sshCmd.Stderr = os.Stderr
			sshCmd.Stdin = os.Stdin
			err := sshCmd.Run()
			if err != nil {
				fmt.Printf("Unable to start SSH client: %v\n", err)
			}
		}
	},
}

func init() {
	rootCmd.AddCommand(sshCmd)

	sshCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	sshCmd.Flags().IntP("port", "p", 22, "Define the remote ssh port")
	sshCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	sshCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
	sshCmd.Flags().BoolP("proxy", "", false, "Proxy mode for SSH ProxyCommand")
}
