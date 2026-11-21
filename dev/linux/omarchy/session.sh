#!/usr/bin/env bash
# Starts the development desktop: a session D-Bus, PipeWire, a headless labwc that hosts Hyprland
# full screen on its Wayland backend, and (from hyprland.lua) wayvnc and noVNC on port 6080.
#
#   psvguard-session            run the whole session (the container's command)
#   psvguard-session hyprland   start Hyprland inside labwc (labwc runs this)
#   psvguard-session run CMD…   run a command inside the running session (its Wayland display,
#                               D-Bus and Hyprland instance), e.g. from docker exec
set -euo pipefail

if [[ "${1:-}" == run ]]; then
	shift
	# Waybar is started by Hyprland, so its environment names Hyprland's own Wayland display;
	# Hyprland's environment names labwc's, the one it is nested in.
	pid=$(pgrep -x waybar | head -n 1)
	[[ -n "$pid" ]] || { echo "psvguard-session: the session is not running" >&2; exit 1; }
	while IFS= read -r -d '' kv; do
		case "$kv" in
		WAYLAND_DISPLAY=* | DBUS_SESSION_BUS_ADDRESS=* | HYPRLAND_INSTANCE_SIGNATURE=* | XDG_*=*) export "${kv?}" ;;
		esac
	done <"/proc/$pid/environ"
	exec "$@"
fi

logs=/tmp/session
mkdir -p "$logs"

if [[ "${1:-}" == hyprland ]]; then
	wlr-randr --output HEADLESS-1 --custom-mode 1600x900 >"$logs/wlr-randr.log" 2>&1 || true
	exec Hyprland --config /etc/psvguard-dev/hyprland.lua >"$logs/hyprland.log" 2>&1
fi

mkdir -p -m 0700 "$XDG_RUNTIME_DIR"

if [[ -z "${DBUS_SESSION_BUS_ADDRESS:-}" ]]; then
	exec dbus-run-session -- "$0" "$@"
fi

pipewire >"$logs/pipewire.log" 2>&1 &
wireplumber >"$logs/wireplumber.log" 2>&1 &

# labwc renders on the GPU passed in; Hyprland cannot run on software rendering.
export WLR_BACKENDS=headless WLR_LIBINPUT_NO_DEVICES=1
labwc --config-dir /etc/psvguard-dev/labwc --startup "$0 hyprland" >"$logs/labwc.log" 2>&1 &
host_pid=$!

# Show the session's logs on the container's output.
tail -F "$logs"/*.log &
wait "$host_pid"
