package main

import (
	"bytes"
	"embed"
	"image"
	"image/color"
	"image/png"
	"strings"

	qt "github.com/mappu/miqt/qt6"
	"github.com/srwiley/oksvg"
	"github.com/srwiley/rasterx"
)

// Font Awesome Free icons (CC BY 4.0, attribution in each file). They're
// rasterized here rather than by Qt's SVG plugin, which static builds would
// have to link in, and tinted with the palette so they follow the theme.
//
//go:embed icons/*.svg
var iconFiles embed.FS

//go:embed Icon.png
var appIconPNG []byte

// iconSizes are rendered for every icon, QIcon scales the nearest one.
var iconSizes = []int{16, 18, 24, 32, 36, 48, 64}

// iconCache holds the icons tinted for the current palette, cleared when it
// changes.
var iconCache = map[string]*qt.QIcon{}

// icon returns the named icon with a pixmap per size for the normal (text
// color), disabled and selected (highlighted text) modes.
func icon(name string) *qt.QIcon {
	if ic, ok := iconCache[name]; ok {
		return ic
	}
	src, err := iconFiles.ReadFile("icons/" + name + ".svg")
	if err != nil {
		panic("no icon " + name)
	}
	pal := qt.QGuiApplication_Palette()
	ic := qt.NewQIcon()
	for _, m := range []struct {
		mode qt.QIcon__Mode
		cg   qt.QPalette__ColorGroup
		role qt.QPalette__ColorRole
	}{
		{qt.QIcon__Normal, qt.QPalette__Active, qt.QPalette__WindowText},
		{qt.QIcon__Disabled, qt.QPalette__Disabled, qt.QPalette__WindowText},
		{qt.QIcon__Selected, qt.QPalette__Active, qt.QPalette__HighlightedText},
	} {
		c := pal.Color(m.cg, m.role)
		tint := color.NRGBA{uint8(c.Red()), uint8(c.Green()), uint8(c.Blue()), 0xff}
		for _, s := range iconSizes {
			pm := pixmapPNG(rasterize(src, s, tint))
			ic.AddPixmap2(pm, m.mode)
			pm.Delete()
		}
	}
	iconCache[name] = ic
	return ic
}

// rowPixmaps holds the icons at the row size, rows paint them all the time.
// QIcon.Pixmap returns a new pixmap each call, freed by a finalizer on
// another thread.
var rowPixmaps = map[struct {
	name string
	mode qt.QIcon__Mode
}]*qt.QPixmap{}

func rowPixmap(name string, mode qt.QIcon__Mode) *qt.QPixmap {
	k := struct {
		name string
		mode qt.QIcon__Mode
	}{name, mode}
	pm, ok := rowPixmaps[k]
	if !ok {
		s := qt.NewQSize2(rowIconSize, rowIconSize)
		defer s.Delete()
		pm = icon(name).Pixmap5(s, mode)
		rowPixmaps[k] = pm
	}
	return pm
}

// resetIcons drops the tinted icons after a palette change.
func resetIcons() {
	for _, ic := range iconCache {
		ic.Delete()
	}
	clear(iconCache)
	clear(rowPixmaps) // freed by their finalizers
}

// rasterize draws an SVG centered in a size×size square, filled with tint.
func rasterize(src []byte, size int, tint color.NRGBA) *image.NRGBA {
	// The files fill with currentColor, which oksvg doesn't know. Only the
	// shape matters, the tint colors it.
	svg, err := oksvg.ReadIconStream(strings.NewReader(strings.ReplaceAll(string(src), "currentColor", "#000")))
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	if err != nil {
		return img
	}
	vb := svg.ViewBox
	scale := min(float64(size)/vb.W, float64(size)/vb.H)
	w, h := vb.W*scale, vb.H*scale
	svg.SetTarget((float64(size)-w)/2, (float64(size)-h)/2, w, h)
	mask := image.NewRGBA(img.Rect)
	svg.Draw(rasterx.NewDasher(size, size, rasterx.NewScannerGV(size, size, mask, mask.Bounds())), 1)
	for i := 3; i < len(mask.Pix); i += 4 {
		a := mask.Pix[i]
		img.Pix[i-3], img.Pix[i-2], img.Pix[i-1], img.Pix[i] = tint.R, tint.G, tint.B, uint8(uint16(a)*uint16(tint.A)/0xff)
	}
	return img
}

func pixmapPNG(img image.Image) *qt.QPixmap {
	var b bytes.Buffer
	png.Encode(&b, img)
	pm := qt.NewQPixmap()
	pm.LoadFromData4(b.Bytes(), "PNG")
	return pm
}

// appIcon is the window icon.
func appIcon() *qt.QIcon {
	pm := qt.NewQPixmap()
	pm.LoadFromData4(appIconPNG, "PNG")
	defer pm.Delete()
	return qt.NewQIcon2(pm)
}

// osIconName picks the icon for an OS description.
func osIconName(desc string) string {
	s := strings.ToLower(desc)
	for _, m := range [][2]string{
		{"windows", "windows"}, {"raspbian", "raspberry-pi"}, {"raspberry", "raspberry-pi"},
		{"ubuntu", "ubuntu"}, {"fedora", "fedora"}, {"debian", "debian"}, {"suse", "opensuse"}, {"red hat", "redhat"},
		{"rhel", "redhat"}, {"macos", "apple"}, {"mac os", "apple"}, {"darwin", "apple"}, {"linux", "linux"},
	} {
		if strings.Contains(s, m[0]) {
			return m[1]
		}
	}
	return "server"
}
