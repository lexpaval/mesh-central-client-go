package main

/*
#cgo LDFLAGS: -framework CoreGraphics -framework CoreFoundation
#include <CoreGraphics/CoreGraphics.h>
#include <unistd.h>

// macOS acts on Cmd+Tab, Cmd+Space and the like before the window sees
// them. A session event tap takes every key with Cmd or Ctrl held and
// posts it to this process alone, past the system's shortcuts, marked so
// the tap lets it through if it comes by again.
static const int64_t mccMark = 0x6d6363;
static CFMachPortRef mccTap;

static CGEventRef mccTapped(CGEventTapProxy proxy, CGEventType type, CGEventRef event, void *info) {
	if (type == kCGEventTapDisabledByTimeout || type == kCGEventTapDisabledByUserInput) {
		CGEventTapEnable(mccTap, true);
		return event;
	}
	if (CGEventGetIntegerValueField(event, kCGEventSourceUserData) == mccMark ||
		!(CGEventGetFlags(event) & (kCGEventFlagMaskCommand | kCGEventFlagMaskControl))) {
		return event;
	}
	CGEventRef copy = CGEventCreateCopy(event);
	CGEventSetIntegerValueField(copy, kCGEventSourceUserData, mccMark);
	CGEventPostToPid(getpid(), copy);
	CFRelease(copy);
	return NULL;
}

// mccTapStart is 0 without the Accessibility permission, asked for once.
static int mccTapStart(void) {
	static int asked;
	if (!mccTap) {
		if (!asked) {
			asked = 1;
			CGRequestPostEventAccess(); // the Accessibility prompt
		}
		mccTap = CGEventTapCreate(kCGSessionEventTap, kCGHeadInsertEventTap, kCGEventTapOptionDefault,
			CGEventMaskBit(kCGEventKeyDown) | CGEventMaskBit(kCGEventKeyUp), mccTapped, NULL);
		if (!mccTap) {
			return 0;
		}
		CFRunLoopSourceRef source = CFMachPortCreateRunLoopSource(NULL, mccTap, 0);
		CFRunLoopAddSource(CFRunLoopGetMain(), source, kCFRunLoopCommonModes);
		CFRelease(source);
	}
	CGEventTapEnable(mccTap, true);
	return 1;
}

static void mccTapStop(void) {
	if (mccTap) {
		CGEventTapEnable(mccTap, false);
	}
}
*/
import "C"

import qt "github.com/mappu/miqt/qt6"

const sysKeysSupported = true

var sysKeysDenied bool

// grabSysKeys has the system's shortcuts reach the window as key presses
// while it has the keyboard, the existing key mapping passes them on.
func grabSysKeys(*qt.QWidget, func(vk int, ext, down bool)) {
	if C.mccTapStart() == 0 && !sysKeysDenied {
		sysKeysDenied = true
		logf("desktop: system keys need Accessibility access for MeshCentral Client, in System Settings, Privacy & Security")
	}
}

func releaseSysKeys() { C.mccTapStop() }
