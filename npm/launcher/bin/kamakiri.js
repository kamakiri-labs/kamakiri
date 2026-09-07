#!/usr/bin/env node
// The command npm installs as `kamakiri`. It resolves the platform package
// carrying this machine's binary and runs it, passing everything through: the
// arguments as given, the three standard streams, the exit code, and the
// signal a run died of. An interrupt is the binary's to answer, so this sits
// out SIGINT, SIGTERM and SIGHUP for as long as the binary is running.
//
// The shebang is what runs this on macOS and Linux, where npm links the file
// itself. Windows gets generated .cmd and .ps1 shims that invoke node
// explicitly and never read the line.
//
// It downloads nothing and runs no install script. The binary is already on
// disk when this runs, because npm installed the platform package as an
// optional dependency whose os/cpu fields matched this machine.
"use strict";

const { spawnSync } = require("child_process");

// The six platforms a release publishes a binary for, spelled the way
// process.platform and process.arch spell them. The packaging side writes the
// same six names on the packages themselves, so this list and that one are the
// two halves of one string and have to agree.
const SUPPORTED = [
  "darwin-arm64",
  "darwin-x64",
  "linux-arm64",
  "linux-x64",
  "win32-arm64",
  "win32-x64",
];

const RELEASES_URL = "https://github.com/kamakiri-labs/kamakiri/releases";
const INSTALL_SCRIPT_URL = "https://get.kamakiri-labs.jp/install.sh";

// The three signals this stands aside for while the binary runs. A Ctrl-C and
// a hangup are aimed at the whole foreground job, so each reaches this process
// alongside the binary it started. SIGTERM is here because the CLI answers that
// one itself and prints how to pick an interrupted run back up, and this
// process dying on it first is what would cut that short.
//
// The set stops there on purpose. SIGQUIT stays the way out of a binary that
// traps the others and will not stop, and a listener on SIGTSTP that does
// nothing would swallow a Ctrl-Z instead of suspending the job.
const JOB_SIGNALS = ["SIGINT", "SIGTERM", "SIGHUP"];

// Where this machine's binary lives, or null for a platform no release covers.
// A pure function of the pair rather than a read of process, so the two win32
// answers can be checked from any machine: every end-to-end run of this file
// happens on the host's own platform, and the .exe half of the split would
// otherwise first be exercised on a user's Windows box.
function platformTarget(platform, arch) {
  const pair = platform + "-" + arch;
  if (SUPPORTED.indexOf(pair) === -1) {
    return null;
  }
  return {
    packageName: "@kamakiri-labs/cli-" + pair,
    // The packaging side appends the same .exe to the two win32 packages. A
    // shim that forgot it would report a package that is present and correct
    // as missing, and only on Windows.
    binarySubpath: platform === "win32" ? "bin/kamakiri.exe" : "bin/kamakiri",
  };
}

function say(lines) {
  process.stderr.write(lines.join("\n") + "\n");
}

function main(args) {
  const target = platformTarget(process.platform, process.arch);
  if (target === null) {
    say([
      "kamakiri: there is no kamakiri binary for " + process.platform + "-" + process.arch + ".",
      "The platforms a release covers are listed on the releases page, " + RELEASES_URL + ".",
      "The install script at " + INSTALL_SCRIPT_URL + " installs from that same set, so it",
      "has nothing for this machine either.",
    ]);
    return 1;
  }

  const request = target.packageName + "/" + target.binarySubpath;
  let binary;
  try {
    // The subpath is resolved rather than joined onto a guessed directory, so
    // the binary is found wherever the install put the platform package: under
    // the launcher, hoisted to the top of a project's node_modules, or in a
    // store a package manager links from.
    binary = require.resolve(request);
  } catch {
    say([
      "kamakiri: the platform package " + target.packageName + " is not installed,",
      "so there is no binary to run. Optional dependencies are the likely reason: an install",
      "run with --no-optional, or with optional dependencies omitted in an npm configuration,",
      "skips it. Installing again with them enabled brings it in, with",
      "npm install -g kamakiri@latest for a global install, or npm install in the project that",
      "depends on kamakiri.",
    ]);
    return 1;
  }

  // Ignored for as long as the binary is running, so the interrupt is the
  // binary's to answer. Node kills a process outright on any of these when
  // nothing is listening, and a Ctrl-C reaches this process at the same moment
  // it reaches the binary, so without this the shim would die first and hand
  // the shell its prompt back over whatever the CLI was still printing about
  // the run it was interrupted in the middle of. The listener does nothing on
  // purpose: it exists to displace that default, not to act.
  const ignoreWhileRunning = () => {};
  for (const signal of JOB_SIGNALS) {
    process.on(signal, ignoreWhileRunning);
  }

  const result = spawnSync(binary, args, { stdio: "inherit" });

  // Dropped again the moment the binary is gone, which puts the default back.
  // The re-raise below counts on that: with the listener still registered it
  // would run the listener instead, and this process would survive a death it
  // is supposed to be passing on.
  for (const signal of JOB_SIGNALS) {
    process.removeListener(signal, ignoreWhileRunning);
  }

  // spawnSync answers in one of three ways, and each one means something
  // different to whoever ran the command.
  if (result.error) {
    // The binary was never started, so there is no exit code to pass on and
    // reporting one would say the CLI ran and failed.
    say([
      "kamakiri: the binary at " + binary + " could not be started (" +
        (result.error.code || result.error.message) + ").",
      "Installing the package again replaces it, with npm install -g kamakiri@latest for a",
      "global install, or npm install in the project that depends on kamakiri.",
    ]);
    return 1;
  }
  if (result.signal) {
    // Re-raised on this process rather than turned into a number, so a caller
    // sees the same death the binary died of: a command killed by a signal has
    // to look killed to the shell above it, and a number in that position
    // would read as a run that finished and failed. Windows reports no signal
    // at all, so nothing there reaches this and the status below is what a
    // killed child comes back as.
    process.kill(process.pid, result.signal);
  }
  if (typeof result.status === "number") {
    return result.status;
  }
  // Reached only if the re-raised signal did not end this process, or if
  // spawnSync answered in none of its three ways. Neither is a run that
  // finished, so neither reports success.
  return 1;
}

// Guarded so that requiring this file yields its functions without running a
// command, which is what lets the platform-pair choice be checked directly.
//
// The code is set rather than exited on. A write to stderr that lands on a pipe
// is asynchronous, and process.exit ends the process with whatever is still
// queued unwritten; these messages are short enough to fit the pipe buffer and
// would come through either way, so setting the code is the spelling that does
// not rest on their length. Nothing else is pending once spawnSync has
// returned, so the process ends on this line regardless.
if (require.main === module) {
  process.exitCode = main(process.argv.slice(2));
}

module.exports = { platformTarget };
