<#
Builds one self-contained Windows executable, build/standalone/Piik.exe, plus
Piik-Guide.md beside it. The executable embeds the web UI, the native capture
helper and the public-link tunnel; on first run it unpacks the helpers into a
runtime/ folder next to itself (see internal/app/payload).

Inputs (the same files a normal run uses):
  runtime/native/piik-capture.exe        capture launcher
  runtime/native/piik-capture-bin.exe    native capture + encode helper
  runtime/tunnel/cloudflared.exe         public-link tunnel
  runtime/tunnel/THIRD-PARTY-NOTICES.txt

Usage: pwsh scripts/build-standalone.ps1 [-OutputDirectory build/standalone] [-RebuildHelper]
  -RebuildHelper  rebuild the native helper from native/capture/windows first
#>
param(
  [string]$OutputDirectory = 'build/standalone',
  [switch]$RebuildHelper
)
$ErrorActionPreference = 'Stop'
$root = (Resolve-Path (Join-Path $PSScriptRoot '..')).Path
Set-Location $root

if ($RebuildHelper) {
  & pwsh -NoProfile -File native/capture/windows/build.ps1 -OutputDirectory build/native-release
  if ($LASTEXITCODE -ne 0) { throw 'native helper build failed' }
  Copy-Item build/native-release/piik-capture.exe runtime/native/piik-capture-bin.exe -Force
}

$payload = [ordered]@{
  'native/piik-capture.exe'         = 'runtime/native/piik-capture.exe'
  'native/piik-capture-bin.exe'     = 'runtime/native/piik-capture-bin.exe'
  'tunnel/cloudflared.exe'          = 'runtime/tunnel/cloudflared.exe'
  'tunnel/THIRD-PARTY-NOTICES.txt'  = 'runtime/tunnel/THIRD-PARTY-NOTICES.txt'
}
foreach ($source in $payload.Values) {
  if (-not (Test-Path $source)) { throw "missing $source" }
}

Write-Host 'Building the web UI...'
npm run build:web
if ($LASTEXITCODE -ne 0) { throw 'web build failed' }

$files = Join-Path $root 'internal/app/payload/files'
if (Test-Path $files) { Remove-Item $files -Recurse -Force }
foreach ($entry in $payload.GetEnumerator()) {
  $target = Join-Path $files ($entry.Key + '.gz')
  New-Item -ItemType Directory -Force -Path (Split-Path $target) | Out-Null
  $in = [IO.File]::OpenRead((Join-Path $root $entry.Value))
  $out = [IO.File]::Create($target)
  $zip = New-Object IO.Compression.GZipStream($out, [IO.Compression.CompressionLevel]::Optimal)
  $in.CopyTo($zip); $zip.Dispose(); $out.Dispose(); $in.Dispose()
  Write-Host ("  embedded {0} ({1:N1} MB -> {2:N1} MB)" -f $entry.Key, ((Get-Item (Join-Path $root $entry.Value)).Length / 1MB), ((Get-Item $target).Length / 1MB))
}

New-Item -ItemType Directory -Force -Path $OutputDirectory | Out-Null
$exe = Join-Path $OutputDirectory 'Piik.exe'
Write-Host 'Building Piik.exe...'
go build -tags standalone -trimpath -ldflags '-s -w' -o $exe ./cmd/piik-app
if ($LASTEXITCODE -ne 0) { throw 'go build failed' }
Copy-Item standalone/Piik-Guide.md (Join-Path $OutputDirectory 'Piik-Guide.md') -Force
Remove-Item $files -Recurse -Force
Write-Host ("Done: {0} ({1:N1} MB)" -f $exe, ((Get-Item $exe).Length / 1MB))
