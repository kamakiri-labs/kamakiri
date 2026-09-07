# Removes what `irm https://get.kamakiri-labs.jp/install.ps1 | iex` put on this
# machine, and nothing else. It is what
# `irm https://get.kamakiri-labs.jp/uninstall.ps1 | iex` runs.
#
# Two environment variables change where it looks, and they are the two the
# installer and the CLI read:
#
#   KAMAKIRI_INSTALL_DIR  the directory the binary was installed in (default
#                         $env:USERPROFILE\.local\bin). A binary installed under
#                         another value is found by running this under the same
#                         one.
#   XDG_CONFIG_HOME       where the CLI keeps its config directory (default
#                         $env:USERPROFILE\.config).
#
# Three things go: the binary, with the copy `kamakiri upgrade` sets aside as
# kamakiri.exe.old when it replaces one; the CLI's config directory, which holds
# the record of the install, the saved API key, the update-nudge state and the
# language setting; and, on Windows, the install directory's entry in the user
# PATH, which the installer added. Nothing else in the install directory is
# touched: not another tool, and not a file an upgrade staged there. The saved
# key is a copy, so removing it revokes nothing; the key stays valid on the
# account until it is revoked there.
#
# Same shape as install.ps1, for the reasons it gives: the whole body is one
# function, invoked at the end inside a try whose catch writes a refusal out as
# the lines it carries. `iex` runs this text in the caller's own live session, so
# nothing here may end that session or leave anything behind in it: a refusal
# throws rather than exits, every preference it sets is assigned inside the
# function where it is local to it, and the exit that follows the write is
# fenced to a run whose text came from a file.
#
# Windows PowerShell 5.1 is the floor, as it is for install.ps1: the is-Windows
# test is $env:OS, which Windows sets for every process and no other platform
# does, and nothing here uses anything 5.1 does not have.

