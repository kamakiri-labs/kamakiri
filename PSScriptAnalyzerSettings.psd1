# The rules install.ps1 and uninstall.ps1 are linted against, named here rather
# than left to the analyzer's defaults so that the lint on a developer machine
# and the lint in CI are the same lint. `Invoke-ScriptAnalyzer -EnableExit` exits
# on the count of everything it reports, Information and Warning included, so a
# rule either script deliberately trips has to be excluded here or the lint
# never passes.
@{
    ExcludeRules = @(
        # Both scripts print milestone lines so a finished run stays readable in
        # scrollback, and those lines are messages to the person watching rather
        # than data for a caller. Write-Output would put them on the success
        # stream, where `irm ... | iex | Out-Null`, or any other use of the
        # expression's value, would swallow them silently. The rule's own
        # objection, that Write-Host cannot be captured or redirected, was true
        # before PowerShell 5.0; from 5.0 on it writes to the information stream
        # and both work, and 5.1 is this script's floor.
        'PSAvoidUsingWriteHost'
    )
}
