param(
  [string]$Version = (Get-Content (Join-Path $PSScriptRoot "..\VERSION") -Raw).Trim(),
  [string]$Dist = "dist",
  [switch]$SkipChecks
)

$ErrorActionPreference = "Stop"
$RepoRoot = (Resolve-Path (Join-Path $PSScriptRoot "..")).Path
$Git = Get-Command git.exe -ErrorAction Stop
$GitInstall = Split-Path (Split-Path $Git.Source -Parent) -Parent
$Candidates = @(
  (Join-Path $GitInstall "bin\bash.exe"),
  "C:\Program Files\Git\bin\bash.exe",
  "C:\Program Files (x86)\Git\bin\bash.exe"
)
$Bash = $Candidates | Where-Object { Test-Path -LiteralPath $_ } | Select-Object -First 1
if (-not $Bash) {
  throw "Git Bash is required to run the authoritative release pipeline"
}

$Names = @("VERSION", "DIST", "SKIP_CHECKS")
$Previous = @{}
foreach ($Name in $Names) {
  $Previous[$Name] = [Environment]::GetEnvironmentVariable($Name, "Process")
}

Push-Location $RepoRoot
try {
  $env:VERSION = $Version
  $env:DIST = $Dist.Replace("\", "/")
  $env:SKIP_CHECKS = if ($SkipChecks) { "1" } else { "0" }
  & $Bash scripts/release.sh
  if ($LASTEXITCODE -ne 0) {
    throw "release pipeline failed with exit code $LASTEXITCODE"
  }
} finally {
  Pop-Location
  foreach ($Name in $Names) {
    if ($null -eq $Previous[$Name]) {
      Remove-Item "Env:$Name" -ErrorAction SilentlyContinue
    } else {
      Set-Item "Env:$Name" $Previous[$Name]
    }
  }
}
