package meshcentral

import (
	"context"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// File management runs over the agent's files channel (relay protocol 5),
// the one the web UI's Files tab and meshctrl upload/download use. The agent
// answers ls, download, upload and uploadhash, but mkdir, rm, rename and copy
// send no reply and swallow their errors, so those are checked with a List.

// FileEntry types as the agent reports them.
const (
	FileDrive = 1 // a drive, in the root listing of Windows agents
	FileDir   = 2
	FileFile  = 3
)

type FileEntry struct {
	Name string    `json:"name"`
	Type int       `json:"type"`
	Size int64     `json:"size"`           // file size, or drive size
	Free int64     `json:"free,omitempty"` // drives only
	Mod  time.Time `json:"mod"`
}

func (e FileEntry) IsDir() bool { return e.Type != FileFile }

// FileSession is one files channel to a device. Calls must not overlap.
type FileSession struct {
	conn   *websocket.Conn
	notice func(string)
	reqid  int
	broken bool
}

// fileReplyTimeout bounds the wait for any message from the agent, including
// the local user's answer when the device asks for consent.
const fileReplyTimeout = 90 * time.Second

// The agent answers download in 16 KB blocks, one per ack. downloadWindow
// acks are kept ahead of the data so the blocks stream instead of waiting a
// round trip each; acks past the end are ignored. uploadWindow is the number
// of uploadChunk frames in flight, the agent acks each once written. Both
// keep 4 MiB in flight: the speed through the relay is about what's in
// flight per round trip, 4 MiB covers 1 Gbit/s at 30 ms. The relay holds
// back a side that sends faster than the other reads, rather than buffer.
const (
	downloadWindow = 256
	uploadWindow   = 64
	uploadChunk    = 65535
)

type fileMsg struct {
	Action string `json:"action"`
	Sub    string `json:"sub"`
	ReqID  any    `json:"reqid"`
	ID     any    `json:"id"`
	Dir    *[]struct {
		N string          `json:"n"`
		T int             `json:"t"`
		S int64           `json:"s"`
		F int64           `json:"f"`
		D json.RawMessage `json:"d"`
	} `json:"dir"`
	Hash        *string `json:"hash"`
	CtrlChannel any     `json:"ctrlChannel"`
	Type        string  `json:"type"`
	Msg         *string `json:"msg"`
	MsgID       int     `json:"msgid"`
}

// OpenFiles opens a files channel to nodeID. notice (may be nil) gets the
// agent's status messages, such as waiting for the local user to allow access.
func OpenFiles(nodeID string, notice func(string)) (*FileSession, error) {
	conn, err := dialTunnel(nodeID, 5)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to server: %w", err)
	}
	s, err := NewFileSession(conn, notice)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return s, nil
}

// NewFileSession starts a files channel on a relay tunnel conn that was
// just dialed, waiting for the device to join it.
func NewFileSession(conn *websocket.Conn, notice func(string)) (*FileSession, error) {
	if notice == nil {
		notice = func(string) {}
	}
	s := &FileSession{conn: conn, notice: notice}
	for {
		conn.SetReadDeadline(time.Now().Add(fileReplyTimeout))
		_, b, err := conn.ReadMessage()
		if err != nil {
			return nil, fmt.Errorf("the device did not answer (offline, or no file access): %w", err)
		}
		if string(b) == "c" || string(b) == "cr" { // "cr" when the session is recorded
			if string(b) == "cr" {
				notice("The server records this session")
			}
			break
		}
	}
	if err := conn.WriteMessage(websocket.TextMessage, []byte("5")); err != nil {
		return nil, err
	}
	return s, nil
}

// Broken reports that the channel failed, every call fails from then on
// and a new session is needed. Other errors leave it usable.
func (s *FileSession) Broken() bool { return s.broken }

func (s *FileSession) Close() error {
	s.conn.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	return s.conn.Close()
}

func (s *FileSession) nextID() string {
	s.reqid++
	return "mcc" + strconv.Itoa(s.reqid)
}

func (s *FileSession) send(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.write(websocket.TextMessage, b)
}

func (s *FileSession) write(msgType int, b []byte) error {
	if err := s.conn.WriteMessage(msgType, b); err != nil {
		s.broken = true
		return fmt.Errorf("files channel closed: %w", err)
	}
	return nil
}

