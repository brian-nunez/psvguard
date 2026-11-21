#!/usr/bin/env bash
# Starts the development desktop: a private system bus, the session bus, PipeWire, a headless
# GNOME Shell in Ubuntu's session mode on a 1600x900 virtual monitor, gnome-remote-desktop serving
# it over VNC on 127.0.0.1:5900, and noVNC on port 6080.
#
# DBUS_SESSION_BUS_ADDRESS, DBUS_SYSTEM_BUS_ADDRESS, WAYLAND_DISPLAY and the XDG_* variables come
# from the image, so `docker compose exec ubuntu ...` joins the session without extra setup.
set -euo pipefail

logs=/tmp/session
mkdir -p "$logs" /tmp/dbus-system
mkdir -p -m 0700 "$XDG_RUNTIME_DIR"
rm -f "$XDG_RUNTIME_DIR/bus" /tmp/dbus-system/socket

# GNOME Shell needs a system bus. The session bus listens on a fixed path (rather than through
# dbus-run-session's random address) so that `docker exec` shells can reach it.
dbus-daemon --config-file=/etc/psvguard-dev/system-bus.conf --nofork --nopidfile >"$logs/dbus-system.log" 2>&1 &
dbus-daemon --session --address="$DBUS_SESSION_BUS_ADDRESS" --nofork --nopidfile >"$logs/dbus-session.log" 2>&1 &
for _ in $(seq 50); do
	[[ -S "$XDG_RUNTIME_DIR/bus" && -S /tmp/dbus-system/socket ]] && break
	sleep 0.1
done

pipewire >"$logs/pipewire.log" 2>&1 &
wireplumber >"$logs/wireplumber.log" 2>&1 &

# Mutter renders with Mesa's llvmpipe; no GPU is needed. --mode=ubuntu loads Ubuntu's extensions,
# among them ubuntu-appindicators (org.kde.StatusNotifierWatcher). --no-x11: no Xwayland.
gnome-shell --headless --wayland --no-x11 --virtual-monitor 1600x900 --mode=ubuntu >"$logs/gnome-shell.log" 2>&1 &
shell_pid=$!

gdbus wait --session --timeout 60 org.gnome.Mutter.RemoteDesktop

# Headless VNC always asks for a password. The port is published on 127.0.0.1 only, and noVNC's
# defaults.json fills the same password in.
grdctl --headless vnc set-password psvguard >"$logs/grdctl.log" 2>&1
printf 'psvguard\n' | vncpasswd -f >"$logs/vncpasswd"
/usr/libexec/gnome-remote-desktop-daemon --headless </dev/null >"$logs/gnome-remote-desktop.log" 2>&1 &

/opt/websockify/run --web /opt/novnc 0.0.0.0:6080 127.0.0.1:5900 >"$logs/novnc.log" 2>&1 &

# Show the session's logs on the container's output; stop when GNOME Shell exits so the container
# restarts.
tail -F "$logs"/*.log &
wait "$shell_pid"
