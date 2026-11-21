# An Ubuntu 26.04 GNOME Wayland desktop, for building psvguard and testing it by hand through noVNC.
# Development only: it is not how psvguard ships.

# Stage 1: fetch noVNC and websockify at pinned versions.
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

# Stage 2: fetch golangci-lint's release binary, checked against its published checksum.
FROM alpine:3.22.2 AS golangci

ARG GOLANGCI_LINT_VERSION=2.15.0
ARG GOLANGCI_LINT_SHA256=9b7fe6a6bf6334b98decf8d86b11415ea7889afdabdd1959fdce23af2a9ebcea

RUN apk add --no-cache curl \
 && curl -fsSL -o /tmp/golangci-lint.tar.gz "https://github.com/golangci/golangci-lint/releases/download/v${GOLANGCI_LINT_VERSION}/golangci-lint-${GOLANGCI_LINT_VERSION}-linux-amd64.tar.gz" \
 && echo "${GOLANGCI_LINT_SHA256}  /tmp/golangci-lint.tar.gz" | sha256sum -c - \
 && tar -xzf /tmp/golangci-lint.tar.gz -C /tmp --strip-components=1 \
 && install -m 0755 /tmp/golangci-lint /usr/local/bin/golangci-lint

# Stage 3: rebuild Ubuntu's gnome-remote-desktop source package with its VNC backend, which Ubuntu
# builds without (upstream's default is vnc=false). grd-headless-vnc.patch makes headless VNC mirror
# the session's monitor instead of adding a second, empty virtual one per client, and sets unused
# file descriptors to -1: without a GPU, upstream closes fd 0 (its own listening socket) and then
# spins at full CPU after the first client disconnects. The version gets a +vnc1 suffix so apt
# installs it over the archive's.
FROM ubuntu:resolute-20261024.1 AS grd

ENV DEBIAN_FRONTEND=noninteractive
COPY grd-headless-vnc.patch /build/grd-headless-vnc.patch
RUN sed -i 's/^Types: deb$/Types: deb deb-src/' /etc/apt/sources.list.d/ubuntu.sources \
 && apt-get update \
 && apt-get build-dep -y gnome-remote-desktop \
 && apt-get install -y --no-install-recommends dpkg-dev libvncserver-dev \
 && cd /build \
 && apt-get source gnome-remote-desktop \
 && cd gnome-remote-desktop-*/ \
 && patch -p1 < /build/grd-headless-vnc.patch \
 && sed -i 's/-Dfdk_aac=false/-Dfdk_aac=false -Dvnc=true/' debian/rules \
 && sed -i 's/^Build-Depends:/Build-Depends: libvncserver-dev,/' debian/control \
 && sed -i '1s/(\([^)]*\))/(\1+vnc1)/' debian/changelog \
 && DEB_BUILD_OPTIONS=nocheck dpkg-buildpackage -b -uc -us \
 && mkdir /debs && cp ../gnome-remote-desktop_*.deb /debs/

# Stage 4: the desktop.
FROM ubuntu:resolute-20261024.1

LABEL maintainer="Brian Nunez" \
      version="1" \
      description="Ubuntu 26.04 GNOME Wayland desktop for psvguard development, viewed through noVNC" \
      platform="linux/amd64"

# ubuntu-session brings GNOME Shell, Ubuntu's Shell extensions (ubuntu-appindicators among them)
# and xdg-desktop-portal-gnome. gnome-remote-desktop (rebuilt above) serves the session over VNC;
# vncsnapshot and vncpasswd take screenshots through it. The -dev packages carry the headers
# psvguard's cgo bus builds against; ffmpeg provides ffplay for test videos.
ENV DEBIAN_FRONTEND=noninteractive
COPY --from=grd /debs /tmp/debs
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      /tmp/debs/gnome-remote-desktop_*.deb \
      ubuntu-session gnome-shell gnome-shell-ubuntu-extensions \
      xdg-desktop-portal xdg-desktop-portal-gnome \
      pipewire wireplumber dbus dbus-bin libglib2.0-bin dconf-cli \
      libgl1-mesa-dri libegl-mesa0 \
      libnotify-bin ptyxis fonts-dejavu-core fonts-ubuntu \
      vncsnapshot tigervnc-tools \
      ca-certificates curl procps git jq python3 python3-numpy \
      build-essential pkg-config golang-go \
      libgtk-3-dev libayatana-appindicator3-dev libpipewire-0.3-dev libgtk-layer-shell-dev \
      ffmpeg \
 && rm -rf /var/lib/apt/lists/* /tmp/debs \
 && getcap -r /usr/bin /usr/libexec 2>/dev/null | awk '{print $1}' | xargs -r setcap -r
# Binaries with file capabilities outside Docker's bounding set fail to exec with
# "Operation not permitted"; dropping the capabilities only loses realtime scheduling.

COPY --from=novnc /opt/novnc /opt/novnc
COPY --from=novnc /opt/websockify /opt/websockify
COPY --from=golangci /usr/local/bin/golangci-lint /usr/local/bin/golangci-lint

COPY novnc-defaults.json /opt/novnc/defaults.json
COPY system-bus.conf /etc/psvguard-dev/system-bus.conf
COPY 90_psvguard-dev.gschema.override /usr/share/glib-2.0/schemas/
RUN glib-compile-schemas /usr/share/glib-2.0/schemas

# The same uid as the repository's owner, so build output in the bind mount is not root-owned.
# Ubuntu's image has a user "ubuntu" with uid 1000; it is removed first. ~/Desktop keeps Ubuntu's
# desktop-icons extension from failing in a restart loop.
ARG UID=1000
RUN userdel --remove ubuntu 2>/dev/null || true \
 && useradd --create-home --uid "${UID}" --shell /bin/bash dev \
 && install -d -o dev -g dev /home/dev/Desktop /home/dev/go

COPY --chmod=0755 session.sh /usr/local/bin/psvguard-session

USER dev
WORKDIR /src
ENV XDG_RUNTIME_DIR=/tmp/xdg-dev \
    DBUS_SESSION_BUS_ADDRESS=unix:path=/tmp/xdg-dev/bus \
    DBUS_SYSTEM_BUS_ADDRESS=unix:path=/tmp/dbus-system/socket \
    WAYLAND_DISPLAY=wayland-0 \
    XDG_SESSION_TYPE=wayland \
    XDG_CURRENT_DESKTOP=ubuntu:GNOME \
    XDG_SESSION_DESKTOP=ubuntu \
    DESKTOP_SESSION=ubuntu \
    GNOME_SHELL_SESSION_MODE=ubuntu \
    GOPATH=/home/dev/go \
    GOFLAGS=-buildvcs=false

EXPOSE 6080
CMD ["/usr/local/bin/psvguard-session"]
