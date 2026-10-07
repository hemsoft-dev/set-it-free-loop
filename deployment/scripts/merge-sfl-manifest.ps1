function Merge-SflManifest {
    [CmdletBinding()]
    param(
        [AllowNull()]
        [pscustomobject] $ExistingManifest,

        [Parameter(Mandatory)]
        [pscustomobject] $IncomingManifest
    )

    if ($null -eq $ExistingManifest -or $IncomingManifest.tier -ne 'review') {
        return $IncomingManifest
    }

    $existingTierProperty = $ExistingManifest.PSObject.Properties['tier']
    $existingTier = if ($null -ne $existingTierProperty -and $existingTierProperty.Value) {
        [string] $existingTierProperty.Value
    } else {
        [string] $IncomingManifest.tier
    }

    if ($existingTier -notin @('review', 'reviewer')) {
        foreach ($field in @('source', 'sourceSha', 'version')) {
            $existing = $ExistingManifest.PSObject.Properties[$field]
            $incoming = $IncomingManifest.PSObject.Properties[$field]
            if ($null -eq $existing -or $null -eq $incoming -or
                [string]::IsNullOrWhiteSpace([string] $existing.Value) -or
                [string] $existing.Value -ine [string] $incoming.Value) {
                throw "Reviewer-only deployment cannot change $field for existing $existingTier workflows. Run gh sfl sync with the intended source release to update the complete installed tier."
            }
        }
    }

    $existingComponentsProperty = $ExistingManifest.PSObject.Properties['components']
    $existingComponents = if ($null -ne $existingComponentsProperty) {
        @($existingComponentsProperty.Value) |
            Where-Object { $_ -notin @('sfl-pr-review', 'sfl-pr-review-recovery') }
    } else {
        @()
    }

    $existingEngineProperty = $ExistingManifest.PSObject.Properties['enginePolicy']
    $existingEngine = if ($null -ne $existingEngineProperty) {
        $existingEngineProperty.Value
    } else {
        $null
    }

    $existingWorkflows = @()
    $defaultProfile = [string] $IncomingManifest.enginePolicy.defaultProfile
    if ($null -ne $existingEngine) {
        $workflowsProperty = $existingEngine.PSObject.Properties['workflows']
        if ($null -ne $workflowsProperty) {
            $existingWorkflows = @($workflowsProperty.Value)
        }

        $defaultProfileProperty = $existingEngine.PSObject.Properties['defaultProfile']
        if ($null -ne $defaultProfileProperty -and $defaultProfileProperty.Value) {
            $defaultProfile = [string] $defaultProfileProperty.Value
        }
    }

    $workflowMap = [ordered]@{}
    foreach ($workflow in $existingWorkflows) {
        if ([string] $workflow.name -eq 'sfl-pr-review') {
            continue
        }
        $workflowMap[[string] $workflow.name] = $workflow
    }
    foreach ($workflow in @($IncomingManifest.enginePolicy.workflows)) {
        $workflowMap[[string] $workflow.name] = $workflow
    }

    [pscustomobject] [ordered]@{
        version = $IncomingManifest.version
        deployedAt = $IncomingManifest.deployedAt
        tier = $existingTier
        source = $IncomingManifest.source
        sourceSha = $IncomingManifest.sourceSha
        components = @(
            $existingComponents + @($IncomingManifest.components) |
                Sort-Object -Unique
        )
        enginePolicy = [pscustomobject] [ordered]@{
            defaultProfile = $defaultProfile
            workflows = @($workflowMap.Values)
        }
    }
}
