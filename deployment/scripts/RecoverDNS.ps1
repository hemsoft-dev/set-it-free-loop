[CmdletBinding(SupportsShouldProcess)]
param(
    [string]$ZoneId = "c4fb19a03acf1631cc846cea7b6040b0",
    [switch]$DryRun
)

$ErrorActionPreference = "Stop"

function Write-Info([string]$Message) {
    Write-Host "[INFO] $Message"
}

function Write-Warn([string]$Message) {
    Write-Host "[WARN] $Message" -ForegroundColor Yellow
}

$token = $env:CLOUDFLARE_API_TOKEN
if ([string]::IsNullOrWhiteSpace($token)) {
    throw "CLOUDFLARE_API_TOKEN is not set."
}

$headers = @{
    Authorization = "Bearer $token"
    "Content-Type" = "application/json"
}

$baseUrl = "https://api.cloudflare.com/client/v4/zones/$ZoneId/dns_records"

$desiredRecords = @(
    @{
        type = "A"
        name = "setitfreeloop.org"
        content = "76.76.21.21"
        ttl = 1
        proxied = $false
    },
    @{
        type = "CNAME"
        name = "www.setitfreeloop.org"
        content = "cname.vercel-dns.com"
        ttl = 1
        proxied = $false
    }
)

Write-Info "Loading existing DNS records for zone $ZoneId..."
$existingResp = Invoke-RestMethod -Method Get -Uri "$baseUrl?per_page=200" -Headers $headers
if (-not $existingResp.success) {
    throw "Failed to load DNS records: $($existingResp.errors | ConvertTo-Json -Compress)"
}

foreach ($desired in $desiredRecords) {
    $match = $existingResp.result | Where-Object {
        $_.name -eq $desired.name -and $_.type -eq $desired.type
    } | Select-Object -First 1

    if (-not $match) {
        $action = "Create $($desired.type) $($desired.name) -> $($desired.content)"
        if ($DryRun) {
            Write-Info "[DryRun] $action"
            continue
        }

        if ($PSCmdlet.ShouldProcess($desired.name, $action)) {
            $body = $desired | ConvertTo-Json -Depth 5
            $createResp = Invoke-RestMethod -Method Post -Uri $baseUrl -Headers $headers -Body $body
            if (-not $createResp.success) {
                throw "Create failed for $($desired.name): $($createResp.errors | ConvertTo-Json -Compress)"
            }
            Write-Info "Created $($desired.type) $($desired.name)."
        }
        continue
    }

    $needsUpdate = (
        $match.content -ne $desired.content -or
        [int]$match.ttl -ne [int]$desired.ttl -or
        [bool]$match.proxied -ne [bool]$desired.proxied
    )

    if (-not $needsUpdate) {
        Write-Info "Already correct: $($desired.type) $($desired.name)."
        continue
    }

    $action = "Update $($desired.type) $($desired.name)"
    if ($DryRun) {
        Write-Info "[DryRun] $action"
        continue
    }

    if ($PSCmdlet.ShouldProcess($desired.name, $action)) {
        $body = $desired | ConvertTo-Json -Depth 5
        $updateResp = Invoke-RestMethod -Method Put -Uri "$baseUrl/$($match.id)" -Headers $headers -Body $body
        if (-not $updateResp.success) {
            throw "Update failed for $($desired.name): $($updateResp.errors | ConvertTo-Json -Compress)"
        }
        Write-Info "Updated $($desired.type) $($desired.name)."
    }
}

Write-Info "DNS recovery complete."
Write-Info "Tip: run 'Resolve-DnsName setitfreeloop.org -Type A' and 'Resolve-DnsName www.setitfreeloop.org -Type CNAME' after propagation."
