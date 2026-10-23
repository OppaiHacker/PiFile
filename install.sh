#!/usr/bin/env bash
# PiFile installer: dependencies, build, and "Convert…" in your file managers.
# Usage: ./install.sh [-y]   (-y answers yes to every question)
# All questions come first; then independent steps run in parallel.
set -euo pipefail
cd "$(dirname "$0")"

BIN="$HOME/.local/bin"
YES=0
[[ ${1:-} == -y ]] && YES=1

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ask() {
	((YES)) && return 0
	local a
	read -rp "$1 [Y/n] " a
	[[ -z $a || $a == [yYtT]* ]]
}

# ---- questions -------------------------------------------------------------
. /etc/os-release
case " $ID ${ID_LIKE:-} " in
*" arch "*)
	pm=(sudo pacman -S --needed --noconfirm)
	pkgs=(go gcc pkgconf gtk4 gtk4-layer-shell ffmpeg imagemagick libvips ghostscript poppler tesseract tesseract-data-eng libarchive file libnotify)
	nautilus_dev=libnautilus-extension ;;
*" debian "* | *" ubuntu "*)
	pm=(sudo apt-get install -y)
	pkgs=(golang-go gcc pkg-config libgtk-4-dev libgtk4-layer-shell-dev ffmpeg imagemagick libvips-tools ghostscript poppler-utils tesseract-ocr libarchive-tools file libnotify-bin)
	nautilus_dev=libnautilus-extension-dev ;;
*" fedora "* | *" rhel "*)
	pm=(sudo dnf install -y)
	pkgs=(golang gcc pkgconf gtk4-devel gtk4-layer-shell-devel ffmpeg-free ImageMagick vips-tools ghostscript poppler-utils tesseract bsdtar file libnotify)
	nautilus_dev=nautilus-devel ;;
*" suse "* | *" opensuse "*)
	pm=(sudo zypper install -y)
	pkgs=(go gcc pkg-config gtk4-devel gtk4-layer-shell-devel ffmpeg ImageMagick vips-tools ghostscript poppler-tools tesseract-ocr bsdtar file libnotify-tools)
	nautilus_dev=nautilus-devel ;;
*)
	pm=() pkgs=() nautilus_dev= ;;
esac

bold "System: ${PRETTY_NAME:-$ID}"
want_pkgs=0
if ((${#pm[@]})); then
	echo "Packages: ${pkgs[*]}"
	ask "Install them (needs sudo)?" && want_pkgs=1
else
	echo "Unknown distribution. Install by hand: go, gcc, pkg-config, gtk4 and gtk4-layer-shell (dev),"
	echo "ffmpeg, imagemagick, vips, ghostscript, poppler, tesseract, bsdtar (libarchive), file, notify-send."
fi

default_fm=$(xdg-mime query default inode/directory 2>/dev/null | tr '[:upper:]' '[:lower:]' || true)
found=() chosen=()
for fm in nautilus dolphin thunar nemo caja pcmanfm pcmanfm-qt; do
	command -v "$fm" >/dev/null && found+=("$fm")
done
if ((${#found[@]} == 0)); then
	echo "No supported file manager found; use pifile from the terminal (pifile --help)."
else
	bold "File managers found"
	for i in "${!found[@]}"; do
		mark=
		[[ $default_fm == *"${found[i]}"* && ! ($default_fm == *pcmanfm-qt* && ${found[i]} == pcmanfm) ]] && mark=" (default)"
		echo "  $((i + 1))) ${found[i]}$mark"
	done
	pick=
	((YES)) || read -rp "Add \"Convert…\" to which? Numbers separated by spaces, Enter = all: " pick
	if [[ -z $pick ]]; then
		chosen=("${found[@]}")
	else
		for n in $pick; do
			[[ $n =~ ^[0-9]+$ ]] && ((n >= 1 && n <= ${#found[@]})) && chosen+=("${found[n - 1]}")
		done
	fi
fi

want_ext=0
if [[ " ${chosen[*]} " == *" nautilus "* ]]; then
	ask "Nautilus: middle click on a file opens the radial menu. Install that extension (needs sudo)?" && want_ext=1
fi
want_engines=0
ask "Download LibreOffice, pandoc and calibre for best document quality (about 700 MB, no root)?" && want_engines=1

# ---- work ------------------------------------------------------------------
((want_pkgs || want_ext)) && sudo -v # ask for the password once, up front

pkg_install() {
	"${pm[@]}" "$@" && return 0
	local p missing=()
	for p in "$@"; do "${pm[@]}" "$p" || missing+=("$p"); done
	((${#missing[@]} == 0)) || echo "Not installed (install by hand if you need them): ${missing[*]}"
}

# Go modules download while the package manager runs.
command -v go >/dev/null && GOTOOLCHAIN=auto go mod download 2>/dev/null &
if ((want_pkgs)); then
	extra=()
	((want_ext)) && [[ -n $nautilus_dev ]] && extra=("$nautilus_dev")
	bold "Installing packages"
	pkg_install "${pkgs[@]}" "${extra[@]}"
fi
wait

mkdir -p "$BIN"
# Debian/Ubuntu ship ImageMagick 6, which has `convert` but no `magick`.
if ! command -v magick >/dev/null && command -v convert >/dev/null; then
	printf '#!/bin/sh\nexec convert "$@"\n' >"$BIN/magick"
	chmod +x "$BIN/magick"
fi

ext_build() {
	if ! pkg-config --exists libnautilus-extension-4; then
		echo "libnautilus-extension-4 (Nautilus 43+) not found; right-click > Scripts > Convert… still works."
		return
	fi
	local tmp
	tmp=$(mktemp -d)
	cc -shared -fPIC -O2 -o "$tmp/libpifile-nautilus.so" nautilus/pifile-nautilus.c \
		$(pkg-config --cflags --libs libnautilus-extension-4 gtk4)
	sudo install -Dm644 "$tmp/libpifile-nautilus.so" \
		"$(pkg-config --variable=extensiondir libnautilus-extension-4)/libpifile-nautilus.so"
	rm -rf "$tmp"
	echo "Nautilus extension installed"
}

bold "Building (the first build compiles GTK bindings and can take several minutes)"
((want_ext)) && { ext_build & }
GOTOOLCHAIN=auto go build -o "$BIN/pifile.new" . 2> >(grep -v -E 'cgo-generated|free|^#' >&2)
mv "$BIN/pifile.new" "$BIN/pifile"
echo "Installed $BIN/pifile"
case ":$PATH:" in *":$BIN:"*) ;; *) echo "Note: $BIN is not in PATH; add it in your shell profile." ;; esac

# Engines download in the background while the file managers are set up.
((want_engines)) && { "$BIN/pifile" engines & }
((${#chosen[@]})) && "$BIN/pifile" install "${chosen[@]}"
wait

for fm in "${chosen[@]}"; do
	case $fm in
	nautilus) ((want_ext)) && { nautilus -q 2>/dev/null || true; } ;;
	dolphin) command -v kbuildsycoca6 >/dev/null && kbuildsycoca6 >/dev/null 2>&1 || true ;;
	thunar) thunar -q 2>/dev/null || true ;;
	esac
done

bold "Done"
"$BIN/pifile" tools
