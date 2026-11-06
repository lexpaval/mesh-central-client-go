// Taking the compositor's shortcuts (Super, Alt+Tab) for a window on
// Wayland, through the keyboard-shortcuts-inhibit protocol.

#include <stdint.h>

// From Qt, syskeys_linux.cpp: NULL when not on Wayland.
void *mcc_wl_display(void);
void *mcc_wl_seat(void);

// mcc_inhibit_shortcuts asks for the shortcuts while surface has the
// keyboard, 0 when the compositor doesn't offer it.
int mcc_inhibit_shortcuts(uintptr_t surface);
void mcc_release_shortcuts(void);