// read returns the next JSON message, or a data block. The agent sends JSON
// in binary frames too; data blocks never start with '{'.
func (s *FileSession) read() (*fileMsg, []byte, error) {
	for {
		s.conn.SetReadDeadline(time.Now().Add(fileReplyTimeout))
		_, b, err := s.conn.ReadMessage()
		if err != nil {
			s.broken = true
			return nil, nil, fmt.Errorf("files channel closed: %w", err)
		}
		if len(b) == 0 || b[0] != '{' {
			return nil, b, nil
		}
		var m fileMsg
		if json.Unmarshal(b, &m) != nil {
			continue
		}
		if m.CtrlChannel != nil {
			if m.Type == "console" && m.Msg != nil {
				if m.MsgID == 2 {
					s.broken = true
					return nil, nil, fmt.Errorf("file access refused on the device: %s", *m.Msg)
				}
				s.notice(*m.Msg)
			}
			continue
		}
		return &m, nil, nil
	}
}

// await skips data blocks and other replies until match accepts a message.
func (s *FileSession) await(match func(*fileMsg) bool) (*fileMsg, error) {
	for {
		m, _, err := s.read()
		if err != nil {
			return nil, err
		}
		if m != nil && match(m) {
			return m, nil
		}
	}
}

// List returns the entries of dir. An empty dir lists the drives on Windows
// agents and / elsewhere.
func (s *FileSession) List(dir string) ([]FileEntry, error) {
	id := s.nextID()
	if err := s.send(map[string]any{"action": "ls", "reqid": id, "path": dir}); err != nil {
		return nil, err
	}
	m, err := s.await(func(m *fileMsg) bool { return m.ReqID == id })
	if err != nil {
		return nil, err
	}
	if m.Dir == nil {
		return nil, fmt.Errorf("%s: not found or access denied", dir)
	}
	entries := make([]FileEntry, 0, len(*m.Dir))
	for _, d := range *m.Dir {
		entries = append(entries, FileEntry{Name: d.N, Type: d.T, Size: d.S, Free: d.F, Mod: parseFileTime(d.D)})
	}
	return entries, nil
}

// parseFileTime reads an entry's time, an ISO string from current agents or
// seconds since the epoch from older ones.
func parseFileTime(raw json.RawMessage) time.Time {
	var secs float64
	if json.Unmarshal(raw, &secs) == nil {
		return time.Unix(int64(secs), 0)
	}
	var str string
	if json.Unmarshal(raw, &str) == nil {
		if t, err := time.Parse(time.RFC3339Nano, str); err == nil {
			return t
		}
	}
	return time.Time{}
}

// Stat finds path in its folder's listing. Errors wrap fs.ErrNotExist when
// the folder exists but has no such entry.
func (s *FileSession) Stat(path string) (FileEntry, error) {
	if isRootPath(path) {
		return FileEntry{Name: path, Type: FileDir}, nil
	}
	dir, name := SplitPath(path)
	entries, err := s.List(dir)
	if err != nil {
		return FileEntry{}, err
	}
	if e, ok := findEntry(entries, name, isWindowsPath(path)); ok {
		return e, nil
	}
	return FileEntry{}, fmt.Errorf("%s: %w", path, fs.ErrNotExist)
}

// findEntry looks name up, ignoring case on Windows when nothing matches exactly.
func findEntry(entries []FileEntry, name string, foldCase bool) (FileEntry, bool) {
	for _, e := range entries {
		if e.Name == name {
			return e, true
		}
	}
	if foldCase {
		for _, e := range entries {
			if strings.EqualFold(e.Name, name) {
				return e, true
			}
		}
	}
	return FileEntry{}, false
}

func (s *FileSession) exists(path string) (bool, error) {
	_, err := s.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	return err == nil, err
}

// Download writes the file at path to w and checks it against the agent's
// hash of the file. progress (may be nil) gets the bytes written so far.
// The protocol has no offset, a broken download has to start over.
// Canceling ctx stops it after the block in hand.
func (s *FileSession) Download(ctx context.Context, path string, w io.Writer, progress func(int64)) error {
	id := s.nextID()
	if err := s.send(map[string]any{"action": "download", "sub": "start", "id": id, "path": path}); err != nil {
		return err
	}
	m, err := s.await(func(m *fileMsg) bool { return m.Action == "download" && m.ID == id })
	if err != nil {
		return err
	}
	if m.Sub != "start" {
		return fmt.Errorf("%s: the agent could not open the file", path)
	}
	ack := map[string]any{"action": "download", "sub": "ack", "id": id}
	if err := s.send(map[string]any{"action": "download", "sub": "startack", "id": id}); err != nil {
		return err
	}
	for range downloadWindow - 1 {
		if err := s.send(ack); err != nil {
			return err
		}
	}

	h := sha512.New384()
	var n int64
	for {
		// Blocks still in flight after a stop are skipped by the next
		// call, they come before any reply to it.
		if err := ctx.Err(); err != nil {
			s.send(map[string]any{"action": "download", "sub": "stop", "id": id})
			return err
		}
		m, b, err := s.read()
		if err != nil {
			return err
		}
		if m != nil {
			if m.Action == "download" && m.Sub == "cancel" && m.ID == id {
				return fmt.Errorf("%s: download canceled by the agent", path)
			}
			continue
		}
		if len(b) < 4 {
			continue
		}
		if _, err := w.Write(b[4:]); err != nil {
			s.send(map[string]any{"action": "download", "sub": "stop", "id": id})
			return err
		}
		h.Write(b[4:])
		n += int64(len(b) - 4)
		if progress != nil {
			progress(n)
		}
		if b[3]&1 != 0 { // end flag
			break
		}
		if err := s.send(ack); err != nil {
			return err
		}
	}
	return s.checkHash(path, h.Sum(nil), "file changed during the download?")
}

