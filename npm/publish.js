// Publishes the seven npm package trees a release build produced:
//
//   node npm/publish.js --version vX.Y.Z[-suffix]
//
// Run from the directory holding dist/npm, which build.js beside this file
// writes. Those seven trees are the whole interface between the two: each one
// carries in its package.json the name it publishes under, and the directory
// it sits in is a layout detail nothing here reads a name from.
//
// The order is the six platform packages, then a wait until the registry
// serves all six, then the launcher. The launcher pins the six at exactly this
// version, so an install that reached it first would resolve a package the
// registry cannot hand out yet.
//
// Running it again is the recovery for a release that stopped halfway: every
// package the registry already serves at this version is skipped as success,
// and a publish that loses the race to another run counts the same way, so a
// second run converges on the same seven published packages. That again is a
// re-run of the job that failed, never a run on somebody's own machine: every
// publish here asks for provenance, and npm generates an attestation only
// inside GitHub Actions or GitLab CI and refuses outright anywhere else.
//
// Authentication has two modes, because the channel moves from one to the
// other and neither should change this file. With NPM_TOKEN set it writes a
// throwaway npmrc outside the working directory and points npm at it; with no
// token it configures nothing at all and npm authenticates the publish against
// the registry itself.
"use strict";

const { spawnSync } = require("child_process");
const fs = require("fs");
const os = require("os");
const path = require("path");

// The directory the release build wrote and build.js put the package trees
// under, relative to the working directory. Not a flag, for the reason it is
// not one there either: what is published is what that script left behind.
const DIST_DIR = "dist";
const OUT_DIR = path.join(DIST_DIR, "npm");

const LAUNCHER_NAME = "kamakiri";
const SCOPE = "@kamakiri-labs";
const SCOPE_PREFIX = SCOPE + "/";
// Six platform packages and the launcher, which is what one release is.
const PLATFORM_COUNT = 6;
const TREE_COUNT = PLATFORM_COUNT + 1;

// A release tag, the same shape build.js takes, since the two are handed the
// same argument. npm's own version field is that string with the v dropped.
const RELEASE_TAG = /^v(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)(-[0-9A-Za-z.-]+)?$/;

// The line an npmrc carries to authenticate against the public registry.
const AUTH_LINE = "//registry.npmjs.org/:_authToken=";

// The npm the tokenless path needs. Publishing authenticated by npm's own
// exchange with the registry, rather than by a token, arrived in this release;
// an older npm asked to do it looks for a token that is not there.
const NPM_FLOOR = "11.5.1";

// The wording reported of a publish that lands on a version the registry
// already serves. npm documents that such a publish fails, but not the sentence
// it fails with: this one is read off a report on npm's issue tracker, and
// nothing here has seen npm print it. Matched loosely and never trusted on its
// own, which is what makes resting on it safe: a failure carrying it is
// confirmed against the registry, and only that answer decides, so a wording
// that has drifted costs a run rather than a wrong success.
const CONFLICT_WORDING = "cannot publish over the previously published version";

// How long the wait for the registry to serve what was just published runs.
// The ceiling has to outlive the five minutes a package document is served
// with as its cache lifetime: a look inside that window can be answered by a
// cache holding a document written before the publish, so a shorter ceiling
// can run out while the release is perfectly healthy. Both are readable from
// the environment so a run can drive the mechanism without spending the wall
// clock on it.
const POLL_LOOKS = { variable: "KAMAKIRI_NPM_POLL_LOOKS", fallback: 24 };
const POLL_INTERVAL_MS = { variable: "KAMAKIRI_NPM_POLL_INTERVAL_MS", fallback: 15000 };

function fail(...lines) {
  for (const line of lines) {
    process.stderr.write(line + "\n");
  }
  process.exit(1);
}

function say(line) {
  process.stdout.write(line + "\n");
}

