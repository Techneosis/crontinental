#!/usr/bin/env bash
# Install Crontinental for the current user, by symlink: edits to this checkout
# are live, and there is no copy to fall out of date.
#
#   ./install.sh              link everything and start the tick
#   ./install.sh --uninstall  stop the tick and remove the links (the ledger stays)
#
# Paths are fixed: the tick's unit runs %h/.local/bin/crontinental.
set -euo pipefail

src=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
bin=$HOME/.local/bin
units=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
comp=${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions

links=(
  "$src/bin/crontinental:$bin/crontinental"
  "$src/systemd/crontinental.service:$units/crontinental.service"
  "$src/systemd/crontinental.timer:$units/crontinental.timer"
  "$src/completions/crontinental.bash:$comp/crontinental"
)

if [[ ${1:-} == --uninstall ]]; then
  systemctl --user disable --now crontinental.timer 2>/dev/null || true
  for pair in "${links[@]}"; do
    dst=${pair#*:}
    [[ -L $dst ]] && rm "$dst" && echo "removed $dst"
  done
  systemctl --user daemon-reload
  echo "uninstalled; the ledger is left at ${XDG_DATA_HOME:-$HOME/.local/share}/crontinental/"
  exit 0
fi

python3 -c 'import sys; sys.exit(sys.version_info < (3, 10))' \
  || { echo "crontinental needs Python 3.10 or newer" >&2; exit 1; }
command -v systemd-analyze >/dev/null && systemctl --user show-environment >/dev/null 2>&1 \
  || { echo "crontinental needs a running systemd user manager" >&2; exit 1; }

for pair in "${links[@]}"; do
  from=${pair%%:*} dst=${pair#*:}
  mkdir -p "$(dirname "$dst")"
  if [[ -e $dst && ! -L $dst ]]; then
    echo "refusing to replace $dst: it is a real file, not a link" >&2
    exit 1
  fi
  ln -sfn "$from" "$dst"
  echo "linked $dst"
done

systemctl --user daemon-reload
systemctl --user enable --now crontinental.timer
echo "the Concierge is on duty: $(systemctl --user is-active crontinental.timer)"
[[ :$PATH: == *":$bin:"* ]] || echo "note: $bin is not on your PATH"
