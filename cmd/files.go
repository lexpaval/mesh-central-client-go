package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/pterm/pterm"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/lexpaval/mesh-central-client-go/internal/meshcentral"
)

var filesCmd = &cobra.Command{
	Use:   "files",
	Short: "List, transfer and manage files on a node",
	Long: `Works over the agent's file channel, like the Files tab of the web UI.
Remote paths are absolute: /home/user on Linux and macOS, C:\Users on Windows.
Leave out -i to pick the device, and a remote path to browse for it.`,
}

var filesLsCmd = &cobra.Command{
	Use:   "ls [path]",
	Short: "List a remote folder (the drives on Windows if no path)",
	Args:  cobra.MaximumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		path := ""
		if len(args) == 1 {
			path = args[0]
		}
		asJSON, _ := cmd.Flags().GetBool("json")
		s := openFiles(cmd, false)
		entries, err := s.List(path)
		if err != nil {
			filesFail(err)
		}
		sortEntries(entries)
		if asJSON {
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			enc.Encode(entries)
		} else {
			for _, e := range entries {
				fmt.Println(formatEntry(e))
			}
		}
		closeFiles(s)
	},
}

var filesGetCmd = &cobra.Command{
	Use:   "get [remote]... [local|-]",
	Short: "Download files (into the current folder, a local folder, a file, or - for stdout)",
	Run: func(cmd *cobra.Command, args []string) {
		remotes, local := args, "."
		if len(args) > 1 {
			remotes, local = args[:len(args)-1], args[len(args)-1]
		}
		if local == "-" && len(remotes) > 1 {
			filesFail(errors.New("only one file can go to stdout"))
		}
		localDir := false
		if st, err := os.Stat(local); err == nil && st.IsDir() {
			localDir = true
		} else if len(remotes) > 1 {
			filesFail(fmt.Errorf("%s: not a folder", local))
		}

		s := openFiles(cmd, local == "-")
		if len(remotes) == 0 {
			remotes = []string{pickRemote(s, "Download", "", true, false)}
		}
		for _, remote := range remotes {
			e, err := s.Stat(remote)
			if err != nil {
				filesFail(err)
			}
			if e.IsDir() {
				filesFail(fmt.Errorf("%s: is a folder", remote))
			}
			if local == "-" {
				if err := s.Download(remote, os.Stdout, nil); err != nil {
					filesFail(err)
				}
				continue
			}
			target := local
			if localDir {
				target = filepath.Join(local, e.Name)
			}
			if err := download(s, remote, target, e.Size); err != nil {
				filesFail(err)
			}
		}
		closeFiles(s)
	},
}

func download(s *meshcentral.FileSession, remote, target string, size int64) error {
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	p := newProgress(filepath.Base(target), size)
	err = s.Download(remote, f, p.update)
	p.done()
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(target)
	}
	return err
}

var filesPutCmd = &cobra.Command{
	Use:   "put <local|->... [remote]",
	Short: "Upload files (into a remote folder, or as a remote file; - reads stdin)",
	Args:  cobra.MinimumNArgs(1),
	Run: func(cmd *cobra.Command, args []string) {
		locals, remote := args[:len(args)-1], args[len(args)-1]
		if len(args) == 1 {
			if args[0] == "-" {
				filesFail(errors.New("give the remote file to write stdin to"))
			}
			locals, remote = args, ""
		}
		for _, l := range locals {
			if l == "-" {
				if len(locals) > 1 {
					filesFail(errors.New("stdin can only be uploaded alone"))
				}
				continue
			}
			st, err := os.Stat(l)
			if err != nil {
				filesFail(err)
			}
			if st.IsDir() {
				filesFail(fmt.Errorf("%s: is a folder", l))
			}
		}

		s := openFiles(cmd, false)
		if remote == "" {
			remote = pickRemote(s, "Upload into", "", false, true)
		}
		e, err := s.Stat(remote)
		remoteDir := err == nil && e.IsDir()
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			filesFail(err)
		}
		if !remoteDir && len(locals) > 1 {
			filesFail(fmt.Errorf("%s: not a folder", remote))
		}
		for _, l := range locals {
			if l == "-" {
				if remoteDir {
					filesFail(fmt.Errorf("%s: is a folder, give the file name to write", remote))
				}
				if err := s.Upload(remote, os.Stdin, nil); err != nil {
					filesFail(err)
				}
				continue
			}
			target := remote
			if remoteDir {
				target = meshcentral.JoinPath(remote, filepath.Base(l))
			}
			if err := upload(s, l, target); err != nil {
				filesFail(err)
			}
		}
		closeFiles(s)
	},
}

