# kamakiri

`kamakiri` is the command-line client for Kamakiri Pages, a static-site hosting
service. You build your site locally, deploy the output directory, and it goes
live over HTTPS on a `kamakiri-pages.jp` subdomain or on a domain of your own.
Kamakiri Pages is an invitation-only private beta today, so `kamakiri login`
only succeeds for an email that has been invited. To request an invitation,
email beta@kamakiri-labs.jp.

The CLI is a single static Go binary with no runtime dependencies. Most commands
that change something are synchronous: they return once the change is actually
live for a visitor, not once the request was accepted. Where a command waits,
`--no-wait` makes it return as soon as the change is queued instead.

## Install

### Install scripts

On macOS and Linux:

```sh
curl -fsSL https://get.kamakiri-labs.jp/install.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://get.kamakiri-labs.jp/install.ps1 | iex
```

Either one resolves the latest release for your platform, verifies the download
against `checksums.txt`, and puts the binary in place. It goes to `~/.local/bin`
as `kamakiri` on macOS and Linux, and to `%USERPROFILE%\.local\bin` as
`kamakiri.exe` on Windows. Set `KAMAKIRI_INSTALL_DIR` to install somewhere else.
Neither needs root or administrator rights, and running the command again
updates an existing install (see Updating below).

On macOS and Linux, when the install directory is not on your `PATH` the script
prints the line to add to your shell's startup file, guessed from your `SHELL`
environment variable; it edits no dotfile itself.
On Windows the directory is added to your user environment when it is not there
already, so a terminal opened afterwards has it and so does the session you ran
the command in.

### npm

```sh
npm install -g kamakiri
```

This needs Node, which comes with npm. It installs a small launcher package
named `kamakiri`, which provides the command, and one package carrying the
binary for your machine. The binary packages are
`@kamakiri-labs/cli-<platform>-<arch>`, published for `darwin-arm64`, `darwin-x64`,
`linux-arm64`, `linux-x64`, `win32-arm64` and `win32-x64`. The launcher depends
on all six optionally, so npm installs only the one that matches. The `kamakiri`
command then behaves like a binary you installed directly: same arguments, same
output, same exit code, and Ctrl-C reaches the CLI. The package carries the
binary itself, so the install fetches nothing from anywhere but the registry,
and no install script runs.

`npx kamakiri@latest` runs the CLI without installing anything, and a project can
take `kamakiri` as a dependency of its own.

On a platform no release covers the install still succeeds, since the binary
packages are optional, and the command tells you there is no binary for your
platform and architecture, then points you at the releases page and the install
script. An npm copy is updated through npm rather than with `kamakiri upgrade`
(see Updating below).

### Homebrew

```sh
brew install kamakiri-labs/tap/kamakiri
```

This installs the cask our tap carries. It covers macOS, and Linux on Homebrew
4.5.0 or newer. Homebrew verifies the binary it downloads against the SHA-256
checksum the cask carries, so there is nothing for you to check by hand. On macOS
the install clears the quarantine attribute the system puts on what a cask
downloads, so nothing is left for you to clear by hand either. A Homebrew copy is
updated with `brew upgrade kamakiri` rather than with `kamakiri upgrade` (see
Updating below).

### Download a binary

