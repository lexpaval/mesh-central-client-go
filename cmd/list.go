package cmd

import (
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

	for i, device := range *d {
		istr := strconv.Itoa(i)
		options = append(options, istr+" "+device.Name+" ("+device.IP+")")
	}

	// Get terminal height and set max selection height
	termHeight := pterm.GetTerminalHeight()
	maxHeight := termHeight - 5 // Leave space for prompt and borders
	if maxHeight < 5 {
		maxHeight = 5
	}

	selectedOption, _ := pterm.DefaultInteractiveSelect.
		WithOptions(options).
		WithMaxHeight(maxHeight).
		Show()

	index, _ := strconv.Atoi(strings.Split(selectedOption, " ")[0])

	nodeid := (*d)[index].Id

	return nodeid
}

func printDevices(d *[]meshcentral.Device) {
	// print devices
	listData := [][]string{}
	listData = append(listData, []string{"Hostname", "Connect IP", "OS"})
	for _, device := range *d {
		listData = append(listData, []string{
			device.Name,
			device.IP,
			device.OS,
		})
	}

	pterm.DefaultTable.WithHasHeader().WithData(listData).Render()
}