func upload(s *meshcentral.FileSession, local, target string) error {
	f, err := os.Open(local)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	p := newProgress(filepath.Base(local), st.Size())
	err = s.Upload(target, f, p.update)
	p.done()
	return err
}

var filesMkdirCmd = &cobra.Command{
	Use:   "mkdir [path]...",
	Short: "Create remote folders",
	Run: func(cmd *cobra.Command, args []string) {
		s := openFiles(cmd, false)
		if len(args) == 0 {
			dir := pickRemote(s, "Create a folder in", "", false, true)
			name, _ := pterm.DefaultInteractiveTextInput.Show("Folder name")
			if name == "" {
				filesFail(errors.New("no folder name given"))
			}
			args = []string{meshcentral.JoinPath(dir, name)}
		}
		for _, p := range args {
			if err := s.Mkdir(p); err != nil {
				filesFail(err)
			}
		}
		closeFiles(s)
	},
}

var filesRmCmd = &cobra.Command{
	Use:   "rm [path]...",
	Short: "Delete remote files, or folders with -r",
	Run: func(cmd *cobra.Command, args []string) {
		recursive, _ := cmd.Flags().GetBool("recursive")
		s := openFiles(cmd, false)
		if len(args) == 0 {
			p := pickRemote(s, "Delete", "", true, true)
			e, err := s.Stat(p)
			if err != nil {
				filesFail(err)
			}
			question := fmt.Sprintf("Delete %s?", p)
			if e.IsDir() {
				question = fmt.Sprintf("Delete %s and everything in it?", p)
				recursive = true
			}
			if ok, _ := pterm.DefaultInteractiveConfirm.Show(question); !ok {
				closeFiles(s)
				return
			}
			args = []string{p}
		}
		for _, p := range args {
			if err := s.Remove(p, recursive); err != nil {
				filesFail(err)
			}
		}
		closeFiles(s)
	},
}

var filesMvCmd = &cobra.Command{
	Use:   "mv [source]... [target]",
	Short: "Move or rename remote files and folders",
	Run: func(cmd *cobra.Command, args []string) {
		s := openFiles(cmd, false)
		forEachTarget(s, pickSourceTarget(s, args, "Move", true), s.Rename)
		closeFiles(s)
	},
}

var filesCpCmd = &cobra.Command{
	Use:   "cp [source]... [target]",
	Short: "Copy remote files (the agent can't copy folders)",
	Run: func(cmd *cobra.Command, args []string) {
		s := openFiles(cmd, false)
		forEachTarget(s, pickSourceTarget(s, args, "Copy", false), s.Copy)
		closeFiles(s)
	},
}

// pickSourceTarget browses for the source and target folder of mv and cp
// when left out, the target starting from the source's folder.
func pickSourceTarget(s *meshcentral.FileSession, args []string, verb string, dirs bool) []string {
	if len(args) == 0 {
		args = []string{pickRemote(s, verb, "", true, dirs)}
	}
	if len(args) == 1 {
		from, _ := meshcentral.SplitPath(args[0])
		args = append(args, pickRemote(s, verb+" "+args[0]+" into", from, false, true))
	}
	return args
}