function parseArgs(argv) {
  let version = "";
  for (let i = 0; i < argv.length; i += 1) {
    if (argv[i] === "--version") {
      if (version !== "") {
        fail(
          "publish.js: --version was given more than once, first as " + version + ".",
          "One tag decides both what is checked and what is published, and the second would quietly be the one that reaches the registry.",
        );
      }
      i += 1;
      // An empty value is no value: taken as one it would leave the flag
      // looking as though it had never been given, so a second --version
      // behind it would go through and decide the release.
      if (i >= argv.length || argv[i] === "") {
        fail("publish.js: --version was given no value. It takes a release tag, as --version v0.1.0.");
      }
      version = argv[i];
      continue;
    }
    fail(
      "publish.js: " + argv[i] + " is not an argument this takes.",
      "The only one is --version vX.Y.Z, the tag the packages were stamped with.",
    );
  }
  if (version === "") {
    fail("publish.js: --version is required. It takes a release tag, as --version v0.1.0.");
  }
  const match = RELEASE_TAG.exec(version);
  if (match === null) {
    fail(
      "publish.js: " + version + " is not a release tag.",
      "It has to read vMAJOR.MINOR.PATCH, with an optional prerelease suffix, as v0.1.0 or v0.1.0-rc1.",
    );
  }
  return { tag: version, npmVersion: version.slice(1), prerelease: match[4] !== undefined };
}

// One of the two bounds on the wait, from the environment or from the value
// this ships with. A value that cannot be read is refused rather than replaced
// by that default: a typo that fell back would leave the real bound at
// something nobody chose, with nothing saying so. An unset variable and an
// empty one mean the same thing, since a workflow that passes a value through
// spells an absent one as empty.
function pollSetting(setting) {
  const raw = process.env[setting.variable];
  if (raw === undefined || raw === "") {
    return setting.fallback;
  }
  if (!/^[1-9][0-9]*$/.test(raw)) {
    fail(
      "publish.js: " + setting.variable + " is " + raw + ", which is not a whole number above zero.",
      "It is one of the two bounds on the wait for the registry to serve what this publishes.",
    );
  }
  return Number(raw);
}

