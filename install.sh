#!/bin/sh
# Installs the latest codep release.
#   curl -fsSL https://raw.githubusercontent.com/DeveloperMos/codep/main/install.sh | sh
# Set CODEP_BIN_DIR to change the install folder (default ~/.local/bin).
set -eu

REPO="${CODEP_REPO:-DeveloperMos/codep}"
BIN_DIR="${CODEP_BIN_DIR:-$HOME/.local/bin}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
  darwin|linux) ;;
  *) echo "codep: unsupported OS: $os" >&2; exit 1 ;;
esac
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) echo "codep: unsupported CPU: $arch" >&2; exit 1 ;;
esac

name="codep_${os}_${arch}"
base="${CODEP_BASE_URL:-https://github.com/$REPO/releases/latest/download}"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $name from $REPO"
curl -fsSL -o "$tmp/$name.tar.gz" "$base/$name.tar.gz"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
if command -v sha256sum >/dev/null 2>&1; then sha="sha256sum"; else sha="shasum -a 256"; fi
(cd "$tmp" && grep " $name.tar.gz\$" checksums.txt | $sha -c - >/dev/null) || {
  echo "codep: checksum mismatch, aborting" >&2
  exit 1
}
tar -xzf "$tmp/$name.tar.gz" -C "$tmp"
mkdir -p "$BIN_DIR"
install -m 755 "$tmp/$name/codep" "$BIN_DIR/codep"
echo "Installed $("$BIN_DIR/codep" version) to $BIN_DIR/codep"

case ":$PATH:" in
  *":$BIN_DIR:"*) ;;
  *) echo "Add $BIN_DIR to your PATH, for example: echo 'export PATH=\"$BIN_DIR:\$PATH\"' >> ~/.zshrc" ;;
esac

# An alias or function named codep in the shell runs instead of the program.
conflicts=""
for rc in .zshrc .zprofile .zshenv .bashrc .bash_profile .profile .config/fish/config.fish; do
  [ -f "$HOME/$rc" ] || continue
  lines=$(grep -nE '^[[:space:]]*(alias[[:space:]]+codep[=[:space:]]|codep[[:space:]]*\(\)|function[[:space:]]+codep([[:space:]]|$|\())' "$HOME/$rc" | cut -d: -f1 | tr '\n' ' ')
  for n in $lines; do conflicts="$conflicts  $HOME/$rc:$n
"; done
done
if [ -n "$conflicts" ]; then
  echo
  echo "Warning: your shell defines its own codep, which runs instead of this program:"
  printf '%s' "$conflicts"
  echo "Remove it and open a new terminal. Until then call this program as: command codep ..."
fi

if [ "$os" = darwin ]; then
  echo
  echo "Next:"
  echo "  codep install                      # start the bridge in the background"
  echo "  open ~/.config/codep/proxies.txt   # add your proxies"
  echo "  codep .                            # open VS Code through the proxy"
fi
