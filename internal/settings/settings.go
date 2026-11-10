// Package settings defines psvguard's options in one table that drives defaults, validation,
// command-line flags, the saved settings file and the controls the panel shows. Settings resolve
// default, then saved, then flag.
package settings

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/brian-nunez/psv"

	"github.com/brian-nunez/psvguard/internal/coverage"
)

// Settings is the effective configuration.
type Settings struct {
	Enabled      bool
	Preset       string
	HotPeak      int
	MinHotArea   float64
	FlashingArea float64
	FullArea     float64
	CalmSeconds  float64
	FPS          int
	CaptureWidth int
	ShowPanel    bool
}

// Defaults returns the out-of-the-box settings.
func Defaults() Settings {
	p := coverage.DefaultParams()

	return Settings{
		Enabled:      true,
		Preset:       "wcag22",
		HotPeak:      p.HotPeak,
		MinHotArea:   p.MinHotArea,
		FlashingArea: p.FlashingArea,
		FullArea:     p.FullArea,
		CalmSeconds:  p.Calm.Seconds(),
		FPS:          60,
		CaptureWidth: 640,
		ShowPanel:    true,
	}
}

// Preset is a named set of psv thresholds.
type Preset struct {
	Key, Label string
	Thresholds func() psv.Thresholds
}

// Presets lists the threshold presets psv provides.
var Presets = []Preset{
	{"wcag22", "WCAG 2.2", psv.WCAG22},
	{"trace24", "Trace 2024", psv.Trace24},
	{"broadcast", "Broadcast (ITU-R BT.1702)", psv.Broadcast},
}

// Thresholds returns the psv thresholds of the selected preset.
func (s Settings) Thresholds() psv.Thresholds {
	for _, p := range Presets {
		if p.Key == s.Preset {
			return p.Thresholds()
		}
	}

	return psv.DefaultThresholds()
}

// Params returns the covering policy the settings select. excludesOwnWindows comes from the
// platform, not from the settings.
func (s Settings) Params(excludesOwnWindows bool) coverage.Params {
	p := coverage.DefaultParams()
	p.HotPeak = s.HotPeak
	p.MinHotArea = s.MinHotArea
	p.FlashingArea = s.FlashingArea
	p.FullArea = s.FullArea
	p.Calm = time.Duration(s.CalmSeconds * float64(time.Second))
	p.ExcludesOwnWindows = excludesOwnWindows

	return p
}

// Kind tells the UI which control to render.
type Kind string

// Control kinds.
const (
	Bool   Kind = "bool"   // checkbox
	Float  Kind = "float"  // slider between Min and Max
	Int    Kind = "int"    // number field between Min and Max
	Choice Kind = "choice" // pop-up of Choices
)

// Option is one option of a Choice field.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Field describes one option.
type Field struct {
	Key     string // file key and flag name
	Label   string // UI label
	Help    string // flag usage and UI tooltip
	Kind    Kind
	Min     float64
	Max     float64
	Choices []Option
	// Restart reports that changing the option restarts capture.
	Restart bool
	// Hidden fields are flags and saved settings but not panel controls.
	Hidden bool

	get func(*Settings) string
	set func(*Settings, string) error
}

func boolField(key, label, help string, p func(*Settings) *bool) Field {
	return Field{
		Key: key, Label: label, Help: help, Kind: Bool,
		get: func(s *Settings) string { return strconv.FormatBool(*p(s)) },
		set: func(s *Settings, v string) error {
			b, err := strconv.ParseBool(strings.TrimSpace(v))
			if err != nil {
				return fmt.Errorf("%s: want true or false, got %q", key, v)
			}

			*p(s) = b

			return nil
		},
	}
}

func intField(key, label, help string, lo, hi int, p func(*Settings) *int) Field {
	return Field{
		Key: key, Label: label, Help: help, Kind: Int, Min: float64(lo), Max: float64(hi),
		get: func(s *Settings) string { return strconv.Itoa(*p(s)) },
		set: func(s *Settings, v string) error {
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < lo || n > hi {
				return fmt.Errorf("%s: want a whole number from %d to %d, got %q", key, lo, hi, v)
			}

			*p(s) = n

			return nil
		},
	}
}

func floatField(key, label, help string, lo, hi float64, p func(*Settings) *float64) Field {
	return Field{
		Key: key, Label: label, Help: help, Kind: Float, Min: lo, Max: hi,
		get: func(s *Settings) string { return strconv.FormatFloat(*p(s), 'f', -1, 64) },
		set: func(s *Settings, v string) error {
			f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
			if err != nil || f < lo || f > hi {
				return fmt.Errorf("%s: want a number from %g to %g, got %q", key, lo, hi, v)
			}

			*p(s) = f

			return nil
		},
	}
}

func presetChoices() []Option {
	out := make([]Option, len(Presets))
	for i, p := range Presets {
		out[i] = Option{Value: p.Key, Label: p.Label}
	}

	return out
}

