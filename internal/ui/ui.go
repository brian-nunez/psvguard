// Package ui is the contract between the controller and whatever draws the menu bar item and the
// status panel. The controller sends view models; the UI reports actions. Both cross the bridge as
// JSON, so the native side never knows Go types.
package ui

import (
	"github.com/brian-nunez/psvguard/internal/settings"
)

// State is the overall state shown in the menu bar.
type State string

// States.
const (
	Starting   State = "starting"
	Permission State = "permission" // capture permission is needed
	Paused     State = "paused"     // protection is off, or the system is asleep
	Watching   State = "watching"
	Covering   State = "covering"
	Warning    State = "warning" // flashing found but the platform cannot cover it
	Error      State = "error"
)

// Disclaimer is shown in the panel at all times.
const Disclaimer = "Experimental aid: it reduces exposure to flashing but does not eliminate it, and it does not detect spatial patterns."

// Status is the menu bar state and the panel header.
type Status struct {
	State   State  `json:"state"`
	Title   string `json:"title"`
	Detail  string `json:"detail"`
	Enabled bool   `json:"enabled"`
	// PermissionButton shows the button that opens the system's capture permission settings.
	PermissionButton bool   `json:"permissionButton"`
	Disclaimer       string `json:"disclaimer"`
	Version          string `json:"version"`
}

// Control is one setting rendered in the panel.
type Control struct {
	Key     string            `json:"key"`
	Label   string            `json:"label"`
	Help    string            `json:"help"`
	Kind    settings.Kind     `json:"kind"`
	Value   string            `json:"value"`
	Min     float64           `json:"min"`
	Max     float64           `json:"max"`
	Choices []settings.Option `json:"choices,omitempty"`
}

// Controls returns the panel controls for s.
func Controls(s settings.Settings) []Control {
	out := make([]Control, 0, len(settings.Fields))

	for _, f := range settings.Fields {
		if f.Hidden {
			continue
		}

		v, _ := s.Get(f.Key)
		out = append(out, Control{
			Key: f.Key, Label: f.Label, Help: f.Help, Kind: f.Kind, Value: v,
			Min: f.Min, Max: f.Max, Choices: f.Choices,
		})
	}

	return out
}

// DisplayStats is the debug view of one display.
type DisplayStats struct {
	ID       string  `json:"id"`
	Name     string  `json:"name"`
	Size     string  `json:"size"`
	FPS      float64 `json:"fps"`      // frames analysed per second
	Captured float64 `json:"captured"` // frames received per second
	WatchMs  float64 `json:"watchMs"`  // mean time per psv Watch call
	HotArea  float64 `json:"hotArea"`  // percent of the display
	Covers   int     `json:"covers"`
	Dropped  uint64  `json:"dropped"`
	Repeated uint64  `json:"repeated"`
	Misses   uint64  `json:"misses"`
}

// Stats is the debug view.
type Stats struct {
	Displays []DisplayStats `json:"displays"`
}

// ActionKind is what the user did.
type ActionKind string

// Actions.
const (
	Toggle            ActionKind = "toggle"
	SetSetting        ActionKind = "setting"
	ResetSettings     ActionKind = "resetSettings" // restore every setting except on/off to its default
	RequestPermission ActionKind = "requestPermission"
	OpenPermission    ActionKind = "openPermission"
	Quit              ActionKind = "quit"
)

// Action is one user action.
type Action struct {
	Kind  ActionKind `json:"kind"`
	Key   string     `json:"key,omitempty"`
	Value string     `json:"value,omitempty"`
}
