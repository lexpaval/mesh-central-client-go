#include <QtGui/QGuiApplication>
#include <QtGui/qguiapplication_platform.h>

extern "C" {
#include "syskeys_linux.h"
}

#if QT_CONFIG(wayland)
static QNativeInterface::QWaylandApplication *wayland() {
	return qGuiApp ? qGuiApp->nativeInterface<QNativeInterface::QWaylandApplication>() : nullptr;
}

void *mcc_wl_display(void) {
	auto *w = wayland();
	return w ? w->display() : nullptr;
}

void *mcc_wl_seat(void) {
	auto *w = wayland();
	return w ? w->seat() : nullptr;
}
#else
void *mcc_wl_display(void) { return nullptr; }
void *mcc_wl_seat(void) { return nullptr; }
#endif
