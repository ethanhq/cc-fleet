# install.ps1 — one-line installer for cc-fleet on Windows.
#
# Downloads a prebuilt binary from GitHub Releases (no Go toolchain, no clone),
# installs cc-fleet.exe plus the ccf.exe alias, and — if Claude Code is present —
# installs the cc-fleet skill via the plugin.
#
#   irm https://raw.githubusercontent.com/ethanhq/cc-fleet/main/install.ps1 | iex
#
# Args don't survive `irm | iex`; use env vars to override the defaults:
#   $env:CCF_VERSION  = 'vX.Y.Z'   # install a specific release (default: latest)
#   $env:CCF_PREFIX   = 'C:\dir'   # install dir (default: %LOCALAPPDATA%\cc-fleet\bin)
#   $env:CCF_BASE_URL = '...'      # asset base (mirror or local file:/// test)
#
# Windows PowerShell 5.1 compatible. The body runs in a child scope so that under
# `irm | iex` the strict mode / error preference don't leak into the caller's
# session, and a failure can't close the caller's console.

& {

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'
[Net.ServicePointManager]::SecurityProtocol = [Net.ServicePointManager]::SecurityProtocol -bor [Net.SecurityProtocolType]::Tls12

$REPO        = 'ethanhq/cc-fleet'   # GitHub owner/repo (Release source + plugin marketplace)
$MARKETPLACE = 'ethanhq'            # claude plugin marketplace name
$PLUGIN      = 'cc-fleet'           # claude plugin name
$SCOPE       = 'user'               # plugin install scope

function Die($msg) { throw "install.ps1: $msg" }

# --- platform detection -------------------------------------------------------

switch ($env:PROCESSOR_ARCHITECTURE) {
    'AMD64' { $arch = 'amd64' }
    'ARM64' { $arch = 'arm64' }
    default { Die "unsupported architecture '$($env:PROCESSOR_ARCHITECTURE)' (cc-fleet supports amd64 and arm64)" }
}

# --- prefix -------------------------------------------------------------------

if ($env:CCF_PREFIX) {
    $PREFIX = $env:CCF_PREFIX
} else {
    $PREFIX = Join-Path $env:LOCALAPPDATA 'cc-fleet\bin'
}

# --- resolve version ----------------------------------------------------------

$VERSION = $env:CCF_VERSION
if ($VERSION -and ($VERSION -notlike 'v*')) { $VERSION = "v$VERSION" }

if (-not $VERSION -and -not $env:CCF_BASE_URL) {
    # Read the tag from the /releases/latest redirect — no JSON parsing needed.
    # A 3xx with -MaximumRedirection 0 throws, so the Location usually arrives on
    # the exception's response: a WebException carries a header collection (5.1),
    # an HttpResponseException a typed Location property (7+).
    $loc = $null
    try {
        $resp = Invoke-WebRequest -Uri "https://github.com/$REPO/releases/latest" -MaximumRedirection 0 -UseBasicParsing
        $loc = $resp.Headers['Location']
    } catch {
        $r = $_.Exception.Response
        if ($r -is [System.Net.HttpWebResponse]) {
            $loc = $r.Headers['Location']
        } elseif ($r) {
            $loc = "$($r.Headers.Location)"
        }
    }
    if ($loc) { $VERSION = ($loc -split '/tag/')[-1] }
    if (-not $VERSION) { Die "could not resolve the latest version; set `$env:CCF_VERSION = 'vX.Y.Z'" }
}

# Asset base: the dir holding the zip + checksums.txt. CCF_BASE_URL overrides it
# for a mirror or a local test (e.g. file:///path/to/dist).
if ($env:CCF_BASE_URL) {
    $ASSET_BASE = $env:CCF_BASE_URL
} else {
    $ASSET_BASE = "https://github.com/$REPO/releases/download/$VERSION"
}
$ZIP = "cc-fleet-windows-$arch.zip"

# --- download + verify --------------------------------------------------------

$tmp = Join-Path ([IO.Path]::GetTempPath()) ("cc-fleet-" + [Guid]::NewGuid().ToString('N'))
New-Item -ItemType Directory -Path $tmp -Force | Out-Null
try {
    $zipPath = Join-Path $tmp $ZIP
    $sumPath = Join-Path $tmp 'checksums.txt'

    $verLabel = if ($VERSION) { $VERSION } else { 'local' }
    Write-Host "==> Downloading $ZIP ($verLabel)"
    Invoke-WebRequest -Uri "$ASSET_BASE/$ZIP" -OutFile $zipPath -UseBasicParsing
    Invoke-WebRequest -Uri "$ASSET_BASE/checksums.txt" -OutFile $sumPath -UseBasicParsing

    $expected = $null
    foreach ($line in Get-Content $sumPath) {
        $parts = $line -split '\s+', 2
        if ($parts.Count -eq 2 -and $parts[1].Trim() -eq $ZIP) { $expected = $parts[0].Trim(); break }
    }
    if (-not $expected) { Die "no checksum for $ZIP in checksums.txt" }
    $actual = (Get-FileHash -Algorithm SHA256 -Path $zipPath).Hash
    if ($expected -ne $actual) {
        Write-Host "  expected $expected"
        Write-Host "  actual   $actual"
        Die "checksum mismatch for $ZIP"
    }
    Write-Host "==> Checksum OK"

    # --- extract + install binary ---------------------------------------------

    Expand-Archive -Path $zipPath -DestinationPath $tmp -Force
    $extract = Join-Path $tmp "cc-fleet-windows-$arch"   # archive wraps in this dir

    New-Item -ItemType Directory -Path $PREFIX -Force | Out-Null
    # No symlinks on Windows — copy the exe to both names.
    Copy-Item -Path (Join-Path $extract 'cc-fleet.exe') -Destination (Join-Path $PREFIX 'cc-fleet.exe') -Force
    Copy-Item -Path (Join-Path $extract 'cc-fleet.exe') -Destination (Join-Path $PREFIX 'ccf.exe') -Force
    Write-Host "==> Installed $PREFIX\cc-fleet.exe (+ ccf alias)"

    # Install manifest (co-located with the binary): `uninstall --all` reads it
    # to choose the removal commands; self-update is unsupported on Windows.
    $manifest = "{`"method`":`"tarball`",`"plugin_scope`":`"$SCOPE`",`"skill`":`"plugin`"}"
    Set-Content -Path (Join-Path $PREFIX '.cc-fleet-install.json') -Value $manifest -Encoding ascii
} finally {
    Remove-Item -Recurse -Force $tmp -ErrorAction SilentlyContinue
}

# --- skill --------------------------------------------------------------------

if (Get-Command claude -ErrorAction SilentlyContinue) {
    # Redirected native stderr becomes error records in 5.1, which 'Stop' would
    # turn terminating — an already-added marketplace must stay a no-op.
    $prevEap = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    claude plugin marketplace add $REPO --scope $SCOPE 2>$null | Out-Null
    $ErrorActionPreference = $prevEap
    claude plugin install "$PLUGIN@$MARKETPLACE" --scope $SCOPE
    if ($LASTEXITCODE -eq 0) {
        Write-Host "==> Installed the cc-fleet skill via plugin (scope: $SCOPE)"
        Write-Host "    uninstall: claude plugin uninstall $PLUGIN@$MARKETPLACE"
    } else {
        Write-Host "==> Could not install the plugin automatically. To add the skill, run:"
        Write-Host "    claude plugin marketplace add $REPO --scope $SCOPE"
        Write-Host "    claude plugin install $PLUGIN@$MARKETPLACE --scope $SCOPE"
    }
} else {
    Write-Host "==> 'claude' not on PATH — skipped plugin install. To add the skill later:"
    Write-Host "    claude plugin marketplace add $REPO --scope $SCOPE"
    Write-Host "    claude plugin install $PLUGIN@$MARKETPLACE --scope $SCOPE"
}

# --- Claude Code precondition note --------------------------------------------

if (-not (Get-Command claude -ErrorAction SilentlyContinue)) {
    Write-Host ""
    Write-Host "==> Note: Claude Code ('claude') was not found on PATH."
    Write-Host "    cc-fleet drives Claude Code — install it to run subagents / workflows:"
    Write-Host "    https://docs.anthropic.com/claude-code"
}

# --- PATH check + next steps --------------------------------------------------

$userPath = [Environment]::GetEnvironmentVariable('Path', 'User')
$onPath = $false
if ($userPath) {
    foreach ($p in ($userPath -split ';')) {
        if ($p.Trim().TrimEnd('\') -ieq $PREFIX.TrimEnd('\')) { $onPath = $true; break }
    }
}
if ($onPath) {
    Write-Host "==> $PREFIX is already on your user PATH."
} else {
    if ($userPath) { $newPath = "$userPath;$PREFIX" } else { $newPath = $PREFIX }
    [Environment]::SetEnvironmentVariable('Path', $newPath, 'User')
    Write-Host ""
    Write-Host "==> Added $PREFIX to your user PATH. Open a new terminal for it to take effect."
}

Write-Host ""
Write-Host "==> Next steps"
Write-Host ""
Write-Host "   cc-fleet             # launch the interactive TUI — register a provider and get started"
Write-Host "                        #   (config is auto-created on first save; no init needed)"
Write-Host "   cc-fleet doctor      # optional: run the health checks"
Write-Host ""
Write-Host "   The live teammate lane needs tmux (unix only); subagent / workflow / run work here."

}