// Everything about the seven trees that can be known without touching the
// registry, checked before anything is published. This is the one script here
// that writes somewhere irreversible, and what it reads is a directory it did
// not build itself: the packaging ran earlier and this run publishes what it
// was handed, so a dist/npm that is partial, stale or altered on the way is
// reachable, and none of it costs anything to rule out before the first
// publish.
function readPackages(npmVersion) {
  let entries;
  try {
    entries = fs.readdirSync(OUT_DIR, { withFileTypes: true });
  } catch (err) {
    fail(
      "publish.js: " + OUT_DIR + " could not be read (" + err.message + ").",
      "It is the directory build.js writes the package trees into, and this publishes what it finds there.",
      "Run this from the directory the release build wrote " + DIST_DIR + " into, after build.js has run.",
    );
  }
  const found = entries.map((entry) => entry.name).sort();
  if (entries.length !== TREE_COUNT) {
    fail(
      "publish.js: " + OUT_DIR + " holds " + entries.length + " entries, and a release is exactly " + TREE_COUNT + " package trees.",
      "It holds: " + (found.join(", ") || "nothing") + ".",
    );
  }

  const packages = [];
  for (const entry of entries) {
    const dir = path.join(OUT_DIR, entry.name);
    if (!entry.isDirectory()) {
      fail(
        "publish.js: " + dir + " is not a directory, and every package this publishes is a tree.",
        "Nothing that is not one of the seven the packaging wrote belongs in " + OUT_DIR + ".",
      );
    }
    const manifest = path.join(dir, "package.json");
    let value;
    try {
      value = JSON.parse(fs.readFileSync(manifest, "utf8"));
      if (value === null || typeof value !== "object" || Array.isArray(value)) {
        throw new Error("it holds no package manifest");
      }
    } catch (err) {
      fail(
        "publish.js: " + manifest + " could not be read (" + err.message + ").",
        "It is what names the package this would publish, and the directory it sits in is not that name.",
      );
    }
    if (typeof value.name !== "string" || value.name === "") {
      fail(
        "publish.js: " + manifest + " names no package.",
        "The name it carries is the name this publishes under, so there is nothing here to publish.",
      );
    }
    if (typeof value.version !== "string" || value.version === "") {
      fail(
        "publish.js: " + manifest + " carries no version.",
        "Every one of the seven is stamped with the tag being released, and this one is stamped with nothing.",
      );
    }
    // Against the tag with its leading v stripped, never the tag itself: npm
    // refuses a leading v in a version field, so compared raw this could never
    // pass, and a check that never passes is a check nobody notices.
    if (value.version !== npmVersion) {
      fail(
        "publish.js: " + value.name + " in " + dir + " is stamped " + value.version + ", and this was asked to publish " + npmVersion + ".",
        "The check for what is already published and the publish itself would then be about two different versions.",
      );
    }
    packages.push({ name: value.name, dir: dir, manifest: value });
  }

  // Sorted by the name each publishes under, so the order a release goes out
  // in is the same one every time and a log of one run reads against another.
  const platforms = packages
    .filter((pkg) => pkg.name.startsWith(SCOPE_PREFIX))
    .sort((a, b) => (a.name < b.name ? -1 : a.name > b.name ? 1 : 0));
  const launchers = packages.filter((pkg) => pkg.name === LAUNCHER_NAME);
  if (platforms.length !== PLATFORM_COUNT || launchers.length !== 1) {
    fail(
      "publish.js: " + OUT_DIR + " holds " + platforms.length + " " + SCOPE + " packages and " + launchers.length + " named " + LAUNCHER_NAME + ", and a release is " + PLATFORM_COUNT + " and one.",
      "It holds: " + packages.map((pkg) => pkg.name).sort().join(", ") + ".",
    );
  }
  // The launcher has to be picked out before its pins can be read, which is
  // why the two checks below sit here rather than beside the count above.
  const launcher = launchers[0];

  // A name is what a manifest carries, not what its directory is called, so
  // two trees can perfectly well publish under one name. Six names that are
  // not six packages publish one of them twice, leave another unpublished, and
  // report a release that went out whole.
  const names = platforms.map((pkg) => pkg.name);
  const repeated = names.filter(
    (name, index) => names.indexOf(name) === index && names.lastIndexOf(name) !== index,
  );
  if (repeated.length > 0) {
    fail(
      "publish.js: " + OUT_DIR + " holds more than one tree publishing as " + repeated.join(", ") + ".",
      "It holds: " + names.join(", ") + ".",
      "Each of the " + PLATFORM_COUNT + " goes out under a name of its own, and one published twice is another published never.",
    );
  }
  // The launcher's pins are what an install resolves, and both halves of each
  // one, the name and the version it is pinned at, are generated from what sits
  // beside it here, so the two agreeing is the contract. Compared by name alone
  // this would pass a tree whose pinned versions no longer say what the six
  // beside it are stamped with, and the launcher would go out pinning six
  // packages at a version nobody published.
  const pinned = pinnedSpecs(launcher.manifest);
  const required = names.map((name) => name + "@" + npmVersion);
  if (pinned.join(", ") !== required.join(", ")) {
    fail(
      "publish.js: " + launcher.name + " in " + launcher.dir + " pins " + (pinned.join(", ") || "nothing") + ".",
      OUT_DIR + " holds " + required.join(", ") + ", and those are the packages and versions it has to pin, exactly those.",
      "A pin no package here answers to is an install that fails on the platform it names, whether the name is wrong or the version is.",
    );
  }
  return { platforms: platforms, launcher: launcher };
}

// What the launcher declares it depends on, each as the name and the version
// pinned to it, sorted the way the names beside it are so that the two sets
// compare as one string. A manifest carrying no such object at all pins
// nothing, which is not six packages either. A pin whose value is not a string
// is rendered as whatever it is rather than dropped, since the refusal is about
// what was found there.
function pinnedSpecs(manifest) {
  const pins = manifest.optionalDependencies;
  if (pins === null || typeof pins !== "object" || Array.isArray(pins)) {
    return [];
  }
  return Object.keys(pins)
    .sort()
    .map((name) => name + "@" + String(pins[name]));
}

