#include <string.h>
#include <wayland-client.h>

#include "syskeys_linux.h"

// keyboard-shortcuts-inhibit-unstable-v1, as wayland-scanner writes it.
extern const struct wl_interface mcc_inhibitor_interface;

static const struct wl_interface *types[] = {
	NULL,
	&mcc_inhibitor_interface,
	&wl_surface_interface,
	&wl_seat_interface,
};

static const struct wl_message manager_requests[] = {
	{"destroy", "", types + 0},
	{"inhibit_shortcuts", "noo", types + 1},
};

static const struct wl_interface manager_interface = {
	"zwp_keyboard_shortcuts_inhibit_manager_v1", 1, 2, manager_requests, 0, NULL,
};

static const struct wl_message inhibitor_requests[] = {
	{"destroy", "", types + 0},
};

static const struct wl_message inhibitor_events[] = {
	{"active", "", types + 0},
	{"inactive", "", types + 0},
};

const struct wl_interface mcc_inhibitor_interface = {
	"zwp_keyboard_shortcuts_inhibitor_v1", 1, 1, inhibitor_requests, 2, inhibitor_events,
};

// The manager and inhibitor live on a queue of their own, Qt's listeners
// never see them.
static struct wl_display *display;
static struct wl_event_queue *queue;
static struct wl_proxy *manager;
static struct wl_proxy *inhibitor;

static void global(void *data, struct wl_registry *registry, uint32_t name, const char *iface, uint32_t version) {
	if (!manager && strcmp(iface, manager_interface.name) == 0) {
		manager = wl_registry_bind(registry, name, &manager_interface, 1);
	}
}

static void global_remove(void *data, struct wl_registry *registry, uint32_t name) {}

static const struct wl_registry_listener registry_listener = {global, global_remove};

static void bind_manager(void) {
	static int done;
	if (done) {
		return;
	}
	done = 1;
	display = mcc_wl_display();
	if (!display) {
		return;
	}
	queue = wl_display_create_queue(display);
	struct wl_display *wrapper = wl_proxy_create_wrapper(display);
	wl_proxy_set_queue((struct wl_proxy *)wrapper, queue);
	struct wl_registry *registry = wl_display_get_registry(wrapper);
	wl_proxy_wrapper_destroy(wrapper);
	wl_registry_add_listener(registry, &registry_listener, NULL);
	wl_display_roundtrip_queue(display, queue);
	wl_registry_destroy(registry);
}

int mcc_inhibit_shortcuts(uintptr_t surface) {
	mcc_release_shortcuts();
	bind_manager();
	void *seat = mcc_wl_seat();
	if (!manager || !seat || !surface) {
		return 0;
	}
	inhibitor = wl_proxy_marshal_flags(manager, 1, &mcc_inhibitor_interface, 1, 0, NULL, (void *)surface, seat);
	wl_display_flush(display);
	return inhibitor != NULL;
}

void mcc_release_shortcuts(void) {
	if (!inhibitor) {
		return;
	}
	wl_proxy_marshal_flags(inhibitor, 0, NULL, 1, WL_MARSHAL_FLAG_DESTROY);
	inhibitor = NULL;
	wl_display_flush(display);
	wl_display_dispatch_queue_pending(display, queue); // its active and inactive events
}
