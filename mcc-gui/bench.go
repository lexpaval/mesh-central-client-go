package main

import (
	"os"
	"strconv"
)

// benchShells is set from MCC_GUI_BENCH for tools/guibench, -1 without it:
// the GUI then connects with the selected profile at start, skipping TLS
// verification, and opens that many shells once the devices are in.
var benchShells = func() int {
	if n, err := strconv.Atoi(os.Getenv("MCC_GUI_BENCH")); err == nil {
		return n
	}
	return -1
}()

func benchStart() {
	if benchShells >= 0 {
		connect(true)
	}
}

// benchLoaded opens the shells on the first devices shown.
func benchLoaded() {
	n := benchShells
	for _, g := range groupOrder {
		for _, id := range groupChildren[g] {
			if n <= 0 {
				return
			}
			if d, ok := shownDevice(id); ok {
				openShell(d, 1)
				n--
			}
		}
	}
}
