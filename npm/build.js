// Turns a release build into the seven npm package trees the CLI publishes:
// six platform packages, each carrying that platform's binary, and the
// launcher whose bin shim resolves and runs it.
//
//   node npm/build.js --version vX.Y.Z[-suffix]
//
// It reads dist/artifacts.json, verifies every binary it packs against the
// checksum recorded there, and writes the trees under dist/npm: the launcher
// at dist/npm/kamakiri and each platform package at
// dist/npm/cli-<platform>-<arch>. That directory is the whole interface to the
// publishing side, which reads each tree's package.json for the name to
// publish it under.
//
// The static half of the launcher (the shim and the README npmjs.com renders)
// is in launcher/ beside this file, and the licence notice all seven ship is at
// the root of this tree; everything else is generated here.
"use strict";

const crypto = require("crypto");
const fs = require("fs");
const path = require("path");

// The directory goreleaser writes, relative to the working directory, and not
// a flag. The manifest's own paths are workspace-relative and already carry
// this prefix, so the working directory is what names the build; a flag
// pointing this somewhere else could only ever disagree with them.
const DIST_DIR = "dist";
const MANIFEST = path.join(DIST_DIR, "artifacts.json");
const OUT_DIR = path.join(DIST_DIR, "npm");

// Where the static launcher files are read from, resolved against this file so
// the working directory decides nothing about them.
const LAUNCHER_DIR = path.join(__dirname, "launcher");

// The notice behind the license field below. It sits at the root of this tree,
// one directory up from here, and a copy goes into each of the seven packages:
// a package that declares a license and ships no notice is one nobody can read
// the terms of. npm puts a LICENSE file in the tarball whatever the files list
// says, so no generated package.json names it.
const LICENSE_FILE = path.join(__dirname, "..", "LICENSE");

const LAUNCHER_NAME = "kamakiri";
const SCOPE = "@kamakiri-labs";
const REPOSITORY_URL = "git+https://github.com/kamakiri-labs/kamakiri.git";
const LICENSE = "MIT";

// The six release targets, and the one place Go's vocabulary for a platform
// meets Node's. The build names a target with GOOS and GOARCH; npm's os and
// cpu fields, the platform package names, and the lookup the launcher's shim
// makes at runtime are all spelled the way process.platform and process.arch
// spell them. windows is win32 and amd64 is x64; the rest is the same word
// twice. Nothing else translates between the two vocabularies, so a
// transposed row here ships one platform's binary under another's package
// name, and npm then installs it on exactly the machines that cannot run it.
const TARGETS = [
  { goos: "darwin", goarch: "amd64", platform: "darwin", arch: "x64" },
  { goos: "darwin", goarch: "arm64", platform: "darwin", arch: "arm64" },
  { goos: "linux", goarch: "amd64", platform: "linux", arch: "x64" },
  { goos: "linux", goarch: "arm64", platform: "linux", arch: "arm64" },
  { goos: "windows", goarch: "amd64", platform: "win32", arch: "x64" },
  { goos: "windows", goarch: "arm64", platform: "win32", arch: "arm64" },
];

// A release tag: three numbers behind a v, with an optional prerelease tail.
// npm's own version field takes the same string with the v dropped, since it
// refuses a leading one.
const RELEASE_TAG = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$/;

function fail(...lines) {
  for (const line of lines) {
    process.stderr.write(line + "\n");
  }
  process.exit(1);
}

