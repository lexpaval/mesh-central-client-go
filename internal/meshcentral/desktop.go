package meshcentral

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// Remote desktop runs over the agent's KVM channel (relay protocol 2), the
// one the web UI's Desktop tab uses. Both ways it carries binary commands, a
// 2-byte command and a 2-byte length (header included), big-endian. Each
// websocket message starts a command, one longer than the message continues
// in the next ones. Commands over 64 KB come as a jumbo command (27, length
// 8) whose 4 bytes after the header are the length of the command that
// follows. The screen arrives as JPEG tiles, input goes back as Windows
// virtual-key codes, UTF-16 units and mouse events in screen pixels.

const (
	kvmKey         = 1
	kvmMouse       = 2
	kvmTile        = 3
	kvmCompression = 5
	kvmScreen      = 7
	kvmPause       = 8
	kvmCtrlAltDel  = 10
	kvmGetDisplays = 11
	kvmSetDisplay  = 12
	kvmInitTouch   = 14
	kvmMessage     = 17
	kvmJumbo       = 27
	kvmAlert       = 65
	kvmInputLock   = 87
	kvmCursor      = 88
	kvmKeyUnicode  = 85
)

// AllDisplays is the display ID of all screens as one.
const AllDisplays = 65535

// Mouse buttons as the agent takes them, the release of each is twice its
// press.
const (
	MouseLeft   = 0x02
	MouseRight  = 0x08
	MouseMiddle = 0x20
)

// DesktopEvents are called from the session's reader goroutine, any may be
// nil.
type DesktopEvents struct {
	// Screen reports a new screen size, the frame is cleared.
	Screen func(w, h int)
	// Frame reports that r of the frame changed.
	Frame func(r image.Rectangle)
	// Displays lists the device's display IDs and the one shown, once
	// asked for with GetDisplays.
	Displays func(ids []int, selected int)
	// Notice gets the agent's status messages, such as waiting for the
	// local user to allow access.
	Notice func(string)
	// Cursor is the remote mouse cursor's shape, the agent's index: default,
	// progress, crosshair, pointer, help, text, no-drop, move, nesw-resize,
	// ns-resize, nwse-resize, w-resize, alias, wait, none, not-allowed,
	// col-resize, row-resize, copy, zoom-in, zoom-out as CSS names them.
	Cursor func(int)
}

// Desktop is one remote desktop session. Input methods may be called from
// any goroutine and don't block on the network.
type Desktop struct {
	conn    *websocket.Conn
	ev      DesktopEvents
	out     chan []byte
	done    chan struct{}
	err     error // set before done closes
	closing atomic.Bool

	mu      sync.Mutex
	w, h    int
	pix     []byte // w*h BGRX pixels, QImage's RGB32
	quality int
	frameMs int
	started bool // the screen size arrived, input goes through

	// The reader's partial command, while a long one spans messages.
	acc     []byte
	accSize int
	accCmd  int
}

// desktopJoinTimeout bounds the wait for the device to join the tunnel.
const desktopJoinTimeout = 90 * time.Second

// The device controls what it sends, these bound what that may cost here: a
// command (a JPEG tile mostly), the screen (64 Mpx is four 4K displays) and
// the displays listed.
const (
	maxDesktopCommand  = 16 << 20
	maxDesktopPixels   = 64 << 20
	maxDesktopDisplays = 64
)

// desktopTileSlack is how far a tile may reach past the screen. Agents
// capture into a frame padded to their 32 pixel tile grid and send whole
// tiles, so edge tiles cover some padding, which blit leaves out.
const desktopTileSlack = 256

// OpenDesktop opens a remote desktop session to nodeID.
func OpenDesktop(nodeID string, ev DesktopEvents) (*Desktop, error) {
	conn, err := dialTunnel(nodeID, 2)
	if err != nil {
		return nil, fmt.Errorf("unable to connect to server: %w", err)
	}
	d, err := NewDesktop(conn, ev)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return d, nil
}

// NewDesktop starts a desktop session on a relay tunnel conn that was just
// dialed, waiting for the device to join it.
func NewDesktop(conn *websocket.Conn, ev DesktopEvents) (*Desktop, error) {
	d := &Desktop{conn: conn, ev: ev, out: make(chan []byte, 1024), done: make(chan struct{}), quality: 50, frameMs: 100}
	conn.SetReadLimit(maxTunnelMessage) // dialTunnel's, for conns dialed elsewhere (tests)
	if err := joinTunnel(conn, 2, desktopJoinTimeout, "desktop", d.notice); err != nil {
		return nil, err
	}
	conn.SetReadDeadline(time.Time{})
	go d.write()
	go d.read()
	return d, nil
}