// Fields is the single source of truth for every option, in panel order.
var Fields = []Field{
	boolField("enabled", "Protection on", "watch the screen and cover flashing",
		func(s *Settings) *bool { return &s.Enabled }),
	{
		Key: "preset", Label: "Thresholds", Help: "psv threshold preset: wcag22, trace24 or broadcast",
		Kind: Choice, Choices: presetChoices(), Restart: true,
		get: func(s *Settings) string { return s.Preset },
		set: func(s *Settings, v string) error {
			v = strings.TrimSpace(v)
			for _, p := range Presets {
				if p.Key == v {
					s.Preset = v

					return nil
				}
			}

			return fmt.Errorf("preset: want wcag22, trace24 or broadcast, got %q", v)
		},
	},
	intField("hot-peak", "Cover at transitions per second", "cover an area once its pixels make this many transitions within a second (WCAG fails above 6)",
		2, 6, func(s *Settings) *int { return &s.HotPeak }),
	floatField("min-hot-area", "Minimum area (% of display)", "smallest share of the display, in percent, that must be building up flashing before it is covered",
		1, 50, func(s *Settings) *float64 { return &s.MinHotArea }),
	floatField("flashing-area", "Keep covering while flashing over (%)", "keep covers up while this share of the display, in percent, already flashes",
		0.2, 10, func(s *Settings) *float64 { return &s.FlashingArea }),
	floatField("full-area", "Cover whole display above (%)", "cover the whole display when the covered share, in percent, reaches this",
		5, 100, func(s *Settings) *float64 { return &s.FullArea }),
	floatField("calm", "Keep covered after flashing (s)", "seconds an area stays covered after it last built up flashing",
		1, 10, func(s *Settings) *float64 { return &s.CalmSeconds }),
	func() Field {
		f := intField("fps", "Analysis frame rate", "frames per second analysed for each display",
			30, 120, func(s *Settings) *int { return &s.FPS })
		f.Restart, f.Hidden = true, true

		return f
	}(),
	func() Field {
		f := intField("capture-width", "Capture width (px)", "width, in pixels, that each display is scaled to for analysis",
			320, 1280, func(s *Settings) *int { return &s.CaptureWidth })
		f.Restart, f.Hidden = true, true

		return f
	}(),
	boolField("panel", "Show panel on launch", "show the status panel when psvguard starts",
		func(s *Settings) *bool { return &s.ShowPanel }),
}

// FieldByKey returns the field for key.
func FieldByKey(key string) (Field, bool) {
	for _, f := range Fields {
		if f.Key == key {
			return f, true
		}
	}

	return Field{}, false
}

// Get returns the string form of one option.
func (s Settings) Get(key string) (string, error) {
	f, ok := FieldByKey(key)
	if !ok {
		return "", fmt.Errorf("unknown setting %q", key)
	}

	return f.get(&s), nil
}

// Set validates and sets one option.
func (s *Settings) Set(key, value string) error {
	f, ok := FieldByKey(key)
	if !ok {
		return fmt.Errorf("unknown setting %q", key)
	}

	return f.set(s, value)
}

// Map returns every option as strings, the saved file's form.
func (s Settings) Map() map[string]string {
	m := make(map[string]string, len(Fields))
	for _, f := range Fields {
		m[f.Key] = f.get(&s)
	}

	return m
}

// Apply sets every known option in m and returns the errors for invalid ones; unknown keys are
// ignored so older files keep loading.
func (s *Settings) Apply(m map[string]string) error {
	var errs []error

	for _, f := range Fields {
		v, ok := m[f.Key]
		if !ok {
			continue
		}

		if err := f.set(s, v); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// Flags registers one flag per option on fs. After fs.Parse, Overrides returns the options the
// command line set.
type Flags struct {
	fs     *flag.FlagSet
	values map[string]*string
}

// Register adds the option flags to fs.
func Register(fs *flag.FlagSet) *Flags {
	def := Defaults()
	fl := &Flags{fs: fs, values: map[string]*string{}}

	for _, f := range Fields {
		fl.values[f.Key] = fs.String(f.Key, f.get(&def), f.Help)
	}

	return fl
}

// Overrides returns the options set on the command line, validated.
func (fl *Flags) Overrides() (map[string]string, error) {
	out := map[string]string{}

	var errs []error

	fl.fs.Visit(func(fg *flag.Flag) {
		if p, ok := fl.values[fg.Name]; ok {
			var probe Settings
			if err := probe.Set(fg.Name, *p); err != nil {
				errs = append(errs, err)

				return
			}

			out[fg.Name] = *p
		}
	})

	return out, errors.Join(errs...)
}

// Store saves settings as JSON in one file.
type Store struct{ Path string }

// DefaultStore returns the store under the user's configuration directory:
// ~/Library/Application Support/psvguard on macOS, ~/.config/psvguard on Linux.
func DefaultStore() (Store, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Store{}, fmt.Errorf("find the settings folder: %w", err)
	}

	return Store{Path: filepath.Join(dir, "psvguard", "settings.json")}, nil
}

// Load returns the saved options, or none when nothing is saved yet.
func (st Store) Load() (map[string]string, error) {
	b, err := os.ReadFile(st.Path)
	if errors.Is(err, fs.ErrNotExist) {
		return map[string]string{}, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}

	m := map[string]string{}
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("read settings %s: %w", st.Path, err)
	}

	return m, nil
}

// Save writes s, replacing the file atomically.
func (st Store) Save(s Settings) error {
	b, err := json.MarshalIndent(s.Map(), "", "  ")
	if err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	if err := os.MkdirAll(filepath.Dir(st.Path), 0o755); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	tmp := st.Path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}

	if err := os.Rename(tmp, st.Path); err != nil {
		return errors.Join(fmt.Errorf("save settings: %w", err), os.Remove(tmp))
	}

	return nil
}

// Resolve returns the defaults, overridden by saved, overridden by flags. Invalid saved values are
// reported and skipped; invalid flags are an error.
func Resolve(saved, flags map[string]string) (Settings, error, error) {
	s := Defaults()
	savedErr := s.Apply(saved)

	if err := s.Apply(flags); err != nil {
		return s, savedErr, err
	}

	return s, savedErr, nil
}
