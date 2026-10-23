#!/usr/bin/env bash
# Builds the release assets in an Ubuntu 26.04 container, the oldest base with
# the GLib 2.88 the GTK bindings need, so they run on that and anything newer:
#
#   docker run --rm -v "$PWD:/src" ubuntu:26.04 /src/release/build.sh
set -euo pipefail
export DEBIAN_FRONTEND=noninteractive
GO=1.25.5

apt-get update -qq
apt-get install -y -qq curl ca-certificates gcc pkg-config libgtk-4-dev libgirepository1.0-dev \
	libgtk4-layer-shell-dev libnautilus-extension-dev >/dev/null

curl -fsSL "https://go.dev/dl/go$GO.linux-amd64.tar.gz" | tar -xz -C /usr/local
export PATH=/usr/local/go/bin:$PATH GOTOOLCHAIN=local

cd /src
out=/src/dist
rm -rf "$out" && mkdir -p "$out"
go build -buildvcs=false -trimpath -ldflags "-s -w" -o "$out/pifile-linux-amd64" . 2> >(grep -v -E 'cgo-generated|free|^#' >&2)
cc -shared -fPIC -O2 -o "$out/libpifile-nautilus.so" nautilus/pifile-nautilus.c \
	$(pkg-config --cflags --libs libnautilus-extension-4 gtk4)
cp nautilus/pifile-nautilus.c "$out/"
cd "$out" && sha256sum * >SHA256SUMS
chown -R "$(stat -c %u:%g /src)" "$out"
ls -l "$out"