// Where a throwaway npmrc can be written. Never under the working directory:
// what goes in it is a credential that publishes under our name, and a file
// there is one artifact upload away from being published itself.
function tempBase() {
  const runner = process.env.RUNNER_TEMP || "";
  const base = path.resolve(runner !== "" && path.isAbsolute(runner) ? runner : os.tmpdir());
  const cwd = process.cwd();
  // Compared as the filesystem resolves them, since a symlink is exactly how a
  // directory that reads as being somewhere else turns out to be in the
  // working directory after all. A path that does not resolve stands for
  // itself, which leaves the comparison the textual one.
  if (containedIn(resolved(base), resolved(cwd))) {
    fail(
      "publish.js: the temporary directory " + base + " is inside " + cwd + ", the directory this publishes from.",
      "The npmrc holding the token goes there, and nothing holding a token is written where a release could pick it up.",
    );
  }
  return base;
}

function resolved(dir) {
  try {
    return fs.realpathSync(dir);
  } catch {
    return dir;
  }
}

function containedIn(dir, parent) {
  return dir === parent || dir.startsWith(parent + path.sep);
}

function configureAuth() {
  const token = process.env.NPM_TOKEN || "";
  if (token === "") {
    say("No NPM_TOKEN is set, so npm authenticates the publish against the registry itself.");
    assertNpmFloor();
    return;
  }
  const base = tempBase();
  let dir;
  try {
    dir = fs.mkdtempSync(path.join(base, "kamakiri-npm-"));
  } catch (err) {
    fail(
      "publish.js: no directory could be made under " + base + " (" + err.message + ").",
      "It is where the npmrc holding the token goes, and this publishes nothing without somewhere to put it.",
    );
  }
  // Removed on the way out of every exit this takes itself, refusals included,
  // which is why it is an exit hook and not a finally block the refusals below
  // jump straight over. The one end it does not cover is a signal: node ends
  // the process on SIGINT or SIGTERM without running the hook, so an
  // interrupted run leaves this directory behind, under a base outside the
  // working directory. A handler for those two is not the answer and must not
  // be added. Everything from here to the last publish is synchronous, so node
  // reaches no turn of its event loop to call one on until the run is over:
  // registering one would leave the cleanup exactly where it is, and stop
  // Ctrl-C from ending the run at all.
  process.on("exit", () => {
    try {
      fs.rmSync(dir, { recursive: true, force: true });
    } catch {
      // Nothing useful is left to do about it at this point.
    }
  });
  const npmrc = path.join(dir, ".npmrc");
  try {
    // Written at the mode it has to keep and then held there: the umask
    // narrows what a write asks for and leaves a chmod alone, and the mode of
    // this file is the whole reason it is not simply an environment variable.
    fs.writeFileSync(npmrc, AUTH_LINE + token + "\n", { mode: 0o600 });
    fs.chmodSync(npmrc, 0o600);
  } catch (err) {
    fail(
      "publish.js: the npmrc holding the token could not be written to " + npmrc + " (" + err.message + ").",
      "It is what authenticates the publish, and this publishes nothing without it.",
    );
  }
  process.env.NPM_CONFIG_USERCONFIG = npmrc;
  // Gone from the environment now that it is on disk, where only this user can
  // read it. npm publish runs the lifecycle scripts of the tree it publishes,
  // and that tree is one this run was handed rather than one it built, so a
  // credential left in the environment is one handed to scripts nobody here
  // wrote.
  delete process.env.NPM_TOKEN;
  say("NPM_TOKEN is set, so the publish authenticates through an npmrc under " + base + ".");
}

// How an npm run ended, for the refusals that report it. An npm killed by a
// signal has no exit status of its own, so the signal is what there is to name
// and "exited null" would be the only thing a reader had to go on.
function howItEnded(result) {
  return result.signal ? "was killed by " + result.signal : "exited " + result.status;
}

