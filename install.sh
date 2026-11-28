#!/usr/bin/env bash
set -euo pipefail

MIN_GO_MAJOR=1
MIN_GO_MINOR=27
MIN_GO_PATCH=4

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"

prefix=""
skip_tests=0
check_only=0
init_config=0
tmp_dir=""

usage() {
    cat <<'EOF'
Usage: ./install.sh [OPTIONS]

Build and install the brd server binary.

Options:
  --prefix DIR    Set the install directory.
                  Default: /usr/local/bin if writable or root, else $HOME/.local/bin.
  --skip-tests    Skip go vet and go test before build.
  --check-only    Check host prerequisites and exit.
  --init-config   Create config.json with mode 0600 if missing.
  -h, --help      Show this help message and exit.
EOF
}

default_prefix() {
    if [[ "${EUID:-$(id -u)}" -eq 0 ]] || [[ -d "/usr/local/bin" && -w "/usr/local/bin" ]]; then
        printf '%s\n' "/usr/local/bin"
    elif [[ -n "${HOME:-}" ]]; then
        printf '%s\n' "${HOME}/.local/bin"
    else
        printf 'Error: HOME is unset or empty, and you cannot write to /usr/local/bin. Pass --prefix DIR.\n' >&2
        return 1
    fi
}

cleanup() {
    if [[ -n "${tmp_dir:-}" && -d "$tmp_dir" ]]; then
        rm -rf "$tmp_dir"
    fi
}

on_signal() {
    cleanup
    exit 1
}

trap cleanup EXIT
trap on_signal INT TERM HUP

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --prefix)
                if [[ $# -lt 2 || -z "$2" ]]; then
                    printf 'Error: --prefix requires a directory path.\n' >&2
                    exit 1
                fi
                prefix="$2"
                shift 2
                ;;
            --prefix=*)
                prefix="${1#*=}"
                if [[ -z "$prefix" ]]; then
                    printf 'Error: --prefix requires a directory path.\n' >&2
                    exit 1
                fi
                shift
                ;;
            --skip-tests)
                skip_tests=1
                shift
                ;;
            --check-only)
                check_only=1
                shift
                ;;
            --init-config)
                init_config=1
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            -*)
                printf "Error: Unknown option '%s'. Run './install.sh --help' for usage.\n" "$1" >&2
                exit 1
                ;;
            *)
                printf "Error: Unexpected argument '%s'. Run './install.sh --help' for usage.\n" "$1" >&2
                exit 1
                ;;
        esac
    done
}

