#!/usr/bin/env bash
# Digitorn Installer — https://github.com/mbathe/digitorn
# Usage: curl -fsSL https://github.com/mbathe/digitorn/releases/latest/download/install.sh | bash
set -euo pipefail

REPO="mbathe/digitorn"
INSTALL_DIR="${DIGITORN_DIR:-$HOME/.local/digitorn}"
BIN_DIR="${HOME}/.local/bin"
VERSION="${DIGITORN_VERSION:-latest}"

# ── Utils ──────────────────────────────────────────────────────────
info()  { printf "\033[36m▶\033[0m %s\n" "$*"; }
ok()    { printf "\033[32m✓\033[0m %s\n" "$*"; }
warn()  { printf "\033[33m⚠\033[0m %s\n" "$*"; }
err()   { printf "\033[31m✗\033[0m %s\n" "$*"; exit 1; }

detect_platform() {
    local os arch

    case "$(uname -s)" in
        Linux)  os="linux" ;;
        Darwin) os="darwin" ;;
        *)      err "Unsupported OS: $(uname -s). Digitorn supports Linux and macOS." ;;
    esac

    case "$(uname -m)" in
        x86_64|amd64) arch="amd64" ;;
        aarch64|arm64) arch="arm64" ;;
        *) err "Unsupported architecture: $(uname -m). Only amd64 and arm64 are supported." ;;
    esac

    echo "${os}_${arch}"
}

fetch_latest_version() {
    local url="https://api.github.com/repos/${REPO}/releases/latest"
    local tag
    tag=$(curl -fsSL "$url" | grep '"tag_name":' | sed 's/.*"tag_name": "\([^"]*\)".*/\1/')
    if [[ -z "$tag" ]]; then
        err "Could not determine the latest version from GitHub"
    fi
    echo "$tag"
}

fetch_version_for_tag() {
    local tag="$1"
    local url="https://api.github.com/repos/${REPO}/releases/tags/${tag}"
    local found
    found=$(curl -fsSL "$url" | grep '"tag_name":' | sed 's/.*"tag_name": "\([^"]*\)".*/\1/' 2>/dev/null || true)
    if [[ -z "$found" ]]; then
        # fallback: just use the tag as-is
        echo "$tag"
    else
        echo "$found"
    fi
}

