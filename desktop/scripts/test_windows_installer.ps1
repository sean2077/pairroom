# Run only on a disposable Windows CI runner: installs/uninstalls the actual package.
[CmdletBinding()]
param([Parameter(Mandatory)][string]$Installer)
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest
if ($env:CI -ne 'true' -or -not $env:RUNNER_TEMP) {
    throw 'This installer smoke test requires a disposable Windows CI runner.'
}
$Installer = (Resolve-Path -LiteralPath $Installer).Path
$desktop = Split-Path $PSScriptRoot -Parent
$version = (Get-Content -LiteralPath (Join-Path $desktop '../VERSION') -Raw).Trim()
$registry = 'HKLM:\Software\Microsoft\Windows\CurrentVersion\Uninstall\com.sean2077.pairroom.desktop_is1'
$legacy = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Uninstall\PairRoom.PackagingSmoke.NSIS'
$runKey = 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Run'
if ((Test-Path $registry) -or (Test-Path $legacy)) { throw 'Refusing to replace an existing installation.' }
if ((Test-Path $runKey) -and $null -ne (Get-Item $runKey).GetValue('PairRoom')) {
    throw 'Refusing to replace an existing PairRoom startup setting.'
}
$base = Join-Path $env:RUNNER_TEMP ('pairroom-installer-smoke-' + [guid]::NewGuid().ToString('N'))
$target = Join-Path $base 'PairRoom install with spaces'
New-Item -ItemType Directory -Path $base | Out-Null
$machinePath = [Environment]::GetEnvironmentVariable('Path', 'Machine')
$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$startupOwned = $false

