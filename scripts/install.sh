#!/bin/sh
set -eu

REPO="${LOOPCODER_INSTALL_REPO:-jasonhnd/loopcoder}"
GITHUB_BASE_URL="${GITHUB_BASE_URL:-https://github.com}"
GITHUB_API_URL="${GITHUB_API_URL:-https://api.github.com}"
BIN_DIR="${LOOPCODER_INSTALL_DIR:-$HOME/.loopcoder/bin}"
REQUESTED_VERSION="${LOOPCODER_VERSION:-}"

usage() {
	printf '%s\n' \
		'Usage:' \
		'  install.sh [--version VERSION]' \
		'' \
		'Installs loopcoder from GitHub Releases into ~/.loopcoder/bin.' \
		'' \
		'Examples:' \
		'  curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh' \
		'  curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.3.3'
}

fail() {
	printf >&2 'loopcoder install: %s\n' "$*"
	exit 1
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

while [ "$#" -gt 0 ]; do
	case "$1" in
		--version)
			[ "$#" -gt 1 ] || fail "--version requires a value"
			REQUESTED_VERSION="$2"
			shift 2
			;;
		--version=*)
			REQUESTED_VERSION="${1#--version=}"
			shift
			;;
		-v)
			[ "$#" -gt 1 ] || fail "-v requires a value"
			REQUESTED_VERSION="$2"
			shift 2
			;;
		-h|--help)
			usage
			exit 0
			;;
		*)
			fail "unknown argument: $1"
			;;
	esac
done

[ -n "${HOME:-}" ] || fail "HOME is not set"

need_cmd curl
need_cmd uname
need_cmd tar
need_cmd awk
need_cmd sed

if command -v sha256sum >/dev/null 2>&1; then
	sha256_file() {
		sha256sum "$1" | awk '{print $1}'
	}
elif command -v shasum >/dev/null 2>&1; then
	sha256_file() {
		shasum -a 256 "$1" | awk '{print $1}'
	}
else
	fail "sha256sum or shasum is required to verify SHA256SUMS"
fi

detect_os() {
	case "$(uname -s)" in
		Linux*) printf '%s\n' linux ;;
		Darwin*) printf '%s\n' darwin ;;
		*) fail "unsupported OS: $(uname -s). Use install.ps1 on Windows." ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
		x86_64|amd64) printf '%s\n' amd64 ;;
		arm64|aarch64) printf '%s\n' arm64 ;;
		*) fail "unsupported architecture: $(uname -m)" ;;
	esac
}

resolve_latest_tag() {
	latest_json=""
	if ! latest_json=$(curl -fsSL \
		-H "Accept: application/vnd.github+json" \
		-H "User-Agent: loopcoder-install" \
		"$GITHUB_API_URL/repos/$REPO/releases/latest"); then
		fail "failed to resolve latest release from GitHub"
	fi

	tag=$(printf '%s\n' "$latest_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | awk 'NR == 1 {print}')
	[ -n "$tag" ] || fail "GitHub latest release response did not include tag_name"
	printf '%s\n' "$tag"
}

normalize_tag() {
	version="$1"
	if [ -z "$version" ] || [ "$version" = "latest" ]; then
		resolve_latest_tag
		return
	fi

	case "$version" in
		v*) printf '%s\n' "$version" ;;
		*) printf 'v%s\n' "$version" ;;
	esac
}

download() {
	url="$1"
	out="$2"
	label="$3"
	if ! curl -fL --silent --show-error \
		-H "User-Agent: loopcoder-install" \
		-o "$out" "$url"; then
		fail "failed to download $label from $url"
	fi
}

extract_expected_hash() {
	sums_file="$1"
	archive_name="$2"
	awk -v file="$archive_name" '
		{
			name = $2
			sub(/^\*/, "", name)
			if (name == file) {
				print $1
				exit
			}
		}
	' "$sums_file"
}

print_path_instructions() {
	profile_label="$1"
	reload_command="$2"
	printf '\n%s is not on PATH.\n' "$BIN_DIR"
	printf 'Add this line to %s:\n' "$profile_label"
	printf '  export PATH="$HOME/.loopcoder/bin:$PATH"\n'
	printf 'Then run:\n'
	printf '  %s\n' "$reload_command"
}

