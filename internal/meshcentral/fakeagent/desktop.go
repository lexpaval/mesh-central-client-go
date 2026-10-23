package fakeagent

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
	"net/http"
	"slices"
	"sync"

	"github.com/gorilla/websocket"
)

// DesktopAgent serves the device side of a relay tunnel as an agent's KVM
// channel (protocol 2): it joins with "c", sends the screen size and, once
// the viewer set the compression, the screen as JPEG tiles, big ones as
// jumbo commands split over two messages. A left click paints a dot there
// and sends that tile, so input round-trips.
type DesktopAgent struct {
	// Screens by display ID, the lowest is shown first. Clicks paint on
	// them.
	Screens map[int]*image.RGBA
	// Input, when set, gets every binary command the viewer sends.
	Input func(cmd []byte)
	// Consent messages are sent as the agent's console messages first.
	Consent []string

	mu sync.Mutex
}

// DemoScreen draws a w*h desktop with a window on it.
func DemoScreen(w, h int) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := range h {
		for x := range w {
			img.Set(x, y, color.RGBA{uint8(30 + 40*y/h), uint8(60 + 60*y/h), uint8(110 + 80*x/w), 255})
		}
	}
	win := image.Rect(w/8, h/8, w*5/8, h*5/8)
	draw.Draw(img, win, image.NewUniform(color.RGBA{236, 236, 236, 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(win.Min.X, win.Min.Y, win.Max.X, win.Min.Y+h/20), image.NewUniform(color.RGBA{60, 64, 72, 255}), image.Point{}, draw.Src)
	draw.Draw(img, image.Rect(0, h-h/16, w, h), image.NewUniform(color.RGBA{24, 26, 30, 255}), image.Point{}, draw.Src)
	return img
}

func kvm(cmd int, payload ...byte) []byte {
	b := make([]byte, 4, 4+len(payload))
	binary.BigEndian.PutUint16(b, uint16(cmd))
	binary.BigEndian.PutUint16(b[2:], uint16(min(4+len(payload), 0xffff)))
	return append(b, payload...)
}

func (a *DesktopAgent) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	up := websocket.Upgrader{}
	c, err := up.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer c.Close()
	c.WriteMessage(websocket.TextMessage, []byte("c"))
	if _, m, err := c.ReadMessage(); err != nil || string(m) != "2" {
		return
	}
	for i, msg := range a.Consent {
		c.WriteMessage(websocket.TextMessage, []byte(`{"ctrlChannel":"102938","type":"console","msg":"`+msg+`","msgid":`+string(rune('1'+i))+`}`))
	}

	ids := slices.Sorted(func(yield func(int) bool) {
		for id := range a.Screens {
			if !yield(id) {
				return
			}
		}
	})
	display := ids[0]
	screen := func() *image.RGBA { return a.Screens[display] }
	sendSize := func() {
		b := screen().Bounds()
		c.WriteMessage(websocket.BinaryMessage, kvm(7, byte(b.Dx()>>8), byte(b.Dx()), byte(b.Dy()>>8), byte(b.Dy())))
	}
	sendTile := func(r image.Rectangle, split bool) {
		var buf bytes.Buffer
		a.mu.Lock()
		jpeg.Encode(&buf, screen().SubImage(r), &jpeg.Options{Quality: 90})
		a.mu.Unlock()
		tile := kvm(3, append([]byte{byte(r.Min.X >> 8), byte(r.Min.X), byte(r.Min.Y >> 8), byte(r.Min.Y)}, buf.Bytes()...)...)
		if len(tile) > 0xffff {
			jumbo := kvm(27, 0, 0, 0, 0)
			binary.BigEndian.PutUint32(jumbo[4:], uint32(len(tile)))
			tile = append(jumbo, tile...)
		}
		if split {
			c.WriteMessage(websocket.BinaryMessage, tile[:len(tile)/2])
			tile = tile[len(tile)/2:]
		}
		c.WriteMessage(websocket.BinaryMessage, tile)
	}
	sendScreen := func() {
		b := screen().Bounds()
		for y := 0; y < b.Dy(); y += 256 {
			sendTile(image.Rect(0, y, b.Dx(), min(y+256, b.Dy())), y == 0)
		}
	}

	sendSize()
	for {
		mt, m, err := c.ReadMessage()
		if err != nil {
			return
		}
		if mt != websocket.BinaryMessage || len(m) < 4 {
			continue
		}
		if a.Input != nil {
			a.Input(m)
		}
		switch binary.BigEndian.Uint16(m) {
		case 5, 6: // compression, refresh
			sendScreen()
		case 2: // mouse
			if len(m) == 10 && m[5] == 0x02 {
				x, y := int(binary.BigEndian.Uint16(m[6:])), int(binary.BigEndian.Uint16(m[8:]))
				dot := image.Rect(x-4, y-4, x+5, y+5).Intersect(screen().Bounds())
				a.mu.Lock()
				draw.Draw(screen(), dot, image.NewUniform(color.RGBA{229, 83, 75, 255}), image.Point{}, draw.Src)
				a.mu.Unlock()
				if !dot.Empty() {
					sendTile(dot, false)
				}
			}
		case 11: // get displays
			p := []byte{byte(len(ids) >> 8), byte(len(ids))}
			for _, id := range append(slices.Clone(ids), display) {
				p = append(p, byte(id>>8), byte(id))
			}
			c.WriteMessage(websocket.BinaryMessage, kvm(11, p...))
		case 12: // set display
			if len(m) == 6 {
				if id := int(binary.BigEndian.Uint16(m[4:])); a.Screens[id] != nil {
					display = id
					sendSize()
					sendScreen()
				}
			}
		}
	}
}
