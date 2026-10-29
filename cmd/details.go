package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

var detailsCmd = &cobra.Command{
	Use:   "details",
	Short: "Show a node's system, hardware, network and security details",
	Long: `Shows what the web UI's General and Details tabs show of a node. Hardware and network details need
the "Device details" right and an agent that has reported them. --json prints the server's data as is.`,
	Args: cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		nodeID, _ := cmd.Flags().GetString("nodeid")
		debug, _ := cmd.Flags().GetBool("debug")
		insecure, _ := cmd.Flags().GetBool("insecure")
		asJSON, _ := cmd.Flags().GetBool("json")
		if asJSON && nodeID == "" {
			fmt.Fprintln(os.Stderr, "-i is required with --json")
			os.Exit(1)
		}

		nodeID = resolveNodeID(nodeID, insecure, debug)
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		info, err := meshcentral.GetDeviceInfo(ctx, nodeID)
		cancel()
		meshcentral.StopSocket()
		if err != nil {
			fmt.Fprintln(os.Stderr, "Unable to read the details:", err)
			os.Exit(1)
		}
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(info)
			return
		}
		color := term.IsTerminal(int(os.Stdout.Fd())) && os.Getenv("NO_COLOR") == ""
		printDetails(os.Stdout, info.Sections(), color)
	},
}

// printDetails writes the sections as indented label/value lines, the values
// lined up through all of them. color shows the statuses in green, yellow
// and red, like the web UI.
func printDetails(w io.Writer, sections []meshcentral.DetailSection, color bool) {
	indent := func(g meshcentral.DetailGroup) string {
		if g.Title != "" {
			return "    "
		}
		return "  "
	}
	width := 0
	for _, s := range sections {
		for _, g := range s.Groups {
			for _, r := range g.Rows {
				width = max(width, utf8.RuneCountInString(indent(g)+r.Label))
			}
		}
	}
	codes := map[meshcentral.State]string{meshcentral.Good: "32", meshcentral.Attention: "33", meshcentral.Bad: "31"}
	for i, s := range sections {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, s.Title)
		for _, g := range s.Groups {
			if g.Title != "" {
				fmt.Fprintln(w, "  "+g.Title)
			}
			for _, r := range g.Rows {
				label := indent(g) + r.Label
				var value strings.Builder
				for _, p := range r.Value {
					if c, ok := codes[p.State]; ok && color {
						value.WriteString("\x1b[" + c + "m" + p.Text + "\x1b[0m")
					} else {
						value.WriteString(p.Text)
					}
				}
				fmt.Fprintf(w, "%s%s  %s\n", label, strings.Repeat(" ", width-utf8.RuneCountInString(label)), value.String())
			}
		}
	}
}

func init() {
	rootCmd.AddCommand(detailsCmd)

	detailsCmd.Flags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	detailsCmd.Flags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	detailsCmd.Flags().BoolP("debug", "", false, "Enable debug logging")
	detailsCmd.Flags().Bool("json", false, "Print the server's data as JSON")
}
