#!/usr/bin/env bash
#
# Push the Homebrew cask a release generated into the tap repository.
#
# Usage: homebrew/publish.sh --version vX.Y.Z
#
# Run from the directory holding dist/, the same convention the npm packaging
# beside it follows: the cask's path is read relative to the working directory.
#
# The release build authors the cask and pushes nothing, so this moves that one
# file, through the GitHub contents API. It is idempotent: a tap already holding
# these exact bytes is reported as current and nothing is written, so a re-run
# of a release that stopped halfway converges rather than landing a second
# commit saying the same thing.
#
# Everything that can be refused is refused before the one write: the version's
# shape, a cask that is missing or names another release, an absent token, and
# an answer from the tap that cannot be read. Nothing is retried and nothing is
# worked around. A run that failed is re-run, and the idempotence is what makes
# that safe.

set -euo pipefail

# Byte semantics for everything matched below, so what this refuses does not
# depend on the machine's locale. Under a UTF-8 locale [0-9] takes in non-ASCII
# digits, and a version written with those would pass the shape check and go out
# as a cask naming a version no comparison can order.
export LC_ALL=C

TAP_OWNER="kamakiri-labs"
TAP_NAME="homebrew-tap"
TAP_BRANCH="main"
# Where the cask sits in the tap, and where the release build leaves it here.
# The first is Homebrew's own convention for a tap; the second is the release
# build's default output path.
TAP_CASK_PATH="Casks/kamakiri.rb"
CASK_FILE="dist/homebrew/Casks/kamakiri.rb"

API_PATH="repos/${TAP_OWNER}/${TAP_NAME}/contents/${TAP_CASK_PATH}"
TAP_FILE_URL="https://github.com/${TAP_OWNER}/${TAP_NAME}/blob/${TAP_BRANCH}/${TAP_CASK_PATH}"

# A release tag: a leading v and three numbers, with an optional prerelease
# suffix. The suffix is accepted by the shape check and then skipped below,
# rather than refused here, so that a prerelease tag pushed by hand stops with
# an explanation instead of with a complaint about its shape.
RELEASE_TAG='^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$'

# On stdout, since --help asks for it. The paths that print it because something
# was wrong redirect it to stderr themselves.
usage() {
  echo "Usage: $0 --version vX.Y.Z"
  echo ""
  echo "  --version vX.Y.Z   the release tag the cask was generated for"
  echo ""
  echo "Run it from the directory holding dist/, with GH_TOKEN carrying a token"
  echo "that may write to the ${TAP_OWNER}/${TAP_NAME} repository."
}

refuse() {
  for line in "$@"; do
    echo "${line}" >&2
  done
  exit 1
}

need_value() {
  echo "The flag $1 needs a value." >&2
  usage >&2
  exit 1
}

version=""