// Upload writes r to path on the device, replacing any file there, and
// checks the result against the agent's hash of it. progress (may be nil)
// gets the bytes the agent wrote so far. Canceling ctx stops it and has the
// agent delete the partial file.
func (s *FileSession) Upload(ctx context.Context, path string, r io.Reader, progress func(int64)) error {
	id := s.nextID()
	if err := s.send(map[string]any{"action": "upload", "reqid": id, "path": path}); err != nil {
		return err
	}

	h := sha512.New384()
	buf := make([]byte, 1+uploadChunk)
	var written int64
	var inFlight []int // sizes of the frames not acked yet
	eof, done := false, false
	for {
		m, err := s.await(func(m *fileMsg) bool {
			return m.ReqID == id || (m.Action == "uploaderror" && m.ReqID == nil)
		})
		if err != nil {
			return err
		}
		switch m.Action {
		case "uploaderror":
			return fmt.Errorf("%s: the agent could not write the file", path)
		case "uploaddone":
			return s.checkHash(path, h.Sum(nil), "file changed during the upload?")
		case "uploadstart", "uploadack":
		default:
			continue
		}
		if err := ctx.Err(); err != nil {
			s.send(map[string]any{"action": "uploadcancel", "reqid": id})
			return err
		}
		frames := 1
		if m.Action == "uploadstart" {
			frames = uploadWindow
		} else if len(inFlight) > 0 {
			written += int64(inFlight[0])
			inFlight = inFlight[1:]
			if progress != nil {
				progress(written)
			}
		}
		for ; frames > 0 && !eof; frames-- {
			k, err := io.ReadFull(r, buf[1:])
			if k > 0 {
				// The agent drops a leading 0, which escapes data that
				// would otherwise start with 0 or look like JSON.
				frame := buf[1 : 1+k]
				if frame[0] == 0 || frame[0] == '{' {
					buf[0] = 0
					frame = buf[:1+k]
				}
				if err := s.write(websocket.BinaryMessage, frame); err != nil {
					return err
				}
				h.Write(buf[1 : 1+k])
				inFlight = append(inFlight, k)
			}
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				eof = true
			} else if err != nil {
				s.send(map[string]any{"action": "uploadcancel", "reqid": id})
				return err
			}
		}
		if eof && len(inFlight) == 0 && !done {
			if err := s.send(map[string]any{"action": "uploaddone", "reqid": id}); err != nil {
				return err
			}
			done = true
		}
	}
}

// Hash returns the agent's SHA-384 of the file at path, in hex.
func (s *FileSession) Hash(path string) (string, error) {
	id := s.nextID()
	if err := s.send(map[string]any{"action": "uploadhash", "reqid": id, "path": path}); err != nil {
		return "", err
	}
	m, err := s.await(func(m *fileMsg) bool { return m.Action == "uploadhash" && m.ReqID == id })
	if err != nil {
		return "", err
	}
	if m.Hash == nil {
		return "", fmt.Errorf("%s: the agent could not read the file", path)
	}
	return *m.Hash, nil
}

func (s *FileSession) checkHash(path string, local []byte, hint string) error {
	remote, err := s.Hash(path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(remote, hex.EncodeToString(local)) {
		return fmt.Errorf("%s: checksum mismatch (%s)", path, hint)
	}
	return nil
}

// Mkdir creates the folder path, whose parent must exist.
func (s *FileSession) Mkdir(path string) error {
	// The agent's mkdir fails silently, or throws if path exists.
	if ok, err := s.exists(path); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s: %w", path, fs.ErrExist)
	}
	if err := s.send(map[string]any{"action": "mkdir", "reqid": s.nextID(), "path": path}); err != nil {
		return err
	}
	if e, err := s.Stat(path); err != nil || !e.IsDir() {
		return fmt.Errorf("%s: the agent could not create the folder", path)
	}
	return nil
}

