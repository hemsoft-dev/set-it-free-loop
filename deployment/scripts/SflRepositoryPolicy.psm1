Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

function Assert-SflSourceRepository {
    param([string] $Repository)
    if ($Repository -cnotin @('HemSoft/set-it-free-loop', 'hemsoft-dev/set-it-free-loop')) {
        throw "Unsupported SFL source repository '$Repository'."
    }
}

function Assert-SflTargetScope {
    param([string] $Repository, [switch] $AllowSource)
    if ($Repository -notmatch '^(HemSoft|hemsoft-dev)/[A-Za-z0-9_.-]+$') {
        throw "Repository '$Repository' is outside the configured SFL scope."
    }
    if ($Repository.Split('/')[1] -in @('.', '..')) { throw 'Invalid repository name.' }
    if (-not $AllowSource -and $Repository.Split('/')[1] -ieq 'set-it-free-loop') {
        throw "The SFL source repository is protected from deployment: $Repository."
    }
}

function Get-SflRepositoryContext {
    param([string] $Repository, [ValidateSet('write', 'admin')] [string] $Access = 'write',
          [switch] $AllowSource, [string] $ExpectedLogin)
    Assert-SflTargetScope -Repository $Repository -AllowSource:$AllowSource
    $login = (& gh api user --jq .login).Trim()
    if ($LASTEXITCODE -ne 0 -or $login -notmatch '^[A-Za-z0-9][A-Za-z0-9-]{0,38}$') {
        throw 'Cannot establish an authenticated GitHub user.'
    }
    if ($ExpectedLogin -and $login -cne $ExpectedLogin) {
        throw "Active gh login is '$login'; expected '$ExpectedLogin'."
    }
    $raw = & gh api --method GET "repos/$Repository"
    if ($LASTEXITCODE -ne 0) { throw "Cannot read repository metadata for $Repository." }
    $metadata = $raw | ConvertFrom-Json
    if ($metadata.full_name -ine $Repository) { throw 'Repository identity mismatch or unexpected redirect.' }
    $raw = & gh api --method GET "repos/$Repository/collaborators/$login/permission"
    if ($LASTEXITCODE -ne 0) { throw "Cannot verify $login permissions on $Repository." }
    $permission = $raw | ConvertFrom-Json
    if ($permission.user.login -ine $login -or
        ($permission.permission -ne 'admin' -and
         ($Access -eq 'admin' -or $permission.permission -notin @('write', 'maintain')))) {
        throw "$login requires $Access or higher repository access on $Repository."
    }
    return [pscustomobject]@{ Login = $login; Metadata = $metadata; Permission = $permission.permission }
}

Export-ModuleMember -Function Assert-SflSourceRepository, Assert-SflTargetScope, Get-SflRepositoryContext
