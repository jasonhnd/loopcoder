#!/bin/sh
set -eu

REPO="${LOOPCODER_INSTALL_REPO:-jasonhnd/loopcoder}"
GITHUB_BASE_URL="${GITHUB_BASE_URL:-https://github.com}"
GITHUB_API_URL="${GITHUB_API_URL:-https://api.github.com}"
BIN_DIR="${LOOPCODER_INSTALL_DIR:-}"
REQUESTED_VERSION="${LOOPCODER_VERSION:-}"
COSIGN_ISSUER="${LOOPCODER_COSIGN_ISSUER:-https://token.actions.githubusercontent.com}"
CHECKSUM_SIGNATURE_ASSET="SHA256SUMS.sigstore"
SUPPORTED_OS="darwin"
SUPPORTED_ARCH="arm64"
UNSUPPORTED_PLATFORM_FIRST_LINE="LoopCoder v0.8.0 supports macOS Apple Silicon only (darwin/arm64)."
UNSUPPORTED_PLATFORM_GUIDANCE="LoopCoder v0.7.0 is the final legacy multi-platform release for Windows, Linux, WSL, containers, and Intel macOS."

usage() {
	printf '%s\n' \
		'Usage:' \
		'  install.sh [--version VERSION]' \
		'' \
		'Installs loopcoder from GitHub Releases into ~/.loopcoder/bin by default.' \
		'Override the install directory with LOOPCODER_INSTALL_DIR (absolute path).' \
		'Set LOOPCODER_NO_MODIFY_PATH=1 to print PATH instructions without editing profiles.' \
		'' \
		'Examples:' \
		'  curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh' \
		'  curl -fsSL https://raw.githubusercontent.com/jasonhnd/loopcoder/main/scripts/install.sh | sh -s -- --version 0.3.3' \
		'  LOOPCODER_INSTALL_DIR="$HOME/tools/loopcoder" curl -fsSL .../install.sh | sh'
}

fail() {
	printf >&2 'loopcoder install: %s\n' "$*"
	exit 1
}

unsupported_platform() {
	actual_os="$1"
	actual_arch="$2"
	printf >&2 '%s\n' "$UNSUPPORTED_PLATFORM_FIRST_LINE"
	printf >&2 'Actual platform: %s/%s.\n' "$actual_os" "$actual_arch"
	printf >&2 'Supported platform: %s/%s.\n' "$SUPPORTED_OS" "$SUPPORTED_ARCH"
	printf >&2 '%s\n' "$UNSUPPORTED_PLATFORM_GUIDANCE"
	exit 78
}

need_cmd() {
	command -v "$1" >/dev/null 2>&1 || fail "$1 is required"
}

normalize_os() {
	case "$1" in
		Darwin*|darwin) printf '%s\n' darwin ;;
		Linux*|linux) printf '%s\n' linux ;;
		MINGW*|MSYS*|CYGWIN*|Windows*|windows|windows*) printf '%s\n' windows ;;
		"") printf '%s\n' unknown ;;
		*) printf '%s\n' "$1" ;;
	esac
}

normalize_arch() {
	case "$1" in
		x86_64|amd64) printf '%s\n' amd64 ;;
		arm64|aarch64) printf '%s\n' arm64 ;;
		"") printf '%s\n' unknown ;;
		*) printf '%s\n' "$1" ;;
	esac
}

detect_os() {
	if [ -n "${LOOPCODER_INSTALL_OS:-}" ]; then
		normalize_os "$LOOPCODER_INSTALL_OS"
		return
	fi
	command -v uname >/dev/null 2>&1 || fail "uname is required"
	normalize_os "$(uname -s)"
}

