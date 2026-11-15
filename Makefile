# psvguard build.
#
#   make build        build the Go binary into build/psvguard
#   make app          assemble and ad-hoc sign "dist/PSV Guard.app" (macOS)
#   make run          build the app and start it with --verbose, logging to the terminal
#   make test         race tests, the example-video and synthetic-clip tests (FFmpeg), and the
#                     Linux and no-cgo builds that prove the core is platform-neutral
#   make test-short   the same without the video tests
#   make smoke        the macOS capture smoke test (needs Screen Recording permission)
#   make release VERSION=vX.Y.Z
#                     clean dist/, build a universal (arm64 and amd64) app, zip it and write
#                     dist/SHA256SUMS
#   make test-scripts test the release scripts
#   make vet | lint | fmt | fmt-check | clean
#
# Releases are made by GitHub Actions (.github/workflows/release.yml), not from a local machine.

BUNDLE_ID := com.briannunez.psvguard
APP_NAME  := PSV Guard
VERSION   ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS   := -X main.version=$(VERSION)
# Info.plist versions are numbers, so the tag's leading v is dropped.
PLIST_VERSION := $(patsubst v%,%,$(VERSION))

BUILD := build
DIST  := dist
APP   := $(DIST)/$(APP_NAME).app
# The release archive keeps the same name in every release, so releases/latest/download/ links work.
ZIP   := $(DIST)/PSV-Guard-macos.zip

.PHONY: all build universal app release run test test-short test-portable test-scripts smoke vet lint fmt fmt-check clean

all: app

build:
	@mkdir -p $(BUILD)
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD)/psvguard ./cmd/psvguard

# The icon is rendered from assets/icon.svg with tools that ship with macOS.
$(BUILD)/AppIcon.icns: assets/icon.svg
	@rm -rf $(BUILD)/icon $(BUILD)/AppIcon.iconset && mkdir -p $(BUILD)/icon $(BUILD)/AppIcon.iconset
	qlmanage -t -s 1024 -o $(BUILD)/icon assets/icon.svg >/dev/null
	@for s in 16 32 128 256 512; do \
		sips -z $$s $$s $(BUILD)/icon/icon.svg.png --out $(BUILD)/AppIcon.iconset/icon_$${s}x$${s}.png >/dev/null; \
		sips -z $$((s*2)) $$((s*2)) $(BUILD)/icon/icon.svg.png --out $(BUILD)/AppIcon.iconset/icon_$${s}x$${s}@2x.png >/dev/null; \
	done
	iconutil -c icns $(BUILD)/AppIcon.iconset -o $@

# A universal binary for Apple silicon and Intel Macs: each architecture is built with cgo and
# joined with lipo. Used for releases; make build targets this Mac only.
universal:
	@mkdir -p $(BUILD)
	CGO_ENABLED=1 GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD)/psvguard-arm64 ./cmd/psvguard
	CGO_ENABLED=1 GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o $(BUILD)/psvguard-amd64 ./cmd/psvguard
	lipo -create -output $(BUILD)/psvguard $(BUILD)/psvguard-arm64 $(BUILD)/psvguard-amd64
	@archs=$$(lipo -archs $(BUILD)/psvguard); [ "$$archs" = "x86_64 arm64" ] || { echo "universal binary has '$$archs', expected 'x86_64 arm64'"; exit 1; }

# Assembles and ad-hoc signs the bundle around $(BUILD)/psvguard. The bundle ID and signing identity
# stay the same across builds so Screen Recording permission is attributed to PSV Guard; see
# README.md if it needs resetting.
define bundle-app
	@rm -rf "$(APP)"
	@mkdir -p "$(APP)/Contents/MacOS" "$(APP)/Contents/Resources"
	cp $(BUILD)/psvguard "$(APP)/Contents/MacOS/psvguard"
	cp $(BUILD)/AppIcon.icns "$(APP)/Contents/Resources/AppIcon.icns"
	sed -e 's/__VERSION__/$(PLIST_VERSION)/g' -e 's/__BUNDLE_ID__/$(BUNDLE_ID)/g' packaging/macos/Info.plist > "$(APP)/Contents/Info.plist"
	plutil -lint "$(APP)/Contents/Info.plist" >/dev/null
	codesign --force --sign - --identifier $(BUNDLE_ID) "$(APP)"
	codesign --verify --verbose=1 "$(APP)"
	@echo "Built $(APP) ($(VERSION))"
endef

# macOS only.
app: build $(BUILD)/AppIcon.icns
	$(bundle-app)

# Release files in dist/: the universal app, zipped with ditto so the signature and bundle
# metadata survive, plus SHA256SUMS.
release: universal $(BUILD)/AppIcon.icns
	rm -rf $(DIST)
	$(bundle-app)
	ditto -c -k --sequesterRsrc --keepParent "$(APP)" $(ZIP)
	cd $(DIST) && shasum -a 256 $(notdir $(ZIP)) > SHA256SUMS
	@cat $(DIST)/SHA256SUMS

run: app
	"$(APP)/Contents/MacOS/psvguard" --verbose

# The race detector slows psv too much for the video tests, so they run separately without it.
test: test-short
	CGO_ENABLED=1 go test -count=1 -run 'TestExampleVideos|TestSyntheticClips' ./internal/monitor/

test-short: test-portable
	CGO_ENABLED=1 go test -race -short ./...

# Everything except the macOS bus must build, vet and test without macOS and without cgo.
test-portable:
	GOOS=linux go build ./...
	GOOS=linux go vet ./...
	CGO_ENABLED=0 go build ./...
	CGO_ENABLED=0 go test -short ./internal/...

test-scripts:
	bash scripts/next-version_test.sh

smoke:
	PSVGUARD_SMOKE=1 go test -count=1 -run Smoke -v ./internal/bridge/

vet:
	CGO_ENABLED=1 go vet ./...
	GOOS=linux go vet ./...

# Linters and formatters are configured in .golangci.yml (golangci-lint v2).
lint:
	CGO_ENABLED=1 golangci-lint run ./...
	GOOS=linux golangci-lint run ./...

fmt:
	golangci-lint fmt ./...

fmt-check:
	golangci-lint fmt --diff ./...

clean:
	rm -rf $(BUILD) $(DIST)