Each release publishes one binary per operating system and architecture on the
[releases page](https://github.com/kamakiri-labs/kamakiri/releases):

- `kamakiri-darwin-amd64` (macOS, Intel)
- `kamakiri-darwin-arm64` (macOS, Apple silicon)
- `kamakiri-linux-amd64`
- `kamakiri-linux-arm64`
- `kamakiri-windows-amd64.exe`
- `kamakiri-windows-arm64.exe`

Every release also publishes `checksums.txt`, holding one
`<sha256>  <asset-name>` line per asset (two spaces, the format `sha256sum` and
`shasum` read). Verify the file you downloaded before you install it: print its
hash and compare it against the matching line.

```sh
sha256sum kamakiri-linux-amd64          # Linux
shasum -a 256 kamakiri-darwin-arm64     # macOS
```

```powershell
Get-FileHash -Algorithm SHA256 kamakiri-windows-amd64.exe
```

`Get-FileHash` prints uppercase hex while `checksums.txt` holds lowercase, so
compare the two ignoring case.

On macOS and Linux, mark the verified file executable, then rename it to
`kamakiri` and put it somewhere on your `PATH`:

```sh
chmod +x kamakiri-darwin-arm64
mv kamakiri-darwin-arm64 ~/.local/bin/kamakiri   # or any directory on your PATH
```

On Windows, rename the downloaded file to `kamakiri.exe` and put it in a
directory on your `PATH`. Keep the `.exe` extension; without it the binary will
not run.

On macOS, a file downloaded through a browser carries a quarantine attribute,
which makes the system check the binary before running it; a file fetched with
`curl` carries no such attribute. A signed and notarized binary passes the
check. We do not sign or notarize yet, so a browser-downloaded binary is
refused until the attribute is cleared:

```sh
xattr -d com.apple.quarantine /path/to/kamakiri
```

### go install

```sh
go install github.com/kamakiri-labs/kamakiri/cmd/kamakiri@latest
```

This works, but it is not a supported channel: releases are neither tested nor
announced through it. A binary built this way also reports its version as
`(dev)`, since the version is stamped in at release build time, and it cannot
update itself: `kamakiri upgrade` only replaces a binary that came from a
release.

### Updating

A binary put in place by an install script or downloaded by hand is updated with
`kamakiri upgrade`, which downloads the latest release for your platform, checks
it against `checksums.txt`, and replaces the binary in place. Re-running an
install script does the same job, so either way is fine. There is no need to
download and verify a new version by hand.

A copy installed through a package manager is updated through that package
manager instead, because `kamakiri upgrade` recognizes it and hands you back to
that package manager rather than replacing the binary. For npm, run
`npm install -g kamakiri@latest` for a global install; update the `kamakiri`
dependency in the project that has one; and for a copy `npx` fetched, there is
nothing to update in place, since `npx kamakiri@latest` fetches the newest
release each time. For Homebrew, run `brew upgrade kamakiri`.

### Uninstall

An install made by an install script is undone by the matching uninstall
script. On macOS and Linux:

```sh
curl -fsSL https://get.kamakiri-labs.jp/uninstall.sh | sh
```

On Windows, in PowerShell:

```powershell
irm https://get.kamakiri-labs.jp/uninstall.ps1 | iex
```

Each removes the binary (from `KAMAKIRI_INSTALL_DIR` when set, the default
install directory otherwise) and the config directory, which holds the saved
API key; on Windows it also takes the install directory out of your user
`PATH`. Nothing else is touched. The key itself stays valid on your account,
since only the local copy is removed. Running it on a machine with nothing to
remove is not an error.

A copy installed through a package manager is removed through that package
manager instead: `npm uninstall -g kamakiri` or `brew uninstall kamakiri`.

## Quick start

```sh
kamakiri login          # email, confirmation code, API key saved locally
kamakiri init           # pick a subdomain, link this directory to a site
kamakiri deploy ./dist  # upload the build output and wait until it is live
```

`kamakiri deploy` takes a directory or a `.tar.gz` (or `.tgz`) archive of output
you have already built; your source never leaves your machine. It uploads, then
waits, and finishes with a `✓ live` line once the site is really serving the new
content. After that, a deploy is just that one command again, and
`kamakiri status` shows where a site stands at any time.

Running `kamakiri` with no arguments prints the usage message, which lists every
command: custom domains, CDN modes, rollbacks and the rest. There is no separate
help command, and this is a usage message rather than a help screen: it goes to
stderr and exits non-zero.

## Deploying from CI

A runner has no credentials file and nobody at the keyboard, so it authenticates
through `KAMAKIRI_API_KEY`. Set it in the runner's environment, from a
repository secret rather than a literal written into the workflow file. While
the variable holds a key, the credentials file is not read at all, and
`kamakiri status` reports the variable as the credential source.

The key is the `api_key` value in `~/.config/kamakiri/credentials.json`, the
file `kamakiri login` writes on your own machine; on Windows,
`%USERPROFILE%\.config\kamakiri\credentials.json`. Copy that value in as the
whole secret, with nothing around it. `login` prints only the path of the file
it wrote, never the key. Running `login` again mints a new key and leaves the
one already in CI valid, so rotating on your own machine does not break the
runner.

A command that needs a credential and finds neither the variable nor a
credentials file exits 1 with one line pointing at both remedies:
`kamakiri login`, or the variable.

## Language

The CLI speaks English and Japanese. It settles on one at startup from your
environment, and `kamakiri language` reports which one is in force and what
chose it. `kamakiri language en` or `kamakiri language ja` saves a preference
that outranks the environment locale on every later command. Setting
`KAMAKIRI_LANG` to `en` or `ja` outranks the saved preference in turn, so a
single run or a single shell can use the other language without changing what
is saved.

## License

MIT. See [LICENSE](LICENSE).
