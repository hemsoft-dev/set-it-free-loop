function Add-SflSourcePin {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string] $Content,

        [Parameter(Mandatory)]
        [string] $PinComment
    )

    $withoutExistingPin = [regex]::Replace(
        $Content,
        '(?m)^(?:# Deployed from:[^\r\n]*\r?\n# To upgrade:[^\r\n]*|<!-- Deployed from:[^\r\n]* -->\r?\n<!-- To upgrade:[^\r\n]* -->|<!--\r?\nDeployed from:[^\r\n]*\r?\nTo upgrade:[^\r\n]*\r?\n-->)\r?\n?',
        ''
    )
    $frontmatter = [regex]::Match($withoutExistingPin, '(?s)\A---\r?\n.*?\r?\n---\r?\n?')
    if (-not $frontmatter.Success) {
        throw 'Workflow content does not start with YAML frontmatter.'
    }

    $pin = $PinComment.TrimEnd("`r", "`n") + "`n"
    $rest = $withoutExistingPin.Substring($frontmatter.Length).TrimStart("`r", "`n")
    $frontmatter.Value.TrimEnd("`r", "`n") + "`n" + $pin + "`n" + $rest
}

function Add-SflYamlSourcePin {
    [CmdletBinding()]
    param(
        [Parameter(Mandatory)]
        [string] $Content,

        [Parameter(Mandatory)]
        [string] $SourceRef,

        [Parameter(Mandatory)]
        [string] $DefaultBranch
    )

    $withoutExistingPin = [regex]::Replace(
        $Content,
        '\A# Deployed from: HemSoft/set-it-free-loop/[^\r\n]+\r?\n# To upgrade: re-run deploy-workflow\.ps1 at the desired SHA\r?\n',
        ''
    )
    $withoutExistingPin = $withoutExistingPin.Replace("`r`n", "`n")
    $sourcePattern = '(?m)^# Source: HemSoft/set-it-free-loop/deployment/infrastructure/sfl-pr-review-auto\.yml@(?:main|[0-9a-f]{40})$'
    $withoutExistingPin = ([regex]::new($sourcePattern)).Replace(
        $withoutExistingPin,
        "# Source: $SourceRef",
        1
    )
    $escapedDefaultBranch = $DefaultBranch.Replace("'", "''")
    $pushBranchPattern = "(?m)^    branches: \[(?:main|'(?:[^']|'')*')\]$"
    $withoutExistingPin = ([regex]::new($pushBranchPattern)).Replace(
        $withoutExistingPin,
        "    branches: ['$escapedDefaultBranch']",
        1
    )
    $reviewBasePattern = "(?m)^  SFL_REVIEW_BASE_BRANCH: (?:main|'(?:[^']|'')*')$"
    $withoutExistingPin = ([regex]::new($reviewBasePattern)).Replace(
        $withoutExistingPin,
        "  SFL_REVIEW_BASE_BRANCH: '$escapedDefaultBranch'",
        1
    )
    return "# Deployed from: $SourceRef`n# To upgrade: re-run deploy-workflow.ps1 at the desired SHA`n$withoutExistingPin"
}
