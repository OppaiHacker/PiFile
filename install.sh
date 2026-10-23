#!/usr/bin/env bash
# PiFile installer: dependencies, pifile itself, and "Convert…" in your file managers.
#
#   curl -fsSL https://raw.githubusercontent.com/OppaiHacker/PiFile/main/install.sh | bash
#   ./install.sh [-y]          (from a clone; -y answers yes to every question)
#
# Downloads the prebuilt release when it runs on this system and builds from
# source otherwise. All questions come first; independent steps run in parallel.
set -euo pipefail

REPO=OppaiHacker/PiFile
BIN="$HOME/.local/bin"
YES=0
[[ ${1:-} == -y ]] && YES=1

# Source checkout next to this script, if any (empty when piped from curl).
SRC=
if [[ -n ${BASH_SOURCE[0]:-} && -f "$(dirname "${BASH_SOURCE[0]}")/go.mod" ]]; then
	SRC=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
fi
TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

bold() { printf '\n\033[1m%s\033[0m\n' "$*"; }
ask() {
	((YES)) && return 0
	local a
	read -rp "$1 [Y/n] " a </dev/tty
	[[ -z $a || $a == [yYtT]* ]]
}

# ---- questions -------------------------------------------------------------
. /etc/os-release
case " $ID ${ID_LIKE:-} " in
*" arch "*)
	pm=(sudo pacman -S --needed --noconfirm)
	pkgs=(gtk4 gtk4-layer-shell ffmpeg imagemagick libvips ghostscript poppler tesseract tesseract-data-eng libarchive file libnotify curl)
	build_pkgs=(go gcc pkgconf git)
	nautilus_dev=libnautilus-extension ;;
*" debian "* | *" ubuntu "*)
	pm=(sudo apt-get install -y)
	pkgs=(libgtk-4-1 libgtk4-layer-shell0 ffmpeg imagemagick libvips-tools ghostscript poppler-utils tesseract-ocr libarchive-tools file libnotify-bin curl)
	build_pkgs=(golang-go gcc pkg-config libgtk-4-dev libgirepository1.0-dev libgtk4-layer-shell-dev git)
	nautilus_dev=libnautilus-extension-dev ;;
*" fedora "* | *" rhel "*)
	pm=(sudo dnf install -y)
	pkgs=(gtk4 gtk4-layer-shell ffmpeg-free ImageMagick vips-tools ghostscript poppler-utils tesseract bsdtar file libnotify curl)
	build_pkgs=(golang gcc pkgconf gtk4-devel gtk4-layer-shell-devel git)
	nautilus_dev=nautilus-devel ;;
*" suse "* | *" opensuse "*)
	pm=(sudo zypper install -y)
	pkgs=(libgtk-4-1 libgtk4-layer-shell0 ffmpeg ImageMagick vips-tools ghostscript poppler-tools tesseract-ocr bsdtar file libnotify-tools curl)
	build_pkgs=(go gcc pkg-config gtk4-devel gtk4-layer-shell-devel git)
	nautilus_dev=nautilus-devel ;;
*)
	pm=() pkgs=() build_pkgs=() nautilus_dev= ;;
esac

bold "System: ${PRETTY_NAME:-$ID}"
want_pkgs=0
if ((${#pm[@]})); then
	echo "Packages: ${pkgs[*]}"
	ask "Install them (needs sudo)?" && want_pkgs=1
else
	echo "Unknown distribution. Install by hand: gtk4, gtk4-layer-shell, ffmpeg, imagemagick,"
	echo "vips, ghostscript, poppler, tesseract, bsdtar (libarchive), file, notify-send, curl."
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
	((YES)) || read -rp "Add \"Convert…\" to which? Numbers separated by spaces, Enter = all: " pick </dev/tty
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

# ---- helpers ---------------------------------------------------------------
pkg_install() {
	"${pm[@]}" "$@" && return 0
	local p missing=()
	for p in "$@"; do "${pm[@]}" "$p" || missing+=("$p"); done
	((${#missing[@]} == 0)) || echo "Not installed (install by hand if you need them): ${missing[*]}"
}

# fetch NAME: downloads a release asset into $TMP and checks it against SHA256SUMS.
fetch() {
	local url="https://github.com/$REPO/releases/latest/download"
	for f in SHA256SUMS "$1"; do
		[[ -f $TMP/$f ]] && continue
		curl -fsSL "$url/$f" -o "$TMP/$f" 2>/dev/null ||
			{ command -v gh >/dev/null && gh release download -R "$REPO" -p "$f" -D "$TMP" --clobber 2>/dev/null; } ||
			return 1
	done
	(cd "$TMP" && grep " $1\$" SHA256SUMS | sha256sum -c --quiet)
}

# source_dir: a checkout to build from (this one, or a fresh shallow clone).
source_dir() {
	if [[ -z $SRC ]]; then
		SRC="$TMP/src"
		git clone -q --depth 1 "https://github.com/$REPO.git" "$SRC" 2>/dev/null ||
			gh repo clone "$REPO" "$SRC" -- -q --depth 1
	fi
}

# ---- work ------------------------------------------------------------------
((want_pkgs || want_ext)) && sudo -v # ask for the password once, up front

prebuilt=0
if [[ $(uname -m) == x86_64 ]]; then
	{ fetch pifile-linux-amd64 >/dev/null 2>&1 && touch "$TMP/ok"; } &
fi
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

# Prebuilt binary, if it runs here (glibc, GLib and GTK new enough).
if [[ -f $TMP/ok ]]; then
	chmod +x "$TMP/pifile-linux-amd64"
	if "$TMP/pifile-linux-amd64" tools >/dev/null 2>&1; then
		mv "$TMP/pifile-linux-amd64" "$BIN/pifile"
		prebuilt=1
		bold "Installed prebuilt $BIN/pifile"
	fi
fi
if ((!prebuilt)); then
	bold "Building from source (the first build compiles GTK bindings and takes several minutes)"
	((${#pm[@]})) && pkg_install "${build_pkgs[@]}"
	source_dir
	(cd "$SRC" && GOTOOLCHAIN=auto go build -o "$BIN/pifile.new" . 2> >(grep -v -E 'cgo-generated|free|^#' >&2))
	mv "$BIN/pifile.new" "$BIN/pifile"
	echo "Installed $BIN/pifile"
fi
case ":$PATH:" in *":$BIN:"*) ;; *) echo "Note: $BIN is not in PATH; add it in your shell profile." ;; esac

ext_install() {
	local so="$TMP/libpifile-nautilus.so" dir
	dir=$(pkg-config --variable=extensiondir libnautilus-extension-4 2>/dev/null || echo /usr/lib/nautilus/extensions-4)
	# Built against the local Nautilus when its headers are there, else the prebuilt one.
	if pkg-config --exists libnautilus-extension-4 && command -v cc >/dev/null &&
		{ [[ -n $SRC ]] || fetch pifile-nautilus.c; }; then
		local c="$TMP/pifile-nautilus.c"
		[[ -n $SRC ]] && c="$SRC/nautilus/pifile-nautilus.c"
		cc -shared -fPIC -O2 -o "$so" "$c" $(pkg-config --cflags --libs libnautilus-extension-4 gtk4)
	elif ! fetch libpifile-nautilus.so; then
		echo "Could not get the Nautilus extension; right-click > Scripts > Convert… still works."
		return
	fi
	sudo install -Dm644 "$so" "$dir/libpifile-nautilus.so"
	echo "Nautilus extension installed"
}

# Engines download in the background while the file managers are set up.
((want_engines)) && { "$BIN/pifile" engines & }
((want_ext)) && ext_install
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
