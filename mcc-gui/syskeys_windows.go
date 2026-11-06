package main

import (
	"unsafe"

	qt "github.com/mappu/miqt/qt6"
	"golang.org/x/sys/windows"
)

const sysKeysSupported = true

// Windows keeps the Windows key, Alt+Tab and the like before the window
// sees them, a low-level keyboard hook takes them first and passes them to
// send instead.
var (
	user32           = windows.NewLazySystemDLL("user32.dll")
	setWindowsHookEx = user32.NewProc("SetWindowsHookExW")
	callNextHookEx   = user32.NewProc("CallNextHookEx")
	unhookWindowsHk  = user32.NewProc("UnhookWindowsHookEx")
	getAsyncKeyState = user32.NewProc("GetAsyncKeyState")
	keyHookProc      = windows.NewCallback(keyHook)

	keyHookHandle uintptr
	keyHookSend   func(vk int, ext, down bool)
	keyHookDown   = map[uint32]bool{} // keys pressed through the hook, their releases follow
)

const (
	whKeyboardLL     = 13
	llkhfExtended    = 0x01
	llkhfInjected    = 0x10
	llkhfAltDown     = 0x20
	vkTab, vkControl = 0x09, 0x11
	vkEscape, vkF4   = 0x1b, 0x73
	vkSpace          = 0x20
	vkLWin, vkRWin   = 0x5b, 0x5c
)

type kbdLLHookStruct struct {
	vkCode, scanCode, flags, time uint32
	extraInfo                     uintptr
}

func keyHook(code int, wparam, lparam uintptr) uintptr {
	if code == 0 && keyHookSend != nil {
		k := (*kbdLLHookStruct)(unsafe.Pointer(lparam))
		down := wparam == 0x100 || wparam == 0x104 // WM_KEYDOWN, WM_SYSKEYDOWN
		if k.flags&llkhfInjected == 0 && takeKey(k, down) {
			keyHookSend(int(k.vkCode), k.flags&llkhfExtended != 0, down)
			return 1
		}
	}
	r, _, _ := callNextHookEx.Call(0, uintptr(code), wparam, lparam)
	return r
}

// takeKey is whether the key is one Windows would keep: the Windows keys and
// everything pressed with them, Alt+Tab, Alt+Esc, Alt+F4, Alt+Space and
// Ctrl+Esc.
func takeKey(k *kbdLLHookStruct, down bool) bool {
	vk := k.vkCode
	if !down {
		taken := keyHookDown[vk]
		delete(keyHookDown, vk)
		return taken
	}
	alt := k.flags&llkhfAltDown != 0
	ctrl, _, _ := getAsyncKeyState.Call(vkControl)
	take := vk == vkLWin || vk == vkRWin || keyHookDown[vkLWin] || keyHookDown[vkRWin] ||
		alt && (vk == vkTab || vk == vkEscape || vk == vkF4 || vk == vkSpace) ||
		ctrl&0x8000 != 0 && vk == vkEscape
	if take {
		keyHookDown[vk] = true
	}
	return take
}

func grabSysKeys(_ *qt.QWidget, send func(vk int, ext, down bool)) {
	releaseSysKeys()
	var mod windows.Handle
	_ = windows.GetModuleHandleEx(0, nil, &mod)
	h, _, err := setWindowsHookEx.Call(whKeyboardLL, keyHookProc, uintptr(mod), 0)
	if h == 0 {
		logf("desktop: keyboard hook: %v", err)
		return
	}
	keyHookHandle, keyHookSend = h, send
}

func releaseSysKeys() {
	if keyHookHandle != 0 {
		unhookWindowsHk.Call(keyHookHandle)
	}
	keyHookHandle, keyHookSend = 0, nil
	clear(keyHookDown)
}
