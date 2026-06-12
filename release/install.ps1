# release/install.ps1 — install a PRE-BUILT cc-fleet from a release archive on Windows.
#
# Shipped INSIDE each cc-fleet-windows-<arch>.zip. It copies the prebuilt
# cc-fleet.exe sitting next to it onto your PATH (no Go toolchain, no download),
# creates the ccf.exe alias, and — if Claude Code is present — installs the
# cc-fleet skill via the plugin.
#
#   .\install.ps1
#
# Override the install dir with $env:CCF_PREFIX (default: %LOCALAPPDATA%\cc-fleet\bin).
# Windows PowerShell 5.1 compatible.

Set-StrictMode -Version Latest
$ErrorActionPreference = 'Stop'

$REPO        = 'ethanhq/cc-fleet'   # GitHub owner/repo (plugin marketplace source)
$MARKETPLACE = 'ethanhq'            # claude plugin marketplace name
$PLUGIN      = 'cc-fleet'           # claude plugin name
$SCOPE       = 'user'               # plugin install scope

function Die($msg) { throw "install.ps1: $msg" }

$SCRIPT_DIR = Split-Path -Parent $MyInvocation.MyCommand.Path

if ($env:CCF_PREFIX) {
    $PREFIX = $env:CCF_PREFIX
} else {
    $PREFIX = Join-Path $env:LOCALAPPDATA 'cc-fleet\bin'
}

# --- Sanity check -------------------------------------------------------------

$srcExe = Join-Path $SCRIPT_DIR 'cc-fleet.exe'
if (-not (Test-Path -Path $srcExe -PathType Leaf)) {
    Write-Host "  (expected $srcExe — is this the extracted release archive?)"
    Die "prebuilt 'cc-fleet.exe' not found next to this script"
}

# --- Binary + ccf alias -------------------------------------------------------

New-Item -ItemType Directory -Path $PREFIX -Force | Out-Null
# No symlinks on Windows — copy the exe to both names.
Copy-Item -Path $srcExe -Destination (Join-Path $PREFIX 'cc-fleet.exe') -Force
Copy-Item -Path $srcExe -Destination (Join-Path $PREFIX 'ccf.exe') -Force
Write-Host "==> Installed: $PREFIX\cc-fleet.exe (+ ccf alias)"

# Install manifest (co-located with the binary): `uninstall --all` reads it
# to choose the removal commands; self-update is unsupported on Windows.
$manifest = "{`"method`":`"tarball`",`"plugin_scope`":`"$SCOPE`",`"skill`":`"plugin`"}"
Set-Content -Path (Join-Path $PREFIX '.cc-fleet-install.json') -Value $manifest -Encoding ascii

# --- Skill --------------------------------------------------------------------

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

# --- PATH check ---------------------------------------------------------------

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

# --- Next steps ---------------------------------------------------------------

Write-Host ""
Write-Host "==> Next steps"
Write-Host ""
Write-Host "   cc-fleet             # launch the interactive TUI — register a provider and get started"
Write-Host "                        #   (config is auto-created on first save; no init needed)"
Write-Host "   cc-fleet doctor      # optional: run the health checks"
Write-Host ""
Write-Host "   The live teammate lane needs tmux (unix only); subagent / workflow / run work here."
Write-Host ""
Write-Host "   See README.md in this archive for the full quick-start."
