# Installs the latest released kamakiri binary for Windows. It is what
# `irm https://get.kamakiri-labs.jp/install.ps1 | iex` runs.
#
# Two environment variables change what it does:
#
#   KAMAKIRI_INSTALL_DIR       where the binary is installed (default
#                              $env:USERPROFILE\.local\bin). Nothing here needs
#                              administrator rights, and the directory has to
#                              stay user-owned for `kamakiri upgrade` to replace
#                              the binary later.
#   KAMAKIRI_INSTALL_REPO_URL  the repository the release is resolved from
#                              (default https://github.com/kamakiri-labs/kamakiri).
#                              It is here so this script can be driven against a
#                              local server instead of GitHub.
#
# The whole body is one function, invoked at the end inside a try whose catch
# writes a refusal out as the lines it carries, on the error stream of whatever
# is running it. `iex` runs this text in the caller's own live session, so nothing here may end that session or leave
# anything behind in it: a refusal throws rather than exits, every preference it
# sets is assigned inside the function where it is local to it, and the exit that
# follows the write is fenced to a run whose text came from a file. A transfer
# that stopped partway needs no guard of its own, unlike install.sh's: `iex`
# parses the whole string before it runs any of it, so a truncated one is a parse
# error that runs nothing.
#
# Windows PowerShell 5.1 is what `powershell.exe` runs on a stock Windows 10 or
# 11, and so is what a pasted one-liner gets; nothing here uses anything newer.
# In particular the is-Windows test is $env:OS, which Windows itself sets for
# every process and no other platform does, so both PowerShells read it the same
# way. $IsWindows does not exist on 5.1 at all, so a branch written against it
# would be skipped on exactly the platform it is there for.

