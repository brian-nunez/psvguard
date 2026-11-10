// Command psvguard watches every display with the psv flash detector and covers the parts of the
// screen that start flashing. It runs as a menu bar app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"runtime/debug"
	"strings"

	"github.com/brian-nunez/psvguard/internal/app"
	"github.com/brian-nunez/psvguard/internal/bridge"
	"github.com/brian-nunez/psvguard/internal/settings"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

// memoryLimit is the soft heap limit. While the screen flashes, psv's region search allocates
// several megabytes per frame that are garbage at once; the limit makes the collector reclaim them
// before the heap grows to many times its live size. GOMEMLIMIT overrides it.
const memoryLimit = 256 << 20

func main() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(memoryLimit)
	}

	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	// Older macOS releases pass a process serial number to apps opened from the Finder.
	kept := args[:0:0]
	for _, a := range args {
		if !strings.HasPrefix(a, "-psn_") {
			kept = append(kept, a)
		}
	}

	fs := flag.NewFlagSet("psvguard", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fl := settings.Register(fs)
	verbose := fs.Bool("verbose", false, "log decisions and per-display stats (frame rate, time per frame, hot area, covers, misses) to stderr")
	trace := fs.Bool("trace-bridge", false, "log every bridge event and command with a timestamp to stderr")
	showVersion := fs.Bool("version", false, "print the version and exit")

	if err := fs.Parse(kept); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}

		return 2
	}

	if *showVersion {
		_, _ = fmt.Fprintln(stderr, "psvguard", version)

		return 0
	}

	logger := log.New(io.Discard, "", 0)
	if *verbose || *trace {
		logger = log.New(stderr, "psvguard ", log.Ltime|log.Lmicroseconds)
	}

	store, err := settings.DefaultStore()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "psvguard:", err)

		return 1
	}

	saved, err := store.Load()
	if err != nil {
		logger.Printf("%v; using defaults", err)

		saved = map[string]string{}
	}

	overrides, err := fl.Overrides()
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "psvguard:", err)

		return 2
	}

	s, savedErr, flagErr := settings.Resolve(saved, overrides)
	if flagErr != nil {
		_, _ = fmt.Fprintln(stderr, "psvguard:", flagErr)

		return 2
	}

	if savedErr != nil {
		logger.Printf("ignoring invalid saved settings in %s: %v", store.Path, savedErr)
	}

	bus, err := bridge.New(bridge.Options{Log: logger, Trace: *trace, Version: version})
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "psvguard:", err)

		return 1
	}

	ctrl := app.New(app.Options{Bus: bus, Settings: s, Store: store, Log: logger, Version: version})

	go func() {
		if err := ctrl.Run(context.Background()); err != nil {
			logger.Printf("controller: %v", err)
		}
	}()

	if err := bus.Run(); err != nil {
		_, _ = fmt.Fprintln(stderr, "psvguard:", err)

		return 1
	}

	return 0
}
