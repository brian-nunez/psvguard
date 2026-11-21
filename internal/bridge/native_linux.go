//go:build linux && cgo

package bridge

/*
#cgo pkg-config: gtk+-3.0 gtk-layer-shell-0 ayatana-appindicator3-0.1 gio-unix-2.0
// libpipewire-0.3.pc adds -fno-strict-overflow, which cgo refuses, so its flags are spelled out.
#cgo CFLAGS: -Wno-deprecated-declarations -I/usr/include/pipewire-0.3 -I/usr/include/spa-0.2 -D_REENTRANT
#cgo LDFLAGS: -lpipewire-0.3 -lm

#include <fcntl.h>
#include <math.h>
#include <stdint.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#include <gio/gio.h>
#include <gio/gunixfdlist.h>
#include <gio/gunixsocketaddress.h>
#include <gtk/gtk.h>
#include <gtk-layer-shell/gtk-layer-shell.h>
#include <libayatana-appindicator/app-indicator.h>
#include <pipewire/pipewire.h>
#include <spa/param/video/format-utils.h>
#include <spa/param/buffers.h>
#include <spa/param/param.h>
#include <spa/debug/types.h>

// This file is the Linux platform layer for Wayland sessions: a command executor. It holds the GTK
// windows, the AppIndicator item, the screen cast portal session and the PipeWire streams, applies
// the commands Go posts, and reports events. It makes no decisions and never sees psv results.

// Go callbacks exported from bus_linux.go. Declared here rather than via _cgo_export.h, which cgo
// generates only after compiling this preamble.
extern void goEvent(int kind, const void *data, size_t len);
extern void goCaptureFrame(uintptr_t handle, int pixelFormat, const uint8_t *base, int width, int height,
                           int bytesPerRow, int64_t ptsNanos);

// Must match EventKind in contract.go.
enum {
	PGEvCapabilities = 1, PGEvReady = 2, PGEvAction = 3, PGEvDisplays = 4, PGEvPermission = 5,
	PGEvStreamStarted = 6, PGEvStreamStopped = 7, PGEvStreamFailed = 8, PGEvPower = 9, PGEvCoversShown = 10,
	PGEvTransition = 11,
};

// Must match CommandKind in contract.go.
enum {
	PGCmdSetCovers = 1, PGCmdClearCovers = 2, PGCmdStatus = 3, PGCmdControls = 4, PGCmdStats = 5,
	PGCmdStartCapture = 6, PGCmdStopCapture = 7, PGCmdCheckPermission = 8, PGCmdRequestPermission = 9,
	PGCmdOpenPermissionPane = 10, PGCmdNotify = 11, PGCmdShowPanel = 12, PGCmdQuit = 13,
};

// Must match frame.Format.
enum { PGFormatBGRA = 1, PGFormatRGBA = 2, PGFormatBGRX = 3, PGFormatRGBX = 4 };

// The cover's colour and opacity, as on macOS.
#define PG_COVER_OPACITY 0.97

typedef struct { double x, y, w, h; } PGRect;

// pg_norm_to_px converts a normalised rectangle (0 to 1, origin top left) on a display of width ×
// height logical pixels into logical pixels, rounded outward to whole device pixels at scale so
// covers leave no hairline gaps, and clipped to the display.
PGRect pg_norm_to_px(double nx, double ny, double nw, double nh, double width, double height, double scale) {
	if (scale <= 0) scale = 1;
	double x0 = nx * width, x1 = (nx + nw) * width;
	double y0 = ny * height, y1 = (ny + nh) * height;
	x0 = floor(x0 * scale + 1e-9) / scale;
	y0 = floor(y0 * scale + 1e-9) / scale;
	x1 = ceil(x1 * scale - 1e-9) / scale;
	y1 = ceil(y1 * scale - 1e-9) / scale;
	x0 = fmax(x0, 0); y0 = fmax(y0, 0);
	x1 = fmin(x1, width); y1 = fmin(y1, height);
	PGRect r = { x0, y0, fmax(0, x1 - x0), fmax(0, y1 - y0) };
	return r;
}

// MARK: - JSON
//
// Payloads cross the bridge as JSON. Commands are parsed into GVariants (objects as a{sv}, arrays
// as av, numbers as d, booleans as b, strings and null as s) and events are written from them.

static void pg_json_string(GString *out, const char *s) {
	g_string_append_c(out, '"');
	for (const unsigned char *p = (const unsigned char *)s; *p; p++) {
		switch (*p) {
		case '"': g_string_append(out, "\\\""); break;
		case '\\': g_string_append(out, "\\\\"); break;
		case '\n': g_string_append(out, "\\n"); break;
		case '\r': g_string_append(out, "\\r"); break;
		case '\t': g_string_append(out, "\\t"); break;
		default:
			if (*p < 0x20) g_string_append_printf(out, "\\u%04x", *p);
			else g_string_append_c(out, (char)*p);
		}
	}
	g_string_append_c(out, '"');
}

static void pg_json_write(GString *out, GVariant *v) {
	if (g_variant_is_of_type(v, G_VARIANT_TYPE_VARIANT)) {
		GVariant *inner = g_variant_get_variant(v);
		pg_json_write(out, inner);
		g_variant_unref(inner);
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_VARDICT)) {
		GVariantIter it;
		const char *key;
		GVariant *val;
		gboolean first = TRUE;
		g_string_append_c(out, '{');
		g_variant_iter_init(&it, v);
		while (g_variant_iter_loop(&it, "{&sv}", &key, &val)) {
			if (!first) g_string_append_c(out, ',');
			first = FALSE;
			pg_json_string(out, key);
			g_string_append_c(out, ':');
			pg_json_write(out, val);
		}
		g_string_append_c(out, '}');
	} else if (g_variant_is_container(v)) {
		GVariantIter it;
		GVariant *child;
		gboolean first = TRUE;
		g_string_append_c(out, '[');
		g_variant_iter_init(&it, v);
		while ((child = g_variant_iter_next_value(&it))) {
			if (!first) g_string_append_c(out, ',');
			first = FALSE;
			pg_json_write(out, child);
			g_variant_unref(child);
		}
		g_string_append_c(out, ']');
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_BOOLEAN)) {
		g_string_append(out, g_variant_get_boolean(v) ? "true" : "false");
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_STRING)) {
		pg_json_string(out, g_variant_get_string(v, NULL));
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_DOUBLE)) {
		g_string_append_printf(out, "%.17g", g_variant_get_double(v));
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_INT64)) {
		g_string_append_printf(out, "%" G_GINT64_FORMAT, g_variant_get_int64(v));
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_UINT64)) {
		g_string_append_printf(out, "%" G_GUINT64_FORMAT, g_variant_get_uint64(v));
	} else if (g_variant_is_of_type(v, G_VARIANT_TYPE_INT32)) {
		g_string_append_printf(out, "%d", g_variant_get_int32(v));
	} else {
		g_string_append(out, "null");
	}
}

typedef struct { const char *p, *end; } PGJson;

static void pg_json_ws(PGJson *j) {
	while (j->p < j->end && (*j->p == ' ' || *j->p == '\n' || *j->p == '\r' || *j->p == '\t')) j->p++;
}

static int pg_hex4(const char *p, gunichar *out) {
	gunichar v = 0;
	for (int i = 0; i < 4; i++) {
		int d = g_ascii_xdigit_value(p[i]);
		if (d < 0) return 0;
		v = v << 4 | (gunichar)d;
	}
	*out = v;
	return 1;
}

static char *pg_json_parse_string(PGJson *j) {
	if (j->p >= j->end || *j->p != '"') return NULL;
	j->p++;
	GString *s = g_string_new(NULL);
	while (j->p < j->end && *j->p != '"') {
		char c = *j->p++;
		if (c != '\\') { g_string_append_c(s, c); continue; }
		if (j->p >= j->end) break;
		c = *j->p++;
		switch (c) {
		case 'b': g_string_append_c(s, '\b'); break;
		case 'f': g_string_append_c(s, '\f'); break;
		case 'n': g_string_append_c(s, '\n'); break;
		case 'r': g_string_append_c(s, '\r'); break;
		case 't': g_string_append_c(s, '\t'); break;
		case 'u': {
			gunichar u;
			if (j->end - j->p < 4 || !pg_hex4(j->p, &u)) goto fail;
			j->p += 4;
			if (u >= 0xd800 && u < 0xdc00 && j->end - j->p >= 6 && j->p[0] == '\\' && j->p[1] == 'u') {
				gunichar lo;
				if (pg_hex4(j->p + 2, &lo) && lo >= 0xdc00 && lo < 0xe000) {
					u = 0x10000 + ((u - 0xd800) << 10) + (lo - 0xdc00);
					j->p += 6;
				}
			}
			g_string_append_unichar(s, u);
			break;
		}
		default: g_string_append_c(s, c);
		}
	}
	if (j->p >= j->end) goto fail;
	j->p++;
	return g_string_free(s, FALSE);
fail:
	g_string_free(s, TRUE);
	return NULL;
}

static GVariant *pg_json_value(PGJson *j, int depth) {
	pg_json_ws(j);
	if (j->p >= j->end || depth > 32) return NULL;
	char c = *j->p;
	if (c == '{') {
		GVariantBuilder b;
		g_variant_builder_init(&b, G_VARIANT_TYPE_VARDICT);
		j->p++;
		pg_json_ws(j);
		if (j->p < j->end && *j->p == '}') { j->p++; return g_variant_builder_end(&b); }
		for (;;) {
			pg_json_ws(j);
			char *key = pg_json_parse_string(j);
			if (!key) break;
			pg_json_ws(j);
			if (j->p >= j->end || *j->p != ':') { g_free(key); break; }
			j->p++;
			GVariant *val = pg_json_value(j, depth + 1);
			if (!val) { g_free(key); break; }
			g_variant_builder_add(&b, "{sv}", key, val);
			g_free(key);
			pg_json_ws(j);
			if (j->p < j->end && *j->p == ',') { j->p++; continue; }
			if (j->p < j->end && *j->p == '}') { j->p++; return g_variant_builder_end(&b); }
			break;
		}
		g_variant_builder_clear(&b);
		return NULL;
	}
	if (c == '[') {
		GVariantBuilder b;
		g_variant_builder_init(&b, G_VARIANT_TYPE("av"));
		j->p++;
		pg_json_ws(j);
		if (j->p < j->end && *j->p == ']') { j->p++; return g_variant_builder_end(&b); }
		for (;;) {
			GVariant *val = pg_json_value(j, depth + 1);
			if (!val) break;
			g_variant_builder_add(&b, "v", val);
			pg_json_ws(j);
			if (j->p < j->end && *j->p == ',') { j->p++; continue; }
			if (j->p < j->end && *j->p == ']') { j->p++; return g_variant_builder_end(&b); }
			break;
		}
		g_variant_builder_clear(&b);
		return NULL;
	}
	if (c == '"') {
		char *s = pg_json_parse_string(j);
		if (!s) return NULL;
		GVariant *v = g_variant_new_string(s);
		g_free(s);
		return v;
	}
	if (j->end - j->p >= 4 && !strncmp(j->p, "true", 4)) { j->p += 4; return g_variant_new_boolean(TRUE); }
	if (j->end - j->p >= 5 && !strncmp(j->p, "false", 5)) { j->p += 5; return g_variant_new_boolean(FALSE); }
	if (j->end - j->p >= 4 && !strncmp(j->p, "null", 4)) { j->p += 4; return g_variant_new_string(""); }
	char buf[64];
	size_t n = 0;
	while (j->p + n < j->end && n < sizeof buf - 1 && strchr("+-0123456789.eE", j->p[n])) n++;
	if (n == 0) return NULL;
	memcpy(buf, j->p, n);
	buf[n] = 0;
	j->p += n;
	return g_variant_new_double(g_ascii_strtod(buf, NULL));
}

// pg_json_parse returns the parsed payload, or an empty a{sv} when it is empty or malformed.
static GVariant *pg_json_parse(const char *data, size_t len) {
	PGJson j = { data, data + len };
	GVariant *v = len ? pg_json_value(&j, 0) : NULL;
	if (!v) v = g_variant_new("a{sv}", NULL);
	return g_variant_ref_sink(v);
}

static const char *pg_str(GVariant *dict, const char *key) {
	const char *s = NULL;
	if (dict && g_variant_is_of_type(dict, G_VARIANT_TYPE_VARDICT) && g_variant_lookup(dict, key, "&s", &s)) return s;
	return "";
}

static double pg_num(GVariant *dict, const char *key) {
	double d = 0;
	if (dict && g_variant_is_of_type(dict, G_VARIANT_TYPE_VARDICT)) g_variant_lookup(dict, key, "d", &d);
	return d;
}

static gboolean pg_bool(GVariant *dict, const char *key) {
	gboolean b = FALSE;
	if (dict && g_variant_is_of_type(dict, G_VARIANT_TYPE_VARDICT)) g_variant_lookup(dict, key, "b", &b);
	return b;
}

// pg_json_parse_go parses a payload and writes it back. Tests use it to check the round trip.
char *pg_json_roundtrip(const char *data, size_t len) {
	GVariant *v = pg_json_parse(data, len);
	GString *out = g_string_new(NULL);
	pg_json_write(out, v);
	g_variant_unref(v);
	return g_string_free(out, FALSE);
}

// MARK: - Events
//
// Events reach Go from one thread, in order, never synchronously from a command handler or a
// capture callback.

typedef struct { int kind; char *data; size_t len; } PGEvent;

static GAsyncQueue *gEvents;
static GThread *gEventThread;

static gpointer pg_event_thread(gpointer unused) {
	for (;;) {
		PGEvent *e = g_async_queue_pop(gEvents);
		if (e->kind < 0) { g_free(e); return NULL; }
		goEvent(e->kind, e->data, e->len);
		g_free(e->data);
		g_free(e);
	}
}

// pg_emit queues an event. payload may be NULL or a floating GVariant, which it consumes.
static void pg_emit(int kind, GVariant *payload) {
	PGEvent *e = g_new0(PGEvent, 1);
	e->kind = kind;
	if (payload) {
		g_variant_ref_sink(payload);
		GString *s = g_string_new(NULL);
		pg_json_write(s, payload);
		g_variant_unref(payload);
		e->len = s->len;
		e->data = g_string_free(s, FALSE);
	}
	g_async_queue_push(gEvents, e);
}

static int64_t pg_now_ns(void) { return g_get_monotonic_time() * 1000; }

static GVariant *pg_dict(const char *first_key, ...) G_GNUC_NULL_TERMINATED;

// pg_dict builds an a{sv} from key, GVariant pairs ending with NULL.
static GVariant *pg_dict(const char *first_key, ...) {
	GVariantBuilder b;
	g_variant_builder_init(&b, G_VARIANT_TYPE_VARDICT);
	va_list ap;
	va_start(ap, first_key);
	for (const char *k = first_key; k; k = va_arg(ap, const char *)) {
		GVariant *v = va_arg(ap, GVariant *);
		if (v) g_variant_builder_add(&b, "{sv}", k, v);
	}
	va_end(ap);
	return g_variant_builder_end(&b);
}

static void pg_action(const char *kind, const char *key, const char *value) {
	pg_emit(PGEvAction, pg_dict("kind", g_variant_new_string(kind),
	                            "key", key ? g_variant_new_string(key) : NULL,
	                            "value", value ? g_variant_new_string(value) : NULL, NULL));
}

// MARK: - Displays

static GdkDisplay *gDisplay;

// pg_monitor_id is a display's ID: GTK 3 has no Wayland connector names, so it is the monitor's
// model and position in the layout.
static char *pg_monitor_id(GdkMonitor *m) {
	GdkRectangle g;
	gdk_monitor_get_geometry(m, &g);
	const char *model = gdk_monitor_get_model(m);
	return g_strdup_printf("%s@%d,%d", model && *model ? model : "display", g.x, g.y);
}

static GdkMonitor *pg_monitor(const char *id) {
	int n = gdk_display_get_n_monitors(gDisplay);
	for (int i = 0; i < n; i++) {
		GdkMonitor *m = gdk_display_get_monitor(gDisplay, i);
		char *mid = pg_monitor_id(m);
		gboolean match = g_strcmp0(mid, id) == 0;
		g_free(mid);
		if (match) return m;
	}
	return NULL;
}

static GVariant *pg_display_list(void) {
	GVariantBuilder b;
	g_variant_builder_init(&b, G_VARIANT_TYPE("av"));
	int n = gdk_display_get_n_monitors(gDisplay);
	gboolean anyPrimary = FALSE;
	for (int i = 0; i < n; i++) anyPrimary |= gdk_monitor_is_primary(gdk_display_get_monitor(gDisplay, i));
	for (int i = 0; i < n; i++) {
		GdkMonitor *m = gdk_display_get_monitor(gDisplay, i);
		GdkRectangle g;
		gdk_monitor_get_geometry(m, &g);
		int scale = gdk_monitor_get_scale_factor(m);
		char *id = pg_monitor_id(m);
		const char *man = gdk_monitor_get_manufacturer(m), *model = gdk_monitor_get_model(m);
		char *name = g_strstrip(g_strdup_printf("%s %s", man && strcmp(man, "Unknown") ? man : "",
		                                        model && strcmp(model, "Unknown") ? model : ""));
		if (!*name) { g_free(name); name = g_strdup_printf("Display %d", i + 1); }
		g_variant_builder_add(&b, "v", pg_dict(
			"id", g_variant_new_string(id),
			"name", g_variant_new_string(name),
			"width", g_variant_new_double(g.width),
			"height", g_variant_new_double(g.height),
			"pixelWidth", g_variant_new_int32(g.width * scale),
			"pixelHeight", g_variant_new_int32(g.height * scale),
			"main", g_variant_new_boolean(anyPrimary ? gdk_monitor_is_primary(m) : i == 0), NULL));
		g_free(id);
		g_free(name);
	}
	return g_variant_builder_end(&b);
}

static guint gDisplaysIdle;

static gboolean pg_emit_displays(gpointer unused) {
	gDisplaysIdle = 0;
	pg_emit(PGEvDisplays, pg_display_list());
	return G_SOURCE_REMOVE;
}

static void pg_displays_changed(void) {
	if (!gDisplaysIdle) gDisplaysIdle = g_timeout_add(200, pg_emit_displays, NULL);
}

static void pg_monitor_notify(GObject *o, GParamSpec *p, gpointer unused) { pg_displays_changed(); }

static void pg_watch_monitor(GdkMonitor *m) {
	g_signal_connect(m, "notify::geometry", G_CALLBACK(pg_monitor_notify), NULL);
	g_signal_connect(m, "notify::scale-factor", G_CALLBACK(pg_monitor_notify), NULL);
}

static void pg_monitor_added(GdkDisplay *d, GdkMonitor *m, gpointer unused) {
	pg_watch_monitor(m);
	pg_displays_changed();
}

static void pg_monitor_removed(GdkDisplay *d, GdkMonitor *m, gpointer unused) { pg_displays_changed(); }

// MARK: - Covers
//
// One layer-shell overlay surface per display holds one rectangle per cover. The surface takes no
// input; each rectangle's opacity eases towards its target over the command's fade time.

typedef struct {
	double x, y, w, h; // logical pixels on the display
	double from, to;   // opacity
	gint64 start;      // monotonic microseconds
	double dur;        // seconds
} PGCover;

typedef struct {
	char *display;
	GtkWidget *win;
	GArray *covers; // PGCover
	guint tick;
	guint gen;
} PGOverlay;

static GHashTable *gOverlays; // display ID → PGOverlay

static double pg_cover_opacity(const PGCover *c, gint64 now) {
	if (c->dur <= 0 || now >= c->start + (gint64)(c->dur * 1e6)) return c->to;
	double t = (double)(now - c->start) / (c->dur * 1e6);
	double e = t < 0.5 ? 2 * t * t : 1 - pow(-2 * t + 2, 2) / 2; // ease in and out
	return c->from + (c->to - c->from) * e;
}

static gboolean pg_overlay_draw(GtkWidget *w, cairo_t *cr, gpointer data) {
	PGOverlay *o = data;
	gint64 now = g_get_monotonic_time();
	cairo_set_operator(cr, CAIRO_OPERATOR_SOURCE);
	cairo_set_source_rgba(cr, 0, 0, 0, 0);
	cairo_paint(cr);
	cairo_set_operator(cr, CAIRO_OPERATOR_OVER);
	for (guint i = 0; i < o->covers->len; i++) {
		PGCover *c = &g_array_index(o->covers, PGCover, i);
		double a = pg_cover_opacity(c, now);
		if (a <= 0) continue;
		cairo_set_source_rgba(cr, 0.09, 0.09, 0.10, a);
		cairo_rectangle(cr, c->x, c->y, c->w, c->h);
		cairo_fill(cr);
		if (c->w >= 220 && c->h >= 40) {
			PangoLayout *l = gtk_widget_create_pango_layout(w, "Flashing covered by PSV Guard");
			int lw, lh;
			pango_layout_get_pixel_size(l, &lw, &lh);
			cairo_set_source_rgba(cr, 0.55, 0.55, 0.57, a);
			cairo_move_to(cr, c->x + (c->w - lw) / 2, c->y + (c->h - lh) / 2);
			pango_cairo_show_layout(cr, l);
			g_object_unref(l);
		}
	}
	return TRUE;
}


static gboolean pg_overlay_tick(gpointer data) {
	PGOverlay *o = data;
	gtk_widget_queue_draw(o->win);
	gint64 now = g_get_monotonic_time();
	gboolean animating = FALSE, visible = FALSE;
	for (guint i = 0; i < o->covers->len; i++) {
		PGCover *c = &g_array_index(o->covers, PGCover, i);
		if (now < c->start + (gint64)(c->dur * 1e6)) animating = TRUE;
		if (pg_cover_opacity(c, now) > 0) visible = TRUE;
	}
	if (animating) return G_SOURCE_CONTINUE;
	o->tick = 0;
	if (!visible) {
		g_array_set_size(o->covers, 0);
		gtk_widget_hide(o->win);
	}
	return G_SOURCE_REMOVE;
}

static PGOverlay *pg_overlay(const char *display, GdkMonitor *m) {
	PGOverlay *o = g_hash_table_lookup(gOverlays, display);
	if (o) {
		gtk_layer_set_monitor(GTK_WINDOW(o->win), m);
		return o;
	}
	o = g_new0(PGOverlay, 1);
	o->display = g_strdup(display);
	o->covers = g_array_new(FALSE, TRUE, sizeof(PGCover));
	o->win = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	GtkWindow *win = GTK_WINDOW(o->win);
	gtk_layer_init_for_window(win);
	gtk_layer_set_namespace(win, "psvguard-cover");
	gtk_layer_set_layer(win, GTK_LAYER_SHELL_LAYER_OVERLAY);
	gtk_layer_set_keyboard_mode(win, GTK_LAYER_SHELL_KEYBOARD_MODE_NONE);
	gtk_layer_set_exclusive_zone(win, -1);
	for (int e = 0; e < GTK_LAYER_SHELL_EDGE_ENTRY_NUMBER; e++) gtk_layer_set_anchor(win, e, TRUE);
	gtk_layer_set_monitor(win, m);
	GdkVisual *v = gdk_screen_get_rgba_visual(gtk_widget_get_screen(o->win));
	if (v) gtk_widget_set_visual(o->win, v);
	gtk_widget_set_app_paintable(o->win, TRUE);
	g_signal_connect(o->win, "draw", G_CALLBACK(pg_overlay_draw), o);
	// An empty input region: clicks and touches go to the windows below. GTK applies the widget's
	// shape each time the surface is mapped.
	cairo_region_t *none = cairo_region_create();
	gtk_widget_input_shape_combine_region(o->win, none);
	cairo_region_destroy(none);
	g_hash_table_insert(gOverlays, o->display, o);
	return o;
}

// pg_overlay_set shows exactly these rectangles, normalised with a top-left origin.
static void pg_overlay_set(const char *display, const double *rects, guint32 count, double fadeIn, double fadeOut) {
	GdkMonitor *m = pg_monitor(display);
	PGOverlay *o = g_hash_table_lookup(gOverlays, display);
	// Without layer-shell (GNOME) there are no covers; capabilities report canOverlay false.
	if (!m || !gtk_layer_is_supported() || (!o && count == 0)) {
		pg_emit(PGEvCoversShown, pg_dict("display", g_variant_new_string(display), "rects", g_variant_new_int32(0),
		                                 "at", g_variant_new_int64(pg_now_ns()), NULL));
		return;
	}
	o = pg_overlay(display, m);
	o->gen++;
	GdkRectangle g;
	gdk_monitor_get_geometry(m, &g);
	double scale = gdk_monitor_get_scale_factor(m);
	gint64 now = g_get_monotonic_time();
	if (o->covers->len < count) g_array_set_size(o->covers, count);
	for (guint i = 0; i < o->covers->len; i++) {
		PGCover *c = &g_array_index(o->covers, PGCover, i);
		double cur = pg_cover_opacity(c, now);
		if (i < count) {
			const double *r = rects + 4 * i;
			PGRect p = pg_norm_to_px(r[0], r[1], r[2], r[3], g.width, g.height, scale);
			c->x = p.x; c->y = p.y; c->w = p.w; c->h = p.h;
			if (c->to < PG_COVER_OPACITY || cur < PG_COVER_OPACITY - 0.01) {
				c->from = cur; c->to = PG_COVER_OPACITY; c->start = now; c->dur = fadeIn;
			}
		} else if (c->to > 0) {
			c->from = cur; c->to = 0; c->start = now; c->dur = fadeOut;
		}
	}
	if (count > 0) gtk_widget_show(o->win);
	gtk_widget_queue_draw(o->win);
	if (!o->tick) o->tick = g_timeout_add(16, pg_overlay_tick, o);
	pg_emit(PGEvCoversShown, pg_dict("display", g_variant_new_string(display), "rects", g_variant_new_int32((gint32)count),
	                                 "at", g_variant_new_int64(pg_now_ns()), NULL));
}

// The packed layout of EncodeCovers in contract.go.
static void pg_set_covers(const uint8_t *p, size_t n) {
	if (n < 2) return;
	uint16_t idLen;
	memcpy(&idLen, p, 2);
	if (n < 2 + (size_t)idLen + 20) return;
	char *display = g_strndup((const char *)p + 2, idLen);
	const uint8_t *q = p + 2 + idLen;
	double fadeIn, fadeOut;
	uint32_t count;
	memcpy(&fadeIn, q, 8);
	memcpy(&fadeOut, q + 8, 8);
	memcpy(&count, q + 16, 4);
	q += 20;
	if ((size_t)(p + n - q) == (size_t)count * 32) {
		double *rects = g_new(double, (gsize)count * 4 + 1);
		memcpy(rects, q, (size_t)count * 32);
		pg_overlay_set(display, rects, count, fadeIn, fadeOut);
		g_free(rects);
	}
	g_free(display);
}

static void pg_clear_covers(GVariant *d) {
	const char *display = pg_str(d, "display");
	GHashTableIter it;
	gpointer key, val;
	GPtrArray *ids = g_ptr_array_new_with_free_func(g_free);
	g_hash_table_iter_init(&it, gOverlays);
	while (g_hash_table_iter_next(&it, &key, &val)) {
		if (!*display || !strcmp(display, key)) g_ptr_array_add(ids, g_strdup(key));
	}
	for (guint i = 0; i < ids->len; i++) pg_overlay_set(ids->pdata[i], NULL, 0, 0.15, 1.0);
	g_ptr_array_free(ids, TRUE);
}

// MARK: - Panel and menu bar item

typedef struct {
	GtkWidget *panel, *title, *detail, *toggle, *permission, *settings, *stats, *disclaimer;
	AppIndicator *indicator;
	GtkWidget *menuStatus, *menuToggle;
	gboolean updating; // controls are being rebuilt: their signals are not user actions
	gboolean layered;
} PGUI;

static PGUI gUI;

static void pg_on_toggle(GtkWidget *w, gpointer unused) { pg_action("toggle", NULL, NULL); }
static void pg_on_permission(GtkWidget *w, gpointer unused) { pg_action("openPermission", NULL, NULL); }
static void pg_on_quit(GtkWidget *w, gpointer unused) { pg_action("quit", NULL, NULL); }
static void pg_on_reset(GtkWidget *w, gpointer unused) { pg_action("resetSettings", NULL, NULL); }

static void pg_show_panel(void) {
	gtk_widget_show_all(gUI.panel);
	gtk_window_present(GTK_WINDOW(gUI.panel));
}

static void pg_hide_panel(void) { gtk_widget_hide(gUI.panel); }

static void pg_on_show_panel(GtkWidget *w, gpointer unused) { pg_show_panel(); }
static void pg_on_hide_panel(GtkWidget *w, gpointer unused) { pg_hide_panel(); }

static gboolean pg_on_panel_key(GtkWidget *w, GdkEventKey *e, gpointer unused) {
	if (e->keyval == GDK_KEY_Escape) { pg_hide_panel(); return TRUE; }
	return FALSE;
}

static gboolean pg_on_panel_delete(GtkWidget *w, GdkEvent *e, gpointer unused) {
	pg_hide_panel();
	return TRUE;
}

static GtkWidget *pg_label(const char *text, const char *css_class, gboolean wrap) {
	GtkWidget *l = gtk_label_new(text);
	gtk_label_set_xalign(GTK_LABEL(l), 0);
	if (wrap) {
		gtk_label_set_line_wrap(GTK_LABEL(l), TRUE);
		gtk_label_set_max_width_chars(GTK_LABEL(l), 46);
	}
	if (css_class) gtk_style_context_add_class(gtk_widget_get_style_context(l), css_class);
	return l;
}

static void pg_setup_ui(void) {
	GtkCssProvider *css = gtk_css_provider_new();
	gtk_css_provider_load_from_data(css,
		".pg-panel { padding: 14px; }"
		".pg-title { font-weight: bold; font-size: 15px; }"
		".pg-dim { opacity: 0.7; font-size: 12px; }"
		".pg-small { opacity: 0.7; font-size: 11px; }"
		".pg-mono { font-family: monospace; font-size: 10px; opacity: 0.7; }", -1, NULL);
	gtk_style_context_add_provider_for_screen(gdk_screen_get_default(), GTK_STYLE_PROVIDER(css),
	                                          GTK_STYLE_PROVIDER_PRIORITY_APPLICATION);
	g_object_unref(css);

	gUI.panel = gtk_window_new(GTK_WINDOW_TOPLEVEL);
	GtkWindow *pw = GTK_WINDOW(gUI.panel);
	gtk_window_set_title(pw, "PSV Guard");
	gtk_window_set_default_size(pw, 380, -1);
	gtk_window_set_resizable(pw, FALSE);
	gtk_window_set_focus_on_map(pw, FALSE);
	gtk_window_set_keep_above(pw, TRUE);
	gtk_window_set_skip_taskbar_hint(pw, TRUE);
	gUI.layered = gtk_layer_is_supported();
	if (gUI.layered) {
		// A drop-down below the bar on compositors with layer-shell, so tiling leaves it alone.
		gtk_layer_init_for_window(pw);
		gtk_layer_set_namespace(pw, "psvguard-panel");
		gtk_layer_set_layer(pw, GTK_LAYER_SHELL_LAYER_TOP);
		gtk_layer_set_keyboard_mode(pw, GTK_LAYER_SHELL_KEYBOARD_MODE_ON_DEMAND);
		gtk_layer_set_anchor(pw, GTK_LAYER_SHELL_EDGE_TOP, TRUE);
		gtk_layer_set_anchor(pw, GTK_LAYER_SHELL_EDGE_RIGHT, TRUE);
		gtk_layer_set_margin(pw, GTK_LAYER_SHELL_EDGE_TOP, 8);
		gtk_layer_set_margin(pw, GTK_LAYER_SHELL_EDGE_RIGHT, 8);
	}
	g_signal_connect(gUI.panel, "key-press-event", G_CALLBACK(pg_on_panel_key), NULL);
	g_signal_connect(gUI.panel, "delete-event", G_CALLBACK(pg_on_panel_delete), NULL);

	GtkWidget *root = gtk_box_new(GTK_ORIENTATION_VERTICAL, 8);
	gtk_style_context_add_class(gtk_widget_get_style_context(root), "pg-panel");
	gtk_container_add(GTK_CONTAINER(gUI.panel), root);

	gUI.title = pg_label("Starting…", "pg-title", TRUE);
	gUI.detail = pg_label("", "pg-dim", TRUE);
	gtk_box_pack_start(GTK_BOX(root), gUI.title, FALSE, FALSE, 0);
	gtk_box_pack_start(GTK_BOX(root), gUI.detail, FALSE, FALSE, 0);

	GtkWidget *buttons = gtk_box_new(GTK_ORIENTATION_HORIZONTAL, 8);
	gUI.toggle = gtk_button_new_with_label("Turn Off");
	gUI.permission = gtk_button_new_with_label("Share Screen…");
	gtk_widget_set_no_show_all(gUI.permission, TRUE);
	GtkWidget *quit = gtk_button_new_with_label("Quit");
	GtkWidget *hide = gtk_button_new_with_label("Close");
	g_signal_connect(gUI.toggle, "clicked", G_CALLBACK(pg_on_toggle), NULL);
	g_signal_connect(gUI.permission, "clicked", G_CALLBACK(pg_on_permission), NULL);
	g_signal_connect(quit, "clicked", G_CALLBACK(pg_on_quit), NULL);
	g_signal_connect(hide, "clicked", G_CALLBACK(pg_on_hide_panel), NULL);
	gtk_box_pack_start(GTK_BOX(buttons), gUI.toggle, FALSE, FALSE, 0);
	gtk_box_pack_start(GTK_BOX(buttons), gUI.permission, FALSE, FALSE, 0);
	gtk_box_pack_start(GTK_BOX(buttons), quit, FALSE, FALSE, 0);
	gtk_box_pack_end(GTK_BOX(buttons), hide, FALSE, FALSE, 0);
	gtk_box_pack_start(GTK_BOX(root), buttons, FALSE, FALSE, 0);

	gtk_box_pack_start(GTK_BOX(root), gtk_separator_new(GTK_ORIENTATION_HORIZONTAL), FALSE, FALSE, 4);
	gtk_box_pack_start(GTK_BOX(root), pg_label("Sensitivity", "pg-title", FALSE), FALSE, FALSE, 0);
	gUI.settings = gtk_grid_new();
	gtk_grid_set_row_spacing(GTK_GRID(gUI.settings), 6);
	gtk_grid_set_column_spacing(GTK_GRID(gUI.settings), 10);
	gtk_box_pack_start(GTK_BOX(root), gUI.settings, FALSE, FALSE, 0);
	GtkWidget *reset = gtk_button_new_with_label("Reset to Defaults");
	gtk_widget_set_tooltip_text(reset, "Restore every setting to its default; protection stays on or off as it is");
	gtk_widget_set_halign(reset, GTK_ALIGN_START);
	g_signal_connect(reset, "clicked", G_CALLBACK(pg_on_reset), NULL);
	gtk_box_pack_start(GTK_BOX(root), reset, FALSE, FALSE, 0);

	gtk_box_pack_start(GTK_BOX(root), gtk_separator_new(GTK_ORIENTATION_HORIZONTAL), FALSE, FALSE, 4);
	gUI.stats = pg_label("", "pg-mono", TRUE);
	gUI.disclaimer = pg_label("", "pg-small", TRUE);
	gtk_box_pack_start(GTK_BOX(root), gUI.stats, FALSE, FALSE, 0);
	gtk_box_pack_start(GTK_BOX(root), gUI.disclaimer, FALSE, FALSE, 0);

	GtkWidget *menu = gtk_menu_new();
	gUI.menuStatus = gtk_menu_item_new_with_label("Starting…");
	gtk_widget_set_sensitive(gUI.menuStatus, FALSE);
	gUI.menuToggle = gtk_menu_item_new_with_label("Turn Protection Off");
	GtkWidget *show = gtk_menu_item_new_with_label("Show Panel…");
	GtkWidget *quitItem = gtk_menu_item_new_with_label("Quit PSV Guard");
	g_signal_connect(gUI.menuToggle, "activate", G_CALLBACK(pg_on_toggle), NULL);
	g_signal_connect(show, "activate", G_CALLBACK(pg_on_show_panel), NULL);
	g_signal_connect(quitItem, "activate", G_CALLBACK(pg_on_quit), NULL);
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), gUI.menuStatus);
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), gtk_separator_menu_item_new());
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), gUI.menuToggle);
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), show);
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), gtk_separator_menu_item_new());
	gtk_menu_shell_append(GTK_MENU_SHELL(menu), quitItem);
	gtk_widget_show_all(menu);

	gUI.indicator = app_indicator_new("psvguard", "security-high-symbolic", APP_INDICATOR_CATEGORY_APPLICATION_STATUS);
	app_indicator_set_title(gUI.indicator, "PSV Guard");
	app_indicator_set_menu(gUI.indicator, GTK_MENU(menu));
	app_indicator_set_secondary_activate_target(gUI.indicator, show);
	app_indicator_set_status(gUI.indicator, APP_INDICATOR_STATUS_ACTIVE);
}

static void pg_apply_status(GVariant *st) {
	const char *state = pg_str(st, "state"), *title = pg_str(st, "title");
	const char *icon = "security-high-symbolic";
	if (!strcmp(state, "covering") || !strcmp(state, "warning") || !strcmp(state, "error")) icon = "dialog-warning-symbolic";
	else if (!strcmp(state, "permission")) icon = "security-medium-symbolic";
	else if (!strcmp(state, "paused")) icon = "security-low-symbolic";
	char *desc = g_strdup_printf("PSV Guard — %s", title);
	app_indicator_set_icon_full(gUI.indicator, icon, desc);
	app_indicator_set_title(gUI.indicator, desc);
	g_free(desc);

	gboolean on = pg_bool(st, "enabled");
	gtk_label_set_text(GTK_LABEL(gUI.title), title);
	gtk_label_set_text(GTK_LABEL(gUI.detail), pg_str(st, "detail"));
	gtk_button_set_label(GTK_BUTTON(gUI.toggle), on ? "Turn Off" : "Turn On");
	gtk_widget_set_visible(gUI.permission, pg_bool(st, "permissionButton"));
	gtk_menu_item_set_label(GTK_MENU_ITEM(gUI.menuStatus), title);
	gtk_menu_item_set_label(GTK_MENU_ITEM(gUI.menuToggle), on ? "Turn Protection Off" : "Turn Protection On");
	const char *ver = pg_str(st, "version");
	char *disc = *ver ? g_strdup_printf("%s\nPSV Guard %s", pg_str(st, "disclaimer"), ver) : g_strdup(pg_str(st, "disclaimer"));
	gtk_label_set_text(GTK_LABEL(gUI.disclaimer), disc);
	g_free(disc);
}

static void pg_apply_stats(GVariant *st) {
	GString *s = g_string_new(NULL);
	GVariant *displays = NULL;
	if (g_variant_lookup(st, "displays", "@av", &displays)) {
		GVariantIter it;
		GVariant *d;
		g_variant_iter_init(&it, displays);
		while (g_variant_iter_loop(&it, "v", &d)) {
			if (s->len) g_string_append_c(s, '\n');
			g_string_append_printf(s, "%s %s · %.0f fps (captured %.0f) · watch %.2f ms\nhot %.1f%% · covers %.0f · dropped %.0f · repeated %.0f · misses %.0f",
			                       pg_str(d, "name"), pg_str(d, "size"), pg_num(d, "fps"), pg_num(d, "captured"),
			                       pg_num(d, "watchMs"), pg_num(d, "hotArea"), pg_num(d, "covers"), pg_num(d, "dropped"),
			                       pg_num(d, "repeated"), pg_num(d, "misses"));
		}
		g_variant_unref(displays);
	}
	gtk_label_set_text(GTK_LABEL(gUI.stats), s->str);
	g_string_free(s, TRUE);
}

static const char *pg_key(gpointer w) { return g_object_get_data(G_OBJECT(w), "pg-key"); }

static void pg_on_check(GtkToggleButton *b, gpointer unused) {
	if (!gUI.updating) pg_action("setting", pg_key(b), gtk_toggle_button_get_active(b) ? "true" : "false");
}

static void pg_on_combo(GtkComboBox *c, gpointer unused) {
	const char *id = gtk_combo_box_get_active_id(c);
	if (!gUI.updating && id) pg_action("setting", pg_key(c), id);
}

// Sliders and number fields report a change once they have been still for a moment.
static gboolean pg_send_range(gpointer w) {
	g_object_set_data(G_OBJECT(w), "pg-timer", NULL);
	char buf[32];
	if (GTK_IS_SPIN_BUTTON(w)) g_snprintf(buf, sizeof buf, "%d", gtk_spin_button_get_value_as_int(GTK_SPIN_BUTTON(w)));
	else g_snprintf(buf, sizeof buf, "%.1f", gtk_range_get_value(GTK_RANGE(w)));
	pg_action("setting", pg_key(w), buf);
	return G_SOURCE_REMOVE;
}

static void pg_on_range(GtkWidget *w, gpointer unused) {
	if (gUI.updating) return;
	guint t = GPOINTER_TO_UINT(g_object_get_data(G_OBJECT(w), "pg-timer"));
	if (t) g_source_remove(t);
	g_object_set_data(G_OBJECT(w), "pg-timer", GUINT_TO_POINTER(g_timeout_add(400, pg_send_range, w)));
}

static void pg_on_control_destroy(GtkWidget *w, gpointer unused) {
	guint t = GPOINTER_TO_UINT(g_object_get_data(G_OBJECT(w), "pg-timer"));
	if (t) g_source_remove(t);
}

static void pg_apply_controls(GVariant *controls) {
	if (!g_variant_is_of_type(controls, G_VARIANT_TYPE("av"))) return;
	gUI.updating = TRUE;
	GList *old = gtk_container_get_children(GTK_CONTAINER(gUI.settings));
	for (GList *l = old; l; l = l->next) gtk_widget_destroy(l->data);
	g_list_free(old);
	GVariantIter it;
	GVariant *c;
	int row = 0;
	g_variant_iter_init(&it, controls);
	while (g_variant_iter_loop(&it, "v", &c)) {
		const char *key = pg_str(c, "key"), *kind = pg_str(c, "kind"), *label = pg_str(c, "label");
		const char *value = pg_str(c, "value"), *help = pg_str(c, "help");
		GtkWidget *w = NULL;
		if (!strcmp(kind, "bool")) {
			w = gtk_check_button_new_with_label(label);
			gtk_toggle_button_set_active(GTK_TOGGLE_BUTTON(w), !strcmp(value, "true"));
			g_signal_connect(w, "toggled", G_CALLBACK(pg_on_check), NULL);
			gtk_grid_attach(GTK_GRID(gUI.settings), w, 0, row, 2, 1);
		} else {
			GtkWidget *l = pg_label(label, NULL, FALSE);
			gtk_widget_set_tooltip_text(l, help);
			gtk_grid_attach(GTK_GRID(gUI.settings), l, 0, row, 1, 1);
			if (!strcmp(kind, "float")) {
				w = gtk_scale_new_with_range(GTK_ORIENTATION_HORIZONTAL, pg_num(c, "min"), pg_num(c, "max"), 0.1);
				gtk_scale_set_digits(GTK_SCALE(w), 1);
				gtk_scale_set_value_pos(GTK_SCALE(w), GTK_POS_RIGHT);
				gtk_widget_set_size_request(w, 160, -1);
				gtk_range_set_value(GTK_RANGE(w), g_ascii_strtod(value, NULL));
				g_signal_connect(w, "value-changed", G_CALLBACK(pg_on_range), NULL);
			} else if (!strcmp(kind, "int")) {
				w = gtk_spin_button_new_with_range(pg_num(c, "min"), pg_num(c, "max"), 1);
				gtk_spin_button_set_value(GTK_SPIN_BUTTON(w), g_ascii_strtod(value, NULL));
				g_signal_connect(w, "value-changed", G_CALLBACK(pg_on_range), NULL);
			} else if (!strcmp(kind, "choice")) {
				w = gtk_combo_box_text_new();
				GVariant *choices = NULL;
				if (g_variant_lookup(c, "choices", "@av", &choices)) {
					GVariantIter ci;
					GVariant *ch;
					g_variant_iter_init(&ci, choices);
					while (g_variant_iter_loop(&ci, "v", &ch))
						gtk_combo_box_text_append(GTK_COMBO_BOX_TEXT(w), pg_str(ch, "value"), pg_str(ch, "label"));
					g_variant_unref(choices);
				}
				gtk_combo_box_set_active_id(GTK_COMBO_BOX(w), value);
				g_signal_connect(w, "changed", G_CALLBACK(pg_on_combo), NULL);
			}
			if (w) {
				gtk_widget_set_hexpand(w, TRUE);
				gtk_grid_attach(GTK_GRID(gUI.settings), w, 1, row, 1, 1);
			}
		}
		if (w) {
			g_object_set_data_full(G_OBJECT(w), "pg-key", g_strdup(key), g_free);
			g_signal_connect(w, "destroy", G_CALLBACK(pg_on_control_destroy), NULL);
			gtk_widget_set_tooltip_text(w, help);
		}
		row++;
	}
	gtk_widget_show_all(gUI.settings);
	gUI.updating = FALSE;
}

// MARK: - D-Bus: notifications, sleep, Hyprland workspaces

static GDBusConnection *gSession;

static guint32 gNotification; // the last notification's ID, which the next one replaces

static void pg_on_notified(GObject *src, GAsyncResult *res, gpointer unused) {
	GVariant *r = g_dbus_connection_call_finish(G_DBUS_CONNECTION(src), res, NULL);
	if (!r) return;
	g_variant_get(r, "(u)", &gNotification);
	g_variant_unref(r);
}

// pg_notify shows a desktop notification with critical urgency: notification servers (GNOME's
// among them) hide ordinary banners over full-screen apps, which is where flashing plays. Critical
// notifications stay until dismissed, so each one replaces the one before.
static void pg_notify(GVariant *d) {
	if (!gSession) return;
	GVariantBuilder hints;
	g_variant_builder_init(&hints, G_VARIANT_TYPE_VARDICT);
	g_variant_builder_add(&hints, "{sv}", "urgency", g_variant_new_byte(2));
	g_variant_builder_add(&hints, "{sv}", "desktop-entry", g_variant_new_string("psvguard"));
	g_dbus_connection_call(gSession, "org.freedesktop.Notifications", "/org/freedesktop/Notifications",
	                       "org.freedesktop.Notifications", "Notify",
	                       g_variant_new("(susssas@a{sv}i)", "PSV Guard", gNotification, "dialog-warning",
	                                     pg_str(d, "title"), pg_str(d, "body"), NULL, g_variant_builder_end(&hints), -1),
	                       G_VARIANT_TYPE("(u)"), G_DBUS_CALL_FLAGS_NONE, -1, NULL, pg_on_notified, NULL);
}

static void pg_on_sleep(GDBusConnection *c, const char *sender, const char *path, const char *iface,
                        const char *signal, GVariant *params, gpointer unused) {
	gboolean sleeping = FALSE;
	g_variant_get(params, "(b)", &sleeping);
	pg_emit(PGEvPower, pg_dict("sleeping", g_variant_new_boolean(sleeping), NULL));
}

static void pg_watch_sleep(void) {
	GDBusConnection *sys = g_bus_get_sync(G_BUS_TYPE_SYSTEM, NULL, NULL);
	if (!sys) return; // no system bus (a container): sleep is not reported
	g_dbus_connection_signal_subscribe(sys, "org.freedesktop.login1", "org.freedesktop.login1.Manager",
	                                   "PrepareForSleep", "/org/freedesktop/login1", NULL,
	                                   G_DBUS_SIGNAL_FLAGS_NONE, pg_on_sleep, NULL, NULL);
}

static void pg_hypr_read(GObject *src, GAsyncResult *res, gpointer data) {
	GDataInputStream *in = G_DATA_INPUT_STREAM(src);
	char *line = g_data_input_stream_read_line_finish_utf8(in, res, NULL, NULL);
	if (!line) { g_object_unref(in); return; }
	// Switching workspaces and entering or leaving full screen animate whole displays.
	if (g_str_has_prefix(line, "workspace>>") || g_str_has_prefix(line, "fullscreen>>"))
		pg_emit(PGEvTransition, pg_dict("at", g_variant_new_int64(pg_now_ns()), NULL));
	g_free(line);
	g_data_input_stream_read_line_async(in, G_PRIORITY_DEFAULT, NULL, pg_hypr_read, NULL);
}

static void pg_hypr_connected(GObject *src, GAsyncResult *res, gpointer data) {
	GSocketConnection *conn = g_socket_client_connect_finish(G_SOCKET_CLIENT(src), res, NULL);
	g_object_unref(src);
	if (!conn) return;
	GDataInputStream *in = g_data_input_stream_new(g_io_stream_get_input_stream(G_IO_STREAM(conn)));
	g_object_set_data_full(G_OBJECT(in), "pg-conn", conn, g_object_unref);
	g_data_input_stream_read_line_async(in, G_PRIORITY_DEFAULT, NULL, pg_hypr_read, NULL);
}

static void pg_watch_hyprland(void) {
	const char *sig = g_getenv("HYPRLAND_INSTANCE_SIGNATURE"), *rt = g_getenv("XDG_RUNTIME_DIR");
	if (!sig || !rt) return;
	char *path = g_strdup_printf("%s/hypr/%s/.socket2.sock", rt, sig);
	GSocketAddress *addr = g_unix_socket_address_new(path);
	g_socket_client_connect_async(g_socket_client_new(), G_SOCKET_CONNECTABLE(addr), NULL, pg_hypr_connected, NULL);
	g_object_unref(addr);
	g_free(path);
}

static gboolean pg_has_tray_host(void) {
	if (!gSession) return FALSE;
	GVariant *r = g_dbus_connection_call_sync(gSession, "org.freedesktop.DBus", "/org/freedesktop/DBus",
	                                          "org.freedesktop.DBus", "NameHasOwner",
	                                          g_variant_new("(s)", "org.kde.StatusNotifierWatcher"),
	                                          G_VARIANT_TYPE("(b)"), G_DBUS_CALL_FLAGS_NONE, 1000, NULL, NULL);
	gboolean has = FALSE;
	if (r) { g_variant_get(r, "(b)", &has); g_variant_unref(r); }
	return has;
}

// MARK: - Capture: the screen cast portal and PipeWire
//
// Each approval in the share dialog is a portal session holding one or more monitors; some
// dialogs offer only one monitor at a time, so a display that no session shows gets a dialog of
// its own, once per run. Each session keeps a restore token, so later runs skip the dialogs.

typedef struct {
	guint32 node;
	int x, y, w, h; // logical position and size, -1 when the portal does not say
} PGPortalStream;

typedef struct {
	int refs;
	gboolean active, removed;
	int step;
	char *restoreToken; // given to SelectSources, then the one Start returned
	char *path;         // session object path
	char *requestPath;
	guint requestSub, closedSub;
	GArray *streams;    // PGPortalStream
	int fd;
	struct pw_core *core;
} PGSession;

typedef struct {
	guint64 id;
	char *display;
	uintptr_t handle;
	PGSession *session;
	struct pw_stream *stream;
	struct spa_hook listener;
	struct spa_video_info_raw info;
	int format; // frame.Format
	gboolean started, failed;
} PGStream;

static struct pw_thread_loop *gLoop;
static struct pw_context *gContext;
static GHashTable *gStreams;  // stream ID → PGStream
static GPtrArray *gSessions;  // PGSession, each holding a reference
static GPtrArray *gTokens;    // restore tokens loaded at start, used by the first request
static GHashTable *gAsked;    // display IDs that got a share dialog of their own
static char *gStatePath;
static gboolean gDenied;      // the user cancelled the last share dialog

static GVariant *pg_stream_info(PGStream *s, const char *error) {
	return pg_dict("stream", g_variant_new_uint64(s->id), "display", g_variant_new_string(s->display),
	               "error", error ? g_variant_new_string(error) : NULL, NULL);
}

static gboolean pg_any_session(gboolean active) {
	for (guint i = 0; i < gSessions->len; i++)
		if (((PGSession *)gSessions->pdata[i])->active == active) return TRUE;
	return FALSE;
}

static void pg_emit_permission(void) {
	const char *state = "needsConsent";
	if (pg_any_session(TRUE)) state = "granted";
	else if (gDenied && gSessions->len == 0) state = "denied";
	pg_emit(PGEvPermission, pg_dict("state", g_variant_new_string(state), NULL));
}

static void pg_load_tokens(void) {
	char *data = NULL;
	gsize len = 0;
	if (!g_file_get_contents(gStatePath, &data, &len, NULL)) return;
	GVariant *v = pg_json_parse(data, len);
	GVariant *list = NULL;
	if (g_variant_lookup(v, "portalRestoreTokens", "@av", &list)) {
		GVariantIter it;
		GVariant *t;
		g_variant_iter_init(&it, list);
		while (g_variant_iter_loop(&it, "v", &t))
			if (g_variant_is_of_type(t, G_VARIANT_TYPE_STRING) && *g_variant_get_string(t, NULL))
				g_ptr_array_add(gTokens, g_variant_dup_string(t, NULL));
		g_variant_unref(list);
	}
	g_variant_unref(v);
	g_free(data);
}

// pg_save_tokens writes the restore tokens of the current sessions.
static void pg_save_tokens(void) {
	GVariantBuilder b;
	g_variant_builder_init(&b, G_VARIANT_TYPE("av"));
	for (guint i = 0; i < gSessions->len; i++) {
		PGSession *s = gSessions->pdata[i];
		if (s->restoreToken) g_variant_builder_add(&b, "v", g_variant_new_string(s->restoreToken));
	}
	GVariant *v = g_variant_ref_sink(pg_dict("portalRestoreTokens", g_variant_builder_end(&b), NULL));
	GString *out = g_string_new(NULL);
	pg_json_write(out, v);
	g_string_append_c(out, '\n');
	g_variant_unref(v);
	char *dir = g_path_get_dirname(gStatePath);
	g_mkdir_with_parents(dir, 0700);
	g_free(dir);
	g_file_set_contents_full(gStatePath, out->str, (gssize)out->len, G_FILE_SET_CONTENTS_CONSISTENT, 0600, NULL);
	g_string_free(out, TRUE);
}

static void pg_session_unref(PGSession *s) {
	if (--s->refs > 0) return;
	g_free(s->restoreToken);
	g_free(s->path);
	g_free(s->requestPath);
	g_array_free(s->streams, TRUE);
	g_free(s);
}

static void pg_stop_stream(PGStream *s);

// pg_session_remove fails the session's streams, disconnects it and drops it.
static void pg_session_remove(PGSession *s, const char *why) {
	if (s->removed) return;
	s->removed = TRUE;
	GList *list = g_hash_table_get_values(gStreams);
	for (GList *l = list; l; l = l->next) {
		PGStream *st = l->data;
		if (st->session != s) continue;
		if (!st->failed) {
			st->failed = TRUE;
			pg_emit(PGEvStreamFailed, pg_stream_info(st, why));
		}
		pg_stop_stream(st);
	}
	g_list_free(list);
	if (s->requestSub) g_dbus_connection_signal_unsubscribe(gSession, s->requestSub);
	if (s->closedSub) g_dbus_connection_signal_unsubscribe(gSession, s->closedSub);
	s->requestSub = s->closedSub = 0;
	if (s->core) {
		pw_thread_loop_lock(gLoop);
		pw_core_disconnect(s->core);
		pw_thread_loop_unlock(gLoop);
		s->core = NULL;
	}
	if (s->fd >= 0) close(s->fd);
	s->fd = -1;
	g_ptr_array_remove(gSessions, s);
	pg_session_unref(s);
}

// pg_session_close asks the portal to end the session too.
static void pg_session_close(PGSession *s) {
	if (s->path)
		g_dbus_connection_call(gSession, "org.freedesktop.portal.Desktop", s->path, "org.freedesktop.portal.Session",
		                       "Close", NULL, NULL, G_DBUS_CALL_FLAGS_NONE, -1, NULL, NULL, NULL);
	pg_session_remove(s, "Screen sharing stopped.");
}

static void pg_close_all(void) {
	while (gSessions->len) pg_session_close(gSessions->pdata[0]);
}

static void pg_session_fail(PGSession *s, gboolean cancelled) {
	if (s->removed) return;
	if (cancelled) gDenied = TRUE;
	pg_session_remove(s, "Screen sharing stopped.");
	pg_save_tokens();
	pg_emit_permission();
}

static void pg_on_closed(GDBusConnection *c, const char *sender, const char *path, const char *iface,
                         const char *signal, GVariant *params, gpointer data) {
	PGSession *s = data;
	pg_session_remove(s, "Screen sharing stopped.");
	pg_save_tokens();
	pg_emit_permission();
}

static void pg_session_step(PGSession *s, GVariant *results);

static void pg_on_response(GDBusConnection *c, const char *sender, const char *path, const char *iface,
                           const char *signal, GVariant *params, gpointer data) {
	PGSession *s = data;
	guint32 code;
	GVariant *results;
	g_variant_get(params, "(u@a{sv})", &code, &results);
	g_dbus_connection_signal_unsubscribe(gSession, s->requestSub);
	s->requestSub = 0;
	if (code != 0) pg_session_fail(s, code == 1);
	else pg_session_step(s, results);
	g_variant_unref(results);
}

static void pg_on_call(GObject *src, GAsyncResult *res, gpointer data) {
	PGSession *s = data;
	GError *err = NULL;
	GVariant *r = g_dbus_connection_call_finish(G_DBUS_CONNECTION(src), res, &err);
	if (r) g_variant_unref(r);
	else {
		g_printerr("psvguard: screen cast portal: %s\n", err->message);
		g_error_free(err);
		pg_session_fail(s, FALSE);
	}
	pg_session_unref(s);
}

// The portal answers each call with a Response signal on a request object whose path the caller
// derives from its unique bus name and a token.
static char *pg_request_path(const char *token) {
	char *sender = g_strdup(g_dbus_connection_get_unique_name(gSession) + 1);
	for (char *p = sender; *p; p++) if (*p == '.') *p = '_';
	char *path = g_strdup_printf("/org/freedesktop/portal/desktop/request/%s/%s", sender, token);
	g_free(sender);
	return path;
}

static int gTokenCounter;

// pg_session_call calls a ScreenCast method, after subscribing to its response. args holds the
// arguments before the options, or is NULL.
static void pg_session_call(PGSession *s, const char *method, GVariant *args, GVariantBuilder *opts) {
	char *token = g_strdup_printf("psvguard%d", ++gTokenCounter);
	g_variant_builder_add(opts, "{sv}", "handle_token", g_variant_new_string(token));
	g_free(s->requestPath);
	s->requestPath = pg_request_path(token);
	g_free(token);
	s->requestSub = g_dbus_connection_signal_subscribe(gSession, "org.freedesktop.portal.Desktop",
	                                                   "org.freedesktop.portal.Request", "Response", s->requestPath,
	                                                   NULL, G_DBUS_SIGNAL_FLAGS_NO_MATCH_RULE, pg_on_response, s, NULL);
	GVariantBuilder all;
	g_variant_builder_init(&all, G_VARIANT_TYPE_TUPLE);
	if (args) {
		GVariantIter it;
		GVariant *child;
		g_variant_ref_sink(args);
		g_variant_iter_init(&it, args);
		while ((child = g_variant_iter_next_value(&it))) {
			g_variant_builder_add_value(&all, child);
			g_variant_unref(child);
		}
		g_variant_unref(args);
	}
	g_variant_builder_add_value(&all, g_variant_builder_end(opts));
	s->refs++;
	g_dbus_connection_call(gSession, "org.freedesktop.portal.Desktop", "/org/freedesktop/portal/desktop",
	                       "org.freedesktop.portal.ScreenCast", method, g_variant_builder_end(&all), NULL,
	                       G_DBUS_CALL_FLAGS_NONE, -1, NULL, pg_on_call, s);
}

static void pg_on_remote(GObject *src, GAsyncResult *res, gpointer data) {
	PGSession *s = data;
	GUnixFDList *fds = NULL;
	GError *err = NULL;
	GVariant *r = g_dbus_connection_call_with_unix_fd_list_finish(G_DBUS_CONNECTION(src), &fds, res, &err);
	if (!r) {
		g_printerr("psvguard: screen cast portal: %s\n", err->message);
		g_error_free(err);
		pg_session_fail(s, FALSE);
	} else if (!s->removed) {
		gint32 idx;
		g_variant_get(r, "(h)", &idx);
		s->fd = g_unix_fd_list_get(fds, idx, NULL);
		pw_thread_loop_lock(gLoop);
		s->core = s->fd >= 0 ? pw_context_connect_fd(gContext, fcntl(s->fd, F_DUPFD_CLOEXEC, 3), NULL, 0) : NULL;
		pw_thread_loop_unlock(gLoop);
		if (!s->core) pg_session_fail(s, FALSE);
		else {
			s->active = TRUE;
			gDenied = FALSE;
			pg_emit_permission();
		}
	}
	if (r) g_variant_unref(r);
	if (fds) g_object_unref(fds);
	pg_session_unref(s);
}

// pg_session_step runs the next step of CreateSession → SelectSources → Start → OpenPipeWireRemote.
static void pg_session_step(PGSession *s, GVariant *results) {
	GVariantBuilder opts;
	g_variant_builder_init(&opts, G_VARIANT_TYPE_VARDICT);
	switch (s->step++) {
	case 0: {
		const char *path = NULL;
		if (!g_variant_lookup(results, "session_handle", "&s", &path)) {
			g_variant_builder_clear(&opts);
			pg_session_fail(s, FALSE);
			return;
		}
		s->path = g_strdup(path);
		s->closedSub = g_dbus_connection_signal_subscribe(gSession, "org.freedesktop.portal.Desktop",
		                                                  "org.freedesktop.portal.Session", "Closed", s->path, NULL,
		                                                  G_DBUS_SIGNAL_FLAGS_NONE, pg_on_closed, s, NULL);
		g_variant_builder_add(&opts, "{sv}", "types", g_variant_new_uint32(1)); // monitors
		g_variant_builder_add(&opts, "{sv}", "multiple", g_variant_new_boolean(TRUE));
		g_variant_builder_add(&opts, "{sv}", "persist_mode", g_variant_new_uint32(2)); // until revoked
		if (s->restoreToken) g_variant_builder_add(&opts, "{sv}", "restore_token", g_variant_new_string(s->restoreToken));
		pg_session_call(s, "SelectSources", g_variant_new("(o)", s->path), &opts);
		return;
	}
	case 1:
		pg_session_call(s, "Start", g_variant_new("(os)", s->path, ""), &opts);
		return;
	default: {
		g_variant_builder_clear(&opts);
		const char *token = NULL;
		g_clear_pointer(&s->restoreToken, g_free);
		if (g_variant_lookup(results, "restore_token", "&s", &token) && *token) s->restoreToken = g_strdup(token);
		pg_save_tokens();
		GVariant *streams = NULL;
		if (g_variant_lookup(results, "streams", "@a(ua{sv})", &streams)) {
			GVariantIter it;
			guint32 node;
			GVariant *props;
			g_variant_iter_init(&it, streams);
			while (g_variant_iter_loop(&it, "(u@a{sv})", &node, &props)) {
				PGPortalStream ps = { node, -1, -1, -1, -1 };
				g_variant_lookup(props, "position", "(ii)", &ps.x, &ps.y);
				g_variant_lookup(props, "size", "(ii)", &ps.w, &ps.h);
				g_array_append_val(s->streams, ps);
			}
			g_variant_unref(streams);
		}
		if (s->streams->len == 0) { pg_session_fail(s, FALSE); return; }
		s->refs++;
		g_dbus_connection_call_with_unix_fd_list(gSession, "org.freedesktop.portal.Desktop",
		                                         "/org/freedesktop/portal/desktop", "org.freedesktop.portal.ScreenCast",
		                                         "OpenPipeWireRemote", g_variant_new("(oa{sv})", s->path, NULL),
		                                         G_VARIANT_TYPE("(h)"), G_DBUS_CALL_FLAGS_NONE, -1, NULL, NULL,
		                                         pg_on_remote, s);
	}
	}
}

// pg_session_start asks the portal for a session, restoring token's choice when it is not NULL.
static void pg_session_start(const char *token) {
	if (!gSession) return;
	PGSession *s = g_new0(PGSession, 1);
	s->refs = 1;
	s->fd = -1;
	s->restoreToken = g_strdup(token);
	s->streams = g_array_new(FALSE, TRUE, sizeof(PGPortalStream));
	g_ptr_array_add(gSessions, s);
	GVariantBuilder opts;
	g_variant_builder_init(&opts, G_VARIANT_TYPE_VARDICT);
	char *handle = g_strdup_printf("psvguard%d", ++gTokenCounter);
	g_variant_builder_add(&opts, "{sv}", "session_handle_token", g_variant_new_string(handle));
	g_free(handle);
	pg_session_call(s, "CreateSession", NULL, &opts);
}

static void pg_request_permission(void) {
	if (gDenied || gSessions->len > 0) { pg_emit_permission(); return; } // the button asks again
	if (gTokens->len == 0) pg_session_start(NULL);
	for (guint i = 0; i < gTokens->len; i++) pg_session_start(gTokens->pdata[i]);
	g_ptr_array_set_size(gTokens, 0);
	pg_emit_permission();
}

// pg_share_again replaces every session with a fresh choice in the share dialog.
static void pg_share_again(void) {
	pg_close_all();
	g_ptr_array_set_size(gTokens, 0);
	g_hash_table_remove_all(gAsked);
	gDenied = FALSE;
	pg_save_tokens();
	pg_session_start(NULL);
	pg_emit_permission();
}

// pg_portal_match returns the portal stream showing a display, and its session: the one at its
// position and size, or the only one of its size, or the only one when there is one display.
static const PGPortalStream *pg_portal_match(GdkMonitor *m, PGSession **session) {
	GdkRectangle g;
	gdk_monitor_get_geometry(m, &g);
	const PGPortalStream *bySize = NULL, *any = NULL;
	PGSession *bySizeSession = NULL, *anySession = NULL;
	int sized = 0, total = 0;
	for (guint i = 0; i < gSessions->len; i++) {
		PGSession *s = gSessions->pdata[i];
		if (!s->active) continue;
		for (guint j = 0; j < s->streams->len; j++) {
			const PGPortalStream *ps = &g_array_index(s->streams, PGPortalStream, j);
			total++;
			any = ps; anySession = s;
			if (ps->x == g.x && ps->y == g.y && ps->w == g.width && ps->h == g.height) { *session = s; return ps; }
			if (ps->w == g.width && ps->h == g.height) { bySize = ps; bySizeSession = s; sized++; }
		}
	}
	if (sized == 1) { *session = bySizeSession; return bySize; }
	if (total == 1 && gdk_display_get_n_monitors(gDisplay) == 1) { *session = anySession; return any; }
	return NULL;
}

static int pg_frame_format(enum spa_video_format f) {
	switch (f) {
	case SPA_VIDEO_FORMAT_BGRA: return PGFormatBGRA;
	case SPA_VIDEO_FORMAT_RGBA: return PGFormatRGBA;
	case SPA_VIDEO_FORMAT_BGRx: return PGFormatBGRX;
	case SPA_VIDEO_FORMAT_RGBx: return PGFormatRGBX;
	default: return 0;
	}
}

// Runs on the PipeWire thread.
static void pg_on_param_changed(void *data, uint32_t id, const struct spa_pod *param) {
	PGStream *s = data;
	if (!param || id != SPA_PARAM_Format) return;
	if (spa_format_video_raw_parse(param, &s->info) < 0) return;
	s->format = pg_frame_format(s->info.format);

	uint8_t buf[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
	const struct spa_pod *params[2];
	// Shared memory only: frames are read on the CPU.
	params[0] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_ParamBuffers, SPA_PARAM_Buffers,
		SPA_PARAM_BUFFERS_buffers, SPA_POD_CHOICE_RANGE_Int(4, 2, 8),
		SPA_PARAM_BUFFERS_dataType, SPA_POD_CHOICE_FLAGS_Int((1 << SPA_DATA_MemPtr) | (1 << SPA_DATA_MemFd)));
	params[1] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_ParamMeta, SPA_PARAM_Meta,
		SPA_PARAM_META_type, SPA_POD_Id(SPA_META_Header),
		SPA_PARAM_META_size, SPA_POD_Int(sizeof(struct spa_meta_header)));
	pw_stream_update_params(s->stream, params, 2);
}

// Runs on the PipeWire thread, the capture thread: one conversion in Go, then the buffer goes back.
static void pg_on_process(void *data) {
	PGStream *s = data;
	struct pw_buffer *b = pw_stream_dequeue_buffer(s->stream);
	if (!b) return;
	struct spa_buffer *sb = b->buffer;
	struct spa_meta_header *h = spa_buffer_find_meta_data(sb, SPA_META_Header, sizeof *h);
	struct spa_data *d = &sb->datas[0];
	if (s->format && d->data && d->chunk && d->chunk->size > 0 && !(d->chunk->flags & SPA_CHUNK_FLAG_CORRUPTED) &&
	    !(h && (h->flags & SPA_META_HEADER_FLAG_CORRUPTED))) {
		int w = (int)s->info.size.width, ht = (int)s->info.size.height;
		int stride = d->chunk->stride > 0 ? d->chunk->stride : w * 4;
		uint32_t off = d->chunk->offset % (d->maxsize ? d->maxsize : 1);
		if ((size_t)off + (size_t)stride * (size_t)(ht - 1) + (size_t)w * 4 <= d->maxsize)
			goCaptureFrame(s->handle, s->format, (const uint8_t *)d->data + off, w, ht, stride, pg_now_ns());
	}
	pw_stream_queue_buffer(s->stream, b);
}

static gboolean pg_stop_failed(gpointer data);

// Runs on the PipeWire thread.
static void pg_on_state(void *data, enum pw_stream_state old, enum pw_stream_state state, const char *error) {
	PGStream *s = data;
	if (state == PW_STREAM_STATE_STREAMING && !s->started) {
		s->started = TRUE;
		pg_emit(PGEvStreamStarted, pg_stream_info(s, NULL));
	} else if (state == PW_STREAM_STATE_ERROR && !s->failed) {
		s->failed = TRUE;
		pg_emit(PGEvStreamFailed, pg_stream_info(s, error ? error : "capture failed"));
		g_idle_add(pg_stop_failed, g_memdup2(&s->id, sizeof s->id));
	}
}

static const struct pw_stream_events pg_stream_events = {
	PW_VERSION_STREAM_EVENTS,
	.state_changed = pg_on_state,
	.param_changed = pg_on_param_changed,
	.process = pg_on_process,
};

// pg_stop_stream stops a stream and reports it stopped. Destroying it under the loop lock waits
// for a process callback in flight, so no frame callback can outlive the stream's handle.
static void pg_stop_stream(PGStream *s) {
	if (s->stream) {
		pw_thread_loop_lock(gLoop);
		pw_stream_disconnect(s->stream);
		pw_stream_destroy(s->stream);
		pw_thread_loop_unlock(gLoop);
		s->stream = NULL;
	}
	pg_emit(PGEvStreamStopped, pg_stream_info(s, NULL));
	g_hash_table_remove(gStreams, &s->id);
}

static gboolean pg_stop_failed(gpointer data) {
	PGStream *s = g_hash_table_lookup(gStreams, data);
	if (s) pg_stop_stream(s);
	g_free(data);
	return G_SOURCE_REMOVE;
}

static void pg_free_stream(gpointer data) {
	PGStream *s = data;
	g_free(s->display);
	g_free(s);
}

static void pg_start_capture(GVariant *d) {
	PGStream *s = g_new0(PGStream, 1);
	s->id = (guint64)pg_num(d, "stream");
	s->display = g_strdup(pg_str(d, "display"));
	s->handle = (uintptr_t)pg_num(d, "handle");
	g_hash_table_insert(gStreams, &s->id, s);

	const char *why = NULL;
	GdkMonitor *m = pg_monitor(s->display);
	const PGPortalStream *ps = NULL;
	if (!pg_any_session(TRUE)) why = "Screen sharing has not been approved.";
	else if (!m) why = "The display is no longer connected.";
	else if (!(ps = pg_portal_match(m, &s->session))) {
		why = "This display is not shared. Choose it in the screen share dialog.";
		// Once per display and run, and not while another dialog is open.
		if (!pg_any_session(FALSE) && !g_hash_table_contains(gAsked, s->display)) {
			g_hash_table_add(gAsked, g_strdup(s->display));
			pg_session_start(NULL);
		}
	}
	if (why) {
		s->failed = TRUE;
		pg_emit(PGEvStreamFailed, pg_stream_info(s, why));
		pg_stop_stream(s);
		return;
	}

	uint8_t buf[1024];
	struct spa_pod_builder b = SPA_POD_BUILDER_INIT(buf, sizeof buf);
	const struct spa_pod *params[1];
	params[0] = spa_pod_builder_add_object(&b, SPA_TYPE_OBJECT_Format, SPA_PARAM_EnumFormat,
		SPA_FORMAT_mediaType, SPA_POD_Id(SPA_MEDIA_TYPE_video),
		SPA_FORMAT_mediaSubtype, SPA_POD_Id(SPA_MEDIA_SUBTYPE_raw),
		SPA_FORMAT_VIDEO_format, SPA_POD_CHOICE_ENUM_Id(5, SPA_VIDEO_FORMAT_BGRx, SPA_VIDEO_FORMAT_BGRx,
		                                                SPA_VIDEO_FORMAT_BGRA, SPA_VIDEO_FORMAT_RGBx, SPA_VIDEO_FORMAT_RGBA),
		SPA_FORMAT_VIDEO_size, SPA_POD_CHOICE_RANGE_Rectangle(&SPA_RECTANGLE(1920, 1080), &SPA_RECTANGLE(1, 1),
		                                                      &SPA_RECTANGLE(16384, 16384)),
		SPA_FORMAT_VIDEO_framerate, SPA_POD_CHOICE_RANGE_Fraction(&SPA_FRACTION(60, 1), &SPA_FRACTION(0, 1),
		                                                          &SPA_FRACTION(1000, 1)));

	pw_thread_loop_lock(gLoop);
	s->stream = pw_stream_new(s->session->core, "psvguard",
	                          pw_properties_new(PW_KEY_MEDIA_TYPE, "Video", PW_KEY_MEDIA_CATEGORY, "Capture",
	                                            PW_KEY_MEDIA_ROLE, "Screen", NULL));
	int err = s->stream ? 0 : -1;
	if (s->stream) {
		pw_stream_add_listener(s->stream, &s->listener, &pg_stream_events, s);
		err = pw_stream_connect(s->stream, PW_DIRECTION_INPUT, ps->node,
		                        PW_STREAM_FLAG_AUTOCONNECT | PW_STREAM_FLAG_MAP_BUFFERS, params, 1);
	}
	pw_thread_loop_unlock(gLoop);
	if (err < 0) {
		s->failed = TRUE;
		pg_emit(PGEvStreamFailed, pg_stream_info(s, "PipeWire could not connect to the screen."));
		pg_stop_stream(s);
	}
}

static void pg_stop_capture(GVariant *d) {
	guint64 id = (guint64)pg_num(d, "stream");
	PGStream *s = g_hash_table_lookup(gStreams, &id);
	if (s) { pg_stop_stream(s); return; }
	pg_emit(PGEvStreamStopped, pg_dict("stream", g_variant_new_uint64(id),
	                                   "display", g_variant_new_string(pg_str(d, "display")), NULL));
}

// MARK: - Commands

static void pg_quit(void) {
	GList *list = g_hash_table_get_values(gStreams);
	for (GList *l = list; l; l = l->next) pg_stop_stream(l->data);
	g_list_free(list);
	pg_close_all();
	GHashTableIter it;
	gpointer key, val;
	g_hash_table_iter_init(&it, gOverlays);
	while (g_hash_table_iter_next(&it, &key, &val)) gtk_widget_hide(((PGOverlay *)val)->win);
	gtk_main_quit();
}

static void pg_apply(int kind, GBytes *bytes) {
	gsize len = 0;
	const char *data = g_bytes_get_data(bytes, &len);
	if (kind == PGCmdSetCovers) { pg_set_covers((const uint8_t *)data, len); return; }
	GVariant *d = pg_json_parse(data, len);
	switch (kind) {
	case PGCmdClearCovers: pg_clear_covers(d); break;
	case PGCmdStatus: pg_apply_status(d); break;
	case PGCmdControls: pg_apply_controls(d); break;
	case PGCmdStats: pg_apply_stats(d); break;
	case PGCmdStartCapture: pg_start_capture(d); break;
	case PGCmdStopCapture: pg_stop_capture(d); break;
	case PGCmdCheckPermission: pg_emit_permission(); break;
	case PGCmdRequestPermission: pg_request_permission(); break;
	case PGCmdOpenPermissionPane: pg_share_again(); break; // there is no settings page: ask again
	case PGCmdNotify: pg_notify(d); break;
	case PGCmdShowPanel: pg_show_panel(); break;
	case PGCmdQuit: pg_quit(); break;
	}
	g_variant_unref(d);
}

typedef struct { int kind; GBytes *bytes; } PGCommand;

static gboolean pg_apply_idle(gpointer data) {
	PGCommand *c = data;
	pg_apply(c->kind, c->bytes);
	g_bytes_unref(c->bytes);
	g_free(c);
	return G_SOURCE_REMOVE;
}

void pg_post(int kind, const void *data, size_t len) {
	PGCommand *c = g_new0(PGCommand, 1);
	c->kind = kind;
	c->bytes = g_bytes_new(data, len);
	g_main_context_invoke_full(NULL, G_PRIORITY_DEFAULT, pg_apply_idle, c, NULL);
}

// pg_run sets up the platform layer and runs the GTK main loop until a quit command. It returns
// non-zero when there is no Wayland display to connect to.
int pg_run(const char *version, const char *statePath) {
	gEvents = g_async_queue_new();
	gEventThread = g_thread_new("psvguard-events", pg_event_thread, NULL);

	gdk_set_allowed_backends("wayland");
	if (!gtk_init_check(NULL, NULL)) return 1;
	gDisplay = gdk_display_get_default();
	g_set_application_name("PSV Guard");

	pw_init(NULL, NULL);
	gLoop = pw_thread_loop_new("psvguard-capture", NULL);
	gContext = pw_context_new(pw_thread_loop_get_loop(gLoop), NULL, 0);
	pw_thread_loop_start(gLoop);

	gSession = g_bus_get_sync(G_BUS_TYPE_SESSION, NULL, NULL);
	gOverlays = g_hash_table_new(g_str_hash, g_str_equal);
	gStreams = g_hash_table_new_full(g_int64_hash, g_int64_equal, NULL, pg_free_stream);
	gSessions = g_ptr_array_new();
	gTokens = g_ptr_array_new_with_free_func(g_free);
	gAsked = g_hash_table_new_full(g_str_hash, g_str_equal, g_free, NULL);
	gStatePath = g_strdup(statePath);
	pg_load_tokens();

	pg_setup_ui();
	int n = gdk_display_get_n_monitors(gDisplay);
	for (int i = 0; i < n; i++) pg_watch_monitor(gdk_display_get_monitor(gDisplay, i));
	g_signal_connect(gDisplay, "monitor-added", G_CALLBACK(pg_monitor_added), NULL);
	g_signal_connect(gDisplay, "monitor-removed", G_CALLBACK(pg_monitor_removed), NULL);
	pg_watch_sleep();
	pg_watch_hyprland();

	GVariantBuilder formats;
	g_variant_builder_init(&formats, G_VARIANT_TYPE("av"));
	g_variant_builder_add(&formats, "v", g_variant_new_string("BGRx"));
	g_variant_builder_add(&formats, "v", g_variant_new_string("BGRA"));
	pg_emit(PGEvCapabilities, pg_dict(
		"excludesOwnWindows", g_variant_new_boolean(FALSE),
		"canOverlay", g_variant_new_boolean(gtk_layer_is_supported()),
		"perDisplayCapture", g_variant_new_boolean(TRUE),
		"consentPerSession", g_variant_new_boolean(TRUE),
		"pixelFormats", g_variant_builder_end(&formats), NULL));
	pg_emit(PGEvDisplays, pg_display_list());
	pg_emit(PGEvReady, NULL);
	if (!pg_has_tray_host()) pg_show_panel(); // nowhere to show the menu bar item

	gtk_main();

	pw_thread_loop_stop(gLoop);
	PGEvent *stop = g_new0(PGEvent, 1);
	stop->kind = -1;
	g_async_queue_push(gEvents, stop);
	g_thread_join(gEventThread);
	return 0;
}
*/
import "C"
