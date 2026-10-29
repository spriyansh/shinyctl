#!/bin/sh
# Installs shinyctl from a GitHub release, without root:
#
#   curl -fsSL https://raw.githubusercontent.com/spriyansh/shinyctl/main/install.sh | sh
#
# It picks the archive for this system (Linux or macOS, amd64 or arm64),
# checks it against the release's checksums.txt, and copies the shinyctl
# binary into ~/.local/bin. Three settings, all optional:
#
#   SHINYCTL_VERSION      the release to install, such as v0.1.0 (default: the latest)
#   SHINYCTL_INSTALL_DIR  where the binary goes (default: ~/.local/bin)
#   SHINYCTL_RELEASES     the releases page to download from, for a mirror
#                         (default: https://github.com/spriyansh/shinyctl/releases)
#
#   curl -fsSL .../install.sh | SHINYCTL_VERSION=v0.1.0 sh
#
# Everything is in functions and runs from the last line, so a download cut
# off half way runs nothing.

set -eu

releases=${SHINYCTL_RELEASES:-https://github.com/spriyansh/shinyctl/releases}

say() { printf '%s\n' "$*"; }
fail() {
	printf 'shinyctl install: %s\n' "$*" >&2
	exit 1
}

# fetch URL FILE downloads URL to FILE with curl or wget.
fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q -O "$2" "$1"
	else
		fail "needs curl or wget to download"
	fi
}

# latest prints the tag of the latest release, from where the releases page
# redirects /latest (no API call, so no rate limit).
latest() {
	url="$releases/latest"
	if command -v curl >/dev/null 2>&1; then
		final=$(curl -fsSLI -o /dev/null -w '%{url_effective}' "$url")
	else
		final=$(wget -q -S --spider "$url" 2>&1 | sed -n 's/^ *[Ll]ocation: *//p' | tail -n 1 | tr -d '\r')
	fi
	tag=${final##*/}
	case $tag in
	v[0-9]*) say "$tag" ;;
	*) fail "could not find the latest release at $url" ;;
	esac
}

# sha256 FILE prints FILE's SHA-256.
sha256() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -d ' ' -f 1
	elif command -v shasum >/dev/null 2>&1; then
		shasum -a 256 "$1" | cut -d ' ' -f 1
	else
		fail "needs sha256sum or shasum to check the download"
	fi
}

main() {
	case $(uname -s) in
	Linux) os=linux ;;
	Darwin) os=darwin ;;
	*) fail "shinyctl runs on Linux and macOS, not $(uname -s)" ;;
	esac
	case $(uname -m) in
	x86_64 | amd64) arch=amd64 ;;
	aarch64 | arm64) arch=arm64 ;;
	*) fail "there is no shinyctl build for $(uname -m); amd64 and arm64 are built" ;;
	esac
	command -v tar >/dev/null 2>&1 || fail "needs tar to unpack the download"

	version=${SHINYCTL_VERSION:-}
	[ -n "$version" ] || version=$(latest)
	case $version in
	v*) ;;
	*) version="v$version" ;;
	esac
	dir=${SHINYCTL_INSTALL_DIR:-$HOME/.local/bin}

	name="shinyctl_${version#v}_${os}_${arch}"
	base="$releases/download/$version"
	tmp=$(mktemp -d 2>/dev/null || mktemp -d -t shinyctl)
	trap 'rm -rf "$tmp"' EXIT INT TERM

	say "Downloading shinyctl $version for $os/$arch"
	fetch "$base/$name.tar.gz" "$tmp/$name.tar.gz" || fail "could not download $base/$name.tar.gz"
	fetch "$base/checksums.txt" "$tmp/checksums.txt" || fail "could not download $base/checksums.txt"

	want=$(awk -v f="$name.tar.gz" '$2 == f { print $1 }' "$tmp/checksums.txt")
	[ -n "$want" ] || fail "checksums.txt has no line for $name.tar.gz"
	got=$(sha256 "$tmp/$name.tar.gz")
	[ "$got" = "$want" ] || fail "the download does not match checksums.txt (got $got, want $want)"

	tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
	[ -f "$tmp/$name/shinyctl" ] || fail "the archive has no $name/shinyctl"
	mkdir -p "$dir"
	# Copy next to the old binary, then rename over it, so a shinyctl that is
	# running is never left half written.
	cp "$tmp/$name/shinyctl" "$dir/.shinyctl.new"
	chmod 755 "$dir/.shinyctl.new"
	mv -f "$dir/.shinyctl.new" "$dir/shinyctl"

	say "Installed shinyctl $("$dir/shinyctl" version) to $dir/shinyctl"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*)
		say ""
		say "$dir is not on your PATH yet."
		if [ "$os" = darwin ]; then
			say "Add it, then open a new terminal:"
			say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.zprofile"
		elif [ "$dir" = "$HOME/.local/bin" ]; then
			say "On Debian, Ubuntu and Raspberry Pi OS it is added when you next log in."
			say "Elsewhere, add it to your shell's profile:"
			say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile"
		else
			say "Add it to your shell's profile, then log in again:"
			say "  echo 'export PATH=\"$dir:\$PATH\"' >> ~/.profile"
		fi
		;;
	esac
	if ! command -v Rscript >/dev/null 2>&1; then
		say ""
		say "Rscript is not on your PATH. shinyctl runs apps with it: install R and the shiny package."
	fi
	if [ -x "$(command -v shinyctl 2>/dev/null || true)" ] && [ "$(command -v shinyctl)" != "$dir/shinyctl" ]; then
		say ""
		say "Another shinyctl comes first on your PATH: $(command -v shinyctl)"
	fi
}

main "$@"
