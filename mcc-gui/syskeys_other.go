//go:build !linux && !windows && !darwin

package main

import qt "github.com/mappu/miqt/qt6"

const sysKeysSupported = false

func grabSysKeys(*qt.QWidget, func(vk int, ext, down bool)) {}

func releaseSysKeys() {}