func (d *Desktop) notice(s string) {
	if d.ev.Notice != nil {
		d.ev.Notice(s)
	}
}

// Done is closed when the session ends, Err then tells why.
func (d *Desktop) Done() <-chan struct{} { return d.done }

// Err is nil when the device closed the session normally.
func (d *Desktop) Err() error {
	<-d.done
	return d.err
}

// Close ends the session.
func (d *Desktop) Close() {
	d.closing.Store(true)
	d.conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, `{"ctrlChannel":"102938","type":"close"}`), time.Now().Add(time.Second))
	d.conn.Close()
	<-d.done
}

// Frame runs f with the frame's pixels, w*h BGRX (QImage's RGB32 on little
// endian), which f must not keep. The reader waits meanwhile.
func (d *Desktop) Frame(f func(pix []byte, w, h int)) {
	d.mu.Lock()
	defer d.mu.Unlock()
	f(d.pix, d.w, d.h)
}

// Size is the remote screen's size, 0 until it's known.
func (d *Desktop) Size() (w, h int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.w, d.h
}

func (d *Desktop) write() {
	ping := time.NewTicker(10 * time.Second)
	defer ping.Stop()
	for {
		var err error
		select {
		case <-d.done:
			return
		case b := <-d.out:
			if len(b) > 0 && b[0] == '{' {
				err = d.conn.WriteMessage(websocket.TextMessage, b)
			} else {
				err = d.conn.WriteMessage(websocket.BinaryMessage, b)
			}
		case <-ping.C:
			err = d.conn.WriteMessage(websocket.TextMessage, fmt.Appendf(nil, `{"ctrlChannel":102938,"type":"rtt","time":%d}`, time.Now().UnixMilli()))
		}
		if err != nil {
			d.conn.Close() // ends the reader
			return
		}
	}
}

// send queues b for the device. Droppable commands (mouse moves) are left
// out when the queue is full, the others wait for room.
func (d *Desktop) send(b []byte, droppable bool) {
	if droppable {
		select {
		case d.out <- b:
		default:
		}
		return
	}
	select {
	case d.out <- b:
	case <-d.done:
	}
}

func kvmCmd(cmd int, payload ...byte) []byte {
	b := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(b, uint16(cmd))
	binary.BigEndian.PutUint16(b[2:], uint16(4+len(payload)))
	return append(b, payload...)
}

// be16 is vs as big-endian 16-bit values.
func be16(vs ...int) []byte {
	b := make([]byte, 0, 2*len(vs))
	for _, v := range vs {
		b = binary.BigEndian.AppendUint16(b, uint16(v))
	}
	return b
}

func (d *Desktop) read() {
	defer close(d.done)
	defer d.conn.Close()
	for {
		mt, b, err := d.conn.ReadMessage()
		if err != nil {
			if !d.closing.Load() && !websocket.IsCloseError(err, websocket.CloseNormalClosure, websocket.CloseGoingAway, websocket.CloseNoStatusReceived) {
				d.err = err
			}
			return
		}
		if mt == websocket.TextMessage {
			d.control(b)
			continue
		}
		if err := d.data(b); err != nil {
			d.err = err
			return
		}
	}
}

// control handles the tunnel's JSON messages.
func (d *Desktop) control(b []byte) {
	var m struct {
		CtrlChannel any     `json:"ctrlChannel"`
		Type        string  `json:"type"`
		Msg         *string `json:"msg"`
	}
	if json.Unmarshal(b, &m) != nil || fmt.Sprint(m.CtrlChannel) != "102938" {
		return
	}
	switch m.Type {
	case "console":
		if m.Msg != nil {
			d.notice(*m.Msg)
		}
	case "ping":
		d.send([]byte(`{"ctrlChannel":"102938","type":"pong"}`), false)
	}
}