function Uninstall-Kamakiri {
    # Stop, so a cmdlet that fails is a refusal rather than a line that carried
    # on. The last two are set because a caller is free to have either of them
    # at Stop, where Write-Warning and Write-Host raise instead of printing: the
    # first would turn a completed uninstall into a failure at the PATH warning,
    # and the second would end the run at its first milestone line. Strict mode
    # is turned off for the same reason: under a caller's own Set-StrictMode a
    # variable that was never assigned raises when it is read, and the finally
    # below reads the registry key whether or not opening it got as far as
    # assigning one.
    $ErrorActionPreference = 'Stop'
    $WarningPreference = 'Continue'
    $InformationPreference = 'Continue'
    Set-StrictMode -Off

    $removed = 0
    # A refusal does not stop the run: what can be removed is, and the refusals
    # are thrown together at the end, so the exit says what is left after
    # everything else has gone.
    $refusals = @()

    # 1. The binary, and the copy an upgrade set aside. The directory is made
    # absolute and its last component followed through a link when it is there,
    # the way the installer resolved it, so the line naming what was removed
    # spells the path the way the install line did, and so the PATH edit below
    # matches the entry the installer wrote, which was that resolved spelling.
    # The spelling the user gave is kept beside it for the same edit, which also
    # matches the name their environment carries.
    $dirGiven = $env:KAMAKIRI_INSTALL_DIR
    if (-not $dirGiven) { $dirGiven = Join-Path $env:USERPROFILE '.local\bin' }
    $dir = $dirGiven
    if (Test-Path -LiteralPath $dirGiven -PathType Container) {
        try {
            $dir = (Resolve-Path -LiteralPath $dirGiven).ProviderPath
            $linkTarget = @((Get-Item -LiteralPath $dir -Force).Target)[0]
            if ($linkTarget -and [System.IO.Path]::IsPathRooted($linkTarget)) {
                $dir = $linkTarget
            }
        }
        catch {
            $dir = $dirGiven
        }
    }
    $binary = Join-Path $dir 'kamakiri.exe'
    $aside = "$binary.old"
    foreach ($file in @($binary, $aside)) {
        # A directory at the binary's name is not something this channel
        # installed, so it is refused rather than removed.
        if (Test-Path -LiteralPath $file -PathType Container) {
            $refusals += "There is a directory at $file, so it was not removed.`n" +
                "Move it aside, or set KAMAKIRI_INSTALL_DIR to the directory kamakiri was installed in, and run this again."
            continue
        }
        if (-not (Test-Path -LiteralPath $file)) { continue }
        try {
            Remove-Item -LiteralPath $file -Force
        }
        catch {
            # What the platform said is kept under the line: on Windows a binary
            # that is running cannot be removed, and that is the reason the
            # user can act on.
            $refusals += "Could not remove $file.`n" + $_.Exception.Message
            continue
        }
        if ($file -eq $aside) {
            Write-Host "Removed $file, the copy an upgrade had set aside."
        }
        else {
            Write-Host "Removed $file."
        }
        $removed++
    }

    # 2. The config directory, whole: everything in it is the CLI's. It is
    # resolved from XDG_CONFIG_HOME or the user profile with no Windows special
    # case, which is where the installer put the marker and where the CLI keeps
    # the rest, so nothing under AppData is looked at. A junction or a link
    # sitting at the directory's name is removed as the one entry it is rather
    # than recursed into: Windows PowerShell 5.1's Remove-Item -Recurse follows
    # a junction and empties whatever it points at, and the CLI never creates
    # one, so what is at such a name is not the CLI's to empty.
    $configHome = $env:XDG_CONFIG_HOME
    if (-not $configHome) { $configHome = Join-Path $env:USERPROFILE '.config' }
    $configDir = Join-Path $configHome 'kamakiri'
    if (Test-Path -LiteralPath $configDir) {
        $hadKey = Test-Path -LiteralPath (Join-Path $configDir 'credentials.json') -PathType Leaf
        try {
            $item = Get-Item -LiteralPath $configDir -Force
            if ($item.Attributes -band [System.IO.FileAttributes]::ReparsePoint) {
                [System.IO.Directory]::Delete($configDir)
            }
            else {
                Remove-Item -LiteralPath $configDir -Recurse -Force
            }
            Write-Host "Removed the config directory $configDir."
            # Said only when there was a key to say it about. A user who reads
            # that their key was removed may take the key itself for gone; it is
            # not.
            if ($hadKey) {
                Write-Host 'It held your saved API key. Only this copy is gone: the key itself stays valid on your account, and nothing here revokes it.'
            }
            $removed++
        }
        catch {
            $refusals += "Could not remove everything under $configDir.`n" +
                $_.Exception.Message + "`n" +
                'Remove what is left of it by hand.'
        }
    }

    # 3. The PATH entry, on Windows, where the installer added one. It is
    # matched the way the installer matched it: both spellings of the directory,
    # each entry read as written and expanded, case-insensitive, trailing
    # separators ignored. The stored value is read raw and written back under
    # the kind it was found as, so the %VAR% entries a user PATH carries are not
    # baked into today's values; the session's own PATH is edited apart from it,
    # since the two come apart the way they did for the install. A PATH that
    # cannot be edited is a warning over a completed uninstall rather than a
    # refusal: an entry naming a directory with no binary in it does nothing,
    # and a non-zero exit here would read as a run that removed nothing.
    if ($env:OS -eq 'Windows_NT') {
        # The value with every entry naming the directory dropped, or nothing at
        # all when no entry did, which is what keeps a value nothing matched
        # from being rewritten.
        function Get-PathValueWithoutDir($value, $wanted) {
            $kept = @()
            $dropped = $false
            foreach ($entry in ($value -split ';')) {
                if (-not $entry) { continue }
                $matched = $false
                foreach ($spelling in @($entry, [System.Environment]::ExpandEnvironmentVariables($entry))) {
                    if ($wanted -contains $spelling.TrimEnd('\', '/')) { $matched = $true }
                }
                if ($matched) { $dropped = $true } else { $kept += $entry }
            }
            if ($dropped) { return ($kept -join ';') }
            return $null
        }
        try {
            $wanted = @($dir.TrimEnd('\', '/'), $dirGiven.TrimEnd('\', '/'))
            $key = [Microsoft.Win32.Registry]::CurrentUser.OpenSubKey('Environment', $true)
            try {
                if (@($key.GetValueNames()) -contains 'Path') {
                    $raw = [string]$key.GetValue('Path', '',
                        [Microsoft.Win32.RegistryValueOptions]::DoNotExpandEnvironmentNames)
                    $kind = $key.GetValueKind('Path')
                    $updated = Get-PathValueWithoutDir $raw $wanted
                    if ($null -ne $updated) {
                        $key.SetValue('Path', $updated, $kind)
                        # The registry write is all SetValue is; this call is
                        # here for the settings-changed broadcast it makes,
                        # which is what refreshes the copy of the environment
                        # Explorer hands to every terminal opened from it. A
                        # null value for a name nothing defines writes nothing
                        # and removes nothing.
                        [Environment]::SetEnvironmentVariable('KAMAKIRI_PATH_REFRESH', $null, 'User')
                        Write-Host "Removed $dir from your user PATH."
                        $removed++
                    }
                }
                $session = Get-PathValueWithoutDir "$env:Path" $wanted
                if ($null -ne $session) { $env:Path = $session }
            }
            finally {
                # Close, not Dispose: .NET Framework, which is what 5.1 runs on,
                # implements IDisposable explicitly on a registry key, and
                # PowerShell surfaces no member declared that way. Close is
                # public on both.
                if ($key) { $key.Close() }
            }
        }
        catch {
            Write-Warning "The user PATH could not be edited. If $dir is in it, remove it there by hand."
        }
    }

    # 4. What happened. Running this on a machine with nothing to remove is not
    # an error: it is the state this script exists to reach.
    if ($refusals.Count -gt 0) {
        throw ($refusals -join "`n")
    }
    if ($removed -eq 0) {
        Write-Host "Nothing to remove: there is no kamakiri at $binary and no config directory at $configDir."
        return
    }
    Write-Host 'kamakiri is uninstalled.'
}

# The refusal a throw carries is the whole of what the user has to act on, so it
# is caught here and written out as the lines it carries rather than rendered by
# the host with its own furniture around it. The host is asked first because not
# every host has a console behind it; the console's error stream is kept behind
# that for a host carrying no user interface to ask. The exit is fenced to a run
# whose text came from a file, which $PSCommandPath holds and which is empty for
# text that arrived through `iex`, where an exit would take the caller's session
# with it. The full reasoning behind each of those choices is at the same place
# in install.ps1, whose catch this one is a copy of.
try {
    Uninstall-Kamakiri
}
catch {
    if ($Host -and $Host.UI) { $Host.UI.WriteErrorLine($_.Exception.Message) }
    else { [Console]::Error.WriteLine($_.Exception.Message) }
    if ($PSCommandPath) { exit 1 }
}