// forEachTarget runs op from each source to the last arg, or into it when
// it's an existing folder, which it must be for several sources.
func forEachTarget(s *meshcentral.FileSession, args []string, op func(from, to string) error) {
	sources, target := args[:len(args)-1], args[len(args)-1]
	e, err := s.Stat(target)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		filesFail(err)
	}
	intoDir := err == nil && e.IsDir()
	if !intoDir && len(sources) > 1 {
		filesFail(fmt.Errorf("%s: not a folder", target))
	}
	for _, src := range sources {
		to := target
		if intoDir {
			_, name := meshcentral.SplitPath(src)
			to = meshcentral.JoinPath(target, name)
		}
		if err := op(src, to); err != nil {
			filesFail(err)
		}
	}
}

// openFiles resolves the node like the other commands and opens a files
// channel. toStdout means stdout carries file data, so there is no device
// picker to draw on it.
func openFiles(cmd *cobra.Command, toStdout bool) *meshcentral.FileSession {
	nodeID, _ := cmd.Flags().GetString("nodeid")
	debug, _ := cmd.Flags().GetBool("debug")
	insecure, _ := cmd.Flags().GetBool("insecure")
	if toStdout && nodeID == "" {
		fmt.Fprintln(os.Stderr, "-i is required when writing to stdout")
		os.Exit(1)
	}

	nodeID = resolveNodeID(nodeID, insecure, debug)
	s, err := meshcentral.OpenFiles(nodeID, func(msg string) { fmt.Fprintln(os.Stderr, msg) })
	if err != nil {
		filesFail(err)
	}
	return s
}

func closeFiles(s *meshcentral.FileSession) {
	s.Close()
	meshcentral.StopSocket()
}

func filesFail(err error) {
	fmt.Fprintln(os.Stderr, err)
	meshcentral.StopSocket()
	os.Exit(1)
}

func formatEntry(e meshcentral.FileEntry) string {
	switch e.Type {
	case meshcentral.FileDrive:
		return fmt.Sprintf("drive  %7s  %7s free  %s", humanSize(e.Size), humanSize(e.Free), e.Name)
	case meshcentral.FileDir:
		return fmt.Sprintf("dir    %7s  %s  %s/", "", formatTime(e.Mod), e.Name)
	default:
		return fmt.Sprintf("file   %7s  %s  %s", humanSize(e.Size), formatTime(e.Mod), e.Name)
	}
}

func formatTime(t time.Time) string {
	if t.IsZero() {
		return fmt.Sprintf("%16s", "")
	}
	return t.Local().Format("2006-01-02 15:04")
}

func humanSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// progress draws a transfer's progress on stderr when it's a terminal.
type progress struct {
	name  string
	total int64
	last  time.Time
	on    bool
}

func newProgress(name string, total int64) *progress {
	return &progress{name: name, total: total, on: term.IsTerminal(int(os.Stderr.Fd()))}
}

func (p *progress) update(n int64) {
	if !p.on || (time.Since(p.last) < 100*time.Millisecond && n != p.total) {
		return
	}
	p.last = time.Now()
	line := fmt.Sprintf("%s  %s", p.name, humanSize(n))
	if p.total > 0 {
		line += fmt.Sprintf(" / %s  %d%%", humanSize(p.total), n*100/p.total)
	}
	fmt.Fprintf(os.Stderr, "\r\033[K%s", line)
}

func (p *progress) done() {
	if p.on && !p.last.IsZero() {
		fmt.Fprintln(os.Stderr)
	}
}

func init() {
	rootCmd.AddCommand(filesCmd)
	filesCmd.AddCommand(filesLsCmd, filesGetCmd, filesPutCmd, filesMkdirCmd, filesRmCmd, filesMvCmd, filesCpCmd)

	filesCmd.PersistentFlags().StringP("nodeid", "i", "", "Mesh Central Node ID")
	filesCmd.PersistentFlags().BoolP("insecure", "k", false, "Skip TLS certificate verification (insecure, for testing only)")
	filesCmd.PersistentFlags().BoolP("debug", "", false, "Enable debug logging")
	filesLsCmd.Flags().Bool("json", false, "Print the entries as JSON")
	filesRmCmd.Flags().BoolP("recursive", "r", false, "Delete folders and everything in them")
}
