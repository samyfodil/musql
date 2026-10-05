# Downloads the latest musql release for this machine and starts Doom.
#   irm https://raw.githubusercontent.com/samyfodil/musql/main/examples/doom/run.ps1 | iex
$ErrorActionPreference = 'Stop'
$arch = if ($env:PROCESSOR_ARCHITECTURE -eq 'ARM64') { 'arm64' } else { 'amd64' }
$name = "musql-windows-$arch"
$dir = Join-Path $env:LOCALAPPDATA 'musql-release'
New-Item -ItemType Directory -Force $dir | Out-Null
$zip = Join-Path $dir "$name.zip"
Write-Host "downloading $name ..."
Invoke-WebRequest "https://github.com/samyfodil/musql/releases/latest/download/$name.zip" -OutFile $zip
Expand-Archive $zip -DestinationPath $dir -Force
& (Join-Path $dir "$name\musql-doom.exe") @args
