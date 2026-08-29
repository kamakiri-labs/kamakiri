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

Once a release binary is installed, updating is one command: `kamakiri
upgrade` downloads the latest release for your platform, checks it against
`checksums.txt`, and replaces the binary in place. There is no need to repeat
the steps above for a new version.

### go install

```sh
go install github.com/kamakiri-labs/kamakiri/cmd/kamakiri@latest
```

This works, but it is not a supported channel: releases are neither tested nor
announced through it. A binary built this way also reports its version as
`(dev)`, since the version is stamped in at release build time, and it cannot
update itself: `kamakiri upgrade` only replaces a binary that came from a
release.

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