// data handles one binary message, as the web UI's viewer does. An error
// ends the session.
func (d *Desktop) data(b []byte) error {
	if d.acc != nil {
		d.acc = append(d.acc, b...)
		if len(d.acc) >= d.accSize {
			cmd, v := d.accCmd, d.acc[:d.accSize]
			d.acc = nil
			return d.command(cmd, v)
		}
		return nil
	}
	if len(b) < 4 {
		return nil
	}
	cmd, size := int(binary.BigEndian.Uint16(b)), int(binary.BigEndian.Uint16(b[2:]))
	if cmd == kvmJumbo && size == 8 {
		if len(b) < 12 {
			return nil
		}
		size = int(binary.BigEndian.Uint32(b[4:]))
		b = b[8:]
		cmd = int(binary.BigEndian.Uint16(b))
	}
	if size < 4 {
		return nil
	}
	if size > maxDesktopCommand {
		return fmt.Errorf("the device sent a %d MB command, over the %d MB limit", size>>20, maxDesktopCommand>>20)
	}
	if size > len(b) {
		d.acc, d.accSize, d.accCmd = append(make([]byte, 0, size), b...), size, cmd
		return nil
	}
	return d.command(cmd, b[:size])
}

// command handles a whole command v, its header included.
func (d *Desktop) command(cmd int, v []byte) error {
	xy := func() (int, int) { return int(binary.BigEndian.Uint16(v[4:])), int(binary.BigEndian.Uint16(v[6:])) }
	switch cmd {
	case kvmScreen:
		if len(v) < 8 {
			return nil
		}
		w, h := xy()
		if w*h > maxDesktopPixels {
			return fmt.Errorf("the device's %d × %d screen is over the %d megapixel limit", w, h, maxDesktopPixels>>20)
		}
		d.mu.Lock()
		changed := w != d.w || h != d.h
		if changed {
			d.w, d.h = w, h
			d.pix = make([]byte, w*h*4)
			for i := 3; i < len(d.pix); i += 4 {
				d.pix[i] = 0xff
			}
		}
		first := !d.started
		d.started = true
		quality, frameMs := d.quality, d.frameMs
		d.mu.Unlock()
		if changed {
			d.send(compression(quality, frameMs), false)
			d.send(kvmCmd(kvmPause, 0), false)
			d.send(kvmCmd(kvmInputLock, 2), false) // asks for the input lock state
		}
		// Releases the modifiers, they may be down from before a switch.
		for _, vk := range []byte{16, 17, 18, 91, 92} {
			d.send(kvmCmd(kvmKey, 1, vk), false)
		}
		d.send(kvmCmd(kvmInitTouch), false)
		if first {
			d.GetDisplays()
		}
		if changed && d.ev.Screen != nil {
			d.ev.Screen(w, h)
		}
	case kvmTile:
		if len(v) < 8 {
			return nil
		}
		x, y := xy()
		// The header's size is what decoding allocates, a tile can't be
		// much larger than the screen.
		cfg, err := jpeg.DecodeConfig(bytes.NewReader(v[8:]))
		if w, h := d.Size(); err != nil || cfg.Width > w+desktopTileSlack || cfg.Height > h+desktopTileSlack {
			return nil
		}
		img, err := jpeg.Decode(bytes.NewReader(v[8:]))
		if err != nil {
			return nil
		}
		d.mu.Lock()
		r := blit(d.pix, d.w, d.h, img, x, y)
		d.mu.Unlock()
		if !r.Empty() && d.ev.Frame != nil {
			d.ev.Frame(r)
		}
	case kvmGetDisplays:
		if len(v) < 6 {
			return nil
		}
		n := int(binary.BigEndian.Uint16(v[4:]))
		if n > maxDesktopDisplays || len(v) < 8+2*n {
			return nil
		}
		ids := make([]int, n)
		for i := range ids {
			ids[i] = int(binary.BigEndian.Uint16(v[6+2*i:]))
		}
		sel := 0
		if n > 0 {
			sel = int(binary.BigEndian.Uint16(v[6+2*n:]))
		}
		if d.ev.Displays != nil {
			d.ev.Displays(ids, sel)
		}
	case kvmMessage:
		d.notice(string(v[4:]))
	case kvmAlert:
		if s := string(v[4:]); !strings.HasPrefix(s, ".") { // '.' marks debug output
			d.notice(s)
		}
	case kvmCursor:
		if len(v) == 5 && d.ev.Cursor != nil {
			d.ev.Cursor(int(v[4]))
		}
	}
	return nil
}

func compression(quality, frameMs int) []byte {
	// JPEG at quality, no scaling (1024 is 100 %), a frame every frameMs.
	return kvmCmd(kvmCompression, append([]byte{1, byte(quality)}, be16(1024, frameMs)...)...)
}

