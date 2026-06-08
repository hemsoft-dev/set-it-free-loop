<#
.SYNOPSIS
    Builds and installs a HemSoft-hardwired gh-sfl extension.

.DESCRIPTION
    Uses the existing gh-sfl source checkout as an input, patches a temporary
    copy to read SFL from HemSoft/set-it-free-loop and this repository's
    deployment layout, then installs gh-sfl.exe into the GitHub CLI extension
    directory.
#>

[CmdletBinding()]
param(
    [string] $GhSflSource = 'D:\github\Relias\set-it-free-loop\gh-sfl',
    [string] $WorkDir = 'D:\tmp\gh-sfl-hemsoft-build',
    [switch] $NoInstall
)

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
$InformationPreference = 'Continue'

$ScriptDir = Split-Path -Parent $MyInvocation.MyCommand.Path
$RepoRoot = Resolve-Path (Join-Path $ScriptDir '..\..')
$LabelsPath = Join-Path $RepoRoot 'deployment\governance\labels.json'
$EnginePolicyPath = Join-Path $RepoRoot 'deployment\engine-policy.json'
$labelNames = (Get-Content $LabelsPath -Raw | ConvertFrom-Json).name
$goLabelEntries = ($labelNames | ForEach-Object { "`t`t`"$($_.Replace('\', '\\').Replace('"', '\"'))`"," }) -join "`n"
$goUninstallLabelEntries = ($labelNames | ForEach-Object { "`t`"$($_.Replace('\', '\\').Replace('"', '\"'))`"," }) -join "`n"

function Get-RequiredJsonProperty([object] $Object, [string] $Name, [string] $Context) {
    if ($null -eq $Object) {
        throw "$Context is null."
    }

    $property = $Object.PSObject.Properties[$Name]
    if ($null -eq $property -or $null -eq $property.Value) {
        throw "$Context is missing required property '$Name'."
    }

    return $property.Value
}

if (-not (Test-Path $EnginePolicyPath)) {
    throw "Engine policy file not found: $EnginePolicyPath"
}

$enginePolicy = Get-Content $EnginePolicyPath -Raw | ConvertFrom-Json
$defaultEngineProfileName = [string](Get-RequiredJsonProperty $enginePolicy 'defaultProfile' 'engine policy')
$engineProfiles = Get-RequiredJsonProperty $enginePolicy 'profiles' 'engine policy'
$defaultEngineProfileProperty = $engineProfiles.PSObject.Properties[$defaultEngineProfileName]
if ($null -eq $defaultEngineProfileProperty -or $null -eq $defaultEngineProfileProperty.Value) {
    throw "Default engine profile '$defaultEngineProfileName' was not found in $EnginePolicyPath."
}

$defaultEngineProfile = $defaultEngineProfileProperty.Value
$engineProvider = [string](Get-RequiredJsonProperty $defaultEngineProfile 'provider' "engine profile '$defaultEngineProfileName'")
$engineModel = [string](Get-RequiredJsonProperty $defaultEngineProfile 'model' "engine profile '$defaultEngineProfileName'")
$engineEffortProperty = $defaultEngineProfile.PSObject.Properties['effort']
$engineEffort = if ($null -ne $engineEffortProperty -and $null -ne $engineEffortProperty.Value) {
    [string] $engineEffortProperty.Value
} else {
    ''
}

if ($engineEffort -and @('low', 'medium', 'high') -notcontains $engineEffort) {
    throw "Default engine profile '$defaultEngineProfileName' uses unsupported effort '$engineEffort'. Use low, medium, or high."
}

$engineRenderedModel = if ($engineEffort) { "$engineModel`?effort=$engineEffort" } else { $engineModel }
$requiredSecretsProperty = $defaultEngineProfile.PSObject.Properties['requiredSecretsAnyOf']
$engineRequiredSecrets = if ($null -ne $requiredSecretsProperty -and $null -ne $requiredSecretsProperty.Value) {
    @($requiredSecretsProperty.Value)
} else {
    @()
}

$goEngineProfile = $defaultEngineProfileName.Replace('\', '\\').Replace('"', '\"')
$goEngineProvider = $engineProvider.Replace('\', '\\').Replace('"', '\"')
$goEngineModel = $engineModel.Replace('\', '\\').Replace('"', '\"')
$goEngineEffort = $engineEffort.Replace('\', '\\').Replace('"', '\"')
$goEngineRenderedModel = $engineRenderedModel.Replace('\', '\\').Replace('"', '\"')
$goEngineRequiredSecrets = ($engineRequiredSecrets | ForEach-Object { "`t`t`"$($_.Replace('\', '\\').Replace('"', '\"'))`"," }) -join "`n"

if (-not (Test-Path $GhSflSource)) {
    throw "gh-sfl source not found: $GhSflSource"
}

if (-not (Get-Command go -ErrorAction SilentlyContinue)) {
    throw 'Go is required to build gh-sfl but was not found on PATH.'
}

if (Test-Path $WorkDir) {
    Remove-Item $WorkDir -Recurse -Force
}
New-Item -ItemType Directory -Path $WorkDir -Force | Out-Null

Copy-Item -Path (Join-Path $GhSflSource '*') -Destination $WorkDir -Recurse -Force

$mainPath = Join-Path $WorkDir 'main.go'
$goModPath = Join-Path $WorkDir 'go.mod'
$readmePath = Join-Path $WorkDir 'README.md'
$initPath = Join-Path $WorkDir 'init.go'
$syncPath = Join-Path $WorkDir 'sync.go'
$addPath = Join-Path $WorkDir 'add.go'
$addonsPath = Join-Path $WorkDir 'addons.go'
$githubPath = Join-Path $WorkDir 'github.go'
$manifestGoPath = Join-Path $WorkDir 'manifest.go'
$statusPath = Join-Path $WorkDir 'status.go'
$uninstallPath = Join-Path $WorkDir 'uninstall.go'

$mainContent = Get-Content $mainPath -Raw
$mainContent = [regex]::Replace($mainContent, 'motherRepoOwner\s*=\s*"[^"]+"', 'motherRepoOwner = "HemSoft"', 1)
$mainContent = [regex]::Replace($mainContent, 'copyrightHolder\s*=\s*"[^"]+"', 'copyrightHolder = "HemSoft"', 1)
Set-Content $mainPath -Value $mainContent -NoNewline

$goModContent = Get-Content $goModPath -Raw
$goModContent = [regex]::Replace($goModContent, '(?m)^module\s+.+$', 'module github.com/HemSoft/gh-sfl', 1)
$goModContent |
    Set-Content $goModPath -NoNewline

if (Test-Path $readmePath) {
    $readmeContent = Get-Content $readmePath -Raw
    $readmeContent = [regex]::Replace($readmeContent, '\b[A-Za-z0-9_.-]+/gh-sfl\b', 'HemSoft/gh-sfl')
    $readmeContent = [regex]::Replace($readmeContent, '\b[A-Za-z0-9_.-]+/set-it-free-loop\b', 'HemSoft/set-it-free-loop')
    $readmeContent |
        Set-Content $readmePath -NoNewline
}

$newTierBlock = @'
var tierWorkflows = map[string][]string{
	"minimal": {
		"sfl-dispatcher.yml",
	},
	"standard": {
		"sfl-dispatcher.yml",
		"sfl-auditor.yml",
		"daily-repo-status.md",
		"repo-audit.md",
		"issue-processor.md",
		"simplisticate.md",
	},
	"full": {
		"sfl-dispatcher.yml",
		"sfl-auditor.yml",
		"daily-repo-status.md",
		"repo-audit.md",
		"issue-processor.md",
		"simplisticate.md",
		"pr-analyzer-a.md",
		"pr-analyzer-b.md",
		"pr-analyzer-c.md",
		"pr-fixer.md",
		"pr-promoter.md",
	},
}
'@

$newComponentBlock = @'
var tierComponents = map[string][]string{
	"minimal":  {"labels", "governance", "sfl-dispatcher"},
	"standard": {"labels", "governance", "sfl-dispatcher", "sfl-auditor", "daily-repo-status", "repo-audit", "issue-processor", "simplisticate"},
	"full":     {"labels", "governance", "sfl-dispatcher", "sfl-auditor", "daily-repo-status", "repo-audit", "issue-processor", "simplisticate", "pr-analyzer-a", "pr-analyzer-b", "pr-analyzer-c", "pr-fixer", "pr-promoter"},
}
'@

$initContent = Get-Content $initPath -Raw
$tierPattern = '(?s)var tierWorkflows = map\[string\]\[\]string\{.*?\n\}\s*\nvar tierComponents'
if ($initContent -notmatch $tierPattern) {
    throw 'Could not find the expected tierWorkflows block in init.go.'
}
$initContent = [regex]::Replace($initContent, $tierPattern, $newTierBlock + "`nvar tierComponents", 1)
$componentPattern = '(?s)var tierComponents = map\[string\]\[\]string\{.*?\n\}'
if ($initContent -notmatch $componentPattern) {
    throw 'Could not find the expected tierComponents block in init.go.'
}
$initContent = [regex]::Replace($initContent, $componentPattern, $newComponentBlock, 1)
$initContent = $initContent.Replace('srcPath := "workflows/" + wf', 'srcPath := sourceWorkflowPath(wf)')
$newManifestBlock = @'
	// Manifest
	deployedAt := time.Now().UTC()
	deployedBy := getCurrentUser()
	if existing, readErr := readRemoteManifest(owner, repo); readErr == nil &&
		existing.Version == sflVersion &&
		existing.Tier == opts.tier &&
		existing.SourceSHA == latestSHA {
		deployedAt = existing.DeployedAt
		deployedBy = existing.DeployedBy
	}
	manifest := &sflManifest{
		Version:    sflVersion,
		Tier:       opts.tier,
		MotherRepo: motherRepoOwner + "/" + motherRepoName,
		DeployedAt: deployedAt,
		DeployedBy: deployedBy,
		SourceSHA:  latestSHA,
		Components: tierComponents[opts.tier],
		Addons:     opts.addons,
		EnginePolicy: hemSoftEnginePolicyManifestForFileMap(fileMap),
	}
'@
$manifestPattern = '(?s)\t// Manifest\r?\n\tghUser := getCurrentUser\(\)\r?\n\tmanifest := &sflManifest\{.*?\r?\n\t\}'
if ($initContent -notmatch $manifestPattern) {
    throw 'Could not find the expected manifest block in init.go.'
}
$initContent = [regex]::Replace($initContent, $manifestPattern, $newManifestBlock, 1)
Set-Content $initPath -Value $initContent -NoNewline

foreach ($path in @($syncPath, $addPath)) {
    $content = (Get-Content $path -Raw).Replace('srcPath := "workflows/" + wf', 'srcPath := sourceWorkflowPath(wf)')
    $content = $content.Replace("manifest.DeployedBy = getCurrentUser()", "manifest.DeployedBy = getCurrentUser()`n`tmanifest.EnginePolicy = hemSoftEnginePolicyManifestForFileMap(fileMap)")
    Set-Content $path -Value $content -NoNewline
}

$manifestGoContent = Get-Content $manifestGoPath -Raw
$manifestStructBlock = @'
type sflManifest struct {
	Version      string                   `json:"version"`
	Tier         string                   `json:"tier"`
	MotherRepo   string                   `json:"motherRepo"`
	DeployedAt   time.Time                `json:"deployedAt"`
	DeployedBy   string                   `json:"deployedBy"`
	SourceSHA    string                   `json:"sourceSHA,omitempty"`
	Components   []string                 `json:"components"`
	Addons       []string                 `json:"addons,omitempty"`
	EnginePolicy *sflEnginePolicyManifest `json:"enginePolicy,omitempty"`
}

type sflEnginePolicyManifest struct {
	DefaultProfile string                      `json:"defaultProfile"`
	Workflows      []sflEngineWorkflowProfile `json:"workflows"`
}

type sflEngineWorkflowProfile struct {
	Name                 string   `json:"name"`
	Profile              string   `json:"profile"`
	Provider             string   `json:"provider"`
	Model                string   `json:"model"`
	Effort               string   `json:"effort,omitempty"`
	RenderedModel        string   `json:"renderedModel"`
	RequiredSecretsAnyOf []string `json:"requiredSecretsAnyOf,omitempty"`
}
'@
$manifestGoContent = [regex]::Replace($manifestGoContent, '(?s)type sflManifest struct \{.*?\n\}', $manifestStructBlock, 1)
Set-Content $manifestGoPath -Value $manifestGoContent -NoNewline

$githubContent = Get-Content $githubPath -Raw
if ($githubContent -notmatch '"net/url"') {
    $githubContent = [regex]::Replace($githubContent, '(\r?\n\t"io")(\r?\n)', "`$1`$2`t`"net/url`"`$2", 1)
}
$githubContent = $githubContent.Replace('fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, l.Name)', 'fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, strings.ReplaceAll(url.PathEscape(l.Name), ":", "%3A"))')
$deployBlock = @'
func deployViaGit(owner, repo, branch string, fileMap map[string]string, commitMsg string, w io.Writer) error {
	if err := applyHemSoftOwnership(fileMap); err != nil {
		return fmt.Errorf("fetching HemSoft CODEOWNERS: %w", err)
	}

	tmpDir, err := os.MkdirTemp("", "gh-sfl-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir)

	// Use the HemSoft SSH profile for Git writes so workflow-file pushes do not
	// depend on the GitHub CLI token's workflow scope.
	fmt.Fprintf(w, "  Cloning %s/%s...\n", owner, repo)
	cloneURL := fmt.Sprintf("git@github-personal1:%s/%s.git", owner, repo)
	cloneCmd := exec.Command("git", "clone", "--depth=1", cloneURL, tmpDir)
	if out, cloneErr := cloneCmd.CombinedOutput(); cloneErr != nil {
		return fmt.Errorf("cloning: %s: %w", string(out), cloneErr)
	}

	for fpath, content := range fileMap {
		dst := filepath.Join(tmpDir, filepath.FromSlash(fpath))
		if mkErr := os.MkdirAll(filepath.Dir(dst), 0755); mkErr != nil {
			return fmt.Errorf("mkdir for %s: %w", fpath, mkErr)
		}
		if wErr := os.WriteFile(dst, []byte(content), 0644); wErr != nil {
			return fmt.Errorf("writing %s: %w", fpath, wErr)
		}
	}

	runGit := func(args ...string) (string, error) {
		cmd := exec.Command("git", args...)
		cmd.Dir = tmpDir
		out, err := cmd.CombinedOutput()
		return string(out), err
	}

	if _, err := runGit("add", "-A"); err != nil {
		return fmt.Errorf("git add: %w", err)
	}

	checkCmd := exec.Command("git", "diff", "--cached", "--quiet")
	checkCmd.Dir = tmpDir
	if checkCmd.Run() == nil {
		fmt.Fprintf(w, "  No changes - already up to date.\n")
		return nil
	}

	if out, err := runGit("commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("git commit: %s: %w", out, err)
	}

	fmt.Fprintf(w, "  Pushing %d files to %s/%s...\n", len(fileMap), owner, repo)
	if out, err := runGit("push", "origin", "HEAD"); err != nil {
		return fmt.Errorf("git push: %s\nHint: github-personal1 may not have push access to %s/%s", out, owner, repo)
	}

	return nil
}

// updateAgentPRBranches
'@
$deployStart = $githubContent.IndexOf('func deployViaGit(')
$deployEnd = if ($deployStart -ge 0) { $githubContent.IndexOf('// updateAgentPRBranches', $deployStart) } else { -1 }
if ($deployStart -lt 0 -or $deployEnd -lt 0) {
    throw 'Could not find the expected deployViaGit function in github.go.'
}
$githubContent = $githubContent.Substring(0, $deployStart) + $deployBlock + $githubContent.Substring($deployEnd + '// updateAgentPRBranches'.Length)
Set-Content $githubPath -Value $githubContent -NoNewline

$statusContent = Get-Content $statusPath -Raw
$requiredLabelsBlock = @"
requiredLabels := []string{
$goLabelEntries
		}
"@
$statusContent = [regex]::Replace($statusContent, '(?s)requiredLabels := \[\]string\{.*?\n\t\t\}', $requiredLabelsBlock, 1)
Set-Content $statusPath -Value $statusContent -NoNewline

$uninstallContent = Get-Content $uninstallPath -Raw
if ($uninstallContent -notmatch '"net/url"') {
    $uninstallContent = [regex]::Replace($uninstallContent, '(\r?\n\t"io")(\r?\n)', "`$1`$2`t`"net/url`"`$2", 1)
}
$uninstallContent = $uninstallContent.Replace('fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, label)', 'fmt.Sprintf("repos/%s/%s/labels/%s", owner, repo, strings.ReplaceAll(url.PathEscape(label), ":", "%3A"))')
$sflLabelsBlock = @"
var sflLabels = []string{
$goUninstallLabelEntries
}
"@
$uninstallContent = [regex]::Replace($uninstallContent, '(?s)var sflLabels = \[\]string\{.*?\n\}', $sflLabelsBlock, 1)
Set-Content $uninstallPath -Value $uninstallContent -NoNewline

@'
package main

var addonWorkflows = map[string][]string{}

var addonDescriptions = map[string]string{}

func validAddon(name string) bool {
	_, ok := addonWorkflows[name]
	return ok
}

func knownAddonNames() []string {
	names := make([]string, 0, len(addonWorkflows))
	for k := range addonWorkflows {
		names = append(names, k)
	}
	return names
}

func addonWorkflowFiles(addons []string) []string {
	var files []string
	for _, addon := range addons {
		if wfs, ok := addonWorkflows[addon]; ok {
			files = append(files, wfs...)
		}
	}
	return files
}
'@ | Set-Content $addonsPath -NoNewline

$hemSoftGoContent = @'
package main

import (
	"fmt"
	"sort"
	"strings"
)

func applyHemSoftOwnership(fileMap map[string]string) error {
	if err := applyHemSoftEnginePolicy(fileMap); err != nil {
		return err
	}

	fileMap["CODEOWNERS"] = `# CODEOWNERS - auto-assign reviewers for HemSoft repositories
# https://docs.github.com/en/repositories/managing-your-repositorys-settings-and-features/customizing-your-repository/about-code-owners

* @HemSoft

# Automation and SFL-managed files
/.github/ @HemSoft
/.github/workflows/ @HemSoft
/.sfl/ @HemSoft
/.sfl/** @HemSoft
`
	return nil
}

func applyHemSoftEnginePolicy(fileMap map[string]string) error {
	for fpath, content := range fileMap {
		if !strings.HasPrefix(fpath, ".github/workflows/") || !strings.HasSuffix(fpath, ".md") {
			continue
		}

		workflowName := strings.TrimSuffix(strings.TrimPrefix(fpath, ".github/workflows/"), ".md")
		rewritten, err := applyHemSoftEnginePolicyToWorkflow(content, workflowName)
		if err != nil {
			return err
		}

		fileMap[fpath] = rewritten
	}

	return nil
}

func applyHemSoftEnginePolicyToWorkflow(content, workflowName string) (string, error) {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return "", fmt.Errorf("workflow %s does not start with YAML frontmatter", workflowName)
	}

	frontmatterStart := len("---\n")
	closingOffset := strings.Index(normalized[frontmatterStart:], "\n---")
	if closingOffset < 0 {
		return "", fmt.Errorf("workflow %s has no closing YAML frontmatter marker", workflowName)
	}

	frontmatterEnd := frontmatterStart + closingOffset
	frontmatter := normalized[frontmatterStart:frontmatterEnd]
	rest := normalized[frontmatterEnd+len("\n---"):]

	frontmatter = removeTopLevelEngineBlock(frontmatter)
	frontmatter = insertTopLevelEngineBlock(frontmatter, hemSoftEngineBlock())

	return "---\n" + frontmatter + "\n---" + rest, nil
}

func removeTopLevelEngineBlock(frontmatter string) string {
	lines := strings.Split(frontmatter, "\n")
	output := make([]string, 0, len(lines))
	skippingEngine := false

	for _, line := range lines {
		if isTopLevelYamlKey(line, "engine") {
			skippingEngine = true
			continue
		}

		if skippingEngine {
			if strings.TrimSpace(line) == "" || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
				continue
			}
			skippingEngine = false
		}

		output = append(output, line)
	}

	return strings.TrimRight(strings.Join(output, "\n"), "\n")
}

func insertTopLevelEngineBlock(frontmatter, engineBlock string) string {
	lines := strings.Split(frontmatter, "\n")
	output := make([]string, 0, len(lines)+4)
	inserted := false

	for _, line := range lines {
		if !inserted && isTopLevelYamlKey(line, "network") {
			output = append(output, strings.Split(engineBlock, "\n")...)
			output = append(output, "")
			inserted = true
		}
		output = append(output, line)
	}

	if !inserted {
		if strings.TrimSpace(frontmatter) != "" {
			output = append(output, "")
		}
		output = append(output, strings.Split(engineBlock, "\n")...)
	}

	return strings.Join(output, "\n")
}

func isTopLevelYamlKey(line, key string) bool {
	if strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
		return false
	}

	trimmed := strings.TrimSpace(line)
	return trimmed == key+":" || strings.HasPrefix(trimmed, key+": ")
}

func hemSoftEngineBlock() string {
	return "engine:\n  id: {{ENGINE_PROVIDER}}\n  model: {{ENGINE_RENDERED_MODEL}}"
}

func hemSoftEnginePolicyManifestForFileMap(fileMap map[string]string) *sflEnginePolicyManifest {
	workflowNames := make([]string, 0)
	for fpath := range fileMap {
		if !strings.HasPrefix(fpath, ".github/workflows/") || !strings.HasSuffix(fpath, ".md") {
			continue
		}

		workflowNames = append(
			workflowNames,
			strings.TrimSuffix(strings.TrimPrefix(fpath, ".github/workflows/"), ".md"),
		)
	}
	sort.Strings(workflowNames)

	workflows := make([]sflEngineWorkflowProfile, 0, len(workflowNames))
	for _, workflowName := range workflowNames {
		workflows = append(workflows, sflEngineWorkflowProfile{
			Name:          workflowName,
			Profile:       "{{ENGINE_PROFILE}}",
			Provider:      "{{ENGINE_PROVIDER}}",
			Model:         "{{ENGINE_MODEL}}",
			Effort:        "{{ENGINE_EFFORT}}",
			RenderedModel: "{{ENGINE_RENDERED_MODEL}}",
			RequiredSecretsAnyOf: []string{
{{ENGINE_REQUIRED_SECRETS}}
			},
		})
	}

	return &sflEnginePolicyManifest{
		DefaultProfile: "{{ENGINE_PROFILE}}",
		Workflows:      workflows,
	}
}

func sourceWorkflowPath(name string) string {
	switch name {
	case "sfl-dispatcher.yml", "sfl-auditor.yml":
		return "deployment/infrastructure/" + name
	default:
		return "deployment/workflows/" + name
	}
}
'@
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_PROFILE}}', $goEngineProfile)
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_PROVIDER}}', $goEngineProvider)
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_MODEL}}', $goEngineModel)
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_EFFORT}}', $goEngineEffort)
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_RENDERED_MODEL}}', $goEngineRenderedModel)
$hemSoftGoContent = $hemSoftGoContent.Replace('{{ENGINE_REQUIRED_SECRETS}}', $goEngineRequiredSecrets)
$hemSoftGoContent | Set-Content (Join-Path $WorkDir 'hemsoft.go') -NoNewline

Push-Location $WorkDir
try {
    go vet ./...
    if ($LASTEXITCODE -ne 0) { throw "go vet failed with exit code $LASTEXITCODE" }
    go test ./...
    if ($LASTEXITCODE -ne 0) { throw "go test failed with exit code $LASTEXITCODE" }

    $date = Get-Date -Format 'yyyy-MM-dd'
    go build -ldflags "-X main.version=hemsoft -X main.buildDate=$date" -o gh-sfl.exe .
    if ($LASTEXITCODE -ne 0) { throw "go build failed with exit code $LASTEXITCODE" }

    if (-not $NoInstall) {
        $extDir = Join-Path $env:LOCALAPPDATA 'GitHub CLI\extensions\gh-sfl'
        if (-not (Test-Path $extDir)) {
            New-Item -ItemType Directory -Path $extDir -Force | Out-Null
        }
        Copy-Item gh-sfl.exe (Join-Path $extDir 'gh-sfl.exe') -Force
    }
}
finally {
    Pop-Location
}

Write-Information ''
if ($NoInstall) {
    Write-Information "Built HemSoft gh-sfl at $WorkDir\gh-sfl.exe"
} else {
    Write-Information 'Installed HemSoft gh-sfl. Verify from a HemSoft checkout with:'
    Write-Information '  gh sfl version'
    Write-Information '  gh sfl init --repo HemSoft/hs-buddy'
}