check_go() {
    local go_bin raw_ver major minor patch
    if ! go_bin="$(command -v go 2>/dev/null)"; then
        printf 'Error: Go is not installed. Install Go %d.%d.%d or newer.\n' \
            "$MIN_GO_MAJOR" "$MIN_GO_MINOR" "$MIN_GO_PATCH" >&2
        return 1
    fi

    raw_ver="$(go env GOVERSION 2>/dev/null || true)"
    if [[ -z "$raw_ver" ]]; then
        raw_ver="$(go version 2>/dev/null || true)"
    fi

    if [[ "$raw_ver" =~ go([0-9]+)\.([0-9]+)(\.([0-9]+))? ]]; then
        major="${BASH_REMATCH[1]}"
        minor="${BASH_REMATCH[2]}"
        patch="${BASH_REMATCH[4]:-0}"
    else
        printf "Error: Installer could not parse Go version '%s'. Install Go %d.%d.%d or newer.\n" \
            "$raw_ver" "$MIN_GO_MAJOR" "$MIN_GO_MINOR" "$MIN_GO_PATCH" >&2
        return 1
    fi

    if (( 10#$major < MIN_GO_MAJOR ||
          (10#$major == MIN_GO_MAJOR && 10#$minor < MIN_GO_MINOR) ||
          (10#$major == MIN_GO_MAJOR && 10#$minor == MIN_GO_MINOR && 10#$patch < MIN_GO_PATCH) )); then
        printf 'Error: Go version %s is too old. Install Go %d.%d.%d or newer.\n' \
            "$raw_ver" "$MIN_GO_MAJOR" "$MIN_GO_MINOR" "$MIN_GO_PATCH" >&2
        return 1
    fi

    printf 'OK: Go %s (%s)\n' "$raw_ver" "$go_bin"
    return 0
}

check_browser() {
    local candidate found
    if [[ -n "${CHROME_PATH:-}" ]]; then
        if [[ -f "$CHROME_PATH" && -x "$CHROME_PATH" ]]; then
            printf 'OK: Browser %s (from CHROME_PATH)\n' "$CHROME_PATH"
            return 0
        fi
        printf "Error: CHROME_PATH '%s' is not an executable file. Set CHROME_PATH to a valid browser binary.\n" "$CHROME_PATH" >&2
        return 1
    fi

    for candidate in chromium chromium-browser google-chrome; do
        if found="$(command -v "$candidate" 2>/dev/null)"; then
            printf 'OK: Browser %s\n' "$found"
            return 0
        fi
    done

    printf 'Error: Chromium is not installed. Install chromium, chromium-browser, or google-chrome, or set CHROME_PATH.\n' >&2
    return 1
}

report_optional_tools() {
    local tool_path
    if tool_path="$(command -v g++ 2>/dev/null)"; then
        printf 'Optional OK: g++ (%s) for firmware unit tests.\n' "$tool_path"
    else
        printf 'Optional missing: g++ (needed only for firmware unit tests).\n'
    fi

    if tool_path="$(command -v esphome 2>/dev/null)"; then
        printf 'Optional OK: esphome (%s) for firmware flashing.\n' "$tool_path"
    else
        printf 'Optional missing: esphome (needed only for firmware flashing).\n'
    fi
}

check_prerequisites() {
    local failed=0
    if ! check_go; then
        failed=1
    fi
    if ! check_browser; then
        failed=1
    fi
    report_optional_tools

    if [[ "$failed" -ne 0 ]]; then
        printf 'Required prerequisites failed. Install the missing tools. Run this script again.\n' >&2
        return 1
    fi
    return 0
}

init_config_file() {
    local example_path target_path
    example_path="${SCRIPT_DIR}/config.example.json"
    target_path="${SCRIPT_DIR}/config.json"

    if [[ ! -f "$example_path" ]]; then
        printf 'Error: config.example.json does not exist in %s.\n' "$SCRIPT_DIR" >&2
        exit 1
    fi
    if [[ -e "$target_path" ]]; then
        printf 'Config file already exists at %s. Installer kept the existing file.\n' "$target_path"
        return 0
    fi

    install -m 0600 "$example_path" "$target_path"
    printf 'Created config file at %s with mode 0600. Edit this file before you start brd.\n' "$target_path"
}

main() {
    parse_args "$@"

    if [[ -z "$prefix" ]]; then
        prefix="$(default_prefix)"
    fi

    if ! check_prerequisites; then
        exit 1
    fi

    if [[ "$check_only" -eq 1 ]]; then
        printf 'All required prerequisites passed.\n'
        exit 0
    fi

    cd "$SCRIPT_DIR"

    if [[ "$skip_tests" -eq 0 ]]; then
        printf 'Running go vet ./...\n'
        go vet ./...
        printf 'Running go test -race ./...\n'
        go test -race ./...
    fi

    tmp_dir="$(mktemp -d)"
    printf 'Building ./cmd/brd\n'
    go build -o "${tmp_dir}/brd" ./cmd/brd

    mkdir -p -- "$prefix"
    install -m 0755 -- "${tmp_dir}/brd" "${prefix}/brd"
    printf 'Installed brd to %s/brd\n' "$prefix"

    if [[ "$init_config" -eq 1 ]]; then
        init_config_file
    fi
}

main "$@"
