-- Hyprland for psvguard development: close to Omarchy's defaults, without its shell.
hl.monitor({ output = "", mode = "preferred", position = "auto", scale = 1 })

hl.env("XDG_CURRENT_DESKTOP", "Hyprland")
hl.env("XDG_SESSION_DESKTOP", "Hyprland")
hl.env("XDG_SESSION_TYPE", "wayland")
hl.env("GDK_BACKEND", "wayland")

hl.config({
  general = { gaps_in = 5, gaps_out = 10, border_size = 2, layout = "dwindle" },
  misc = { disable_hyprland_logo = true, disable_splash_rendering = true },
})

hl.on("hyprland.start", function()
  -- The portal and its Hyprland backend are D-Bus activated and need the session's variables.
  hl.exec_cmd("dbus-update-activation-environment --all")
  hl.exec_cmd("waybar --config /etc/psvguard-dev/waybar.jsonc > /tmp/session/waybar.log 2>&1")
  hl.exec_cmd("mako > /tmp/session/mako.log 2>&1")
  hl.exec_cmd("wayvnc --render-cursor 127.0.0.1 5900 > /tmp/session/wayvnc.log 2>&1")
  hl.exec_cmd("/opt/websockify/run --web /opt/novnc 0.0.0.0:6080 127.0.0.1:5900 > /tmp/session/novnc.log 2>&1")
  hl.exec_cmd("foot")
end)

hl.bind("SUPER + Return", hl.dsp.exec_cmd("foot"))
for workspace = 1, 4 do
  hl.bind("SUPER + " .. workspace, hl.dsp.focus({ workspace = tostring(workspace) }))
end
