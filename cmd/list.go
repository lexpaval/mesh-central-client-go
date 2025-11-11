package cmd

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
	"github.com/spf13/cobra"

	"github.com/pterm/pterm"
)

var listCmd = &cobra.Command{
	Use:     "list",
	Aliases: []string{"ls"},
	Short:   "List connected nodes on server",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {

		meshcentral.ApplySettings(
			"",
			0,
			0,
			"",
			false,
			false,
		)

		meshcentral.StartSocket()

		d := meshcentral.GetDevices()
		meshcentral.StopSocket()

		filterAndSortDevices(&d)

		printDevices(&d)
	},
}

var searchCmd = &cobra.Command{
	Use:     "search",
	Aliases: []string{"s"},
	Short:   "Search for a node on the server",
	Long:    ``,
	Run: func(cmd *cobra.Command, args []string) {

		meshcentral.ApplySettings(
			"",
			0,
			0,
			"",
			false,
			false,
		)

		meshcentral.StartSocket()

		d := meshcentral.GetDevices()
		meshcentral.StopSocket()

		filterAndSortDevices(&d)
		nodeid := searchDevices(&d)

		pterm.Println("Selected Node:", nodeid)

	},
}

func init() {
	rootCmd.AddCommand(listCmd)
	rootCmd.AddCommand(searchCmd)
}

func filterAndSortDevices(d *[]meshcentral.Device) {
	// filter devices (remove offline devices)
	devices := (*d)[:0]
	for _, device := range *d {
		if device.Pwr != 0 {
			devices = append(devices, device)
		}
	}
	// sort devices alphabetically by name
	sort.Slice(devices, func(i, j int) bool {
		return devices[i].Name < devices[j].Name
	})

	*d = devices
}

func searchDevices(d *[]meshcentral.Device) string {
	var options []string

	// Calculate max widths for padding
	maxNameLen := 0
	maxHostLen := 0
	for _, device := range *d {
		displayName := device.DisplayName
		if displayName == "" {
			displayName = device.Name
		}
		if len(displayName) > maxNameLen {
			maxNameLen = len(displayName)
		}
		if len(device.Name) > maxHostLen {
			maxHostLen = len(device.Name)
		}
	}

	// Ensure minimum column widths for headers
	if maxNameLen < 12 {
		maxNameLen = 12
	}
	if maxHostLen < 8 {
		maxHostLen = 8
	}

	// Create header for prompt
	header := fmt.Sprintf("\n     %-*s  %-*s  %s\n",
		maxNameLen, "NAME", maxHostLen, "HOSTNAME", "IP ADDRESS")

	for i, device := range *d {
		displayName := device.DisplayName
		hostname := device.Name

		if displayName == "" {
			displayName = device.Name
			hostname = ""
		}

		var line string
		if hostname != "" {
			line = fmt.Sprintf("%-3d  %-*s  %-*s  %s",
				i, maxNameLen, displayName, maxHostLen, hostname, device.IP)
		} else {
			line = fmt.Sprintf("%-3d  %-*s  %s",
				i, maxNameLen, displayName, device.IP)
		}

		options = append(options, line)
	}

	termHeight := pterm.GetTerminalHeight()
	maxHeight := termHeight - 7
	if maxHeight < 5 {
		maxHeight = 5
	}

	selectedOption, _ := pterm.DefaultInteractiveSelect.
		WithOptions(options).
		WithDefaultText("Select a device:" + header).
		WithMaxHeight(maxHeight).
		Show()

	index, _ := strconv.Atoi(strings.Fields(selectedOption)[0])
	nodeid := (*d)[index].Id

	return nodeid
}

func printDevices(d *[]meshcentral.Device) {
	listData := [][]string{}
	listData = append(listData, []string{"Name", "Hostname", "IP", "OS"})
	for _, device := range *d {
		displayName := device.DisplayName
		if displayName == "" {
			displayName = "-"
		}
		listData = append(listData, []string{
			displayName,
			device.Name,
			device.IP,
			device.OS,
		})
	}

	pterm.DefaultTable.WithHasHeader().WithData(listData).Render()
}
