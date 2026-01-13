#!/bin/bash
set -euo pipefail

BINARY_NAME="mono"
GITHUB_REPO="gwuah/mono"
INSTALL_DIR="${INSTALL_DIR:-$HOME/.local/bin}"

RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

info() {
    echo -e "${GREEN}==>${NC} $1"
}

warn() {
    echo -e "${YELLOW}warning:${NC} $1"
}

error() {
    echo -e "${RED}error:${NC} $1" >&2
    exit 1
}

detect_os() {
    local os
    os="$(uname -s)"
    case "$os" in
        Linux*)  echo "linux" ;;
        Darwin*) echo "darwin" ;;
        *)       error "Unsupported operating system: $os" ;;
    esac
}

detect_arch() {
    local arch
    arch="$(uname -m)"
    case "$arch" in
        x86_64)  echo "amd64" ;;
        amd64)   echo "amd64" ;;
        arm64)   echo "arm64" ;;
        aarch64) echo "arm64" ;;
        *)       error "Unsupported architecture: $arch" ;;
    esac
}

get_latest_version() {
    local version
    version=$(curl -fsSL "https://api.github.com/repos/${GITHUB_REPO}/releases/latest" | grep '"tag_name":' | sed -E 's/.*"([^"]+)".*/\1/')
    if [[ -z "$version" ]]; then
        error "Failed to fetch latest version from GitHub"
    fi
    echo "$version"
}

download_binary() {
    local version="$1"
    local os="$2"
    local arch="$3"
    local download_url="https://github.com/${GITHUB_REPO}/releases/download/${version}/${BINARY_NAME}-${os}-${arch}"
    local checksum_url="https://github.com/${GITHUB_REPO}/releases/download/${version}/checksums.txt"
    local tmp_dir
    tmp_dir=$(mktemp -d)
    trap 'rm -rf "$tmp_dir"' EXIT

    info "Downloading ${BINARY_NAME} ${version} for ${os}/${arch}..."

    if ! curl -fsSL "$download_url" -o "${tmp_dir}/${BINARY_NAME}"; then
        error "Failed to download binary from ${download_url}"
    fi

    if curl -fsSL "$checksum_url" -o "${tmp_dir}/checksums.txt" 2>/dev/null; then
        info "Verifying checksum..."
        local expected_checksum
        expected_checksum=$(grep "${BINARY_NAME}-${os}-${arch}" "${tmp_dir}/checksums.txt" | awk '{print $1}')
        if [[ -n "$expected_checksum" ]]; then
            local actual_checksum
            if command -v sha256sum &>/dev/null; then
                actual_checksum=$(sha256sum "${tmp_dir}/${BINARY_NAME}" | awk '{print $1}')
            elif command -v shasum &>/dev/null; then
                actual_checksum=$(shasum -a 256 "${tmp_dir}/${BINARY_NAME}" | awk '{print $1}')
            else
                warn "No checksum tool available, skipping verification"
                actual_checksum="$expected_checksum"
            fi
            if [[ "$actual_checksum" != "$expected_checksum" ]]; then
                error "Checksum verification failed"
            fi
            info "Checksum verified"
        fi
    else
        warn "No checksum file available, skipping verification"
    fi

    mkdir -p "$INSTALL_DIR"
    mv "${tmp_dir}/${BINARY_NAME}" "${INSTALL_DIR}/${BINARY_NAME}"
    chmod +x "${INSTALL_DIR}/${BINARY_NAME}"
}

add_to_path_instructions() {
    local shell_name
    shell_name=$(basename "$SHELL")
    local rc_file

    case "$shell_name" in
        bash)
            if [[ -f "$HOME/.bash_profile" ]]; then
                rc_file="$HOME/.bash_profile"
            else
                rc_file="$HOME/.bashrc"
            fi
            ;;
        zsh)  rc_file="$HOME/.zshrc" ;;
        fish) rc_file="$HOME/.config/fish/config.fish" ;;
        *)    rc_file="your shell's configuration file" ;;
    esac

    if [[ ":$PATH:" != *":$INSTALL_DIR:"* ]]; then
        warn "$INSTALL_DIR is not in your PATH"
        echo ""
        echo "Add the following to ${rc_file}:"
        echo ""
        if [[ "$shell_name" == "fish" ]]; then
            echo "  set -gx PATH \$PATH $INSTALL_DIR"
        else
            echo "  export PATH=\"\$PATH:$INSTALL_DIR\""
        fi
        echo ""
        echo "Then restart your shell or run:"
        echo ""
        if [[ "$shell_name" == "fish" ]]; then
            echo "  source ${rc_file}"
        else
            echo "  source ${rc_file}"
        fi
    fi
}

main() {
    local version="${VERSION:-}"
    local os
    local arch

    os=$(detect_os)
    arch=$(detect_arch)

    if [[ -z "$version" ]]; then
        info "Fetching latest version..."
        version=$(get_latest_version)
    fi

    download_binary "$version" "$os" "$arch"

    info "Installed ${BINARY_NAME} to ${INSTALL_DIR}/${BINARY_NAME}"

    add_to_path_instructions

    echo ""
    info "Installation complete! Run '${BINARY_NAME} --help' to get started."
}

main "$@"