function Invoke-Installer([string]$Program, [string[]]$Extra, [string]$Name,
                          [bool]$Success = $true, [string]$Diagnostic = '') {
    $log = Join-Path $base "$Name.log"
    $arguments = @('/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', "/LOG=`"$log`"") + $Extra
    $process = Start-Process -FilePath $Program -ArgumentList $arguments -PassThru -Wait
    if (($process.ExitCode -eq 0) -ne $Success) {
        if (Test-Path $log) { Get-Content $log | Write-Host }
        throw "$Name returned unexpected exit code $($process.ExitCode); see $log"
    }
    if ($Diagnostic -and (Get-Content $log -Raw) -notlike "*$Diagnostic*") {
        throw "$Name did not reach the expected guard '$Diagnostic'; see $log"
    }
    Write-Host "PASS: $Name (exit $($process.ExitCode))"
}

function Assert-Payload {
    foreach ($relative in @('PairRoom.exe', 'bin\pairroom.exe')) {
        $source = if ($relative -eq 'PairRoom.exe') { 'bin\PairRoom.exe' } else { 'bin\cli\pairroom.exe' }
        $installed = Join-Path $target $relative
        if ((Get-FileHash $installed).Hash -ne (Get-FileHash (Join-Path $desktop $source)).Hash) {
            throw "Installed payload mismatch: $relative"
        }
    }
    $metadata = Get-ItemProperty $registry
    if ($metadata.DisplayName -ne 'PairRoom' -or $metadata.DisplayVersion -ne $version) {
        throw 'Installer registration is missing or has the wrong name/version.'
    }
    if ($metadata.InstallLocation.TrimEnd('\') -ne $target) { throw 'Wrong InstallLocation.' }
    if (@(Get-Process -Name PairRoom -ErrorAction SilentlyContinue).Count -ne 0) {
        throw 'A silent installation must not launch PairRoom.'
    }
}

try {
    # Build an older metadata version of the same fixture to test a real version upgrade.
    & $env:PAIRROOM_ISCC /Qp /DPairRoomVersion=0.0.1 /DPairRoomArch=amd64 "/O$base" /Fprior-version `
        (Join-Path $desktop 'build/windows/inno/PairRoom.iss')
    if ($LASTEXITCODE -ne 0) { throw 'Could not compile prior-version fixture.' }
    Invoke-Installer (Join-Path $base 'prior-version.exe') @("/DIR=`"$target`"") 'fresh-install'
    if ((Get-ItemProperty $registry).DisplayVersion -ne '0.0.1') { throw 'Prior version was not installed.' }
    $sentinel = Join-Path $target 'user-owned.keep'
    Set-Content $sentinel 'must survive upgrade and uninstall'
    New-Item -Path $runKey -Force | Out-Null
    New-ItemProperty -Path $runKey -Name PairRoom -Value "`"$target\PairRoom.exe`"" -PropertyType String | Out-Null
    $startupOwned = $true

    # No /DIR on upgrade: Inno must recover the existing custom installation path.
    Invoke-Installer $Installer @() 'version-upgrade'
    Assert-Payload
    if ((Get-Item $runKey).GetValue('PairRoom') -ne "`"$target\PairRoom.exe`"") {
        throw 'Upgrade changed the startup setting.'
    }
    Invoke-Installer $Installer @() 'repeat-install'
    Assert-Payload
    if (-not (Test-Path $sentinel)) { throw 'Upgrade removed an unrelated file.' }

    $uninstaller = Join-Path $target 'unins000.exe'
    $lock = [IO.File]::Open((Join-Path $target 'bin\pairroom.exe'), 'Open', 'Read', 'None')
    try {
        Invoke-Installer $Installer @() 'locked-upgrade' $false 'files are in use or not writable'
        Invoke-Installer $uninstaller @() 'locked-uninstall' $false 'files are in use or not writable'
    } finally { $lock.Dispose() }
    Assert-Payload

    Invoke-Installer $uninstaller @() 'uninstall'
    foreach ($relative in @('PairRoom.exe', 'bin\pairroom.exe', 'LICENSE.txt')) {
        if (Test-Path (Join-Path $target $relative)) { throw "Uninstall left $relative" }
    }
    if ((Test-Path $registry) -or -not (Test-Path $sentinel)) {
        throw 'Uninstall must remove its registration but preserve unrelated files.'
    }
    if ($null -ne (Get-Item $runKey).GetValue('PairRoom')) { throw 'Owned startup entry was not removed.' }
    $startupOwned = $false

    # No old uninstaller is executed and no installation files are written.
    New-Item -Path $legacy -Force | Out-Null
    New-ItemProperty $legacy -Name DisplayName -Value PairRoom -PropertyType String | Out-Null
    New-ItemProperty $legacy -Name Publisher -Value 'PairRoom contributors' -PropertyType String | Out-Null
    $blocked = Join-Path $base 'blocked legacy installation'
    Invoke-Installer $Installer @("/DIR=`"$blocked`"") 'legacy-installer-guard' $false 'older PairRoom installer'
    if ((Test-Path $blocked) -or -not (Test-Path $legacy)) {
        throw 'Legacy guard modified the prior installation or wrote a second copy.'
    }
    if ($machinePath -ne [Environment]::GetEnvironmentVariable('Path', 'Machine') -or
        $userPath -ne [Environment]::GetEnvironmentVariable('Path', 'User')) {
        throw 'Installer unexpectedly changed PATH.'
    }
    Write-Host 'All Windows installer smoke checks passed.'
} finally {
    if (Test-Path $legacy) { Remove-Item -LiteralPath $legacy -Recurse -Force }
    if ($startupOwned) { Remove-ItemProperty $runKey -Name PairRoom -ErrorAction SilentlyContinue }
    # Preserve logs for CI artifacts, even if a smoke assertion failed.
    $uninstaller = Join-Path $target 'unins000.exe'
    if (Test-Path $uninstaller) {
        Invoke-Installer $uninstaller @() 'cleanup-uninstall'
    }
    Remove-Item (Join-Path $base 'prior-version.exe') -ErrorAction SilentlyContinue
}
