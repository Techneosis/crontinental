#!/usr/bin/env bash
# Install Crontinental for the current user, by symlink: edits to this checkout
# are live, and there is no copy to fall out of date.
#
#   ./install.sh                link everything, start the tick, set up Claude Code
#   ./install.sh --claude       ...and add the CLAUDE.md section without asking
#   ./install.sh --no-claude    skip Claude Code entirely
#   ./install.sh --claude-only  only the Claude Code part (no binary, no tick)
#   ./install.sh --uninstall    remove all of it (the ledger stays)
#   ./install.sh --uninstall --claude-only   remove only the Claude Code part
#
# Claude Code: when ~/.claude (or $CLAUDE_CONFIG_DIR) exists, the crontinental
# skill is linked into its skills/, and a short routing section is offered for
# its CLAUDE.md. That section sits between marker comments, so re-running
# updates it in place and --uninstall takes it out again. CLAUDE.md is written
# in place, never replaced, so a CLAUDE.md that is itself a symlink (dotfiles)
# stays one.
#
# Paths are fixed: the tick's unit runs %h/.local/bin/crontinental.
set -euo pipefail

src=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
bin=$HOME/.local/bin
units=${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user
comp=${XDG_DATA_HOME:-$HOME/.local/share}/bash-completion/completions
claude=${CLAUDE_CONFIG_DIR:-$HOME/.claude}

BEGIN='<!-- crontinental:begin (managed by crontinental/install.sh; edit agents/claude-md-snippet.md upstream, not here) -->'
END='<!-- crontinental:end -->'

uninstall=0 claude_only=0 claude_md=ask
for arg in "$@"; do
  case $arg in
    --uninstall)   uninstall=1 ;;
    --claude-only) claude_only=1 ;;
    --claude)      claude_md=yes ;;
    --no-claude)   claude_md=never ;;
    -h|--help)     sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "unknown option: $arg (try --help)" >&2; exit 2 ;;
  esac
done

links=(
  "$src/build/crontinental:$bin/crontinental"
  "$src/systemd/crontinental.service:$units/crontinental.service"
  "$src/systemd/crontinental.timer:$units/crontinental.timer"
  "$src/completions/crontinental.bash:$comp/crontinental"
)

link() {  # link <from> <to>, refusing to clobber anything that isn't a link
  local from=$1 dst=$2
  mkdir -p "$(dirname "$dst")"
  if [[ -e $dst && ! -L $dst ]]; then
    echo "refusing to replace $dst: it is a real file, not a link" >&2
    exit 1
  fi
  ln -sfn "$from" "$dst"
  echo "linked $dst"
}

# Rewrite a file through any symlink: content in place, never a rename.
rewrite() {  # rewrite <file> <awk program> [awk args...]
  local f=$1 prog=$2 tmp; shift 2
  tmp=$(mktemp)
  awk "$@" "$prog" "$f" >"$tmp" && cat "$tmp" >"$f"
  rm -f "$tmp"
}

has_block() { [[ -f $1 ]] && grep -qF '<!-- crontinental:begin' "$1"; }

claude_install() {
  [[ $claude_md == never ]] && return 0
  if [[ ! -d $claude ]]; then
    echo "Claude Code not found at $claude; skipping its setup (CLAUDE_CONFIG_DIR points elsewhere)"
    return 0
  fi
  link "$src/agents/skills/crontinental" "$claude/skills/crontinental"

  local md=$claude/CLAUDE.md
  if has_block "$md"; then
    rewrite "$md" '
      index($0, "<!-- crontinental:begin") == 1 { print b; while ((getline l < snip) > 0) print l; skip = 1; next }
      index($0, "<!-- crontinental:end") == 1   { skip = 0; print e; next }
      !skip' -v b="$BEGIN" -v e="$END" -v snip="$src/agents/claude-md-snippet.md"
    echo "updated the crontinental section of $md"
    return 0
  fi

  if [[ $claude_md == ask ]]; then
    if [[ -t 0 ]]; then
      echo
      echo "Add a short section to $md so Claude reaches for crontinental"
      echo "(rather than cron, a hand-written timer, or a session-scoped loop)?"
      read -r -p "[Y/n] " reply
      [[ -z $reply || $reply == [Yy]* ]] || { echo "skipped; ./install.sh --claude adds it later"; return 0; }
    else
      echo "not adding a CLAUDE.md section without asking; ./install.sh --claude adds it"
      return 0
    fi
  fi
  { [[ -s $md ]] && echo; echo "$BEGIN"; cat "$src/agents/claude-md-snippet.md"; echo "$END"; } >>"$md"
  echo "added a crontinental section to $md"
}

claude_uninstall() {
  [[ -L $claude/skills/crontinental ]] && rm "$claude/skills/crontinental" && echo "removed $claude/skills/crontinental"
  if has_block "$claude/CLAUDE.md"; then
    # Also drops the one blank line install put before the section.
    rewrite "$claude/CLAUDE.md" '
      held { if (index($0, "<!-- crontinental:begin") != 1) print ""; held = 0 }
      index($0, "<!-- crontinental:begin") == 1 { skip = 1; next }
      index($0, "<!-- crontinental:end") == 1   { skip = 0; next }
      skip { next }
      $0 == "" { held = 1; next }
      { print }
      END { if (held) print "" }'
    echo "removed the crontinental section from $claude/CLAUDE.md"
  fi
}

case $uninstall$claude_only in
  01) claude_install; exit 0 ;;
  11) claude_uninstall; exit 0 ;;
  10)
    systemctl --user disable --now crontinental.timer 2>/dev/null || true
    for pair in "${links[@]}"; do
      dst=${pair#*:}
      [[ -L $dst ]] && rm "$dst" && echo "removed $dst"
    done
    systemctl --user daemon-reload
    claude_uninstall
    echo "uninstalled; the ledger is left at ${XDG_DATA_HOME:-$HOME/.local/share}/crontinental/"
    exit 0 ;;
esac

command -v go >/dev/null \
  || { echo "building crontinental from a checkout needs Go (https://go.dev/dl/)" >&2; exit 1; }
command -v systemd-analyze >/dev/null && systemctl --user show-environment >/dev/null 2>&1 \
  || { echo "crontinental needs a running systemd user manager" >&2; exit 1; }

# Build to a temp name and rename into place: a tick that starts mid-build
# then runs the old binary or the new one, never half of one.
(cd "$src" && CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o build/crontinental.new ./cmd/crontinental)
mv -f "$src/build/crontinental.new" "$src/build/crontinental"
echo "built $src/build/crontinental"

for pair in "${links[@]}"; do
  link "${pair%%:*}" "${pair#*:}"
done

systemctl --user daemon-reload
systemctl --user enable --now crontinental.timer
echo "the Concierge is on duty: $(systemctl --user is-active crontinental.timer)"
[[ :$PATH: == *":$bin:"* ]] || echo "note: $bin is not on your PATH"

claude_install
