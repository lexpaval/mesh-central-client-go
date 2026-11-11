package main

// The remote keyboard map sends the keys a layout changes by their place on
// the keyboard, for the device's own layout to type, as the web UI's remote
// keyboard map does. The rest, function, navigation and keypad keys, are
// the same in every layout and go as Qt names them.

// usPlaces is the Windows virtual-key code of the key in each place on a US
// keyboard, by PC (set 1) scan code, 0x100 marking the extended ones.
var usPlaces = func() map[uint]sentKey {
	m := map[uint]sentKey{}
	for sc, vks := range map[uint][]int{
		0x02: {'1', '2', '3', '4', '5', '6', '7', '8', '9', '0', 0xbd, 0xbb},
		0x10: {'Q', 'W', 'E', 'R', 'T', 'Y', 'U', 'I', 'O', 'P', 0xdb, 0xdd},
		0x1e: {'A', 'S', 'D', 'F', 'G', 'H', 'J', 'K', 'L', 0xba, 0xde, 0xc0},
		0x2b: {0xdc, 'Z', 'X', 'C', 'V', 'B', 'N', 'M', 0xbc, 0xbe, 0xbf},
		0x56: {0xe2}, // the ISO key between left Shift and Z
		// Modifiers, as a layout may make right Alt AltGr.
		0x2a: {0x10}, 0x36: {0x10}, 0x1d: {0x11}, 0x11d: {0x11}, 0x38: {0x12}, 0x138: {0x12},
		0x15b: {0x5b}, 0x15c: {0x5c},
	} {
		for i, vk := range vks {
			c := sc + uint(i)
			m[c] = sentKey{vk: vk, ext: c&0x100 != 0}
		}
	}
	return m
}()

// evdevExtended are the Linux key codes of the extended keys usPlaces has,
// the others are their scan codes.
var evdevExtended = map[uint]uint{97: 0x11d, 100: 0x138, 125: 0x15b, 126: 0x15c}

// macPlaces is the scan code of each macOS virtual key code in usPlaces.
var macPlaces = []uint{
	0x1e, 0x1f, 0x20, 0x21, 0x23, 0x22, 0x2c, 0x2d, 0x2e, 0x2f, 0x56, 0x30, 0x10, 0x11, 0x12, 0x13,
	0x15, 0x14, 0x02, 0x03, 0x04, 0x05, 0x07, 0x06, 0x0d, 0x0a, 0x08, 0x0c, 0x09, 0x0b, 0x1b, 0x18,
	0x16, 0x1a, 0x17, 0x19, 0, 0x26, 0x24, 0x28, 0x25, 0x27, 0x2b, 0x33, 0x35, 0x31, 0x32, 0x34,
	0, 0, 0x29, 0, 0, 0, 0x15c, 0x15b, 0x2a, 0, 0x38, 0x1d, 0x36, 0x138, 0x11d,
}

// placedKey is the key in the place of the one pressed, from the scan code
// Qt reports on Windows (0x100 extended) and the X11 and Wayland key code
// (Linux's plus 8), or the virtual key code on macOS.
func placedKey(goos string, scanCode, virtualKey uint) (sentKey, bool) {
	var sc uint
	switch goos {
	case "windows":
		sc = scanCode
	case "darwin":
		if virtualKey < uint(len(macPlaces)) {
			sc = macPlaces[virtualKey]
		}
	default:
		if scanCode >= 8 {
			sc = scanCode - 8
			if c, ok := evdevExtended[sc]; ok {
				sc = c
			}
		}
	}
	k, ok := usPlaces[sc]
	return k, ok
}
