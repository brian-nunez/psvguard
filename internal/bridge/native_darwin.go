//go:build darwin && cgo

package bridge

/*
#cgo CFLAGS: -x objective-c -fobjc-arc
#cgo LDFLAGS: -framework AppKit -framework ScreenCaptureKit -framework CoreMedia -framework CoreVideo -framework CoreGraphics -framework QuartzCore

#import <AppKit/AppKit.h>
#import <CoreGraphics/CoreGraphics.h>
#import <CoreMedia/CoreMedia.h>
#import <CoreVideo/CoreVideo.h>
#import <QuartzCore/QuartzCore.h>
#import <ScreenCaptureKit/ScreenCaptureKit.h>
#include <math.h>
#include <stdint.h>
#include <unistd.h>

// This file is the macOS platform layer: a command executor. It holds the AppKit and
// ScreenCaptureKit objects, applies the commands Go posts, and reports events. It makes no
// decisions and never sees psv results.

// Go callbacks exported from bus_darwin.go. Declared here rather than via _cgo_export.h, which cgo
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
enum { PGFormatBGRA = 1 };

typedef struct { double x, y, w, h; } PGRect;

// pg_norm_to_points converts a normalised rectangle (0 to 1, origin top left) on a display whose
// frame in points (origin bottom left) is fx, fy, fw, fh into points, rounded outward to whole
// device pixels so covers on Retina displays leave no hairline gaps. A multiplication and a flip.
PGRect pg_norm_to_points(double nx, double ny, double nw, double nh,
                         double fx, double fy, double fw, double fh, double scale) {
	if (scale <= 0) scale = 1;
	double x0 = fx + nx * fw, x1 = fx + (nx + nw) * fw;
	double yTop = fy + fh - ny * fh, yBottom = fy + fh - (ny + nh) * fh;
	x0 = floor(x0 * scale + 1e-9) / scale;
	x1 = ceil(x1 * scale - 1e-9) / scale;
	yBottom = floor(yBottom * scale + 1e-9) / scale;
	yTop = ceil(yTop * scale - 1e-9) / scale;
	x0 = fmax(x0, fx); x1 = fmin(x1, fx + fw);
	yBottom = fmax(yBottom, fy); yTop = fmin(yTop, fy + fh);
	PGRect r = { x0, yBottom, fmax(0, x1 - x0), fmax(0, yTop - yBottom) };
	return r;
}

int pg_screen_capture_granted(void) { return CGPreflightScreenCaptureAccess() ? 1 : 0; }

// Events go to Go through one serial queue, never synchronously from a command handler.
static dispatch_queue_t gEvents;

static void pg_emit(int kind, id obj) {
	NSData *d = nil;
	if (obj) d = [NSJSONSerialization dataWithJSONObject:obj options:0 error:nil];
	dispatch_async(gEvents, ^{ goEvent(kind, d.bytes, d.length); });
}

static NSString *pg_str(id v) {
	if ([v isKindOfClass:[NSString class]]) return v;
	if ([v respondsToSelector:@selector(stringValue)]) return [v stringValue];
	return @"";
}

static NSTextField *pg_label(NSString *text, CGFloat size, BOOL bold, BOOL secondary) {
	NSTextField *l = [NSTextField labelWithString:text ?: @""];
	l.font = bold ? [NSFont boldSystemFontOfSize:size] : [NSFont systemFontOfSize:size];
	if (secondary) l.textColor = [NSColor secondaryLabelColor];
	l.lineBreakMode = NSLineBreakByTruncatingTail;
	return l;
}

static NSTextField *pg_wrap(NSString *text, CGFloat size, BOOL secondary) {
	NSTextField *l = [NSTextField wrappingLabelWithString:text ?: @""];
	l.font = [NSFont systemFontOfSize:size];
	if (secondary) l.textColor = [NSColor secondaryLabelColor];
	l.preferredMaxLayoutWidth = 330;
	return l;
}

static CGDirectDisplayID pg_display_id(NSScreen *s) {
	return [s.deviceDescription[@"NSScreenNumber"] unsignedIntValue];
}

static NSScreen *pg_screen(NSString *displayID) {
	for (NSScreen *s in NSScreen.screens) {
		if ([[@(pg_display_id(s)) stringValue] isEqualToString:displayID]) return s;
	}
	return nil;
}

// MARK: - Cover panels

// PGCoverPanel never takes focus.
@interface PGCoverPanel : NSPanel
@end
@implementation PGCoverPanel
- (BOOL)canBecomeKeyWindow { return NO; }
- (BOOL)canBecomeMainWindow { return NO; }
@end

// PGOverlay is one display's click-through cover window and its pool of cover layers.
@interface PGOverlay : NSObject
@property(strong) NSString *displayID;
@property(strong) PGCoverPanel *panel;
@property(strong) NSMutableArray<CALayer *> *layers;
@property NSUInteger generation;
@end

@implementation PGOverlay

- (instancetype)initWithDisplay:(NSString *)displayID {
	if (!(self = [super init])) return nil;
	self.displayID = displayID;
	self.layers = [NSMutableArray array];
	self.panel = [[PGCoverPanel alloc] initWithContentRect:NSZeroRect
	                                             styleMask:NSWindowStyleMaskBorderless | NSWindowStyleMaskNonactivatingPanel
	                                               backing:NSBackingStoreBuffered
	                                                 defer:NO];
	self.panel.opaque = NO;
	self.panel.backgroundColor = [NSColor clearColor];
	self.panel.hasShadow = NO;
	self.panel.ignoresMouseEvents = YES;
	self.panel.releasedWhenClosed = NO;
	self.panel.hidesOnDeactivate = NO;
	self.panel.level = NSScreenSaverWindowLevel;
	self.panel.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces | NSWindowCollectionBehaviorFullScreenAuxiliary |
	                                NSWindowCollectionBehaviorStationary | NSWindowCollectionBehaviorIgnoresCycle;
	self.panel.contentView.wantsLayer = YES;
	self.panel.sharingType = NSWindowSharingNone; // keep covers out of other apps' captures too
	[self fitScreen];
	return self;
}

- (void)fitScreen {
	NSScreen *s = pg_screen(self.displayID);
	if (s) [self.panel setFrame:s.frame display:NO];
}

- (CALayer *)newCoverLayer {
	CALayer *l = [CALayer layer];
	l.backgroundColor = [NSColor colorWithSRGBRed:0.09 green:0.09 blue:0.10 alpha:1].CGColor;
	l.opacity = 0;
	CATextLayer *t = [CATextLayer layer];
	t.string = @"Flashing covered by PSV Guard";
	t.fontSize = 13;
	t.font = (__bridge CFTypeRef)[NSFont systemFontOfSize:13];
	t.foregroundColor = [NSColor colorWithSRGBRed:0.55 green:0.55 blue:0.57 alpha:1].CGColor;
	t.alignmentMode = kCAAlignmentCenter;
	t.contentsScale = self.panel.backingScaleFactor ?: 2;
	[l addSublayer:t];
	[self.panel.contentView.layer addSublayer:l];
	return l;
}

static void pg_layout_label(CALayer *l) {
	CATextLayer *t = (CATextLayer *)l.sublayers.firstObject;
	CGSize sz = l.bounds.size;
	t.frame = CGRectMake(0, sz.height / 2 - 9, sz.width, 18);
	t.hidden = sz.width < 220 || sz.height < 40;
}

static void pg_fade(CALayer *l, float to, double seconds) {
	CALayer *p = l.presentationLayer ?: l;
	CABasicAnimation *a = [CABasicAnimation animationWithKeyPath:@"opacity"];
	a.fromValue = @(p.opacity);
	a.toValue = @(to);
	a.duration = seconds;
	a.timingFunction = [CAMediaTimingFunction functionWithName:kCAMediaTimingFunctionEaseInEaseOut];
	[l addAnimation:a forKey:@"fade"];
	l.opacity = to;
}

// setRects shows exactly these rectangles, normalised with a top-left origin.
- (void)setRects:(NSArray<NSArray<NSNumber *> *> *)rects fadeIn:(double)fadeIn fadeOut:(double)fadeOut {
	NSUInteger gen = ++self.generation;
	NSScreen *s = pg_screen(self.displayID);
	if (!s) return;
	[self fitScreen];
	NSSize size = s.frame.size;
	double scale = s.backingScaleFactor;

	if (rects.count > 0) [self.panel orderFrontRegardless];
	while (self.layers.count < rects.count) [self.layers addObject:[self newCoverLayer]];

	[CATransaction begin];
	[CATransaction setDisableActions:YES];
	for (NSUInteger i = 0; i < self.layers.count; i++) {
		CALayer *l = self.layers[i];
		if (i < rects.count) {
			NSArray<NSNumber *> *r = rects[i];
			PGRect p = pg_norm_to_points(r[0].doubleValue, r[1].doubleValue, r[2].doubleValue, r[3].doubleValue,
			                             0, 0, size.width, size.height, scale);
			CGRect frame = CGRectMake(p.x, p.y, p.w, p.h);
			BOOL visible = l.opacity > 0;
			if (visible && !CGRectEqualToRect(l.frame, frame)) {
				CALayer *pres = l.presentationLayer ?: l;
				CABasicAnimation *pos = [CABasicAnimation animationWithKeyPath:@"position"];
				pos.fromValue = [NSValue valueWithPoint:NSPointFromCGPoint(pres.position)];
				pos.duration = fadeIn;
				CABasicAnimation *bounds = [CABasicAnimation animationWithKeyPath:@"bounds"];
				bounds.fromValue = [NSValue valueWithRect:NSRectFromCGRect(pres.bounds)];
				bounds.duration = fadeIn;
				[l addAnimation:pos forKey:@"position"];
				[l addAnimation:bounds forKey:@"bounds"];
			}
			l.frame = frame;
			pg_layout_label(l);
			if (l.opacity < 0.97f) pg_fade(l, 0.97f, fadeIn);
		} else if (l.opacity > 0) {
			pg_fade(l, 0, fadeOut);
		}
	}
	[CATransaction commit];

	if (rects.count == 0) {
		dispatch_after(dispatch_time(DISPATCH_TIME_NOW, (int64_t)((fadeOut + 0.1) * NSEC_PER_SEC)), dispatch_get_main_queue(), ^{
			if (self.generation == gen) [self.panel orderOut:nil];
		});
	}

	pg_emit(PGEvCoversShown, @{@"display" : self.displayID, @"rects" : @(rects.count), @"at" : @((int64_t)(CACurrentMediaTime() * 1e9))});
}

@end

// MARK: - Capture streams

@interface PGStream : NSObject <SCStreamOutput, SCStreamDelegate>
@property uint64_t streamID;
@property(strong) NSString *displayID;
@property uintptr_t handle;
@property(strong) SCStream *stream;
@property(strong) dispatch_queue_t queue;
@property BOOL stopRequested;
@property BOOL stopReported;
@end

@implementation PGStream

- (NSDictionary *)info:(NSString *)error {
	NSMutableDictionary *d = [@{@"stream" : @(self.streamID), @"display" : self.displayID} mutableCopy];
	if (error) d[@"error"] = error;
	return d;
}

// reportStopped tells Go the stream is gone, which releases its frame target. It first drains the
// serial sample queue, so a frame callback still in flight finishes before the target goes.
- (void)reportStopped {
	@synchronized(self) {
		if (self.stopReported) return;
		self.stopReported = YES;
	}
	self.stopRequested = YES;
	NSDictionary *info = [self info:nil];
	dispatch_async(self.queue, ^{ pg_emit(PGEvStreamStopped, info); });
}

- (void)fail:(NSError *)err {
	pg_emit(PGEvStreamFailed, [self info:err.localizedDescription ?: @"capture failed"]);
	[self reportStopped];
}

// Runs on the stream's sample handler queue, while ScreenCaptureKit holds the sample buffer.
- (void)stream:(SCStream *)stream didOutputSampleBuffer:(CMSampleBufferRef)sb ofType:(SCStreamOutputType)type {
	if (type != SCStreamOutputTypeScreen || self.stopRequested) return;
	@autoreleasepool {
		CFArrayRef atts = CMSampleBufferGetSampleAttachmentsArray(sb, false);
		if (atts && CFArrayGetCount(atts) > 0) {
			NSDictionary *a = (__bridge NSDictionary *)CFArrayGetValueAtIndex(atts, 0);
			NSNumber *status = a[SCStreamFrameInfoStatus];
			if (status && status.integerValue != SCFrameStatusComplete) return;
		}
		CVPixelBufferRef pb = CMSampleBufferGetImageBuffer(sb);
		if (!pb) return;
		CVPixelBufferLockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
		const uint8_t *base = CVPixelBufferGetBaseAddress(pb);
		int w = (int)CVPixelBufferGetWidth(pb), h = (int)CVPixelBufferGetHeight(pb);
		int bpr = (int)CVPixelBufferGetBytesPerRow(pb);
		CMTime pts = CMSampleBufferGetPresentationTimeStamp(sb);
		int64_t ns = CMTIME_IS_NUMERIC(pts) ? (int64_t)(CMTimeGetSeconds(pts) * 1e9) : (int64_t)(CACurrentMediaTime() * 1e9);
		if (base) goCaptureFrame(self.handle, PGFormatBGRA, base, w, h, bpr, ns);
		CVPixelBufferUnlockBaseAddress(pb, kCVPixelBufferLock_ReadOnly);
	}
}

- (void)stream:(SCStream *)stream didStopWithError:(NSError *)error {
	[self fail:error];
}

@end

// MARK: - Status panel

@interface PGPanel : NSPanel
@end
@implementation PGPanel
- (BOOL)canBecomeKeyWindow { return YES; }
- (void)cancelOperation:(id)sender { [self orderOut:nil]; }
@end

@interface PGFlipped : NSView
@end
@implementation PGFlipped
- (BOOL)isFlipped { return YES; }
@end

@interface PGApp : NSObject <NSWindowDelegate>
@property(strong) NSString *version;
@property(strong) NSStatusItem *statusItem;
@property(strong) PGPanel *panel;
@property(strong) NSTextField *titleLabel;
@property(strong) NSTextField *detailLabel;
@property(strong) NSTextField *statsLabel;
@property(strong) NSButton *toggleButton;
@property(strong) NSButton *permissionButton;
@property(strong) NSStackView *settingsStack;
@property(strong) NSDictionary *status;
@property(strong) NSMutableDictionary<NSString *, PGOverlay *> *overlays;
@property(strong) NSMutableDictionary<NSNumber *, PGStream *> *streams;
@property BOOL requested;
@property BOOL positioned;
@end

@implementation PGApp

- (NSImage *)symbol:(NSString *)name {
	NSImage *img = [NSImage imageWithSystemSymbolName:name accessibilityDescription:@"PSV Guard"];
	img.template = YES;
	return img;
}

- (void)addFull:(NSView *)v to:(NSStackView *)stack inset:(CGFloat)inset {
	[stack addArrangedSubview:v];
	[v.widthAnchor constraintEqualToAnchor:stack.widthAnchor constant:-2 * inset].active = YES;
}

- (void)setup {
	self.overlays = [NSMutableDictionary dictionary];
	self.streams = [NSMutableDictionary dictionary];
	[self setupMainMenu];

	self.statusItem = [[NSStatusBar systemStatusBar] statusItemWithLength:NSVariableStatusItemLength];
	NSStatusBarButton *b = self.statusItem.button;
	b.image = [self symbol:@"shield.lefthalf.filled"];
	b.toolTip = @"PSV Guard";
	b.target = self;
	b.action = @selector(statusClicked:);
	[b sendActionOn:NSEventMaskLeftMouseUp | NSEventMaskRightMouseUp];

	self.panel = [[PGPanel alloc] initWithContentRect:NSMakeRect(0, 0, 360, 600)
	                                        styleMask:NSWindowStyleMaskTitled | NSWindowStyleMaskClosable |
	                                                  NSWindowStyleMaskUtilityWindow | NSWindowStyleMaskNonactivatingPanel
	                                          backing:NSBackingStoreBuffered
	                                            defer:NO];
	self.panel.title = @"PSV Guard";
	self.panel.releasedWhenClosed = NO;
	self.panel.hidesOnDeactivate = NO;
	self.panel.becomesKeyOnlyIfNeeded = YES;
	self.panel.floatingPanel = YES;
	self.panel.level = NSFloatingWindowLevel;
	self.panel.collectionBehavior = NSWindowCollectionBehaviorCanJoinAllSpaces | NSWindowCollectionBehaviorFullScreenAuxiliary;
	self.panel.delegate = self;

	NSStackView *root = [NSStackView stackViewWithViews:@[]];
	root.orientation = NSUserInterfaceLayoutOrientationVertical;
	root.alignment = NSLayoutAttributeLeading;
	root.spacing = 8;
	root.edgeInsets = NSEdgeInsetsMake(14, 14, 14, 14);
	root.translatesAutoresizingMaskIntoConstraints = NO;
	NSView *content = self.panel.contentView;
	[content addSubview:root];
	[NSLayoutConstraint activateConstraints:@[
		[root.leadingAnchor constraintEqualToAnchor:content.leadingAnchor],
		[root.trailingAnchor constraintEqualToAnchor:content.trailingAnchor],
		[root.topAnchor constraintEqualToAnchor:content.topAnchor],
		[root.bottomAnchor constraintLessThanOrEqualToAnchor:content.bottomAnchor],
	]];

	self.titleLabel = pg_label(@"Starting…", 15, YES, NO);
	self.detailLabel = pg_wrap(@"", 12, YES);
	[self addFull:self.titleLabel to:root inset:14];
	[self addFull:self.detailLabel to:root inset:14];

	self.toggleButton = [NSButton buttonWithTitle:@"Turn Off" target:self action:@selector(toggleClicked:)];
	self.permissionButton = [NSButton buttonWithTitle:@"Open System Settings" target:self action:@selector(permissionClicked:)];
	self.permissionButton.hidden = YES;
	NSButton *quit = [NSButton buttonWithTitle:@"Quit" target:self action:@selector(quitClicked:)];
	NSStackView *buttons = [NSStackView stackViewWithViews:@[ self.toggleButton, self.permissionButton, quit ]];
	buttons.spacing = 8;
	[root addArrangedSubview:buttons];

	NSBox *line = [[NSBox alloc] init];
	line.boxType = NSBoxSeparator;
	[self addFull:line to:root inset:14];

	[root addArrangedSubview:pg_label(@"Sensitivity", 12, YES, NO)];
	self.settingsStack = [NSStackView stackViewWithViews:@[]];
	self.settingsStack.orientation = NSUserInterfaceLayoutOrientationVertical;
	self.settingsStack.alignment = NSLayoutAttributeLeading;
	self.settingsStack.spacing = 8;
	[self addFull:self.settingsStack to:root inset:14];

	NSBox *line2 = [[NSBox alloc] init];
	line2.boxType = NSBoxSeparator;
	[self addFull:line2 to:root inset:14];

	self.statsLabel = pg_wrap(@"", 10, YES);
	self.statsLabel.font = [NSFont monospacedSystemFontOfSize:10 weight:NSFontWeightRegular];
	[self addFull:self.statsLabel to:root inset:14];

	NSTextField *disclaimer = pg_wrap(@"", 11, YES);
	disclaimer.identifier = @"disclaimer";
	[self addFull:disclaimer to:root inset:14];

	NSNotificationCenter *nc = [NSNotificationCenter defaultCenter];
	[nc addObserver:self selector:@selector(screensChanged:) name:NSApplicationDidChangeScreenParametersNotification object:nil];
	NSNotificationCenter *ws = [NSWorkspace sharedWorkspace].notificationCenter;
	[ws addObserver:self selector:@selector(willSleep:) name:NSWorkspaceWillSleepNotification object:nil];
	[ws addObserver:self selector:@selector(didWake:) name:NSWorkspaceDidWakeNotification object:nil];
	[ws addObserver:self selector:@selector(spaceChanged:) name:NSWorkspaceActiveSpaceDidChangeNotification object:nil];
}

- (void)setupMainMenu {
	// An accessory app has no visible menu bar, but text fields still need the Edit shortcuts.
	NSMenu *main = [[NSMenu alloc] init];
	NSMenuItem *editItem = [[NSMenuItem alloc] init];
	NSMenu *edit = [[NSMenu alloc] initWithTitle:@"Edit"];
	[edit addItemWithTitle:@"Cut" action:@selector(cut:) keyEquivalent:@"x"];
	[edit addItemWithTitle:@"Copy" action:@selector(copy:) keyEquivalent:@"c"];
	[edit addItemWithTitle:@"Paste" action:@selector(paste:) keyEquivalent:@"v"];
	[edit addItemWithTitle:@"Select All" action:@selector(selectAll:) keyEquivalent:@"a"];
	editItem.submenu = edit;
	[main addItem:editItem];
	NSApp.mainMenu = main;
}

// MARK: Events

- (NSArray *)displayList {
	NSMutableArray *out = [NSMutableArray array];
	for (NSScreen *s in NSScreen.screens) {
		CGDirectDisplayID did = pg_display_id(s);
		NSRect f = s.frame;
		CGFloat scale = s.backingScaleFactor;
		[out addObject:@{
			@"id" : [@(did) stringValue],
			@"name" : s.localizedName ?: @"Display",
			@"width" : @(f.size.width),
			@"height" : @(f.size.height),
			@"pixelWidth" : @((NSInteger)llround(f.size.width * scale)),
			@"pixelHeight" : @((NSInteger)llround(f.size.height * scale)),
			@"main" : CGDisplayIsMain(did) ? @YES : @NO,
		}];
	}
	return out;
}

- (void)screensChanged:(NSNotification *)n {
	for (PGOverlay *o in self.overlays.allValues) [o fitScreen];
	pg_emit(PGEvDisplays, [self displayList]);
}

- (void)willSleep:(NSNotification *)n { pg_emit(PGEvPower, @{@"sleeping" : @YES}); }
- (void)didWake:(NSNotification *)n { pg_emit(PGEvPower, @{@"sleeping" : @NO}); }
// Switching Spaces, including entering and leaving full screen, slides the whole display.
- (void)spaceChanged:(NSNotification *)n {
	pg_emit(PGEvTransition, @{@"at" : @((int64_t)(CACurrentMediaTime() * 1e9))});
}

- (void)action:(NSString *)kind key:(NSString *)key value:(NSString *)value {
	NSMutableDictionary *a = [@{@"kind" : kind} mutableCopy];
	if (key) a[@"key"] = key;
	if (value) a[@"value"] = value;
	pg_emit(PGEvAction, a);
}

- (void)toggleClicked:(id)sender { [self action:@"toggle" key:nil value:nil]; }
- (void)permissionClicked:(id)sender { [self action:@"openPermission" key:nil value:nil]; }
- (void)quitClicked:(id)sender { [self action:@"quit" key:nil value:nil]; }
- (void)showPanelClicked:(id)sender { [self showPanel]; }

- (void)statusClicked:(id)sender {
	NSEvent *e = NSApp.currentEvent;
	if (e.type == NSEventTypeRightMouseUp || (e.modifierFlags & NSEventModifierFlagControl)) {
		NSMenu *m = [[NSMenu alloc] init];
		NSMenuItem *st = [m addItemWithTitle:pg_str(self.status[@"title"]) action:nil keyEquivalent:@""];
		st.enabled = NO;
		[m addItem:[NSMenuItem separatorItem]];
		BOOL on = [self.status[@"enabled"] boolValue];
		[m addItemWithTitle:(on ? @"Turn Protection Off" : @"Turn Protection On") action:@selector(toggleClicked:) keyEquivalent:@""].target = self;
		[m addItemWithTitle:@"Show Panel…" action:@selector(showPanelClicked:) keyEquivalent:@""].target = self;
		[m addItem:[NSMenuItem separatorItem]];
		[m addItemWithTitle:@"Quit PSV Guard" action:@selector(quitClicked:) keyEquivalent:@""].target = self;
		self.statusItem.menu = m;
		[self.statusItem.button performClick:nil];
		self.statusItem.menu = nil;
		return;
	}
	if (self.panel.isVisible) [self.panel orderOut:nil];
	else [self showPanel];
}

- (void)showPanel {
	if (!self.positioned) {
		NSWindow *w = self.statusItem.button.window;
		NSRect sf = w ? w.frame : NSZeroRect;
		NSScreen *screen = w.screen ?: [NSScreen mainScreen];
		NSRect vis = screen.visibleFrame;
		NSRect f = self.panel.frame;
		CGFloat x = NSIsEmptyRect(sf) ? NSMaxX(vis) - f.size.width - 12 : NSMidX(sf) - f.size.width / 2;
		x = MAX(NSMinX(vis) + 8, MIN(x, NSMaxX(vis) - f.size.width - 8));
		[self.panel setFrameOrigin:NSMakePoint(x, NSMaxY(vis) - f.size.height - 8)];
		self.positioned = YES;
	}
	[self.panel orderFrontRegardless];
}

- (BOOL)windowShouldClose:(NSWindow *)sender {
	[self.panel orderOut:nil];
	return NO;
}

// MARK: Commands

- (void)apply:(int)kind data:(NSData *)data {
	switch (kind) {
	case PGCmdSetCovers: [self setCovers:data]; break;
	case PGCmdClearCovers: [self clearCovers:[self json:data]]; break;
	case PGCmdStatus: [self applyStatus:[self json:data]]; break;
	case PGCmdControls: [self setControls:[self json:data]]; break;
	case PGCmdStats: [self setStats:[self json:data]]; break;
	case PGCmdStartCapture: [self startCapture:[self json:data]]; break;
	case PGCmdStopCapture: [self stopCapture:[self json:data]]; break;
	case PGCmdCheckPermission: [self reportPermission]; break;
	case PGCmdRequestPermission:
		self.requested = YES;
		CGRequestScreenCaptureAccess();
		[self reportPermission];
		break;
	case PGCmdOpenPermissionPane:
		[[NSWorkspace sharedWorkspace]
		    openURL:[NSURL URLWithString:@"x-apple.systempreferences:com.apple.preference.security?Privacy_ScreenCapture"]];
		break;
	case PGCmdNotify: [self notify:[self json:data]]; break;
	case PGCmdShowPanel: [self showPanel]; break;
	case PGCmdQuit: [self quit]; break;
	}
}

- (id)json:(NSData *)data {
	if (data.length == 0) return @{};
	return [NSJSONSerialization JSONObjectWithData:data options:0 error:nil] ?: @{};
}

- (void)reportPermission {
	NSString *state = CGPreflightScreenCaptureAccess() ? @"granted" : (self.requested ? @"denied" : @"needsConsent");
	pg_emit(PGEvPermission, @{@"state" : state});
}

// The packed layout of EncodeCovers in contract.go.
- (void)setCovers:(NSData *)d {
	const uint8_t *p = d.bytes;
	size_t n = d.length;
	if (n < 2) return;
	uint16_t idLen;
	memcpy(&idLen, p, 2);
	if (n < 2 + (size_t)idLen + 20) return;
	NSString *display = [[NSString alloc] initWithBytes:p + 2 length:idLen encoding:NSUTF8StringEncoding];
	const uint8_t *q = p + 2 + idLen;
	double fadeIn, fadeOut;
	uint32_t count;
	memcpy(&fadeIn, q, 8);
	memcpy(&fadeOut, q + 8, 8);
	memcpy(&count, q + 16, 4);
	q += 20;
	if ((size_t)(p + n - q) != (size_t)count * 32) return;
	NSMutableArray *rects = [NSMutableArray arrayWithCapacity:count];
	for (uint32_t i = 0; i < count; i++) {
		double v[4];
		memcpy(v, q + 32 * i, 32);
		[rects addObject:@[ @(v[0]), @(v[1]), @(v[2]), @(v[3]) ]];
	}
	PGOverlay *o = self.overlays[display];
	if (!o) {
		if (count == 0) return;
		o = [[PGOverlay alloc] initWithDisplay:display];
		self.overlays[display] = o;
	}
	[o setRects:rects fadeIn:fadeIn fadeOut:fadeOut];
}

- (void)clearCovers:(NSDictionary *)d {
	NSString *display = pg_str(d[@"display"]);
	for (PGOverlay *o in self.overlays.allValues) {
		if (display.length == 0 || [o.displayID isEqualToString:display]) [o setRects:@[] fadeIn:0.15 fadeOut:1.0];
	}
}

- (void)applyStatus:(NSDictionary *)st {
	self.status = st;
	NSString *state = pg_str(st[@"state"]);
	NSStatusBarButton *b = self.statusItem.button;
	b.contentTintColor = nil;
	if ([state isEqualToString:@"covering"]) {
		b.image = [self symbol:@"eye.slash.fill"];
		b.contentTintColor = [NSColor systemOrangeColor];
	} else if ([state isEqualToString:@"warning"] || [state isEqualToString:@"error"]) {
		b.image = [self symbol:@"exclamationmark.triangle.fill"];
		b.contentTintColor = [NSColor systemOrangeColor];
	} else if ([state isEqualToString:@"permission"]) {
		b.image = [self symbol:@"exclamationmark.shield"];
	} else if ([state isEqualToString:@"paused"]) {
		b.image = [self symbol:@"shield.slash"];
	} else {
		b.image = [self symbol:@"shield.lefthalf.filled"];
	}
	b.toolTip = [NSString stringWithFormat:@"PSV Guard — %@", pg_str(st[@"title"])];
	self.titleLabel.stringValue = pg_str(st[@"title"]);
	self.detailLabel.stringValue = pg_str(st[@"detail"]);
	self.toggleButton.title = [st[@"enabled"] boolValue] ? @"Turn Off" : @"Turn On";
	self.permissionButton.hidden = ![st[@"permissionButton"] boolValue];
	for (NSView *v in self.panel.contentView.subviews.firstObject.subviews) {
		if ([v.identifier isEqualToString:@"disclaimer"]) {
			NSString *ver = pg_str(st[@"version"]);
			((NSTextField *)v).stringValue = ver.length ? [NSString stringWithFormat:@"%@\nPSV Guard %@", pg_str(st[@"disclaimer"]), ver]
			                                            : pg_str(st[@"disclaimer"]);
		}
	}
}

- (void)setStats:(NSDictionary *)st {
	NSMutableArray *lines = [NSMutableArray array];
	for (NSDictionary *d in st[@"displays"]) {
		[lines addObject:[NSString stringWithFormat:@"%@ %@ · %.0f fps (captured %.0f) · watch %.2f ms\nhot %.1f%% · covers %@ · dropped %@ · repeated %@ · misses %@",
		                                            pg_str(d[@"name"]), pg_str(d[@"size"]), [d[@"fps"] doubleValue],
		                                            [d[@"captured"] doubleValue], [d[@"watchMs"] doubleValue], [d[@"hotArea"] doubleValue],
		                                            pg_str(d[@"covers"]), pg_str(d[@"dropped"]), pg_str(d[@"repeated"]), pg_str(d[@"misses"])]];
	}
	self.statsLabel.stringValue = [lines componentsJoinedByString:@"\n"];
}

- (void)setControls:(NSArray *)controls {
	if (![controls isKindOfClass:[NSArray class]]) return;
	for (NSView *v in [self.settingsStack.arrangedSubviews copy]) {
		[self.settingsStack removeArrangedSubview:v];
		[v removeFromSuperview];
	}
	for (NSDictionary *c in controls) {
		NSString *key = pg_str(c[@"key"]), *kind = pg_str(c[@"kind"]), *label = pg_str(c[@"label"]);
		NSString *value = pg_str(c[@"value"]), *help = pg_str(c[@"help"]);
		NSView *row = nil;
		if ([kind isEqualToString:@"bool"]) {
			NSButton *cb = [NSButton checkboxWithTitle:label target:self action:@selector(checkChanged:)];
			cb.state = [value isEqualToString:@"true"] ? NSControlStateValueOn : NSControlStateValueOff;
			cb.identifier = key;
			cb.toolTip = help;
			row = cb;
		} else if ([kind isEqualToString:@"float"]) {
			NSSlider *sl = [NSSlider sliderWithValue:value.doubleValue minValue:[c[@"min"] doubleValue] maxValue:[c[@"max"] doubleValue]
			                                  target:self action:@selector(sliderChanged:)];
			sl.continuous = NO;
			sl.identifier = key;
			sl.toolTip = help;
			[sl.widthAnchor constraintEqualToConstant:120].active = YES;
			NSTextField *val = pg_label([NSString stringWithFormat:@"%g", value.doubleValue], 11, NO, YES);
			val.identifier = [key stringByAppendingString:@".value"];
			NSStackView *st = [NSStackView stackViewWithViews:@[ pg_label(label, 12, NO, NO), sl, val ]];
			st.toolTip = help;
			row = st;
		} else if ([kind isEqualToString:@"int"]) {
			NSTextField *f = [NSTextField textFieldWithString:value];
			f.identifier = key;
			f.toolTip = help;
			f.target = self;
			f.action = @selector(fieldChanged:);
			f.cell.sendsActionOnEndEditing = YES;
			[f.widthAnchor constraintEqualToConstant:50].active = YES;
			row = [NSStackView stackViewWithViews:@[ pg_label(label, 12, NO, NO), f ]];
		} else if ([kind isEqualToString:@"choice"]) {
			NSPopUpButton *p = [[NSPopUpButton alloc] init];
			for (NSDictionary *ch in c[@"choices"]) {
				[p addItemWithTitle:pg_str(ch[@"label"])];
				p.lastItem.representedObject = pg_str(ch[@"value"]);
			}
			for (NSMenuItem *it in p.itemArray) {
				if ([it.representedObject isEqualToString:value]) [p selectItem:it];
			}
			p.identifier = key;
			p.toolTip = help;
			p.target = self;
			p.action = @selector(popupChanged:);
			row = [NSStackView stackViewWithViews:@[ pg_label(label, 12, NO, NO), p ]];
		}
		if (row) [self.settingsStack addArrangedSubview:row];
	}
}

- (void)checkChanged:(NSButton *)s {
	[self action:@"setting" key:s.identifier value:(s.state == NSControlStateValueOn ? @"true" : @"false")];
}
- (void)sliderChanged:(NSSlider *)s {
	[self action:@"setting" key:s.identifier value:[NSString stringWithFormat:@"%.1f", s.doubleValue]];
}
- (void)fieldChanged:(NSTextField *)s { [self action:@"setting" key:s.identifier value:s.stringValue]; }
- (void)popupChanged:(NSPopUpButton *)s {
	[self action:@"setting" key:s.identifier value:s.selectedItem.representedObject ?: @""];
}

- (void)notify:(NSDictionary *)d {
	// Overlays always work on macOS, so this only runs if Go asks; show it in the panel.
	self.titleLabel.stringValue = pg_str(d[@"title"]);
	self.detailLabel.stringValue = pg_str(d[@"body"]);
	[self showPanel];
}

- (void)startCapture:(NSDictionary *)d {
	PGStream *ps = [[PGStream alloc] init];
	ps.streamID = [d[@"stream"] unsignedLongLongValue];
	ps.displayID = pg_str(d[@"display"]);
	ps.handle = (uintptr_t)[d[@"handle"] unsignedLongLongValue];
	ps.queue = dispatch_queue_create("com.briannunez.psvguard.capture",
	                                 dispatch_queue_attr_make_with_qos_class(DISPATCH_QUEUE_SERIAL, QOS_CLASS_USER_INTERACTIVE, 0));
	self.streams[@(ps.streamID)] = ps;
	int width = [d[@"width"] intValue], height = [d[@"height"] intValue], fps = [d[@"fps"] intValue];
	CGDirectDisplayID did = (CGDirectDisplayID)[ps.displayID longLongValue];

	[SCShareableContent getShareableContentExcludingDesktopWindows:NO
	                                           onScreenWindowsOnly:NO
	                                             completionHandler:^(SCShareableContent *content, NSError *err) {
		dispatch_async(dispatch_get_main_queue(), ^{
			if (ps.stopRequested) { [ps reportStopped]; [self.streams removeObjectForKey:@(ps.streamID)]; return; }
			if (err || !content) { [ps fail:err]; [self.streams removeObjectForKey:@(ps.streamID)]; return; }
			SCDisplay *display = nil;
			for (SCDisplay *sd in content.displays) {
				if (sd.displayID == did) display = sd;
			}
			if (!display) {
				[ps fail:[NSError errorWithDomain:@"psvguard" code:1 userInfo:@{NSLocalizedDescriptionKey : @"display not found"}]];
				[self.streams removeObjectForKey:@(ps.streamID)];
				return;
			}
			// Leave psvguard's own windows, the covers above all, out of the capture.
			pid_t me = getpid();
			NSMutableArray *apps = [NSMutableArray array];
			for (SCRunningApplication *a in content.applications) {
				if (a.processID == me) [apps addObject:a];
			}
			SCContentFilter *filter;
			if (apps.count > 0) {
				filter = [[SCContentFilter alloc] initWithDisplay:display excludingApplications:apps exceptingWindows:@[]];
			} else {
				NSMutableArray *own = [NSMutableArray array];
				for (SCWindow *w in content.windows) {
					if (w.owningApplication.processID == me) [own addObject:w];
				}
				filter = [[SCContentFilter alloc] initWithDisplay:display excludingWindows:own];
			}
			SCStreamConfiguration *cfg = [[SCStreamConfiguration alloc] init];
			cfg.width = width;
			cfg.height = height;
			cfg.minimumFrameInterval = CMTimeMake(1, fps > 0 ? fps : 60);
			cfg.pixelFormat = kCVPixelFormatType_32BGRA;
			cfg.colorSpaceName = kCGColorSpaceSRGB;
			cfg.showsCursor = NO;
			cfg.scalesToFit = YES;
			cfg.queueDepth = 4;
			SCStream *stream = [[SCStream alloc] initWithFilter:filter configuration:cfg delegate:ps];
			NSError *addErr = nil;
			if (![stream addStreamOutput:ps type:SCStreamOutputTypeScreen sampleHandlerQueue:ps.queue error:&addErr]) {
				[ps fail:addErr];
				[self.streams removeObjectForKey:@(ps.streamID)];
				return;
			}
			ps.stream = stream;
			[stream startCaptureWithCompletionHandler:^(NSError *startErr) {
				dispatch_async(dispatch_get_main_queue(), ^{
					if (startErr) {
						[ps fail:startErr];
						[self.streams removeObjectForKey:@(ps.streamID)];
						return;
					}
					pg_emit(PGEvStreamStarted, [ps info:nil]);
					if (ps.stopRequested) [self stopStream:ps];
				});
			}];
		});
	}];
}

- (void)stopStream:(PGStream *)ps {
	ps.stopRequested = YES;
	if (!ps.stream) return; // still starting: reported once the start finishes
	[ps.stream stopCaptureWithCompletionHandler:^(NSError *err) {
		dispatch_async(dispatch_get_main_queue(), ^{
			[ps.stream removeStreamOutput:ps type:SCStreamOutputTypeScreen error:nil];
			[ps reportStopped];
			[self.streams removeObjectForKey:@(ps.streamID)];
		});
	}];
}

- (void)stopCapture:(NSDictionary *)d {
	NSNumber *sid = @([d[@"stream"] unsignedLongLongValue]);
	PGStream *ps = self.streams[sid];
	if (!ps) {
		pg_emit(PGEvStreamStopped, @{@"stream" : sid, @"display" : pg_str(d[@"display"])});
		return;
	}
	[self stopStream:ps];
}

- (void)quit {
	for (PGStream *ps in self.streams.allValues) {
		ps.stopRequested = YES;
		[ps.stream stopCaptureWithCompletionHandler:nil];
	}
	for (PGOverlay *o in self.overlays.allValues) [o.panel orderOut:nil];
	[NSApp stop:nil];
	// -stop: takes effect after the next event, so post one.
	NSEvent *e = [NSEvent otherEventWithType:NSEventTypeApplicationDefined location:NSZeroPoint modifierFlags:0
	                               timestamp:0 windowNumber:0 context:nil subtype:0 data1:0 data2:0];
	[NSApp postEvent:e atStart:YES];
}

@end

static PGApp *gApp;

void pg_run(const char *version) {
	@autoreleasepool {
		gEvents = dispatch_queue_create("com.briannunez.psvguard.events", DISPATCH_QUEUE_SERIAL);
		[NSApplication sharedApplication];
		[NSApp setActivationPolicy:NSApplicationActivationPolicyAccessory];
		gApp = [[PGApp alloc] init];
		gApp.version = [NSString stringWithUTF8String:version ?: ""];
		[gApp setup];
		pg_emit(PGEvCapabilities, @{
			@"excludesOwnWindows" : @YES,
			@"canOverlay" : @YES,
			@"perDisplayCapture" : @YES,
			@"consentPerSession" : @NO,
			@"pixelFormats" : @[ @"BGRA" ],
		});
		pg_emit(PGEvDisplays, [gApp displayList]);
		pg_emit(PGEvReady, nil);
		[NSApp run];
	}
}

void pg_post(int kind, const void *data, size_t len) {
	NSData *d = (data && len) ? [NSData dataWithBytes:data length:len] : [NSData data];
	dispatch_async(dispatch_get_main_queue(), ^{ [gApp apply:kind data:d]; });
}
*/
import "C"
