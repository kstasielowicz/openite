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
  [string]$Version = "latest",          # e.g. v0.5.0; default "latest" = newest published release
  [string[]]$Apps,                      # e.g. -Apps firefox,vlc
  [string[]]$Preset,                    # e.g. -Preset essentials,gaming
  [switch]$Yes,                         # don't ask before installing the apps
  [switch]$NoPath,                      # don't modify PATH
  [string]$Repo = "kstasielowicz/openite", # owner/name of the GitHub repository
  [string]$BaseUrl,                     # download from here instead of GitHub (e.g. an internal mirror); checksum still enforced
  [string]$InstallDir                   # default: %LOCALAPPDATA%\Programs\Openite
)
$ErrorActionPreference = "Stop"
$ProgressPreference = "SilentlyContinue"   # the progress bar makes Windows PowerShell 5.1 downloads many times slower
[Net.ServicePointManager]::SecurityProtocol = [Net.SecurityProtocolType]::Tls12

# ---- small UI helpers (plain ASCII markers so every console font renders them) ----
function Say($text, $color = "Gray") { Write-Host $text -ForegroundColor $color }
function Step($n, $total, $text) { Write-Host ("  [{0}/{1}] " -f $n, $total) -ForegroundColor Cyan -NoNewline; Write-Host $text -NoNewline }
function Done($extra = "") { Write-Host "  OK" -ForegroundColor Green -NoNewline; if ($extra) { Write-Host "  $extra" -ForegroundColor DarkGray } else { Write-Host "" } }
function Fail($text) { Write-Host "  FAILED" -ForegroundColor Red; throw $text }

Write-Host ""
Write-Host "  Openite installer" -ForegroundColor Cyan
Write-Host ("  " + ("-" * 52)) -ForegroundColor DarkGray

$arch = switch ($env:PROCESSOR_ARCHITECTURE) { "AMD64" { "amd64" } "ARM64" { "arm64" } default { throw "Unsupported CPU architecture: $env:PROCESSOR_ARCHITECTURE" } }
$asset = "openite-windows-$arch.exe"
$base = if ($BaseUrl) { $BaseUrl.TrimEnd("/") } elseif ($Version -eq "latest") { "https://github.com/$Repo/releases/latest/download" } else { "https://github.com/$Repo/releases/download/$Version" }
$total = 4

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("openite-" + [Guid]::NewGuid())
New-Item -ItemType Directory -Path $tmp | Out-Null
try {
  Step 1 $total "Downloading $asset ($Version) ..."
  $file = Join-Path $tmp $asset
  try {
    Invoke-WebRequest "$base/$asset" -OutFile $file -UseBasicParsing
    Invoke-WebRequest "$base/SHA256SUMS" -OutFile (Join-Path $tmp "SHA256SUMS") -UseBasicParsing
  } catch { Fail "Download failed: $($_.Exception.Message)`n  Is the release published and the repository public?" }
  Done ("{0:N1} MB" -f ((Get-Item $file).Length / 1MB))

  Step 2 $total "Verifying checksum ..."
  $line = Get-Content (Join-Path $tmp "SHA256SUMS") | Where-Object { $_ -match "\s\*?$([regex]::Escape($asset))$" } | Select-Object -First 1
  if (-not $line) { Fail "SHA256SUMS has no entry for $asset. Aborting." }
  $expected = ($line -split "\s+")[0].ToLower()
  $actual = (Get-FileHash $file -Algorithm SHA256).Hash.ToLower()
  if ($expected -ne $actual) { Fail "Checksum mismatch for $asset!`n  expected $expected`n  got      $actual`n  Not installing." }
  Done "SHA-256 matches"

  Step 3 $total "Checking build attestation ..."
  if (Get-Command gh -ErrorAction SilentlyContinue) {
    & gh attestation verify $file --repo $Repo *> $null
    if ($LASTEXITCODE -eq 0) { Done "signed by GitHub Actions" }
    else { Fail "GitHub build attestation check FAILED for $asset. Not installing." }
  } else {
    Write-Host "  skipped" -ForegroundColor Yellow -NoNewline; Write-Host "  (install the GitHub CLI `gh` to verify this too)" -ForegroundColor DarkGray
  }

  Step 4 $total "Installing ..."
  $dest = if ($InstallDir) { $InstallDir } else { Join-Path $env:LOCALAPPDATA "Programs\Openite" }
  New-Item -ItemType Directory -Path $dest -Force | Out-Null
  $exe = Join-Path $dest "openite.exe"
  try { Copy-Item $file $exe -Force } catch { Fail "Could not write $exe (is openite running? close it and retry)." }
  Done $dest

  if (-not $NoPath) {
    $userPath = [Environment]::GetEnvironmentVariable("Path", "User")
    if (($userPath -split ";") -notcontains $dest) {
      [Environment]::SetEnvironmentVariable("Path", (($userPath.TrimEnd(";") + ";" + $dest).TrimStart(";")), "User")
      Say "        Added to your PATH. Open a NEW terminal to use 'openite' directly." DarkGray
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
  Write-Host ""
  Write-Host "  Ready. Try one of these:" -ForegroundColor Green
  Write-Host "    openite pick     " -ForegroundColor Cyan -NoNewline; Write-Host "choose presets and apps with the arrow keys"
  Write-Host "    openite ui       " -ForegroundColor Cyan -NoNewline; Write-Host "open the web interface"
  Write-Host "    openite help     " -ForegroundColor Cyan -NoNewline; Write-Host "all commands"
  Write-Host ""
}
