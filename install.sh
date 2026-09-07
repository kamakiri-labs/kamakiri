#!/bin/sh
# Installs the latest released kamakiri binary for this platform. It is what
# `curl -fsSL https://get.kamakiri-labs.jp/install.sh | sh` runs.
#
# Two environment variables change what it does:
#
#   KAMAKIRI_INSTALL_DIR       where the binary is installed (default
#                              $HOME/.local/bin). Nothing here needs root, and
#                              the directory has to stay user-owned for
#                              `kamakiri upgrade` to replace the binary later.
#   KAMAKIRI_INSTALL_REPO_URL  the repository the release is resolved from
#                              (default https://github.com/kamakiri-labs/kamakiri).
#                              It is here so this script can be driven against a
#                              local server instead of GitHub.
#
# The whole body is one function, invoked on the last line, and nothing above it
# has any effect. A shell reads a pipe incrementally and runs what has arrived,
# so a transfer that stops partway must not execute the part that got through:
# with this shape the prefix either defines a function nobody calls or fails to
# parse, and either way nothing happens.

# Keeps the directory resolution below from being diverted by an exported CDPATH.
CDPATH=
set -e

main() {
  repo="${KAMAKIRI_INSTALL_REPO_URL:-https://github.com/kamakiri-labs/kamakiri}"
  releases="${repo}/releases"

  # 1. What to install for.
  os=""
  arch=""
  uname_s="$(uname -s)"
  uname_m="$(uname -m)"
  case "${uname_s}" in
    Darwin) os="darwin" ;;
    Linux) os="linux" ;;
    MINGW* | MSYS* | CYGWIN*)
      echo "This installer is for macOS and Linux." >&2
      echo "On Windows, run this in PowerShell instead:" >&2
      echo "  irm https://get.kamakiri-labs.jp/install.ps1 | iex" >&2
      echo "The releases are also listed at ${releases}" >&2
      exit 1
      ;;
  esac
  case "${uname_m}" in
    x86_64 | amd64) arch="amd64" ;;
    aarch64 | arm64) arch="arm64" ;;
  esac
  if [ -z "${os}" ] || [ -z "${arch}" ]; then
    echo "kamakiri publishes no release for ${uname_s} ${uname_m}." >&2
    echo "The releases are for macOS and Linux, on x86_64 and arm64." >&2
    echo "Check ${releases}" >&2
    exit 1
  fi
  asset="kamakiri-${os}-${arch}"

  # 2. Which release is the latest, read off the redirect rather than by
  # following it. The tag is the one string the server chooses that this script
  # then puts into a URL it downloads from, so the answer is held to the request
  # it answers: same scheme, same host, this repository's releases path, and one
  # segment after it that is a version and nothing else.
  # Asked before the probe, whose failure branch would otherwise swallow a
  # missing curl into a release that could not be determined and name the
  # release host for a tool this machine does not have.
  if ! command -v curl >/dev/null 2>&1; then
    echo "curl is not on PATH, and this installer downloads the release with it." >&2
    echo "Install curl and run this again." >&2
    exit 1
  fi
  echo "Checking for the latest release."
  # The probe is a few hundred bytes of headers, so it is bounded end to end:
  # one still open after ten seconds is not going to answer.
  if ! redirect="$(curl -fs --connect-timeout 10 --max-time 10 -o /dev/null -w '%{redirect_url}' "${releases}/latest")"; then
    # Under -f a repository with no release answers 404 and curl exits 22 with
    # no -w output at all, so the branch has to key on the status. -s without
    # -S, since a release nobody has cut yet is this script's answer to give
    # rather than curl's.
    redirect=""
  fi
  tag=""
  # The quoted prefix matches literally, so scheme, host and path are all held
  # in this one glob.
  case "${redirect}" in
    "${releases}/tag/"*) tag="${redirect#"${releases}/tag/"}" ;;
  esac
  # Bounded ahead of the tests below, which is where a bound is worth having: the
  # value is whatever answered the request, a response header can carry far more
  # than a version, and a tag that gets through is printed to the terminal and
  # goes into the URLs the download comes from. A published version runs to well
  # under ten characters, so this is generous by an order of magnitude.
  if [ "${#tag}" -gt 64 ]; then
    tag=""
  fi
  case "${tag}" in
    "" | */*) tag="" ;;
  esac
  # The character class is not redundant beside the anchored match below: grep
  # reads its input a line at a time, so a value carrying a newline would match
  # on one of them and reach the download URLs whole.
  case "${tag}" in
    *[!0-9v.]*) tag="" ;;
  esac
  if [ -n "${tag}" ] && ! printf '%s\n' "${tag}" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$'; then
    tag=""
  fi
  if [ -z "${tag}" ]; then
    echo "Could not determine the latest release. Check ${releases}" >&2
    exit 1
  fi

  # 3. Where it goes, resolved physically: the marker written at the end records
  # this path, and it is compared against the location of the running binary,
  # which the CLI resolves through its symlinks.
  # The spelling the user gave is kept beside the resolved one for the PATH
  # comparison at the end, which is against the name their shell was told about.
  dir_given="${KAMAKIRI_INSTALL_DIR:-${HOME}/.local/bin}"
  # Paths are printed with printf rather than echo from here on, because the
  # shells this runs under expand backslash escapes in echo's arguments and a
  # path is allowed to hold one. The -- keeps a directory named with a leading
  # dash reading as this script's refusal rather than as a usage error from the
  # tool it was handed to, and mkdir's own message is dropped so that the
  # refusal below is the one the user reads.
  if ! mkdir -p -- "${dir_given}" 2>/dev/null; then
    printf '%s\n' "Could not create the install directory ${dir_given}." >&2
    printf '%s\n' "Set KAMAKIRI_INSTALL_DIR to a directory you can write to and run this again." >&2
    exit 1
  fi
  if ! dir="$(cd -- "${dir_given}" && pwd -P)"; then
    echo "Could not enter the install directory." >&2
    exit 1
  fi
  # Asked before anything claims to be fetching. A directory that is there but
  # cannot be written to is a local matter, and the first thing to fail without
  # this would be the download, which would name the release host for it.
  if [ ! -w "${dir}" ]; then
    printf '%s\n' "The install directory ${dir} cannot be written to." >&2
    printf '%s\n' "Set KAMAKIRI_INSTALL_DIR to a directory you can write to and run this again." >&2
    exit 1
  fi

  # 4. Both files are staged in the install directory, which leaves the final
  # step a rename inside one filesystem rather than a copy that an interruption
  # could truncate over a working binary. Both staged names are dot-prefixed: an
  # un-prefixed checksums.txt would land under its own name in a directory on
  # the user's PATH and outlive the run.
  staged="${dir}/.tmp-${asset}"
  staged_sums="${dir}/.tmp-checksums.txt"
  # The signal list matters: dash runs no EXIT trap on Ctrl-C. The two files are
  # named rather than swept with a .tmp-* glob, because `kamakiri upgrade`
  # stages under the same prefix in the same directory and nothing here owns
  # what it left behind. The handler's own stderr is dropped: it runs on every
  # refusal below, and a file it cannot remove must not put a tool diagnostic
  # under the line the script refused with.
  trap 'rm -f "${staged}" "${staged_sums}" 2>/dev/null; exit 1' EXIT INT TERM HUP
  # Both names are cleared before either is written to. Either one is predictable
  # and this directory need not be one the user has to themselves, so a symlink
  # planted at one would otherwise take the download, and the mode set on it
  # below, wherever it points.
  # What rm cannot remove is a directory sitting at one of the two names, and its
  # own message is dropped so that the refusal here is the one the user reads.
  if ! rm -f "${staged}" "${staged_sums}" 2>/dev/null; then
    printf '%s\n' "The files this install stages in ${dir} could not be cleared." >&2
    printf '%s\n' "Move aside whatever is at ${staged} or ${staged_sums}, or set KAMAKIRI_INSTALL_DIR to another directory, and run this again." >&2
    exit 1
  fi

  printf '%s\n' "Downloading ${asset} into ${dir}."
  # -S, unlike the probe above, because a transport failure here is curl's to
  # explain. The size caps are on what lands in the install directory: a body
  # that keeps coming is a failure rather than a short file, and each cap is a
  # wide multiple of what a release weighs, so it only ever catches something
  # that has already gone wrong. The asset transfer is left with no deadline of
  # its own, so a slow but live download over a thin link finishes rather than
  # being cut off by a clock; what a connection that never opens costs is the
  # connect timeout.
  # A cap reaches only as far as the curl on the machine allows: before 8.4 it
  # holds against a length the response declared and does nothing about a body
  # that arrives without one, so on an older curl an asset body that keeps coming
  # is held by nothing until it fills the disk. What the install rests on in
  # every case is the checksum below.
  if ! curl -fsSL --connect-timeout 10 --max-filesize 209715200 -o "${staged}" "${releases}/download/${tag}/${asset}"; then
    echo "Could not download ${asset} from ${releases}/download/${tag}/" >&2
    exit 1
  fi
  if ! curl -fsSL --connect-timeout 10 --max-time 10 --max-filesize 1048576 -o "${staged_sums}" "${releases}/download/${tag}/checksums.txt"; then
    echo "Could not download checksums.txt from ${releases}/download/${tag}/" >&2
    exit 1
  fi

  # 5. The published sum is the first field of the line whose second field is
  # this asset in full, so one asset's line can never be read as another's. The
  # field is held to 64 hex characters before it is used or echoed, because the
  # mismatch line below prints it back to the user and this file came off the
  # network. A line failing that hold does not end the scan, so a usable line
  # further down the file still wins. The hold is spelled as a length test plus a
  # character class rather than as a `{64}` interval, which mawk and busybox awk
  # do not read.
  published="$(awk -v want="${asset}" \
    '$2 == want && length($1) == 64 && $1 !~ /[^0-9a-fA-F]/ { print $1; exit }' \
    "${staged_sums}")"
  if [ -z "${published}" ]; then
    # The pass above prints nothing but a sum that passed the hold, so it has no
    # way to also say that a line named the asset and carried no usable one. A
    # second pass asks that on its own.
    if awk -v want="${asset}" '$2 == want { named = 1; exit } END { exit !named }' \
      "${staged_sums}"; then
      echo "The line for ${asset} in checksums.txt does not carry a checksum (64 hexadecimal characters)." >&2
      echo "Nothing was installed." >&2
    else
      echo "No line in checksums.txt names ${asset}. Nothing was installed." >&2
    fi
    exit 1
  fi
  # macOS ships no sha256sum. Both are fed on stdin so neither has a file name
  # to quote or escape in the line it prints.
  if command -v sha256sum >/dev/null 2>&1; then
    downloaded="$(sha256sum < "${staged}" | awk '{ print $1 }')"
  elif command -v shasum >/dev/null 2>&1; then
    downloaded="$(shasum -a 256 < "${staged}" | awk '{ print $1 }')"
  else
    echo "Neither sha256sum nor shasum is on PATH, so ${asset} cannot be verified." >&2
    echo "Nothing was installed." >&2
    exit 1
  fi
  # Two spellings of a hex digest are one digest.
  published="$(printf '%s' "${published}" | tr 'A-F' 'a-f')"
  downloaded="$(printf '%s' "${downloaded}" | tr 'A-F' 'a-f')"
  if [ "${published}" != "${downloaded}" ]; then
    echo "${asset} does not match its checksum. checksums.txt lists ${published}, the download is ${downloaded}." >&2
    echo "Nothing was installed." >&2
    exit 1
  fi
  echo "Checksum verified against checksums.txt."

  # 6. Overwriting whatever is at the install path is the point: a re-run is how
  # this channel updates an existing install. A directory there is the one thing
  # that would not be overwritten but moved into, and mv would report success
  # for it, so it is refused here instead; the mv flag that would refuse it is
  # not portable.
  if [ -d "${dir}/kamakiri" ]; then
    printf '%s\n' "There is a directory at ${dir}/kamakiri, so the binary cannot go there." >&2
    printf '%s\n' "Move it aside, or set KAMAKIRI_INSTALL_DIR to another directory, and run this again." >&2
    exit 1
  fi
  # Explicit rather than umask-dependent, and the mode `kamakiri upgrade` gives
  # a binary it installs.
  if ! chmod 755 "${staged}"; then
    printf '%s\n' "Could not make the downloaded ${asset} executable in ${dir}." >&2
    exit 1
  fi
  if ! mv "${staged}" "${dir}/kamakiri"; then
    printf '%s\n' "Could not install ${asset} as ${dir}/kamakiri." >&2
    exit 1
  fi
  # Past the rename the install has happened, so nothing below may turn it into
  # a non-zero exit: a user who reads one takes it for a run that did nothing.
  # That covers this tidying up as much as the marker record further down.
  rm -f "${staged_sums}" 2>/dev/null || :
  trap - EXIT INT TERM HUP

  # 7. The marker records the channel that installed this binary, so `kamakiri
  # upgrade` can tell a scripted install from one a package manager owns. It is
  # written through a temp file in the same directory and renamed into place. A
  # marker that cannot be written is a warning and not a failure: the install
  # itself succeeded, and a CLI that finds no marker treats the binary as one it
  # may replace itself, which is what this one records anyway.
  config_dir="${XDG_CONFIG_HOME:-${HOME}/.config}/kamakiri"
  marker_tmp="${config_dir}/.tmp-install.json"
  # 700 on creation, which is what the credentials file later written beside it
  # needs. A directory that is already there keeps the mode it has.
  # -m applying to the deepest directory alone is what is wanted here: the
  # config directory itself is the one that has to be private, and a ~/.config
  # this run happens to create is an ordinary directory.
  # Every step below is inside a condition or has its failure swallowed, and
  # each one's own stderr is dropped, so the one warning line this block prints
  # is the whole of what a marker that cannot be written costs the user.
  # shellcheck disable=SC2174
  if [ -d "${config_dir}" ] || mkdir -p -m 700 -- "${config_dir}" 2>/dev/null; then
    # The path goes into a JSON string, so the backslash and the double quote
    # are escaped. A control character is left alone: a HOME holding one yields
    # a marker no parser accepts, and that costs nothing here, an unparseable
    # marker being dropped for the same answer a missing one gives.
    # The -- keeps a config directory named with a leading dash from reading as
    # options to mv and rm.
    if ! (escaped="$(printf '%s' "${dir}/kamakiri" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')" \
      && printf '{"version":1,"method":"script","path":"%s"}\n' "${escaped}" > "${marker_tmp}" \
      && mv -- "${marker_tmp}" "${config_dir}/install.json") 2>/dev/null; then
      rm -f -- "${marker_tmp}" 2>/dev/null || :
      printf '%s\n' "Installed, but the record of this install could not be written to ${config_dir}/install.json." >&2
    fi
  else
    printf '%s\n' "Installed, but the record of this install could not be written to ${config_dir}/install.json." >&2
  fi

  # 8. What happened, and the one thing left to do when the directory the binary
  # went into is not somewhere the shell looks. Nothing here edits a dotfile.
  printf '%s\n' "Installed ${tag} at ${dir}/kamakiri."
  # Both spellings count, because PATH carries whichever name the user wrote and
  # a directory reached through a symlink resolves to another one.
  case ":${PATH}:" in
    *":${dir}:"* | *":${dir_given}:"*) ;;
    *)
      case "${SHELL}" in
        */zsh) profile="${HOME}/.zshrc" ;;
        *) profile="${HOME}/.bashrc" ;;
      esac
      echo
      printf '%s\n' "${dir} is not on your PATH. Add this line to ${profile}:"
      echo
      # The PATH the line names is the user's own at the time they run it, so the
      # dollar reaches them as a dollar.
      printf "  export PATH=\"%s:\$PATH\"\n" "${dir}"
      echo
      echo "Then open a new terminal, or run that line in this one, and 'kamakiri version' will work."
      ;;
  esac
}

main "$@"
