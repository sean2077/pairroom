param(
  [string]$Version = (Get-Content VERSION -Raw).Trim(),
  [string]$Dist = "dist",
  [switch]$SkipChecks
)
$ErrorActionPreference = "Stop"
$Commit = (git rev-parse HEAD).Trim()
$BuildDate = (Get-Date).ToUniversalTime().ToString("yyyy-MM-ddTHH:mm:ssZ")
$VersionPkg = "github.com/sean2077/pairroom/internal/version"
$Ldflags = "-s -w -X $VersionPkg.Commit=$Commit -X $VersionPkg.BuildDate=$BuildDate"
if ((git status --porcelain).Length -ne 0) {
  throw "release requires a clean Git worktree"
}
if (-not $SkipChecks) {
  go test -count=1 ./...
  if ($env:PAIRROOM_RACE -eq "1") { go test -race -count=1 ./... }
  go vet ./...
  node --check internal/server/assets/app.js
  node --check internal/server/assets/richtext.js
}
Remove-Item $Dist -Recurse -Force -ErrorAction SilentlyContinue
New-Item $Dist -ItemType Directory | Out-Null
$targets = @(
  @{OS="linux"; Arch="amd64"; Name="pairroom-v$Version-linux-amd64"},
  @{OS="windows"; Arch="amd64"; Name="pairroom-v$Version-windows-amd64.exe"},
  @{OS="darwin"; Arch="arm64"; Name="pairroom-v$Version-darwin-arm64"},
  @{OS="darwin"; Arch="amd64"; Name="pairroom-v$Version-darwin-amd64"}
)
foreach ($target in $targets) {
  $env:CGO_ENABLED="0"; $env:GOOS=$target.OS; $env:GOARCH=$target.Arch
  go build -buildvcs=false -trimpath -ldflags $Ldflags -o (Join-Path $Dist $target.Name) ./cmd/pairroom
}
Remove-Item Env:GOOS, Env:GOARCH -ErrorAction SilentlyContinue
git archive --format=tar.gz --prefix="pairroom-v$Version/" -o "$Dist/pairroom-v$Version-source.tar.gz" $Commit
git archive --format=zip --prefix="pairroom-v$Version/" -o "$Dist/pairroom-v$Version-source.zip" $Commit
Copy-Item "docs/RELEASE_NOTES_v$Version.md" "$Dist/RELEASE_NOTES.md"
go run ./scripts/releasemeta.go --dist $Dist --version $Version --commit $Commit --build-date $BuildDate
& "$Dist/pairroom-v$Version-windows-amd64.exe" version --json | Set-Content "$Dist/version-check.json"
go run ./scripts/releasemeta.go --dist $Dist --version $Version --commit $Commit --build-date $BuildDate
Write-Host "release artifacts written to $Dist"
