<#
.SYNOPSIS
  Installs the Openite command-line tool for the current user (no administrator needed).
  Optionally installs apps right away.

.DESCRIPTION
  What this script does, in full:
    1. downloads openite-windows-<arch>.exe and SHA256SUMS from the project's GitHub release (HTTPS),
    2. checks the file against SHA256SUMS (and GitHub's build attestation if the `gh` CLI is present),
    3. copies it to %LOCALAPPDATA%\Programs\Openite and adds that folder to YOUR user PATH,
    4. only if you passed -Apps / -Preset: runs `openite install ...` (which asks first unless -Yes).
  It does not touch system settings, run other downloaded code, or need admin rights.
  Read it before running it: that is the right habit for any `irm | iex` line.

.EXAMPLE
  # install the tool, then look around
  irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1 | iex
  openite pick

.EXAMPLE
  # install the tool and a preset in one go (pin a version for repeatability)
  & ([scriptblock]::Create((irm https://raw.githubusercontent.com/kstasielowicz/openite/main/install.ps1))) -Preset developer -Apps vivaldi -Yes
#>
param(
  [string]$Version = "latest",      # e.g. v0.3.0 (recommended: pin one)
  [string[]]$Apps,                  # e.g. -Apps firefox,vlc
  [string[]]$Preset,                # e.g. -Preset essentials,gaming
  [switch]$Yes,                     # don't ask before installing the apps
  [switch]$NoPath,                  # don't modify PATH
  [string]$Repo = "kstasielowicz/openite",# owner/name of the GitHub repository
  [string]$BaseUrl,                 # download from here instead of GitHub (e.g. an internal mirror); checksum still enforced
  [string]$InstallDir               # default: %LOCALAPPDATA%\Programs\Openite
)
$ErrorActionPreference = "Stop"
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

$arch = switch ($env:PROCESSOR_ARCHITECTURE) { "AMD64" { "amd64" } "ARM64" { "arm64" } default { throw "Unsupported CPU architecture: $env:PROCESSOR_ARCHITECTURE" } }
$asset = "openite-windows-$arch.exe"
$base = if ($BaseUrl) { $BaseUrl.TrimEnd("/") } elseif ($Version -eq "latest") { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("openite-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Write-Host "Downloading $asset ($Version)..."
  $file = Join-Path $tmp $asset
  Invoke-WebRequest "$base/$asset" -OutFile $file -UseBasicParsing
  Invoke-WebRequest "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS") -UseBasicParsing

  $line = Get-Content (Join-Path $tmp "SHA256SUMS") | Where-Object { $_ -match "\s\*?$([regex]::Escape($asset))$" } | Select-Object -First 1
  if (-not $line) { throw "SHA256SUMS has no entry for $asset. Aborting." }
  $expected = ($line -split "\s+")[0].ToLower()
  $actual = (Get-FileHash $file -Algorithm SHA256).Hash.ToLower()
  if ($expected -ne $actual) { throw "Checksum mismatch for $asset!`n expected $expected`n got      $actual`nNot installing." }
  Write-Host "Checksum OK ($actual)."

  if (Get-Command gh -ErrorAction SilentlyContinue) {
    & gh attestation verify $file --repo $Repo *> $null
    if ($LASTEXITCODE -eq 0) { Write-Host "GitHub build attestation verified." }
    else { throw "GitHub build attestation check FAILED for $asset. Not installing." }
  } else {
    Write-Host "(Tip: install the GitHub CLI and this script will also verify the signed build attestation.)"
  }

  $dest = if ($InstallDir) { $InstallDir } else { Join-Path $env:LOCALAPPDATA "Programs\Openite" }
  New-Item -ItemType Directory -Path $dest -Force | Out-Null
  $exe = Join-Path $dest "openite.exe"
  Copy-Item $file $exe -Force
  Write-Host "Installed to $exe"

  if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -notcontains $dest) {
      [Environment]::SetEnvironmentVariable("Path", (($userPath.TrimEnd(";") + ";" + $dest).TrimStart(";")), "User")
      Write-Host "Added to your user PATH (open a new terminal to use `openite` directly)."
    }
  }
  $env:Path += ";$dest"
} finally {
  Remove-Item $tmp -Recurse -Force -ErrorAction SilentlyContinue
}

if ($Apps -or $Preset) {
  $a = @("install") + @($Apps | Where-Object { $_ })
  foreach ($p in @($Preset | Where-Object { $_ })) { $a += @("--preset", $p) }
  if ($Yes) { $a += "-y" }
  & $exe @a
} else {
  Write-Host "`nDone. Try:  openite pick        (choose apps interactively)`n            openite ui          (web interface)`n            openite help"
}