// Asserted rather than assumed, and only where it matters. The runner's Node
// makes this likely to pass and not certain, and an npm below the floor would
// otherwise fail at the publish itself with a message about credentials.
function assertNpmFloor() {
  const result = spawnSync("npm", ["--version"], { encoding: "utf8" });
  if (result.error) {
    fail(
      "publish.js: npm could not be run (" + result.error.message + ").",
      "It is what publishes, and this asks it its version first because the tokenless publish has a floor of " + NPM_FLOOR + ".",
    );
  }
  const seen = (result.stdout || "").trim();
  const parts = /^(\d+)\.(\d+)\.(\d+)/.exec(seen);
  if (result.status !== 0 || parts === null) {
    fail(
      "publish.js: npm --version " + howItEnded(result) + " and said " + (seen === "" ? "nothing" : seen) + ".",
      "With no version to read there is nothing to hold to the floor of " + NPM_FLOOR + " a tokenless publish needs.",
    );
  }
  const floor = NPM_FLOOR.split(".").map(Number);
  const found = [Number(parts[1]), Number(parts[2]), Number(parts[3])];
  for (let i = 0; i < floor.length; i += 1) {
    if (found[i] > floor[i]) {
      break;
    }
    if (found[i] < floor[i]) {
      fail(
        "publish.js: npm " + seen + " is below the " + NPM_FLOOR + " a tokenless publish needs.",
        "With no NPM_TOKEN set the publish is authenticated by npm's own exchange with the registry, which older releases do not make.",
      );
    }
  }
  say("npm " + seen + " is at or above the " + NPM_FLOOR + " a tokenless publish needs.");
}

// What the registry says about one package at this version, as one of three
// answers, only two of which say anything about the package:
//
//   exit 0 with the version on stdout          published
//   a failure whose body carries an E404       missing
//   anything else                              unreadable, and summarised in a sentence
//
// Both 404s reach the second arm, the one for a package that exists at no such
// version and the one for a name the registry does not know, since they differ
// only in a summary string and mean the same thing here. The third covers exit
// 0 with an empty body too, which is a shape a real npm has not been seen to
// give. What is done about that third answer belongs to the caller, and the two
// callers here differ: wherever a decision to publish or to skip rests on it,
// the run stops (isPublished below), and wherever nothing is being decided, the
// wait, it is a reason to look again. The asymmetry is what fixes the first of
// those: an unreadable answer taken for "not published" republishes nothing,
// but taken for "published" it drops a package out of the release with nothing
// anywhere saying so.
function askRegistry(name, npmVersion) {
  const spec = name + "@" + npmVersion;
  // npm's view goes to the registry every time of its own accord, so
  // --prefer-online adds nothing observable here; it is passed so that what
  // decides a release does not rest on one command's internal default. What no
  // flag of npm's reaches is the cache in front of the registry, which serves a
  // package document for five minutes and can answer out of one written before
  // this run published. That is what the bound on the wait below is sized to
  // outlive; where even that is not enough, running this again is what absorbs
  // it.
  const result = spawnSync("npm", ["view", spec, "version", "--json", "--prefer-online"], {
    encoding: "utf8",
  });
  if (result.error) {
    fail(
      "publish.js: npm could not be run to ask about " + spec + " (" + result.error.message + ").",
      "Nothing is published without an answer about what is published already.",
    );
  }
  const stdout = (result.stdout || "").trim();
  let body;
  let parsed = true;
  try {
    body = JSON.parse(stdout);
  } catch {
    parsed = false;
  }

  if (result.status === 0 && parsed && namesVersion(body, npmVersion)) {
    return { state: "published" };
  }
  if (result.status !== 0 && parsed && errorCode(body) === "E404") {
    return { state: "missing" };
  }

  // Summarised rather than relayed: this stays out of the business of piping
  // npm's own output into the run log, since the same run holds a credential
  // and that output has not been read.
  let answered;
  if (stdout === "") {
    answered = "said nothing";
  } else if (!parsed) {
    answered = "its answer did not parse as JSON";
  } else if (errorCode(body) !== "") {
    const summary = typeof body.error.summary === "string" ? body.error.summary : "";
    answered = "reported " + errorCode(body) + (summary === "" ? "" : " (" + summary + ")");
  } else {
    answered = "its answer was neither the version nor a 404";
  }
  // Carried as one sentence without its full stop, since both callers put
  // something of their own behind it.
  return {
    state: "unreadable",
    said: "npm view " + spec + " " + howItEnded(result) + " and " + answered,
  };
}