# ── Main ────────────────────────────────────────────────────────────
main() {
    local platform version_tag version_dir download_url

    platform=$(detect_platform)
    IFS='_' read -r os arch <<< "$platform"

    info "Digitorn Installer"
    info "Platform: ${os}/${arch}"

    if [[ "$VERSION" == "latest" ]]; then
        version_tag=$(fetch_latest_version)
    else
        version_tag=$(fetch_version_for_tag "$VERSION")
    fi
    info "Version:  ${version_tag}"

    # Strip leading 'v' for directory naming
    version_dir="${version_tag#v}"

    # Check if already installed at this version
    if [[ -f "${INSTALL_DIR}/${version_dir}/digitorn" ]]; then
        ok "Digitorn ${version_tag} is already installed at ${INSTALL_DIR}/${version_dir}"
    else
        # Construct asset name
        local asset="digitorn-${version_dir}-${os}-${arch}.tar.gz"
        download_url="https://github.com/${REPO}/releases/download/${version_tag}/${asset}"

        info "Downloading ${asset}..."

        # Verify the asset exists
        local http_code
        http_code=$(curl -fsSL -o /dev/null -w "%{http_code}" --head "$download_url" 2>/dev/null || echo "000")
        if [[ "$http_code" != "200" ]]; then
            # Try alternate naming (old scheme)
            asset="digitorn-${version_tag}-${os}-${arch}.tar.gz"
            download_url="https://github.com/${REPO}/releases/download/${version_tag}/${asset}"
            http_code=$(curl -fsSL -o /dev/null -w "%{http_code}" --head "$download_url" 2>/dev/null || echo "000")
            if [[ "$http_code" != "200" ]]; then
                err "Release asset not found at GitHub. Check https://github.com/${REPO}/releases"
            fi
        fi

        # Download and verify checksum
        local tmpdir
        tmpdir=$(mktemp -d)
        curl -fsSL "$download_url" -o "${tmpdir}/digitorn.tar.gz"

        local checksum_url="https://github.com/${REPO}/releases/download/${version_tag}/checksums-${version_dir}.txt"
        if curl -fsSL "$checksum_url" -o "${tmpdir}/checksums.txt" 2>/dev/null; then
            info "Verifying checksum..."
            local expected actual
            expected=$(grep "${asset}" "${tmpdir}/checksums.txt" | awk '{print $1}')
            actual=$(sha256sum "${tmpdir}/digitorn.tar.gz" | awk '{print $1}')
            if [[ -n "$expected" && "$expected" != "$actual" ]]; then
                rm -rf "$tmpdir"
                err "Checksum mismatch! Expected: $expected, Got: $actual"
            fi
            ok "Checksum verified"
        fi

        mkdir -p "${INSTALL_DIR}/${version_dir}"
        tar xzf "${tmpdir}/digitorn.tar.gz" -C "${INSTALL_DIR}/${version_dir}" --strip-components=1
        rm -rf "$tmpdir"

        ok "Digitorn ${version_tag} installed at ${INSTALL_DIR}/${version_dir}"
    fi

    # ── Update 'current' symlink ──
    local current_link="${INSTALL_DIR}/current"
    if [[ -L "$current_link" ]] || [[ ! -e "$current_link" ]]; then
        ln -snf "${INSTALL_DIR}/${version_dir}" "$current_link"
        ok "Symlink updated: ${current_link} → ${version_dir}"
    fi

    # ── Symlink binaries to ~/.local/bin ──
    mkdir -p "$BIN_DIR"
    local bin_map=(
        "digitorn:digitorn"
        "digitorn-tui:digitorn-tui"
        "digitornd:digitornd"
    )
    for entry in "${bin_map[@]}"; do
        local src="${current_link}/${entry%%:*}"
        local name="${entry##*:}"
        if [[ -f "$src" ]]; then
            ln -snf "$src" "${BIN_DIR}/${name}"
        fi
    done
    ok "Symlinks created in ${BIN_DIR}/"

    # ── Configuration ──
    local config_dir="${HOME}/.digitorn"
    local config_file="${config_dir}/config.yaml"
    if [[ ! -f "$config_file" ]]; then
        mkdir -p "$config_dir"
        if [[ -f "${current_link}/.digitorn.yaml.example" ]]; then
            cp "${current_link}/.digitorn.yaml.example" "$config_file"
            ok "Config created at ${config_file} — edit it to match your environment"
        fi
    else
        ok "Config already exists at ${config_file}"
    fi

    # ── Install as a service ──
    local digitornd_bin="${BIN_DIR}/digitornd"
    if [[ -f "$digitornd_bin" ]]; then
        info "Installing digitorn as a system service..."
        "$digitornd_bin" -config "$config_file" install 2>/dev/null \
            && ok "Service registered" \
            || warn "Service registration skipped (run '${digitornd_bin} -config ${config_file} install' manually)"
    fi

    # ── PATH check ──
    local need_path=false
    if [[ ":$PATH:" != *":${BIN_DIR}:"* ]]; then
        need_path=true
    fi

    # ── Done ──
    echo ""
    ok "Digitorn ${version_tag} is ready!"
    echo ""
    echo "  Commands:"
    echo "    digitorn chat          Launch the TUI"
    echo "    digitorn list          List installed apps"
    echo "    digitorn upgrade       Check for updates"
    echo "    digitornd status       Daemon status"
    echo ""

    if $need_path; then
        warn "${BIN_DIR} is not in your PATH."
        echo ""
        echo "  Add it to your shell profile:"
        echo ""
        echo "    export PATH=\"\$HOME/.local/bin:\$PATH\""
        echo ""
    fi

    # Check if daemon should be started
    echo "  Next steps:"
    echo "    1. Edit ${config_file} to set your database, providers, etc."
    echo "    2. Start the daemon: digitornd -config ${config_file} run"
    echo "    3. Open the TUI:     digitorn chat"
    echo ""
    info "Run 'digitorn upgrade --check' to verify the installation."
}

main "$@"
