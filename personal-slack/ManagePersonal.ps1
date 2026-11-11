param([Parameter(Mandatory)][ValidateSet('Register','Start','Stop','Hold','Resume','Unlock')][string]$Mode)
$ErrorActionPreference = 'Stop'
$runtime = Join-Path $env:LOCALAPPDATA 'HemSoft\PersonalSlack'
$taskName = 'HemSoft Personal Slack'
$config = Get-Content -Raw -LiteralPath (Join-Path $runtime 'config.json') | ConvertFrom-Json
$release = Split-Path -Parent $config.helper
$stop = Join-Path $runtime 'STOP'
$hold = Join-Path $runtime 'HOLD'
function Assert-PersonalTask {
    $task = Get-ScheduledTask -TaskName $taskName -ErrorAction Stop
    $expected = "//B //Nologo `"$(Join-Path $runtime 'start.vbs')`""
    if ($task.Actions.Count -ne 1 -or $task.Actions[0].Execute -ne "$env:WINDIR\System32\wscript.exe" -or $task.Actions[0].Arguments -ne $expected) {
        throw 'Task name is owned by a different action; preserve it'
    }
}
switch ($Mode) {
    'Register' {
        $runner = Join-Path $release 'RunPersonal.ps1'
        if (-not (Test-Path -LiteralPath $runner)) { throw 'Staged runner missing' }
        $escaped = $runner.Replace('"','""')
        if (Get-ScheduledTask -TaskName $taskName -ErrorAction SilentlyContinue) { Assert-PersonalTask }
        $vbs = 'Set s = CreateObject("WScript.Shell")' + "`r`n" + 'WScript.Quit s.Run("powershell.exe -NoProfile -NonInteractive -WindowStyle Hidden -File ""' + $escaped + '""", 0, True)'
        $launcher = Join-Path $runtime 'start.vbs'
        [IO.File]::WriteAllText($launcher, $vbs, [Text.UTF8Encoding]::new($false))
        $action = New-ScheduledTaskAction -Execute "$env:WINDIR\System32\wscript.exe" -Argument "//B //Nologo `"$launcher`""
        $trigger = New-ScheduledTaskTrigger -AtLogOn -User ([Security.Principal.WindowsIdentity]::GetCurrent().Name)
        $principal = New-ScheduledTaskPrincipal -UserId ([Security.Principal.WindowsIdentity]::GetCurrent().Name) -LogonType Interactive -RunLevel Limited
        $settings = New-ScheduledTaskSettingsSet -Hidden -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1) -ExecutionTimeLimit ([TimeSpan]::Zero) -StartWhenAvailable
        Register-ScheduledTask -TaskName $taskName -Action $action -Trigger $trigger -Principal $principal -Settings $settings -Force | Out-Null
        Disable-ScheduledTask -TaskName $taskName | Out-Null
        Write-Output 'Personal task registered, disabled, and not started.'
    }
    'Start' {
        Assert-PersonalTask
        if (-not $config.qualification) { throw 'Live qualification required' }
        if (Test-Path -LiteralPath $stop) { Remove-Item -LiteralPath $stop }
        Enable-ScheduledTask -TaskName $taskName | Out-Null
        Start-ScheduledTask -TaskName $taskName
        Write-Output 'Personal task start requested; admission proof still required.'
    }
    'Stop' {
        Assert-PersonalTask
        Disable-ScheduledTask -TaskName $taskName | Out-Null
        [IO.File]::WriteAllText($stop, 'Stop this receiver gracefully. Preserve requests and owners.')
        Write-Output 'Personal task disabled and graceful stop requested. Existing T3 owners remain untouched.'
    }
    'Hold' { [IO.File]::WriteAllText($hold, 'New inference admission held. Keep intake and outcomes durable.') }
    'Resume' {
        if (-not $config.qualification) { throw 'Live qualification required' }
        if (Test-Path -LiteralPath $hold) { Remove-Item -LiteralPath $hold }
    }
    'Unlock' {
        $lockPath = Join-Path $runtime 'receiver.lock'
        $lock = Get-Content -Raw -LiteralPath $lockPath | ConvertFrom-Json
        if (-not ($lock.pid -is [int]) -or $lock.pid -le 0) { throw 'Malformed lock; retain for manual reconciliation' }
        if (Get-Process -Id $lock.pid -ErrorAction SilentlyContinue) { throw 'Lock PID still exists; never steal it' }
        Remove-Item -LiteralPath $lockPath
    }
}
