package main

/*
#cgo pkg-config: Qt6Gui wayland-client
#cgo CXXFLAGS: -std=c++17
#include "syskeys_linux.h"
*/
import "C"

import qt "github.com/mappu/miqt/qt6"

const sysKeysSupported = true

var grabbedKeyboard *qt.QWidget

// grabSysKeys takes the local system's shortcuts while w has the keyboard,
// so they reach w as key presses: the compositor's on Wayland, a keyboard
// grab on X11.
func grabSysKeys(w *qt.QWidget, _ func(vk int, ext, down bool)) {
	releaseSysKeys()
	switch qt.QGuiApplication_PlatformName() {
	case "wayland":
		if C.mcc_inhibit_shortcuts(C.uintptr_t(w.Window().WinId())) == 0 {
			logf("desktop: the compositor doesn't let applications take its shortcuts")
		}
	case "xcb":
		w.GrabKeyboard()
		grabbedKeyboard = w
	}
}

func releaseSysKeys() {
	C.mcc_release_shortcuts()
	if grabbedKeyboard != nil {
		grabbedKeyboard.ReleaseKeyboard()
		grabbedKeyboard = nil
	}
}