function parseArgs(argv) {
  let version = "";
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === "--version") {
      if (version !== "") {
        fail(
          "build.js: --version was given more than once, first as " + version + ".",
          "One tag stamps all seven packages, and a second one would quietly be the tag they carry.",
        );
      }
      i += 1;
      // An empty value is no value: it names no tag, and taking it as one would
      // leave the flag looking as though it had never been given, so a second
      // --version behind it would go through and stamp the seven.
      if (i >= argv.length || argv[i] === "") {
        fail("build.js: --version was given no value. It takes a release tag, as --version v0.1.0.");
      }
      version = argv[i];
      continue;
    }
    fail(
      "build.js: " + argv[i] + " is not an argument this takes.",
      "The only one is --version vX.Y.Z, the tag the packages are stamped with.",
    );
  }
  if (version === "") {
    fail("build.js: --version is required. It takes a release tag, as --version v0.1.0.");
  }
  if (!RELEASE_TAG.test(version)) {
    fail(
      "build.js: " + version + " is not a release tag.",
      "It has to read vMAJOR.MINOR.PATCH, with an optional prerelease suffix, as v0.1.0 or v0.1.0-rc1.",
    );
  }
  return { tag: version, npmVersion: version.slice(1) };
}

// The manifest entries for the files a release publishes. The build registers
// each target twice: once for the file it built, and once more for the asset
// that file is published as, which is the entry carrying the published name
// and the checksum. Only the second kind is packed, and what tells them apart
// is extra.Format, which the build-stage twin does not have (it carries
// extra.Builder instead). The internal_type field distinguishes them too and
// is deliberately not read: it is the build's own numbering, unnamed in any
// contract, and a renumbering would silently select the wrong half.
function selectPublishedBinaries(entries) {
  return entries.filter(
    (entry) =>
      entry !== null &&
      typeof entry === "object" &&
      entry.type === "Binary" &&
      entry.extra !== null &&
      typeof entry.extra === "object" &&
      entry.extra.Format === "binary",
  );
}

function readManifest() {
  let entries;
  try {
    entries = JSON.parse(fs.readFileSync(MANIFEST, "utf8"));
    if (!Array.isArray(entries)) {
      throw new Error("it holds no array of artifacts");
    }
  } catch (err) {
    fail(
      "build.js: " + MANIFEST + " could not be read (" + err.message + ").",
      "It is the manifest the release build writes beside the binaries, and this reads the",
      "binaries to package out of it. Run this from the directory the build wrote " + DIST_DIR + " into.",
    );
  }
  return entries;
}

// One target's binary, read off disk and held to the checksum the manifest
// publishes for it. The bytes are verified here rather than trusted because
// they reach this point over a file transfer that this cannot see: what the
// release build produced and what is on disk now are two different claims, and
// the sum that separates them is already in hand.
function readVerifiedBinary(entry, target) {
  if (typeof entry.path !== "string" || entry.path === "") {
    fail(
      "build.js: the " + target.goos + "/" + target.goarch + " entry in " + MANIFEST + " carries no path.",
      "The path is what names the file to package, so there is nothing to read for that target.",
    );
  }
  // The manifest's paths are workspace-relative and already carry the dist/
  // prefix ("dist/kamakiri_linux_amd64_v1/kamakiri"), so they resolve against
  // the working directory. Joining one onto DIST_DIR reads dist/dist/... and
  // fails as a file that is not there, which says nothing about the mistake.
  const source = path.resolve(entry.path);
  // Held inside the build directory rather than taken as given. The checksum
  // below cannot stand in for this: the path and the sum travel together in the
  // one manifest, so a path naming any file at all arrives with the sum of that
  // file, and the packaging would pack whatever it was pointed at.
  if (!source.startsWith(path.resolve(DIST_DIR) + path.sep)) {
    fail(
      "build.js: " + MANIFEST + " puts the " + target.goos + "/" + target.goarch + " binary at " + entry.path + ", which is outside " + DIST_DIR + ".",
      "Everything this packages comes out of the directory the build wrote, and a checksum recorded",
      "beside a path says nothing about where that path points.",
    );
  }
  let bytes;
  try {
    bytes = fs.readFileSync(source);
  } catch (err) {
    fail(
      "build.js: the " + target.goos + "/" + target.goarch + " binary could not be read (" + err.message + ").",
      MANIFEST + " names it at " + entry.path + ", relative to the directory this runs in.",
    );
  }
  const declared = typeof entry.extra.Checksum === "string" ? entry.extra.Checksum : "";
  const hashed = "sha256:" + crypto.createHash("sha256").update(bytes).digest("hex");
  if (declared.toLowerCase() !== hashed) {
    fail(
      "build.js: " + entry.path + " is not the file " + MANIFEST + " describes.",
      "The manifest lists " + (declared === "" ? "no checksum for it" : declared) + ", the file hashes to " + hashed + ".",
    );
  }
  return bytes;
}