ensure_path() {
	case ":${PATH:-}:" in
		*":$BIN_DIR:"*)
			printf '%s is already on PATH.\n' "$BIN_DIR"
			return
			;;
	esac

	shell_path="${SHELL:-sh}"
	shell_name="${shell_path##*/}"
	profile=""
	profile_label=""
	reload_command=""

	case "$shell_name" in
		zsh)
			profile="$HOME/.zshrc"
			profile_label="~/.zshrc"
			reload_command="source ~/.zshrc"
			;;
		bash)
			profile="$HOME/.bashrc"
			profile_label="~/.bashrc"
			reload_command="source ~/.bashrc"
			;;
		sh|dash|ksh)
			profile="$HOME/.profile"
			profile_label="~/.profile"
			reload_command=". ~/.profile"
			;;
		*)
			print_path_instructions "your $shell_name profile" "export PATH=\"\$HOME/.loopcoder/bin:\$PATH\""
			return
			;;
	esac

	if { [ -f "$profile" ] || touch "$profile" 2>/dev/null; } && [ -w "$profile" ]; then
		if grep -F ".loopcoder/bin" "$profile" >/dev/null 2>&1; then
			printf 'A loopcoder PATH entry already exists in %s.\n' "$profile_label"
		else
			{
				printf '\n# loopcoder\n'
				printf 'export PATH="$HOME/.loopcoder/bin:$PATH"\n'
			} >>"$profile" || {
				print_path_instructions "$profile_label" "$reload_command"
				return
			}
			printf 'Added %s to PATH in %s.\n' "$BIN_DIR" "$profile_label"
		fi
		printf 'Open a new terminal or run: %s\n' "$reload_command"
	else
		print_path_instructions "$profile_label" "$reload_command"
	fi
}

OS="$(detect_os)"
ARCH="$(detect_arch)"
TAG="$(normalize_tag "$REQUESTED_VERSION")"
ASSET_VERSION="${TAG#v}"
[ -n "$ASSET_VERSION" ] || fail "version resolved to an empty asset version"

ARCHIVE="loopcoder_${ASSET_VERSION}_${OS}_${ARCH}.tar.gz"
RELEASE_URL="$GITHUB_BASE_URL/$REPO/releases/download/$TAG"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-install.XXXXXX") || fail "failed to create temporary directory"
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

archive_path="$tmp_dir/$ARCHIVE"
sums_path="$tmp_dir/SHA256SUMS"
extract_dir="$tmp_dir/extract"

mkdir -p "$extract_dir"

printf 'Installing loopcoder %s for %s/%s...\n' "$ASSET_VERSION" "$OS" "$ARCH"
download "$RELEASE_URL/$ARCHIVE" "$archive_path" "$ARCHIVE"
download "$RELEASE_URL/SHA256SUMS" "$sums_path" "SHA256SUMS"

expected_hash=$(extract_expected_hash "$sums_path" "$ARCHIVE")
[ -n "$expected_hash" ] || fail "SHA256SUMS does not contain $ARCHIVE; release may be incomplete"

actual_hash=$(sha256_file "$archive_path")
expected_hash=$(printf '%s' "$expected_hash" | tr '[:upper:]' '[:lower:]')
actual_hash=$(printf '%s' "$actual_hash" | tr '[:upper:]' '[:lower:]')

if [ "$expected_hash" != "$actual_hash" ]; then
	fail "checksum mismatch for $ARCHIVE: expected $expected_hash, got $actual_hash"
fi

if ! tar -xzf "$archive_path" -C "$extract_dir"; then
	fail "failed to extract $ARCHIVE"
fi

source_bin="$extract_dir/loopcoder"
[ -f "$source_bin" ] || fail "$ARCHIVE did not contain loopcoder"

mkdir -p "$BIN_DIR"
tmp_bin="$BIN_DIR/.loopcoder.tmp.$$"
if ! cp "$source_bin" "$tmp_bin"; then
	fail "failed to copy loopcoder to $BIN_DIR"
fi
chmod 755 "$tmp_bin" || fail "failed to make loopcoder executable"
if ! mv "$tmp_bin" "$BIN_DIR/loopcoder"; then
	rm -f "$tmp_bin"
	fail "failed to install loopcoder to $BIN_DIR/loopcoder"
fi

ensure_path

printf '\nInstalled loopcoder %s to %s/loopcoder\n' "$ASSET_VERSION" "$BIN_DIR"
printf 'Run:\n'
printf '  loopcoder --version\n'
printf '  loopcoder doctor\n'
