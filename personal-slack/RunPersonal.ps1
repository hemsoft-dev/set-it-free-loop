param([ValidateSet('serve','doctor')][string]$Command = 'serve')
$ErrorActionPreference = 'Stop'
# Read only the existing named Windows environment entries, in this process.
# No profile scripts, credential copies, global auth changes or interactive login.
foreach ($name in @('SFL_APP_LEVEL_TOKEN','SFL_SLACK_BOT_USER_OAUTH_TOKEN','SFL_SLACK_USER_OAUTH_TOKEN')) {
    $value = [Environment]::GetEnvironmentVariable($name, 'Process')
    if (-not $value) { $value = [Environment]::GetEnvironmentVariable($name, 'User') }
    if (-not $value) { $value = [Environment]::GetEnvironmentVariable($name, 'Machine') }
    if (-not $value) { throw "Existing environment entry missing: $name" }
    [Environment]::SetEnvironmentVariable($name, $value, 'Process')
}
$runtime = Join-Path $env:LOCALAPPDATA 'HemSoft\PersonalSlack'
if ($Command -eq 'serve') {
    $acl = Get-Acl -LiteralPath $runtime
    if (-not $acl.AreAccessRulesProtected) { throw 'Personal runtime ACL must be protected' }
    $allowed = @([System.Security.Principal.WindowsIdentity]::GetCurrent().User.Value, 'S-1-5-18')
    foreach ($rule in $acl.Access) {
        $sid = $rule.IdentityReference.Translate([System.Security.Principal.SecurityIdentifier]).Value
        if ($rule.AccessControlType -eq 'Allow' -and $sid -notin $allowed) { throw 'Unexpected personal runtime ACL principal' }
    }
}
& (Get-Command node.exe -ErrorAction Stop).Source (Join-Path $PSScriptRoot 'run.mjs') $Command
exit $LASTEXITCODE