function writeJson(file, value) {
  fs.writeFileSync(file, JSON.stringify(value, null, 2) + "\n");
}

function platformPackage(target, npmVersion, bytes) {
  const name = SCOPE + "/cli-" + target.platform + "-" + target.arch;
  const dir = path.join(OUT_DIR, "cli-" + target.platform + "-" + target.arch);
  // The same split the launcher's shim makes when it resolves this binary.
  const binaryName = target.platform === "win32" ? "kamakiri.exe" : "kamakiri";
  const binaryPath = path.join(dir, "bin", binaryName);

  fs.mkdirSync(path.dirname(binaryPath), { recursive: true });
  fs.writeFileSync(binaryPath, bytes);
  // The packaged binary is marked executable here because nothing else ever
  // marks it: the launcher declares no bin field for it, so npm creates no
  // shim for it and applies no mode of its own, and the release build's own
  // binaries reach this point through a transfer that carries file modes
  // nowhere, so the file read a moment ago can perfectly well be 0644.
  // An explicit chmod rather than the write's mode option, which the process
  // umask still narrows, and never a copy that carries the source file's mode
  // across, which is the mode this cannot trust in the first place.
  fs.chmodSync(binaryPath, 0o755);

  writeJson(path.join(dir, "package.json"), {
    name: name,
    version: npmVersion,
    description: "The kamakiri CLI binary for " + target.platform + " on " + target.arch,
    license: LICENSE,
    // npm checks a provenance attestation against the repository this names,
    // so it has to be the repository the release workflow runs in.
    repository: { type: "git", url: REPOSITORY_URL },
    // What npm holds an install to. A package whose os or cpu does not match
    // is skipped rather than installed, which is what makes the six of these
    // safe to depend on all at once.
    os: [target.platform],
    cpu: [target.arch],
    files: ["bin/" + binaryName],
    // Scoped packages are private by default, and a first publish of one
    // without this is refused. The launcher sets it too, for the reason its
    // manifest states.
    publishConfig: { access: "public" },
    // No exports field, deliberately. The launcher reaches the binary by its
    // subpath, require.resolve("@kamakiri-labs/cli-linux-x64/bin/kamakiri"), which
    // rides the legacy CommonJS subpath resolution a package with no exports
    // map keeps. An exports field replaces that with the map it declares, and
    // every path the map does not list stops resolving.
  });

  fs.copyFileSync(LICENSE_FILE, path.join(dir, "LICENSE"));

  fs.writeFileSync(
    path.join(dir, "README.md"),
    [
      "# " + name,
      "",
      "The kamakiri CLI binary for " + target.platform + " on " + target.arch + ".",
      "",
      "This package carries one platform's binary and declares no command of its own.",
      "Install `kamakiri`, which brings in the one of these that matches your machine",
      "and provides the command itself:",
      "",
      "```sh",
      "npm install -g kamakiri",
      "```",
      "",
      "The documentation and the source are at",
      "[github.com/kamakiri-labs/kamakiri](https://github.com/kamakiri-labs/kamakiri).",
      "",
    ].join("\n"),
  );

  return name;
}