while [[ $# -gt 0 ]]; do
  case "$1" in
    --version)
      # An empty value is no value. Taken as one it would leave the flag looking
      # as though it had never been given, so a second --version behind it would
      # go through and decide what is pushed.
      [[ $# -ge 2 && -n "$2" ]] || need_value "$1"
      if [[ -n "${version}" ]]; then
        echo "--version was given more than once, first as ${version}." >&2
        echo "One tag decides both which cask is checked and which version is pushed, and the second would quietly be the one that reached the tap." >&2
        exit 1
      fi
      version="$2"
      shift 2
      ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "Unknown flag: $1" >&2
      usage >&2
      exit 1
      ;;
  esac
done

if [[ -z "${version}" ]]; then
  echo "--version is required." >&2
  usage >&2
  exit 1
fi

if [[ ! "${version}" =~ ${RELEASE_TAG} ]]; then
  refuse "The version ${version} is not a release tag." \
    "It has to read vMAJOR.MINOR.PATCH, with an optional prerelease suffix, as v0.1.0 or v0.1.0-rc1."
fi

# Ahead of everything, so the fewest possible things happen on the way past it.
# The operator path cannot produce a prerelease tag; what this is for is a tag
# pushed to the repository by hand, which the release workflow's own trigger
# would otherwise carry all the way here.
if [[ -n "${BASH_REMATCH[4]:-}" ]]; then
  echo "Nothing is pushed for ${version}: the tag carries a prerelease suffix, and the tap serves the release line."
  exit 0
fi

release_version="${version#v}"

if [[ -z "${GH_TOKEN:-}" ]]; then
  refuse "GH_TOKEN is empty or unset, and there is nothing to authenticate the push to the tap with." \
    "In the release workflow its value is the HOMEBREW_TAP_TOKEN secret, which has to exist on the repository the workflow runs in." \
    "By hand, export a token that may write to ${TAP_OWNER}/${TAP_NAME}."
fi

if [[ ! -f "${CASK_FILE}" ]]; then
  refuse "There is no ${CASK_FILE} under $(pwd)." \
    "The release build generates it, and this pushes what that build generated." \
    "Run this from the directory the release build wrote dist/ into."
fi

# The cask's own version line, read without a pipeline so that nothing here
# turns on a reader closing one early. The first such line is the cask's: the
# stanzas below it carry no version of their own.
cask_version=""
# The final line counts even with no newline behind it, which a plain read drops.
while IFS= read -r line || [[ -n "${line}" ]]; do
  if [[ "${line}" =~ ^[[:blank:]]*version\ \"([^\"]*)\" ]]; then
    cask_version="${BASH_REMATCH[1]}"
    break
  fi
done < "${CASK_FILE}"

# Nothing else read here carries a version, so this is the whole of what stands
# between a cask generated for one release and a push naming another. It is also
# what keeps a locally rendered cask out of the tap: a snapshot's version is the
# tag plus a marker and a commit, so it fails this, and correctly so.
if [[ "${cask_version}" != "${release_version}" ]]; then
  refuse "${CASK_FILE} names version ${cask_version:-nothing at all}, and this run is publishing ${release_version}." \
    "A cask names the release it rides with, so this one was generated for something other than ${version}." \
    "Nothing was pushed."
fi

content="$(base64 < "${CASK_FILE}" | tr -d '\n')"

gh_out="$(mktemp)"
gh_err="$(mktemp)"
# Every path out of here is an exit rather than a fall-through, refusals
# included, so the cleanup is an exit hook and not a block at the end.
trap 'rm -f "${gh_out}" "${gh_err}"' EXIT

# The two streams are kept apart on the read, because only stdout carries the
# body the answer is discriminated on. Exactly two answers can be acted on and
# everything else stops the run: read as absence, an answer that is neither
# would write over a state nobody saw; read as presence, it would skip a push
# the release needs.
read_status=0
read_answer="$(gh api "${API_PATH}?ref=${TAP_BRANCH}" --jq '.sha, .content' 2> "${gh_err}")" || read_status=$?

unreadable() {
  {
    echo "The tap's answer for ${TAP_CASK_PATH} could not be read, so nothing was pushed."
    echo "Exactly two answers can be acted on: a success carrying the file's blob sha, or a 404 saying it is not there."
    echo "gh exited ${read_status} and answered:"
    echo "${read_answer}"
    cat "${gh_err}"
  } >&2
  exit 1
}

sha=""
if [[ "${read_status}" -eq 0 ]]; then
  sha="$(head -n 1 <<< "${read_answer}")"
  # The sha is the whole of what a readable success has to carry: it is what the
  # write quotes as the claim that this saw what it is replacing, so a success
  # without one is not the success shape however green it looks. The content is
  # held to no such bar, since an empty one is the honest answer for an empty
  # file in the tap, and refusing it would refuse a state this should overwrite.
  if [[ -z "${sha}" ]]; then
    unreadable
  fi
  remote_content="$(tail -n +2 <<< "${read_answer}" | tr -d '\n')"
  # Compared as base64 on both sides rather than by decoding one of them, which
  # spares this a decoder whose flag spelling differs between the platforms a
  # release and an operator run it on. The comparison is as strict as one on the
  # bytes while both sides encode alike, padding included, which is what the API
  # was observed to do; the direction it could fail in is harmless, an equal pair
  # read as different costing a write of the right bytes. Trimming either side
  # would be the harmful direction, since a copy differing by one trailing
  # newline is a different file and has to be written.
  if [[ "${remote_content}" == "${content}" ]]; then
    echo "The tap already holds this cask at ${release_version}, so nothing was pushed."
    echo "${TAP_FILE_URL}"
    exit 0
  fi
else
  # The body carries the status, and gh's own sentence on stderr does not: that
  # is human wording rather than a contract. The whitespace inside the body is
  # not fixed either, the same request having been answered both on one line and
  # indented, so the field is matched with the spacing taken out.
  compact="${read_answer//[[:space:]]/}"
  case "${compact}" in
    *'"status":"404"'*) ;;
    *) unreadable ;;
  esac
fi

# The message is the wording the release build's own default commit template
# renders to for this project and tag, so the tap's history reads the same
# whether the file arrives this way or straight from that build some day.
put_args=(
  -X PUT "${API_PATH}"
  -f "message=Brew cask update for kamakiri version ${version}"
  -f "branch=${TAP_BRANCH}"
  -f "content=${content}"
)
# Only on an update. The contents API takes the blob sha as the writer's claim
# to have seen what it is replacing, and a create has nothing to claim.
if [[ -n "${sha}" ]]; then
  put_args+=(-f "sha=${sha}")
fi

write_status=0
gh api "${put_args[@]}" > "${gh_out}" 2> "${gh_err}" || write_status=$?
if [[ "${write_status}" -ne 0 ]]; then
  # Both streams this time. What went wrong is on stderr as a sentence and on
  # stdout as the API's own body, and which of the two carries the detail
  # depends on what refused the write, so neither is thrown away.
  {
    echo "Writing ${TAP_CASK_PATH} to ${TAP_OWNER}/${TAP_NAME} failed, and gh exited ${write_status}."
    cat "${gh_err}"
    cat "${gh_out}"
    echo "Nothing is retried here. Fix what refused the write and run this again: a tap already holding this cask is left alone, so a second run converges."
  } >&2
  exit 1
fi

echo "Pushed the cask for ${version} to ${TAP_FILE_URL}"