// blit copies img to (x, y) of the w*h frame pix, returning the rectangle
// it covered.
func blit(pix []byte, w, h int, img image.Image, x, y int) image.Rectangle {
	b := img.Bounds()
	r := image.Rect(x, y, x+b.Dx(), y+b.Dy()).Intersect(image.Rect(0, 0, w, h))
	if r.Empty() {
		return r
	}
	switch src := img.(type) {
	case *image.YCbCr:
		// COffset with the subsampling worked out once, not per pixel.
		hs, vs := 0, 0
		switch src.SubsampleRatio {
		case image.YCbCrSubsampleRatio422:
			hs = 1
		case image.YCbCrSubsampleRatio420:
			hs, vs = 1, 1
		case image.YCbCrSubsampleRatio440:
			vs = 1
		case image.YCbCrSubsampleRatio411:
			hs = 2
		case image.YCbCrSubsampleRatio410:
			hs, vs = 2, 1
		}
		m := src.Rect.Min
		for py := r.Min.Y; py < r.Max.Y; py++ {
			sy := b.Min.Y + py - y
			sx := b.Min.X + r.Min.X - x
			yi := src.YOffset(sx, sy)
			crow := (sy>>vs-m.Y>>vs)*src.CStride - m.X>>hs
			o := (py*w + r.Min.X) * 4
			for ; o < (py*w+r.Max.X)*4; o += 4 {
				ci := crow + sx>>hs
				cr, cg, cb := ycbcr(src.Y[yi], src.Cb[ci], src.Cr[ci])
				pix[o], pix[o+1], pix[o+2] = cb, cg, cr
				yi++
				sx++
			}
		}
	case *image.Gray:
		for py := r.Min.Y; py < r.Max.Y; py++ {
			o := (py*w + r.Min.X) * 4
			for px := r.Min.X; px < r.Max.X; px++ {
				g := src.GrayAt(b.Min.X+px-x, b.Min.Y+py-y).Y
				pix[o], pix[o+1], pix[o+2] = g, g, g
				o += 4
			}
		}
	default:
		for py := r.Min.Y; py < r.Max.Y; py++ {
			o := (py*w + r.Min.X) * 4
			for px := r.Min.X; px < r.Max.X; px++ {
				cr, cg, cb, _ := img.At(b.Min.X+px-x, b.Min.Y+py-y).RGBA()
				pix[o], pix[o+1], pix[o+2] = byte(cb>>8), byte(cg>>8), byte(cr>>8)
				o += 4
			}
		}
	}
	return r
}

// ycbcr is color.YCbCrToRGB, inlined.
func ycbcr(y, cb, cr uint8) (uint8, uint8, uint8) {
	yy := int32(y) * 0x10101
	cb1 := int32(cb) - 128
	cr1 := int32(cr) - 128
	clamp := func(v int32) uint8 {
		if uint32(v)&0xff000000 == 0 {
			return uint8(v >> 16)
		}
		return uint8(^(v >> 31))
	}
	return clamp(yy + 91881*cr1), clamp(yy - 22554*cb1 - 46802*cr1), clamp(yy + 116130*cb1)
}

// ready reports whether the screen arrived, input before it is dropped like
// the web UI does.
func (d *Desktop) ready() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.started
}

// MouseMove moves the remote pointer to (x, y) in screen pixels.
func (d *Desktop) MouseMove(x, y int) {
	if d.ready() {
		d.send(kvmCmd(kvmMouse, append([]byte{0, 0}, be16(x, y)...)...), true)
	}
}

// MouseButton presses or releases button (MouseLeft, MouseRight or
// MouseMiddle) at (x, y).
func (d *Desktop) MouseButton(button int, down bool, x, y int) {
	if !d.ready() {
		return
	}
	if !down {
		button *= 2
	}
	d.send(kvmCmd(kvmMouse, append([]byte{0, byte(button)}, be16(x, y)...)...), false)
}

// MouseWheel scrolls at (x, y), delta in 1/120 notches as Qt and Windows
// count them, positive away from the user.
func (d *Desktop) MouseWheel(delta, x, y int) {
	if !d.ready() || delta == 0 {
		return
	}
	// The web UI sends three times the browser's wheel delta, negative as
	// the complement of the magnitude.
	v := delta * 3
	var dv uint16
	if v < 0 {
		dv = ^uint16(-v)
	} else {
		dv = uint16(v)
	}
	d.send(kvmCmd(kvmMouse, append([]byte{0, 0}, be16(x, y, int(dv))...)...), false)
}

// Key presses or releases the key with Windows virtual-key code vk.
// extended marks the keys Windows flags as such: the arrows, Insert, Delete,
// Home, End, Page Up and Down, right Ctrl and Alt, keypad / and Enter.
func (d *Desktop) Key(vk int, down, extended bool) {
	if !d.ready() {
		return
	}
	a := byte(1)
	switch {
	case down && extended:
		a = 4
	case down:
		a = 0
	case extended:
		a = 3
	}
	d.send(kvmCmd(kvmKey, a, byte(vk)), false)
}