// Whether the registry serves this package at this version, for the two places
// that go on to publish or to skip on the answer. There is no third thing to do
// with an unreadable answer here, so it ends the run.
function isPublished(name, npmVersion) {
  const answer = askRegistry(name, npmVersion);
  if (answer.state === "unreadable") {
    fail(
      "publish.js: " + answer.said + ".",
      "This stops rather than guessing at what it means: taken for an answer that " + name + " is published, it would",
      "leave " + name + " out of this release with nothing anywhere saying so.",
    );
  }
  return answer.state === "published";
}

// npm answers an exact version with a one-element array, and a bare string is
// taken the same way rather than refused on a shape.
function namesVersion(body, npmVersion) {
  if (typeof body === "string") {
    return body === npmVersion;
  }
  return Array.isArray(body) && body.indexOf(npmVersion) !== -1;
}

function errorCode(body) {
  if (body === null || typeof body !== "object" || Array.isArray(body)) {
    return "";
  }
  const error = body.error;
  if (error === null || typeof error !== "object" || typeof error.code !== "string") {
    return "";
  }
  return error.code;
}

// One package, published from its own tree. Answers whether this run is what
// put it there, which is what decides whether the registry has to be waited on
// for it: a conflict confirmed below was confirmed by the registry itself, so
// there is nothing left to wait for.
function publishPackage(pkg, npmVersion) {
  const result = spawnSync("npm", ["publish", "--provenance"], { cwd: pkg.dir, encoding: "utf8" });
  if (result.error) {
    fail(
      "publish.js: npm could not be run to publish " + pkg.name + " (" + result.error.message + ").",
      "Nothing after this package was published.",
    );
  }
  // npm's own output is relayed, unlike the answers above: a release is read
  // back out of its log afterwards, and this is the part of the run worth
  // having there. npm prints no token of its own.
  if (result.stdout) {
    process.stdout.write(result.stdout);
  }
  if (result.stderr) {
    process.stderr.write(result.stderr);
  }
  if (result.status === 0) {
    say("  " + pkg.name + " published.");
    return true;
  }

  const output = ((result.stdout || "") + (result.stderr || "")).toLowerCase();
  if (output.indexOf(CONFLICT_WORDING) === -1) {
    fail(
      "publish.js: npm publish " + howItEnded(result) + " for " + pkg.name + " at " + npmVersion + ".",
      "npm's own account of it is above. Nothing after this package was published.",
    );
  }
  // The race between the check above and this publish, closing: something else
  // published this version in between. It counts as success, and what makes
  // that safe is asking the registry rather than trusting the wording of a
  // failure, which is reported rather than observed here.
  if (!isPublished(pkg.name, npmVersion)) {
    fail(
      "publish.js: npm publish " + howItEnded(result) + " for " + pkg.name + ", saying the version is already published, and the registry does not serve it at " + npmVersion + ".",
      "Those two cannot both be true, so this stops rather than counting a package as published on the strength of a message.",
    );
  }
  say("  " + pkg.name + " was published while this ran, and the registry serves it at " + npmVersion + ".");
  return false;
}

