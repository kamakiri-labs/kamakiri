# kamakiri

`kamakiri` is the command-line client for Kamakiri Pages, a static-site hosting
service. You build your site locally, deploy the output directory, and it goes
live over HTTPS on a `kamakiri-pages.jp` subdomain or on a domain of your own.

```sh
npm install -g kamakiri
kamakiri --help
```

`npx kamakiri@latest` runs it without installing anything, and
`npm install -g kamakiri@latest` is how an installed copy is updated.

This package is a small launcher. The CLI itself is a single static Go binary,
published one per platform as `@kamakiri-labs/cli-<platform>-<arch>`; installing
`kamakiri` brings in the one package that matches your machine, as an optional
dependency npm skips everywhere else. The `kamakiri` command resolves that
binary and runs it, passing your arguments, the standard streams and the exit
code straight through.

Kamakiri Pages is an invitation-only private beta today, so `kamakiri login`
only succeeds for an email that has been invited. To request an invitation,
email beta@kamakiri-labs.jp.

The full documentation, the other ways to install the CLI, and the source are
at [github.com/kamakiri-labs/kamakiri](https://github.com/kamakiri-labs/kamakiri).
The CLI is MIT licensed.
