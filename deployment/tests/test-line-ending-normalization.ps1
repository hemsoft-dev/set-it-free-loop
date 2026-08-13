Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

. (Join-Path $PSScriptRoot 'line-ending-test-helpers.ps1')

$lf = "first`nsecond`n"
$crlf = "first`r`nsecond`r`n"
$cr = "first`rsecond`r"

if (-not (Test-NormalizedTextEqual $lf $crlf)) {
    throw 'LF and CRLF text should compare as equivalent.'
}
if (-not (Test-NormalizedTextEqual $lf $cr)) {
    throw 'LF and CR text should compare as equivalent.'
}
if (Test-NormalizedTextEqual $lf "first`nchanged`n") {
    throw 'Line-ending normalization hid a real content change.'
}
if (Test-NormalizedTextEqual 'CaseSensitive' 'casesensitive') {
    throw 'Line-ending normalization weakened case-sensitive comparison.'
}

Write-Output 'Line-ending normalization fixtures passed.'