// A publish the registry accepted is not yet a version it serves, so what this
// run published is waited on before the launcher goes out. Bounded, unlike the
// CLI's own waits: running out is recoverable by running this again, which
// converges because everything already published is skipped, and a wait with
// no end would hold a release job open on a registry that may never answer.
//
// This is the one place an answer that cannot be read is not the end of the
// run. Nothing is decided on it: the package stays pending, the loop looks
// again, and the bound still ends the wait if the registry never comes back.
// Stopping instead would throw away everything this run has already
// published over one bad answer from a registry the next look asks again,
// while looking again costs an interval. What the bound's refusal says keeps
// the two apart: it counts the answers that could not be read, so a wait
// that ran out on a failing registry reads differently from one that ran out
// on a version slow to appear.
function waitForRegistry(names, npmVersion, looks, intervalMs) {
  if (names.length === 0) {
    say("Every platform package was already at " + npmVersion + ", so there is nothing to wait for.");
    return;
  }
  say(
    "Waiting for the registry to serve " +
      (names.length === 1 ? "the package" : "the " + names.length + " packages") +
      " this run published.",
  );
  let pending = names;
  let unreadable = 0;
  let lastUnreadable = "";
  for (let look = 1; look <= looks; look += 1) {
    pending = pending.filter((name) => {
      const answer = askRegistry(name, npmVersion);
      if (answer.state === "unreadable") {
        unreadable += 1;
        lastUnreadable = answer.said;
        // Said as it happens as well as counted at the end, so a wait that
        // converged over a registry having a bad minute leaves that in the log
        // of a run nothing else says anything was wrong with.
        say("  " + answer.said + ", so this looks again.");
        return true;
      }
      return answer.state !== "published";
    });
    if (pending.length === 0) {
      say("  all of them answer at " + npmVersion + ", at look " + look + ".");
      return;
    }
    // Between looks and not after the last one, which nothing would follow.
    if (look < looks) {
      sleep(intervalMs);
    }
  }
  const lines = [
    "publish.js: the registry does not serve " + pending.join(", ") + " at " + npmVersion + ", after " + looks + " looks " + intervalMs + "ms apart.",
  ];
  if (unreadable > 0) {
    lines.push(
      (unreadable === 1 ? "One of the answers along the way could not be read" : unreadable + " of the answers along the way could not be read") +
        ", rather than saying the version is not there yet, so this may be a registry that is failing rather than one that is slow.",
      "The last of them: " + lastUnreadable + ".",
    );
  }
  lines.push(
    "The launcher pins every platform package at that version, so it is not published while one of them is missing:",
    "an install reaching it now would resolve a package the registry cannot hand out. Running this again picks up",
    "from here, since everything already published is skipped.",
  );
  fail(...lines);
}

// Synchronous, because everything else here is: the one place this waits is
// not worth turning the whole script asynchronous for.
function sleep(ms) {
  Atomics.wait(new Int32Array(new SharedArrayBuffer(4)), 0, 0, ms);
}

function main(argv) {
  const { tag, npmVersion, prerelease } = parseArgs(argv);

  // First, ahead of reading anything and ahead of any credential being
  // written: what this guard is for is that a prerelease never reaches the
  // registry, and the earlier it sits the fewer ways there are past it.
  if (prerelease) {
    say("Nothing is published for " + tag + ": the tag carries a prerelease suffix, and the channel publishes releases.");
    return;
  }

  const looks = pollSetting(POLL_LOOKS);
  const intervalMs = pollSetting(POLL_INTERVAL_MS);
  const { platforms, launcher } = readPackages(npmVersion);

  say("Publishing " + tag + " as npm version " + npmVersion + ", from " + OUT_DIR + ".");
  configureAuth();

  const waiting = [];
  for (const pkg of platforms) {
    if (isPublished(pkg.name, npmVersion)) {
      say("  " + pkg.name + " is already at " + npmVersion + ", so nothing is published for it.");
      continue;
    }
    if (publishPackage(pkg, npmVersion)) {
      waiting.push(pkg.name);
    }
  }

  waitForRegistry(waiting, npmVersion, looks, intervalMs);

  if (isPublished(launcher.name, npmVersion)) {
    say("  " + launcher.name + " is already at " + npmVersion + ", so nothing is published for it.");
  } else {
    publishPackage(launcher, npmVersion);
  }

  // Published rather than served: the six were waited for, and the launcher
  // behind them is a publish npm accepted and nothing here asked the registry
  // about again.
  say("All " + TREE_COUNT + " packages are published at " + npmVersion + ".");
}

main(process.argv.slice(2));