function launcherPackage(npmVersion, platformNames) {
  const dir = path.join(OUT_DIR, LAUNCHER_NAME);
  fs.mkdirSync(path.join(dir, "bin"), { recursive: true });

  const optionalDependencies = {};
  for (const name of platformNames) {
    // Pinned exactly, never a range: all seven packages are stamped from one
    // tag and only the six built beside this launcher are known to match it.
    optionalDependencies[name] = npmVersion;
  }

  writeJson(path.join(dir, "package.json"), {
    name: LAUNCHER_NAME,
    version: npmVersion,
    description: "The kamakiri CLI, the command-line client for Kamakiri Pages",
    license: LICENSE,
    repository: { type: "git", url: REPOSITORY_URL },
    // Advisory rather than load-bearing: the shim is CommonJS and uses
    // nothing newer, and this states the floor the channel is tested against.
    engines: { node: ">=18" },
    bin: { kamakiri: "bin/kamakiri.js" },
    // An unscoped package is public by default, but npm still refuses to
    // generate provenance for a package the registry has never seen unless
    // the access is stated outright, so the first publish of this name fails
    // without it.
    publishConfig: { access: "public" },
    // Optional, so an install on a platform none of them match still
    // succeeds and the command reports the missing platform itself, rather
    // than the install failing with npm's own message about a dependency.
    optionalDependencies: optionalDependencies,
    // No files list, deliberately: this tree is generated and holds only what
    // ships, so a list would be a second place to update when it gains a file.
  });

  const shim = path.join(dir, "bin", "kamakiri.js");
  fs.copyFileSync(path.join(LAUNCHER_DIR, "bin", "kamakiri.js"), shim);
  // The shim is the file npm links as the command, and it carries a shebang,
  // so it ships executable rather than depending on the mode a copy landed.
  fs.chmodSync(shim, 0o755);
  // The page npmjs.com renders for the launcher, which is the package a user
  // installs by name.
  fs.copyFileSync(path.join(LAUNCHER_DIR, "README.md"), path.join(dir, "README.md"));
  fs.copyFileSync(LICENSE_FILE, path.join(dir, "LICENSE"));
}

function main(argv) {
  const { tag, npmVersion } = parseArgs(argv);
  const entries = readManifest();

  const published = selectPublishedBinaries(entries);
  if (published.length !== TARGETS.length) {
    fail(
      "build.js: " + MANIFEST + " publishes " + published.length + " binaries, and this packages exactly " + TARGETS.length + ".",
      "It found: " + (published.map((entry) => entry.name).join(", ") || "none") + ".",
    );
  }

  const binaries = TARGETS.map((target) => {
    const matches = published.filter(
      (entry) => entry.goos === target.goos && entry.goarch === target.goarch,
    );
    if (matches.length !== 1) {
      fail(
        "build.js: " + MANIFEST + " publishes " + matches.length + " binaries for " + target.goos + "/" + target.goarch + ", and this packages exactly one.",
        "The six it packages are " + TARGETS.map((t) => t.goos + "/" + t.goarch).join(", ") + ".",
      );
    }
    return readVerifiedBinary(matches[0], target);
  });

  // Cleared rather than written over, so a tree an earlier run left behind can
  // never be picked up as one of this run's and published under a name and a
  // version this build never made. Cleared here rather than at the top, once
  // every binary has been read and verified, so a run that refuses leaves the
  // earlier one's output whole instead of destroying it on the way to failing.
  fs.rmSync(OUT_DIR, { recursive: true, force: true });
  fs.mkdirSync(OUT_DIR, { recursive: true });

  process.stdout.write("Packaging " + tag + " as npm version " + npmVersion + ", from " + MANIFEST + ".\n");

  const names = [];
  TARGETS.forEach((target, index) => {
    const name = platformPackage(target, npmVersion, binaries[index]);
    names.push(name);
    process.stdout.write("  " + name + " carries " + target.goos + "/" + target.goarch + ".\n");
  });

  launcherPackage(npmVersion, names);
  process.stdout.write("  " + LAUNCHER_NAME + " pins all " + names.length + " of them at " + npmVersion + ".\n");
  process.stdout.write("Wrote " + (names.length + 1) + " package trees to " + OUT_DIR + ".\n");
}

main(process.argv.slice(2));
