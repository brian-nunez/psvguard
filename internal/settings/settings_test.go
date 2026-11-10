package settings

import (
	"flag"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/brian-nunez/psv"
)

func TestEveryFieldHasAFlagAndAValidDefault(t *testing.T) {
	def := Defaults()

	for _, f := range Fields {
		v, err := def.Get(f.Key)
		if err != nil {
			t.Fatalf("%s: %v", f.Key, err)
		}

		var s Settings
		if err := s.Set(f.Key, v); err != nil {
			t.Errorf("default for %s is invalid: %v", f.Key, err)
		}

		if f.Label == "" || f.Help == "" {
			t.Errorf("%s needs a label and help", f.Key)
		}
	}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	Register(fs)

	for _, f := range Fields {
		if fs.Lookup(f.Key) == nil {
			t.Errorf("no flag for %s", f.Key)
		}
	}
}

func TestSetValidates(t *testing.T) {
	s := Defaults()

	for key, bad := range map[string]string{
		"enabled":      "maybe",
		"preset":       "nope",
		"hot-peak":     "9",
		"min-hot-area": "0.5",
		"calm":         "100",
		"fps":          "500",
	} {
		if err := s.Set(key, bad); err == nil {
			t.Errorf("Set(%s, %q) accepted an invalid value", key, bad)
		}
	}

	if err := s.Set("unknown", "1"); err == nil {
		t.Error("Set accepted an unknown key")
	}

	if s != Defaults() {
		t.Error("a rejected value changed the settings")
	}
}

func TestResolveOrderIsDefaultSavedFlag(t *testing.T) {
	saved := map[string]string{"hot-peak": "5", "calm": "4", "min-hot-area": "oops", "retired-key": "x"}

	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fl := Register(fs)

	if err := fs.Parse([]string{"-calm", "6"}); err != nil {
		t.Fatal(err)
	}

	flags, err := fl.Overrides()
	if err != nil {
		t.Fatal(err)
	}

	s, savedErr, flagErr := Resolve(saved, flags)
	if flagErr != nil {
		t.Fatal(flagErr)
	}

	if savedErr == nil {
		t.Error("an invalid saved value was not reported")
	}

	if s.HotPeak != 5 || s.CalmSeconds != 6 || s.MinHotArea != Defaults().MinHotArea {
		t.Fatalf("resolved %+v", s)
	}
}

func TestOverridesRejectInvalidFlags(t *testing.T) {
	fs := flag.NewFlagSet("t", flag.ContinueOnError)
	fl := Register(fs)

	if err := fs.Parse([]string{"-hot-peak", "99"}); err != nil {
		t.Fatal(err)
	}

	if _, err := fl.Overrides(); err == nil {
		t.Fatal("an invalid flag was accepted")
	}
}

func TestStoreRoundTrip(t *testing.T) {
	st := Store{Path: filepath.Join(t.TempDir(), "psvguard", "settings.json")}

	m, err := st.Load()
	if err != nil || len(m) != 0 {
		t.Fatalf("empty store: %v %v", m, err)
	}

	s := Defaults()
	s.Preset, s.FullArea = "trace24", 55

	if err := st.Save(s); err != nil {
		t.Fatal(err)
	}

	m, err = st.Load()
	if err != nil {
		t.Fatal(err)
	}

	got := Defaults()
	if err := got.Apply(m); err != nil || got != s {
		t.Fatalf("round trip: %+v, %v", got, err)
	}

	if _, err := os.Stat(st.Path + ".tmp"); !os.IsNotExist(err) {
		t.Error("temporary file left behind")
	}
}

func TestThresholdsAndParams(t *testing.T) {
	s := Defaults()
	if s.Thresholds() != psv.WCAG22() {
		t.Error("default preset is not WCAG 2.2")
	}

	s.Preset = "broadcast"
	if s.Thresholds() != psv.Broadcast() {
		t.Error("broadcast preset")
	}

	s.CalmSeconds, s.HotPeak = 2.5, 5

	p := s.Params(false)
	if p.Calm != 2500*time.Millisecond || p.HotPeak != 5 || p.ExcludesOwnWindows {
		t.Fatalf("params = %+v", p)
	}
}
