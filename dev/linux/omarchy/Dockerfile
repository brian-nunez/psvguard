# A Hyprland desktop like Omarchy's, for building psvguard and testing it by hand through noVNC.
# Development only: it is not how psvguard ships.
#
# Arch is rolling: the base image is pinned, packages come from the live mirrors at build time.
# (The Arch Linux Archive snapshots were tried and serve some packages that fail their signatures.)

# Stage 1: fetch noVNC and websockify, which Arch does not package.
FROM alpine:3.22.2 AS novnc

ARG NOVNC_VERSION=1.6.0
ARG NOVNC_SHA256=5066103959ef4e9b10f37e5a148627360dd8414e4cf8a7db92bdbd022e728aaa
ARG WEBSOCKIFY_VERSION=0.13.0
ARG WEBSOCKIFY_SHA256=b6413e364efd04f3c92ec8c17747e3c4adc20157c2ef1c5d019a26d944a46df8

RUN apk add --no-cache curl \
 && curl -fsSL -o /tmp/novnc.tar.gz "https://github.com/novnc/noVNC/archive/refs/tags/v${NOVNC_VERSION}.tar.gz" \
 && echo "${NOVNC_SHA256}  /tmp/novnc.tar.gz" | sha256sum -c - \
 && curl -fsSL -o /tmp/websockify.tar.gz "https://github.com/novnc/websockify/archive/refs/tags/v${WEBSOCKIFY_VERSION}.tar.gz" \
 && echo "${WEBSOCKIFY_SHA256}  /tmp/websockify.tar.gz" | sha256sum -c - \
 && mkdir -p /opt/novnc /opt/websockify \
 && tar -xzf /tmp/novnc.tar.gz -C /opt/novnc --strip-components=1 \
 && tar -xzf /tmp/websockify.tar.gz -C /opt/websockify --strip-components=1 \
 && ln -s vnc.html /opt/novnc/index.html

# Stage 2: the desktop.
FROM archlinux:base-20261101.0.620205

LABEL maintainer="Brian Nunez" \
      version="1" \
      description="Hyprland desktop for psvguard development, viewed through noVNC" \
      platform="linux/amd64"

# labwc runs headless and hosts Hyprland on its Wayland backend (sway 1.12's xdg-shell is too old
# for Hyprland 0.57); wayvnc serves Hyprland's output. gtk3, gtk-layer-shell, libayatana-appindicator
# and pipewire carry the headers psvguard's cgo bus builds against.
RUN echo 'Server = https://geo.mirror.pkgbuild.com/$repo/os/$arch' > /etc/pacman.d/mirrorlist \
 && pacman-key --init && pacman-key --populate archlinux \
 && pacman -Syy --noconfirm archlinux-keyring \
 && pacman -Su --noconfirm \
 && pacman -S --noconfirm --needed \
      base-devel git curl procps-ng python \
      hyprland xdg-desktop-portal xdg-desktop-portal-hyprland hyprland-guiutils \
      labwc wayvnc waybar wayland-utils foot ttf-dejavu \
      pipewire wireplumber mesa dbus libnotify mako \
      gtk3 gtk-layer-shell libayatana-appindicator \
      go golangci-lint ffmpeg jq wlr-randr grim \
 && pacman -Scc --noconfirm \
 && getcap -r /usr/bin 2>/dev/null | awk '{print $1}' | xargs -r setcap -r
# sway and others carry file capabilities (cap_sys_nice) outside Docker's bounding set, which makes
# exec fail with "Operation not permitted"; dropping them only loses realtime scheduling.

# Hyprland needs a GPU. On hosts with NVIDIA's proprietary driver, Mesa cannot drive the GPU, so the
# userspace driver must match the host's kernel module exactly: pass NVIDIA_VERSION (the host's
# /sys/module/nvidia/version) and use docker-compose.nvidia.yml. Other GPUs use Mesa as installed.
ARG NVIDIA_VERSION=""
RUN if [ -n "${NVIDIA_VERSION}" ]; then \
      pacman -U --noconfirm "https://archive.archlinux.org/packages/n/nvidia-utils/nvidia-utils-${NVIDIA_VERSION}-1-x86_64.pkg.tar.zst" \
      && pacman -Scc --noconfirm; \
    fi

COPY --from=novnc /opt/novnc /opt/novnc
COPY --from=novnc /opt/websockify /opt/websockify

# The same uid as the repository's owner, so build output in the bind mount is not root-owned.
ARG UID=1000
RUN useradd --create-home --uid "${UID}" --shell /bin/bash dev \
 && install -d -o dev -g dev /home/dev/go

COPY --chmod=0755 session.sh /usr/local/bin/psvguard-session
COPY hyprland.lua /etc/psvguard-dev/hyprland.lua
COPY labwc/rc.xml /etc/psvguard-dev/labwc/rc.xml
COPY waybar.jsonc /etc/psvguard-dev/waybar.jsonc

USER dev
WORKDIR /src
ENV XDG_RUNTIME_DIR=/tmp/xdg-dev \
    GOPATH=/home/dev/go \
    GOFLAGS=-buildvcs=false

EXPOSE 6080
CMD ["/usr/local/bin/psvguard-session"]