// Remove deletes path. A folder needs recursive unless it is empty.
func (s *FileSession) Remove(path string, recursive bool) error {
	if isRootPath(path) {
		return fmt.Errorf("%s: refusing to remove a root", path)
	}
	e, err := s.Stat(path)
	if err != nil {
		return err
	}
	dir, _ := SplitPath(path)
	if err := s.send(map[string]any{"action": "rm", "reqid": s.nextID(), "path": dir, "delfiles": []string{e.Name}, "rec": recursive}); err != nil {
		return err
	}
	if ok, err := s.exists(path); err != nil {
		return err
	} else if ok {
		if e.IsDir() && !recursive {
			return fmt.Errorf("%s: the agent could not remove it (not empty?)", path)
		}
		return fmt.Errorf("%s: the agent could not remove it", path)
	}
	return nil
}

// Rename moves from to the new path to, which must not exist. Both have to
// be on the same drive.
func (s *FileSession) Rename(from, to string) error {
	fromRoot, fromRest := splitRoot(from)
	toRoot, toRest := splitRoot(to)
	if !strings.EqualFold(fromRoot, toRoot) {
		return fmt.Errorf("%s: cannot move to another drive", from)
	}
	if err := s.checkMove(from, to); err != nil {
		return err
	}
	// The agent joins path with both names, so naming them from the root
	// lets a rename move between folders too.
	if err := s.send(map[string]any{"action": "rename", "reqid": s.nextID(), "path": fromRoot, "oldname": fromRest, "newname": toRest}); err != nil {
		return err
	}
	if ok, err := s.exists(to); err != nil {
		return err
	} else if !ok {
		return fmt.Errorf("%s: the agent could not move it", from)
	}
	return nil
}

// Copy copies the file from to the new path to, which must not exist.
func (s *FileSession) Copy(from, to string) error {
	e, err := s.Stat(from)
	if err != nil {
		return err
	}
	if e.IsDir() {
		return fmt.Errorf("%s: the agent can only copy files", from)
	}
	if err := s.checkMove(from, to); err != nil {
		return err
	}
	fromDir, _ := SplitPath(from)
	toDir, toName := SplitPath(to)
	if toName == e.Name {
		return s.copyInto(fromDir, toDir, e)
	}
	// The agent only copies a name into another folder, so a new name goes
	// through a scratch folder next to the target.
	id, _ := randomHex()
	tmpDir := JoinPath(toDir, ".mcc-copy-"+id)
	if err := s.Mkdir(tmpDir); err != nil {
		return err
	}
	defer s.Remove(tmpDir, true)
	if err := s.copyInto(fromDir, tmpDir, e); err != nil {
		return err
	}
	return s.Rename(JoinPath(tmpDir, e.Name), to)
}

func (s *FileSession) copyInto(fromDir, toDir string, e FileEntry) error {
	if err := s.send(map[string]any{"action": "copy", "reqid": s.nextID(), "scpath": fromDir, "dspath": toDir, "names": []string{e.Name}}); err != nil {
		return err
	}
	if c, err := s.Stat(JoinPath(toDir, e.Name)); err != nil || c.Size != e.Size {
		return fmt.Errorf("%s: the agent could not copy it", JoinPath(fromDir, e.Name))
	}
	return nil
}

func (s *FileSession) checkMove(from, to string) error {
	if _, err := s.Stat(from); err != nil {
		return err
	}
	if ok, err := s.exists(to); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%s: %w", to, fs.ErrExist)
	}
	return nil
}

// Remote paths take either separator, Windows agents accept both.

func isWindowsPath(p string) bool {
	return len(p) >= 2 && p[1] == ':' && (p[0]|0x20) >= 'a' && (p[0]|0x20) <= 'z'
}

func isRootPath(p string) bool {
	p = strings.TrimRight(p, `/\`)
	return p == "" || (len(p) == 2 && isWindowsPath(p))
}

// SplitPath splits a remote path into its folder and name.
func SplitPath(p string) (dir, name string) {
	p = strings.TrimRight(p, `/\`)
	i := strings.LastIndexAny(p, `/\`)
	if i < 0 {
		return "", p
	}
	dir = p[:i]
	if dir == "" {
		dir = "/"
	}
	return dir, p[i+1:]
}

// JoinPath appends name to a remote folder, with the folder's separator.
func JoinPath(dir, name string) string {
	sep := "/"
	if strings.Contains(dir, `\`) || isWindowsPath(dir) {
		sep = `\`
	}
	return strings.TrimRight(dir, `/\`) + sep + name
}

// splitRoot splits p into its drive ("C:") or "/", and the rest.
func splitRoot(p string) (root, rest string) {
	if isWindowsPath(p) {
		return p[:2], strings.TrimLeft(p[2:], `/\`)
	}
	return "/", strings.TrimLeft(p, "/")
}