// Unicode presses or releases the key typing r, for text that has no key
// of its own on the remote keyboard layout.
func (d *Desktop) Unicode(r rune, down bool) {
	if !d.ready() {
		return
	}
	a := byte(1)
	if down {
		a = 0
	}
	units := []rune{r}
	if r > 0xffff { // a surrogate pair, the agent takes UTF-16 units
		r -= 0x10000
		units = []rune{0xd800 + r>>10, 0xdc00 + r&0x3ff}
	}
	for _, u := range units {
		d.send(kvmCmd(kvmKeyUnicode, a, byte(u>>8), byte(u)), false)
	}
}

// CtrlAltDel sends Ctrl+Alt+Del, which a local keyboard can't pass on.
func (d *Desktop) CtrlAltDel() { d.send(kvmCmd(kvmCtrlAltDel), false) }

// GetDisplays asks for the device's displays, answered through
// DesktopEvents.Displays.
func (d *Desktop) GetDisplays() { d.send(kvmCmd(kvmGetDisplays), false) }

// SetDisplay shows display id, or AllDisplays.
func (d *Desktop) SetDisplay(id int) { d.send(kvmCmd(kvmSetDisplay, be16(id)...), false) }

// SetQuality sets the JPEG quality (1-100) and the time between frames.
func (d *Desktop) SetQuality(quality int, frame time.Duration) {
	quality = min(max(quality, 1), 100)
	ms := min(max(int(frame/time.Millisecond), 10), 65535)
	d.mu.Lock()
	d.quality, d.frameMs = quality, ms
	started := d.started
	d.mu.Unlock()
	if started {
		d.send(compression(quality, ms), false)
	}
}

type clipboardRequest struct {
	tag   string
	reply chan map[string]interface{}
}

// Clipboard messages travel over control.ashx, not the KVM relay.
var clipboardRequests = struct {
	sync.Mutex
	next    uint64
	pending map[[2]string]clipboardRequest
}{pending: make(map[[2]string]clipboardRequest)}

func handleClipboardCommand(m map[string]interface{}) {
	node, _ := m["nodeid"].(string)
	kind, _ := m["type"].(string)
	clipboardRequests.Lock()
	defer clipboardRequests.Unlock()
	p, ok := clipboardRequests.pending[[2]string{node, kind}]
	if !ok || (kind == "getclip" && m["tag"] != p.tag) {
		return
	}
	select {
	case p.reply <- m:
	default:
	}
}

// GetClipboard reads remote text. Some agents do not answer for an empty
// clipboard, so callers must supply a context with a deadline.
func GetClipboard(ctx context.Context, nodeID string) (string, error) {
	return desktopClipboard(ctx, nodeID, nil)
}

// SetClipboard waits for the agent's acknowledgement of a text transfer.
func SetClipboard(ctx context.Context, nodeID, text string) error {
	_, err := desktopClipboard(ctx, nodeID, &text)
	return err
}

func desktopClipboard(ctx context.Context, nodeID string, text *string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	kind := "getclip"
	if text != nil {
		kind = "setclip"
	}
	key := [2]string{nodeID, kind}
	clipboardRequests.Lock()
	if _, busy := clipboardRequests.pending[key]; busy {
		clipboardRequests.Unlock()
		return "", errors.New("a clipboard request is already pending for this device")
	}
	clipboardRequests.next++
	tag := fmt.Sprintf("mcc-clipboard-%d", clipboardRequests.next)
	reply := make(chan map[string]interface{}, 1)
	clipboardRequests.pending[key] = clipboardRequest{tag, reply}
	clipboardRequests.Unlock()
	defer func() {
		clipboardRequests.Lock()
		delete(clipboardRequests.pending, key)
		clipboardRequests.Unlock()
	}()
	m := map[string]any{"action": "msg", "type": kind, "nodeid": nodeID, "tag": tag}
	if text != nil {
		m["data"] = *text
	}
	b, _ := json.Marshal(m)
	if err := send(b); err != nil {
		return "", err
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case m := <-reply:
		if text != nil {
			if m["success"] != true {
				return "", errors.New("the agent could not set its clipboard")
			}
			return "", nil
		}
		value, ok := m["data"].(string)
		if !ok {
			return "", errors.New("the agent returned no clipboard text")
		}
		return value, nil
	}
}