function Install-Kamakiri {
    # Stop, so a cmdlet that fails ends the run rather than carrying on with
    # nothing to work from. Every preference here is assigned inside the
    # function, which is what makes it local, so a session that ran this through
    # `iex` keeps its own. SilentlyContinue is not only tidiness: 5.1 draws a
    # progress bar for every Invoke-WebRequest, and rendering it slows a download
    # to a fraction of the speed the link would give. The last two are set
    # because a caller is free to have either of them at Stop, where Write-Warning
    # and Write-Host raise instead of printing: the first would turn a completed
    # install into a failure at the marker warning, and the second would end the
    # run at its first milestone line.
    $ErrorActionPreference = 'Stop'
    $ProgressPreference = 'SilentlyContinue'
    $WarningPreference = 'Continue'
    $InformationPreference = 'Continue'
    # Strict mode is set here for the locality above and turned off for a reason
    # of its own: under a caller's own Set-StrictMode, reading a property the
    # object does not carry raises instead of answering nothing, and reading a
    # property that may not be there is how a failure carrying no response is
    # told from one that carries one.
    Set-StrictMode -Off


    # Windows PowerShell 5.1 reaches the network through .NET Framework, where
    # one process-wide setting decides which TLS versions are offered, and on a
    # machine whose framework predates 4.7 that setting names TLS 1.0 alone
    # while the release host requires 1.2 or better. Without this the download
    # fails as a secure channel that could not be created, which says nothing
    # about what is wrong. The mutation widens what the session will negotiate
    # and never narrows it, and it is fenced to the hosts that need it:
    # PowerShell 7 chooses per connection, so there it would be inert, and
    # `iex` runs this in a session that outlives it.
    if ($PSVersionTable.PSVersion.Major -lt 6) {
        [Net.ServicePointManager]::SecurityProtocol =
            [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12
    }

    $repo = $env:KAMAKIRI_INSTALL_REPO_URL
    if (-not $repo) { $repo = 'https://github.com/kamakiri-labs/kamakiri' }
    $releases = "$repo/releases"

    # 1. What to install for. PROCESSOR_ARCHITEW6432 is defined only in a 32-bit
    # process on 64-bit Windows, where PROCESSOR_ARCHITECTURE reports x86 and
    # this one carries the true architecture. A release is built for the two
    # below and for nothing else, so anything else is refused naming what was
    # read; a host defining neither variable reads as unset rather than as a
    # line with a hole in it.
    $rawArch = $env:PROCESSOR_ARCHITEW6432
    if (-not $rawArch) { $rawArch = $env:PROCESSOR_ARCHITECTURE }
    if (-not $rawArch) { $rawArch = '(unset)' }
    $arch = ''
    switch ($rawArch) {
        'AMD64' { $arch = 'amd64' }
        'ARM64' { $arch = 'arm64' }
    }
    if (-not $arch) {
        throw "kamakiri publishes no release for Windows $rawArch.`n" +
            "The Windows releases are for AMD64 and ARM64.`n" +
            "Check $releases"
    }
    $asset = "kamakiri-windows-$arch.exe"

    # 2. Which release is the latest, read off the redirect rather than by
    # following it. The tag is the one string the server chooses that this
    # script then puts into a URL it downloads from, so the answer is held to
    # the request it answers: same scheme, same host and port, this
    # repository's releases path, and one segment after it that is a version and
    # nothing else.
    Write-Host 'Checking for the latest release.'
    $latestUrl = "$releases/latest"
    $redirect = ''
    # The probe goes through .NET's own request object rather than
    # Invoke-WebRequest. With redirects disabled, Windows PowerShell 5.1's
    # cmdlet throws a redirection-count error of its own that carries no
    # response, so the Location header is unreachable through it and every
    # stock Windows run would land on the could-not-determine line. The request
    # object hands a 3xx back as an ordinary response on both PowerShells, and
    # its header lookup is case-insensitive, which the lowercase `location`
    # GitHub sends needs. The probe is a few hundred bytes of headers, so it
    # is bounded end to end: one still open after ten seconds is not going to
    # answer.
    $response = $null
    try {
        $request = [System.Net.WebRequest]::Create($latestUrl)
        $request.AllowAutoRedirect = $false
        $request.Timeout = 10000
        $response = $request.GetResponse()
    }
    catch {
        # A repository with no release answers 404 rather than a redirect,
        # which arrives as an exception carrying that response; what the
        # transport says about it is not this script's answer to give: the one
        # line below is. A failure carrying no response reads as no redirect.
        try { $response = $_.Exception.Response } catch { $response = $null }
    }
    if ($null -ne $response) {
        try { $redirect = [string]$response.GetResponseHeader('Location') } catch { $redirect = '' }
        $response.Close()
    }

    $tag = ''
    if ($redirect) {
        try {
            # A Location is allowed to be relative and 5.1 hands the header up
            # as it arrived, so it is resolved against the request before it is
            # held to it. Comparing the resolved absolute URL against a literal
            # prefix holds scheme, host, port and path in one test.
            $base = New-Object System.Uri -ArgumentList $latestUrl
            $resolved = New-Object System.Uri -ArgumentList $base, $redirect
            $prefix = "$releases/tag/"
            if ($resolved.AbsoluteUri.StartsWith($prefix, [System.StringComparison]::Ordinal)) {
                $tag = $resolved.AbsoluteUri.Substring($prefix.Length)
            }
        }
        catch {
            $tag = ''
        }
    }
    # Bounded ahead of the test below, which is where the bound is worth having:
    # the value is whatever answered the request, a response header can carry
    # far more than a version, and a tag that gets through is printed to the
    # terminal and goes into the URLs the download comes from. A published
    # version runs to well under ten characters, so this is generous by an order
    # of magnitude.
    if ($tag.Length -gt 64) { $tag = '' }
    # \A and \z rather than ^ and $, which in .NET also match around a trailing
    # line feed, and a case-sensitive match, so what gets through is a version
    # in full and nothing else.
    if ($tag -cnotmatch '\Av[0-9]+\.[0-9]+\.[0-9]+\z') { $tag = '' }
    if (-not $tag) {
        # A repository with no release lands here (every real run did until the
        # first one was cut), so it is one line saying where to look rather
        # than a diagnostic.
        throw "Could not determine the latest release. Check $releases"
    }

    # 3. Where it goes, made absolute: the marker written at the end records
    # this path, and the CLI compares it as text against the location of the
    # running binary, which it has made absolute and resolved through its links
    # first. The spelling the user gave is kept beside it for the PATH
    # comparison at the end, which is against the name their environment
    # carries.
    $dirGiven = $env:KAMAKIRI_INSTALL_DIR
    if (-not $dirGiven) { $dirGiven = Join-Path $env:USERPROFILE '.local\bin' }
    try {
        $null = New-Item -ItemType Directory -Path $dirGiven -Force
    }
    catch {
        throw "Could not create the install directory $dirGiven.`n" +
            "Set KAMAKIRI_INSTALL_DIR to a directory you can write to and run this again."
    }
    try {
        $dir = (Resolve-Path -LiteralPath $dirGiven).ProviderPath
        # Only the last component is followed: .NET Framework, which is what 5.1
        # runs on, has no call that resolves a whole path through its links, and
        # a relative link target is left alone rather than guessed at.
        $linkTarget = @((Get-Item -LiteralPath $dir -Force).Target)[0]
        if ($linkTarget -and [System.IO.Path]::IsPathRooted($linkTarget)) {
            $dir = $linkTarget
        }
    }
    catch {
        throw "Could not read the install directory $dirGiven."
    }

    # 4. Both files are staged in the install directory, which leaves the final
    # step a move inside one filesystem rather than a copy that an interruption
    # could truncate over a working binary. Both staged names are dot-prefixed:
    # an un-prefixed checksums.txt would land under its own name in a directory
    # on the user's PATH and outlive the run.
    $stagedAsset = Join-Path $dir ".tmp-$asset"
    $stagedSums = Join-Path $dir '.tmp-checksums.txt'
    # Both names are cleared before either is written to. Either one is
    # predictable and this directory need not be one the user has to themselves,
    # so a link planted at one would otherwise take the download wherever it
    # points. What cannot be cleared is a directory sitting at one of the two
    # names, and that is refused here rather than met halfway through a download.
    foreach ($staged in @($stagedAsset, $stagedSums)) {
        try {
            [System.IO.File]::Delete($staged)
        }
        catch {
            throw "The files this install stages in $dir could not be cleared.`n" +
                "Move aside whatever is at $stagedAsset or $stagedSums, or set KAMAKIRI_INSTALL_DIR to another directory, and run this again."
        }
    }
    # Whether the directory can be written to is asked before anything claims to
    # be fetching, by writing the empty file the download replaces. A directory
    # that is there and cannot be written to is a local matter, and the first
    # thing to fail without this would be the download, which would name the
    # release host for it.
    try {
        [System.IO.File]::WriteAllBytes($stagedAsset, [byte[]]@())
    }
    catch {
        throw "The install directory $dir cannot be written to.`n" +
            "Set KAMAKIRI_INSTALL_DIR to a directory you can write to and run this again."
    }

    $target = Join-Path $dir 'kamakiri.exe'
    try {
        Write-Host "Downloading $asset into $dir."
        # -OutFile streams the response to disk. A body taken as the cmdlet's
        # output instead goes through the string layer, which decodes it and
        # corrupts an executable. The asset transfer is left with no deadline of
        # its own, so a slow but live download over a thin link finishes rather
        # than being cut off by a clock, and a host that never answers is bounded
        # by the platform's own connect timeout rather than by one set here.
        # Neither transfer carries a size cap, there being none to set on these
        # requests; what the install rests on in every case is the checksum.
        try {
            Invoke-WebRequest -Uri "$releases/download/$tag/$asset" `
                -OutFile $stagedAsset -UseBasicParsing
        }
        catch {
            throw "Could not download $asset from $releases/download/$tag/`n" +
                $_.Exception.Message
        }
        try {
            Invoke-WebRequest -Uri "$releases/download/$tag/checksums.txt" `
                -OutFile $stagedSums -UseBasicParsing -TimeoutSec 10
        }
        catch {
            throw "Could not download checksums.txt from $releases/download/$tag/`n" +
                $_.Exception.Message
        }

        # 5. The published sum is the first field of the line whose second field
        # is this asset in full, so one asset's line can never be read as
        # another's. It is held to 64 hex characters before it is used or
        # echoed: the mismatch line below prints it back to the user, and this
        # file came off the network. A line failing that hold does not end the
        # scan, so a usable line further down the file still wins.
        $published = ''
        $named = $false
        foreach ($line in [System.IO.File]::ReadAllLines($stagedSums)) {
            $split = $line.IndexOf('  ')
            if ($split -lt 0) { continue }
            if ($line.Substring($split + 2) -cne $asset) { continue }
            $named = $true
            $field = $line.Substring(0, $split)
            if ($field -match '\A[0-9a-fA-F]{64}\z') {
                $published = $field.ToLowerInvariant()
                break
            }
        }
        if (-not $published) {
            if ($named) {
                throw "The line for $asset in checksums.txt does not carry a checksum (64 hexadecimal characters).`n" +
                    "Nothing was installed."
            }
            throw "No line in checksums.txt names $asset. Nothing was installed."
        }
        # Get-FileHash answers in uppercase where checksums.txt publishes
        # lowercase, and two spellings of a hex digest are one digest. Both are
        # folded here so the comparison below can be the exact one.
        $downloaded = (Get-FileHash -LiteralPath $stagedAsset -Algorithm SHA256).Hash.ToLowerInvariant()
        if ($published -cne $downloaded) {
            throw "$asset does not match its checksum. checksums.txt lists $published, the download is $downloaded.`n" +
                "Nothing was installed."
        }
        Write-Host 'Checksum verified against checksums.txt.'

        # 6. Overwriting whatever is at the install path is the point: a re-run
        # is how this channel updates an existing install. A directory there is
        # the one thing that would not be overwritten but moved into, and
        # Move-Item reports success for that, so it is refused here instead.
        if (Test-Path -LiteralPath $target -PathType Container) {
            throw "There is a directory at $target, so the binary cannot go there.`n" +
                "Move it aside, or set KAMAKIRI_INSTALL_DIR to another directory, and run this again."
        }
        try {
            Move-Item -LiteralPath $stagedAsset -Destination $target -Force
        }
        catch {
            throw "Could not install $asset as $target."
        }
    }
    finally {
        # Nothing staged outlives the run: the asset is gone by now on the path
        # that installed it, and both names go on every path that did not. They
        # are named rather than swept with a .tmp-* wildcard, because
        # `kamakiri upgrade` stages under the same prefix in the same directory
        # and nothing here owns what it left behind. There is no equivalent of
        # install.sh's signal traps, and whether a finally block runs at all on
        # Ctrl-C differs between 5.1 and PowerShell 7, so an interrupt can leave
        # a .tmp- file: it is inert, never run and never installed.
        Remove-Item -LiteralPath $stagedAsset, $stagedSums -Force -ErrorAction SilentlyContinue
    }

    # 7. The marker records the channel that installed this binary, so
    # `kamakiri upgrade` can tell a scripted install from one a package manager
    # owns. It is written through a temp file in the same directory and moved
    # into place. A marker that cannot be written is a warning and not a
    # failure: the install itself succeeded, and a CLI that finds no marker
    # treats the binary as one it may replace itself, which is what this one
    # records anyway.
    # The config directory is the CLI's own, which it resolves from
    # XDG_CONFIG_HOME or the user profile with no Windows special case, so the
    # marker goes under the profile's .config and never under AppData; one
    # written anywhere else is one the CLI never reads. It is created with the
    # permissions it inherits, which is also what the CLI does: the mode the CLI
    # asks for is ignored on Windows.
    # Named before the paths are built, so the warning below has something to
    # point at even when building them is what failed, and built inside the try
    # rather than above it: a profile with no USERPROFILE set is enough for the
    # join to fail, and nothing past the move may turn a completed install into a
    # failure.
    $markerPath = 'the kamakiri config directory'
    $markerTmp = ''
    try {
        $configHome = $env:XDG_CONFIG_HOME
        if (-not $configHome) { $configHome = Join-Path $env:USERPROFILE '.config' }
        $configDir = Join-Path $configHome 'kamakiri'
        $markerPath = Join-Path $configDir 'install.json'
        $null = New-Item -ItemType Directory -Path $configDir -Force
        # Made absolute once it exists, the way the install directory is, and the
        # two paths built again from it: the write below goes through .NET, whose
        # working directory is not the session's, so under a relative
        # XDG_CONFIG_HOME the two would disagree about where the file is.
        $configDir = (Resolve-Path -LiteralPath $configDir).ProviderPath
        $markerPath = Join-Path $configDir 'install.json'
        $markerTmp = Join-Path $configDir '.tmp-install.json'
        # An ordered map, because a plain hashtable has no key order on 5.1 and
        # the marker's bytes would then differ from run to run. Serialized
        # rather than interpolated into a string, since a Windows path is full
        # of backslashes and a JSON string has to escape every one of them.
        $marker = [ordered]@{ version = 1; method = 'script'; path = $target }
        # UTF-8 with no byte order mark, written through .NET rather than with
        # Out-File or Set-Content: on 5.1 those write UTF-16, a BOM, and the
        # ANSI code page respectively, and the CLI hands the raw bytes to a JSON
        # parser, so the first two are unreadable to it and the third turns a
        # non-ASCII profile name into replacement characters.
        [System.IO.File]::WriteAllText($markerTmp, (ConvertTo-Json -InputObject $marker -Compress),
            (New-Object System.Text.UTF8Encoding($false)))
        # Move-Item rather than [System.IO.File]::Move, whose overwriting
        # overload does not exist on .NET Framework.
        Move-Item -LiteralPath $markerTmp -Destination $markerPath -Force
    }
    catch {
        if ($markerTmp) { Remove-Item -LiteralPath $markerTmp -Force -ErrorAction SilentlyContinue }
        Write-Warning "Installed, but the record of this install could not be written to $markerPath."
    }

    # 8. What happened, and the one thing left to do when the directory the
    # binary went into is not one the shell looks in. Windows keeps the user's
    # own PATH in a single place, so this edits it instead of printing a line to
    # paste, which is where the two installers deliberately differ.
    Write-Host "Installed $tag at $target."
    if ($env:OS -eq 'Windows_NT') {
        # Asked of the stored value and of the session's own PATH separately
        # below, so the two questions the block answers stay apart. Both
        # spellings of the directory count, the one the user gave and the
        # absolute one, and an entry is read both as written and expanded, since
        # a user PATH normally carries %VAR% entries. The comparison is
        # case-insensitive, the way Windows itself reads a path, so a re-run
        # matches what an earlier one wrote.
        function Test-PathCarriesDir($value, $wanted) {
            foreach ($entry in ($value -split ';')) {
                if (-not $entry) { continue }
                foreach ($spelling in @($entry, [System.Environment]::ExpandEnvironmentVariables($entry))) {
                    if ($wanted -contains $spelling.TrimEnd('\', '/')) { return $true }
                }
            }
            return $false
        }
        try {
            $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
            try {
                $raw = ''
                $kind = [Microsoft.Win32.RegistryValueKind]::String
                if (@($key.GetValueNames()) -contains 'Path') {
                    # Read raw and written back under the kind it was found as.
                    # The obvious round-trip through
                    # [Environment]::GetEnvironmentVariable expands every %VAR%
                    # an entry holds and would bake today's values in
                    # permanently.
                    $raw = [string]$key.GetValue('Path', '',
                        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                    $kind = $key.GetValueKind('Path')
                }
                # Two separate questions, because the answers come apart: a
                # console opened before an earlier run carries the PATH it
                # started with, so the stored value can already name the
                # directory while the session the user is standing in does not.
                # Whichever of the two is missing it gets it, and only that one.
                $wanted = @($dir.TrimEnd('\', '/'), $dirGiven.TrimEnd('\', '/'))
                $inStored = Test-PathCarriesDir $raw $wanted
                $inSession = Test-PathCarriesDir "$env:Path" $wanted
                if (-not $inStored) {
                    # A user PATH commonly ends in a separator, and appending
                    # after one would leave an empty entry behind.
                    $updated = $raw.TrimEnd(';')
                    if ($updated) { $updated = "$updated;$dir" } else { $updated = $dir }
                    $key.SetValue('Path', $updated, $kind)
                    # A registry write is all that call is, where
                    # [Environment]::SetEnvironmentVariable also broadcasts a
                    # settings-changed message to every window. Explorer keeps a
                    # copy of the environment and hands it to everything started
                    # from it, a terminal opened from the Start menu included, and
                    # that copy is refreshed by the broadcast; without one it
                    # would carry the old PATH until the user signed out. So this
                    # call is here for the broadcast alone, which it makes
                    # whatever it was asked to write: a null value for a name
                    # nothing defines writes nothing and removes nothing.
                    [Environment]::SetEnvironmentVariable('KAMAKIRI_PATH_REFRESH', $null, 'User')
                }
                if (-not $inSession) {
                    # The session this ran in is where the user is standing, and
                    # `iex` runs this text in a live one, so it gets the directory
                    # directly rather than a line to paste.
                    $session = "$env:Path".TrimEnd(';')
                    if ($session) { $env:Path = "$session;$dir" } else { $env:Path = $dir }
                }
                # Said once, and only about what actually changed: a stored value
                # that already named the directory is never reported as edited.
                if (-not $inStored) {
                    Write-Host ''
                    Write-Host "$dir was not on your PATH, so it has been added to your user environment."
                    Write-Host "This session has it now, and new terminals pick it up, so 'kamakiri version' will work."
                }
                elseif (-not $inSession) {
                    Write-Host ''
                    Write-Host "$dir is in your user environment already, and this session has it now, so 'kamakiri version' will work."
                }
            }
            finally {
                # Close, not Dispose: .NET Framework, which is what 5.1 runs on,
                # implements IDisposable explicitly on a registry key, and
                # PowerShell surfaces no member declared that way, so Dispose
                # there is a method that cannot be found. Close is public on both.
                if ($key) { $key.Close() }
            }
        }
        catch {
            # Same rule as the marker: the install has happened, so nothing
            # below the move may turn the run into a failure.
            Write-Warning "Installed, but $dir could not be added to your user environment. Add it to your PATH there to run kamakiri by name."
        }
    }
}

# The refusal a throw carries is the whole of what the user has to act on, so it
# is caught here and written out as the lines it carries. An uncaught one is
# rendered by the host instead, which prints the exception's own name around the
# message, the source line that threw with a run of squiggles marking the column
# in it, and on Windows PowerShell a category and error-id block under all of
# that; none of that is anything the user can act on. The write is not
# Write-Error, which raises an error record the host renders with furniture of
# its own, and not Write-Host or Write-Information, neither of which reaches
# stderr at all. It is asked of the host itself first, because not every host has
# a console behind it: an editor's shell and a remote session have none, and
# there a write to the console's error stream goes nowhere while the milestone
# lines still appear, so the user would read the last milestone and then nothing
# at all, which is worse than the rendering this catch removes. The console's
# error stream is kept behind that for a host carrying no user interface to ask,
# where the ask itself would fail inside this catch and hand the refusal back to
# the rendering. Which of the two writes is settled by a test rather than by
# catching the first one failing: inside a catch of its own the error at hand
# would be the failed write and not the refusal, and holding the refusal aside
# for it would mean naming a variable, which under `iex` is a name taken in the
# caller's live session.
#
# $PSCommandPath is what decides whether the run may exit, because it holds the
# file the running text came from and is empty for text that arrived as text,
# which is every `iex` shape. So the exit is reachable when this ran as a file of
# its own, where whatever started it reads the exit code to tell a refusal from
# an install, and unreachable in a session the user is standing in, which an exit
# would end. Nothing under $MyInvocation can stand in for it: under `iex` that
# reports the caller's own frame, so text a script piped into `iex` reads there
# as a file and the exit would take that session with it. The consequence,
# accepted rather than worked around: a refusal under `iex` says what is wrong
# and leaves the exit code alone, there being no way to raise one from inside a
# live session without ending it or writing the error record this catch exists to
# avoid.
try {
    Install-Kamakiri
}
catch {
    if ($Host -and $Host.UI) { $Host.UI.WriteErrorLine($_.Exception.Message) }
    else { [Console]::Error.WriteLine($_.Exception.Message) }
    if ($PSCommandPath) { exit 1 }
}
