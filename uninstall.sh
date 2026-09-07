#!/bin/sh
# Removes what `curl -fsSL https://get.kamakiri-labs.jp/install.sh | sh` put on
# this machine, and nothing else. It is what
# `curl -fsSL https://get.kamakiri-labs.jp/uninstall.sh | sh` runs.
#
# Two environment variables change where it looks, and they are the two the
# installer and the CLI read:
#
#   KAMAKIRI_INSTALL_DIR  the directory the binary was installed in (default
#                         $HOME/.local/bin). A binary installed under another
#                         value is found by running this under the same one.
#   XDG_CONFIG_HOME       where the CLI keeps its config directory (default
#                         $HOME/.config).
#
# Two things go: the binary, and the CLI's config directory, which holds the
# record of the install, the saved API key, the update-nudge state and the
# language setting. Nothing else in either place is touched: not another tool in
# the install directory, and not a file `kamakiri upgrade` staged there. The
# saved key is a copy, so removing it revokes nothing; the key stays valid on the
# account until it is revoked there. Nothing here edits a dotfile, as the
# installer edits none.
#
# The whole body is one function, invoked on the last line, and nothing above it
# has any effect, for the reason install.sh gives: a shell reads a pipe
# incrementally and runs what has arrived, so a transfer that stops partway must
# not execute the part that got through.

# Keeps the directory resolution below from being diverted by an exported CDPATH.
CDPATH=
set -e

main() {
  # A Windows shell environment is where the other installer put the binary, so
  # it is pointed at the other uninstaller rather than told there is nothing to
  # remove, which would be true here and useless.
  case "$(uname -s)" in
    MINGW* | MSYS* | CYGWIN*)
      echo "This uninstaller is for macOS and Linux." >&2
      echo "On Windows, run this in PowerShell instead:" >&2
      echo "  irm https://get.kamakiri-labs.jp/uninstall.ps1 | iex" >&2
      exit 1
      ;;
  esac

  removed=0
  failed=0
  binary_removed=0

  # 1. The binary. The directory is resolved physically when it is there, so the
  # line naming what was removed spells the path the way the install line did.
  # The spelling the user gave is kept beside it for the PATH comparison at the
  # end, which is against the name their shell was told about. A directory that
  # is not there holds no binary, and the given spelling is what the closing
  # line then names.
  dir_given="${KAMAKIRI_INSTALL_DIR:-${HOME}/.local/bin}"
  dir="${dir_given}"
  if [ -d "${dir_given}" ]; then
    dir="$(cd -- "${dir_given}" 2>/dev/null && pwd -P)" || dir="${dir_given}"
  fi
  binary="${dir}/kamakiri"
  # Paths are printed with printf rather than echo, because the shells this
  # runs under expand backslash escapes in echo's arguments and a path is
  # allowed to hold one. The -- keeps a name with a leading dash from reading
  # as options to rm.
  # A directory at the binary's name is not something this channel installed,
  # so it is refused rather than removed. The dangling-symlink test is there
  # because -e answers no for a link whose target is gone, and rm still has a
  # link to remove.
  if [ -d "${binary}" ]; then
    printf '%s\n' "There is a directory at ${binary}, so it was not removed." >&2
    printf '%s\n' "Move it aside, or set KAMAKIRI_INSTALL_DIR to the directory kamakiri was installed in, and run this again." >&2
    failed=1
  elif [ -e "${binary}" ] || [ -L "${binary}" ]; then
    # rm's own message is dropped so that the refusal below is the one the user
    # reads.
    if rm -f -- "${binary}" 2>/dev/null; then
      printf '%s\n' "Removed ${binary}."
      removed=1
      binary_removed=1
    else
      printf '%s\n' "Could not remove ${binary}." >&2
      printf '%s\n' "It is still there. Remove it by hand, or run this again once ${dir} can be written to." >&2
      failed=1
    fi
  fi

  # 2. The config directory, whole: everything in it is the CLI's. The removal
  # goes through rm, which never follows a symlink, so a link sitting at the
  # directory's name goes and what it points at stays. A refusal here does not
  # stop the run from removing the binary above it, and the binary's refusal
  # does not stop this: what can be removed is, and the exit says the rest.
  config_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/kamakiri"
  if [ -e "${config_dir}" ] || [ -L "${config_dir}" ]; then
    had_key=0
    if [ -f "${config_dir}/credentials.json" ]; then
      had_key=1
    fi
    if rm -rf -- "${config_dir}" 2>/dev/null; then
      printf '%s\n' "Removed the config directory ${config_dir}."
      # Said only when there was a key to say it about. A user who reads that
      # their key was removed may take the key itself for gone; it is not.
      if [ "${had_key}" -eq 1 ]; then
        echo "It held your saved API key. Only this copy is gone: the key itself stays valid on your account, and nothing here revokes it."
      fi
      removed=1
    else
      printf '%s\n' "Could not remove everything under ${config_dir}." >&2
      printf '%s\n' "Remove what is left of it by hand." >&2
      failed=1
    fi
  fi

  # 3. What happened. A refusal above has already said what is left and what
  # to do, so the non-zero exit is the whole of what is added to it. Running
  # this on a machine with nothing to remove is not an error: it is the state
  # this script exists to reach.
  if [ "${failed}" -eq 1 ]; then
    exit 1
  fi
  if [ "${removed}" -eq 0 ]; then
    printf '%s\n' "Nothing to remove: there is no kamakiri at ${binary} and no config directory at ${config_dir}."
    return
  fi
  echo "kamakiri is uninstalled."
  # The installer printed a line to add to the shell profile when the install
  # directory was not on PATH, and edited nothing. The reverse is the same
  # shape: one line, printed only when the directory is on PATH, naming the
  # file the installer named. It is an offer rather than an instruction, since
  # the directory is a common one and other tools may live in it. Both
  # spellings count, because PATH carries whichever name the user wrote and a
  # directory reached through a symlink resolves to another one.
  if [ "${binary_removed}" -eq 1 ]; then
    case ":${PATH}:" in
      *":${dir}:"* | *":${dir_given}:"*)
        case "${SHELL}" in
          */zsh) profile="${HOME}/.zshrc" ;;
          *) profile="${HOME}/.bashrc" ;;
        esac
        printf '%s\n' "If you added ${dir} to your PATH for kamakiri alone, that line in ${profile} can come out now."
        ;;
    esac
  fi
}

main "$@"
