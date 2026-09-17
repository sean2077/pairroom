# CI compiler provisioning. Ordinary application builds do not need Inno Setup.
[CmdletBinding()]
param([string]$Destination = (Join-Path $env:RUNNER_TEMP 'pairroom-inno'))
$ErrorActionPreference = 'Stop'
Set-StrictMode -Version Latest

$download = Join-Path $env:TEMP 'pairroom-innosetup-7.0.2-x64.exe'
$uri = 'https://github.com/jrsoftware/issrc/releases/download/is-7_0_2/innosetup-7.0.2-x64.exe'
$sha256 = '5ad54ca3def786f8f4212552e54cc6d8d61329e2d24a1cfee0571d42c2684ff1'
try {
    Invoke-WebRequest -Uri $uri -OutFile $download
    if ((Get-FileHash -LiteralPath $download -Algorithm SHA256).Hash -ne $sha256) {
        throw 'Inno Setup download SHA256 mismatch'
    }
    $process = Start-Process -FilePath $download -Wait -PassThru -ArgumentList @(
        '/VERYSILENT', '/SUPPRESSMSGBOXES', '/NORESTART', '/SP-', "/DIR=`"$Destination`""
    )
    if ($process.ExitCode -ne 0) { throw "Inno Setup installation failed: $($process.ExitCode)" }
    $compiler = Join-Path $Destination 'ISCC.exe'
    if (-not (Test-Path -LiteralPath $compiler -PathType Leaf)) { throw "Missing compiler: $compiler" }
    if ($env:GITHUB_ENV) {
        "PAIRROOM_ISCC=$compiler" | Out-File -FilePath $env:GITHUB_ENV -Encoding utf8 -Append
    }
    Write-Output "Inno Setup compiler: $compiler"
} finally {
    Remove-Item -LiteralPath $download -Force -ErrorAction SilentlyContinue
}