detect_arch() {
	if [ -n "${LOOPCODER_INSTALL_ARCH:-}" ]; then
		normalize_arch "$LOOPCODER_INSTALL_ARCH"
		return
	fi
	command -v uname >/dev/null 2>&1 || fail "uname is required"
	normalize_arch "$(uname -m)"
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

OS="$(detect_os)"
ARCH="$(detect_arch)"
if [ "$OS" != "$SUPPORTED_OS" ] || [ "$ARCH" != "$SUPPORTED_ARCH" ]; then
	unsupported_platform "$OS" "$ARCH"
fi

[ -n "${HOME:-}" ] || fail "HOME is not set"
if [ -z "$BIN_DIR" ]; then
	BIN_DIR="$HOME/.loopcoder/bin"
else
	# Require an absolute install directory so PATH/profile lines are unambiguous.
	case "$BIN_DIR" in
		/*) ;;
		*) fail "LOOPCODER_INSTALL_DIR must be an absolute path, got: $BIN_DIR" ;;
	esac
fi
# Resolve once; every install, PATH, and profile path reuses this value.
BIN_DIR="${BIN_DIR%/}"
[ -n "$BIN_DIR" ] || fail "install directory resolved to empty"

need_cmd curl
need_cmd tar
need_cmd awk
need_cmd sed
need_cmd cosign

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

verify_checksums_signature() {
	sums_file="$1"
	signature_bundle="$2"
	identity="$3"
	issuer="$4"

	if ! cosign verify-blob "$sums_file" \
		--bundle "$signature_bundle" \
		--certificate-identity "$identity" \
		--certificate-oidc-issuer "$issuer"; then
		fail "failed to verify SHA256SUMS signature with cosign identity $identity and issuer $issuer"
	fi
}

# Emit a single-quoted shell literal for path values that may contain spaces
# or other metacharacters. Safe for profile lines on zsh/bash/sh.
shell_single_quote() {
	printf "'"
	# Close, escape, and reopen around each embedded single quote.
	printf '%s' "$1" | sed "s/'/'\\\\''/g"
	printf "'"
}

# One export line for the resolved install directory.
path_export_line() {
	printf 'export PATH=%s:$PATH\n' "$(shell_single_quote "$BIN_DIR")"
}

# True when the profile already references this install directory (literal
# absolute path, or the portable $HOME/.loopcoder/bin form for the default).
profile_has_bin_dir() {
	profile="$1"
	if grep -F "$BIN_DIR" "$profile" >/dev/null 2>&1; then
		return 0
	fi
	default_bin="$HOME/.loopcoder/bin"
	if [ "$BIN_DIR" = "$default_bin" ]; then
		if grep -F '$HOME/.loopcoder/bin' "$profile" >/dev/null 2>&1; then
			return 0
		fi
	fi
	return 1
}

print_path_instructions() {
	profile_label="$1"
	reload_command="$2"
	printf '\n%s is not on PATH.\n' "$BIN_DIR"
	printf 'Add this line to %s:\n' "$profile_label"
	printf '  '
	path_export_line
	if [ -n "$reload_command" ]; then
		printf 'Then run:\n'
		printf '  %s\n' "$reload_command"
	fi
}

ensure_path() {
	case ":${PATH:-}:" in
		*":$BIN_DIR:"*)
			printf '%s is already on PATH.\n' "$BIN_DIR"
			return
			;;
	esac

	# Explicit opt-out: print guidance only, never edit profiles.
	case "${LOOPCODER_NO_MODIFY_PATH:-}" in
		1|true|TRUE|yes|YES)
			print_path_instructions "your shell profile" ""
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
			print_path_instructions "your $shell_name profile" ""
			return
			;;
	esac

	if { [ -f "$profile" ] || touch "$profile" 2>/dev/null; } && [ -w "$profile" ]; then
		if profile_has_bin_dir "$profile"; then
			printf 'A loopcoder PATH entry already exists in %s.\n' "$profile_label"
		else
			{
				printf '\n# loopcoder\n'
				path_export_line
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

TAG="$(normalize_tag "$REQUESTED_VERSION")"
ASSET_VERSION="${TAG#v}"
[ -n "$ASSET_VERSION" ] || fail "version resolved to an empty asset version"

ARCHIVE="loopcoder_${ASSET_VERSION}_${SUPPORTED_OS}_${SUPPORTED_ARCH}.tar.gz"
RELEASE_URL="$GITHUB_BASE_URL/$REPO/releases/download/$TAG"
COSIGN_IDENTITY="${LOOPCODER_COSIGN_IDENTITY:-${GITHUB_BASE_URL%/}/$REPO/.github/workflows/release.yml@refs/tags/$TAG}"

tmp_dir=$(mktemp -d "${TMPDIR:-/tmp}/loopcoder-install.XXXXXX") || fail "failed to create temporary directory"
cleanup() {
	rm -rf "$tmp_dir"
}
trap cleanup EXIT HUP INT TERM

archive_path="$tmp_dir/$ARCHIVE"
sums_path="$tmp_dir/SHA256SUMS"
signature_path="$tmp_dir/$CHECKSUM_SIGNATURE_ASSET"
extract_dir="$tmp_dir/extract"

mkdir -p "$extract_dir"

printf 'Installing loopcoder %s for %s/%s...\n' "$ASSET_VERSION" "$OS" "$ARCH"
download "$RELEASE_URL/SHA256SUMS" "$sums_path" "SHA256SUMS"
download "$RELEASE_URL/$CHECKSUM_SIGNATURE_ASSET" "$signature_path" "$CHECKSUM_SIGNATURE_ASSET"
verify_checksums_signature "$sums_path" "$signature_path" "$COSIGN_IDENTITY" "$COSIGN_ISSUER"
download "$RELEASE_URL/$ARCHIVE" "$archive_path" "$ARCHIVE"

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
