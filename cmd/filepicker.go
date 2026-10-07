package cmd

import (
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/pterm/pterm"
	"golang.org/x/term"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
	"github.com/lexpaval/mesh-central-client-go/internal/progress"
)

// pickRemote browses the device's folders from start ("" for the root) with
// the same picker as the device search, for a path left out on the command
// line. Enter on a folder opens it. It returns the file picked, or with
// wantDir the folder picked through its "./" entry.
func pickRemote(s *meshcentral.FileSession, prompt, start string, wantFile, wantDir bool) string {
	if !term.IsTerminal(int(os.Stdin.Fd())) {
		filesFail(errors.New("missing path, and no terminal to pick one"))
	}
	type choice struct {
		path string
		open bool
	}
	cur := start
	for {
		entries, err := s.List(cur)
		if err != nil {
			filesFail(err)
		}
		drives := len(entries) > 0 && entries[0].Type == meshcentral.FileDrive // Windows root
		if cur == "" && !drives {
			cur = "/"
		}
		sortEntries(entries)
		width := 0
		for _, e := range entries {
			width = max(width, len(e.Name)+1)
		}

		var options []string
		choices := map[string]choice{}
		add := func(label string, c choice) {
			options = append(options, label)
			choices[label] = c
		}
		if wantDir && !drives {
			add("./  (this folder)", choice{cur, false})
		}
		if !drives && cur != "/" {
			parent, _ := meshcentral.SplitPath(cur)
			add("../", choice{parent, true})
		}
		for _, e := range entries {
			p := e.Name
			if !drives {
				p = meshcentral.JoinPath(cur, e.Name)
			}
			switch {
			case e.Type == meshcentral.FileDrive:
				add(fmt.Sprintf("%-*s  %s free", width, e.Name, progress.Size(e.Free)), choice{p, true})
			case e.IsDir():
				add(e.Name+"/", choice{p, true})
			case wantFile:
				add(fmt.Sprintf("%-*s  %s", width, e.Name, progress.Size(e.Size)), choice{p, false})
			}
		}
		if len(options) == 0 {
			filesFail(fmt.Errorf("%s: nothing to pick", cur))
		}

		where := cur
		if drives {
			where = "drives"
		}
		selected, err := pterm.DefaultInteractiveSelect.
			WithOptions(options).
			WithDefaultText(fmt.Sprintf("%s (%s)", prompt, where)).
			WithMaxHeight(max(5, pterm.GetTerminalHeight()-6)).
			Show()
		if err != nil {
			filesFail(err)
		}
		c := choices[selected]
		if !c.open {
			return c.path
		}
		cur = c.path
	}
}

// sortEntries puts folders first, then sorts by name.
func sortEntries(entries []meshcentral.FileEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name < entries[j].Name
	})
}
