# FledgeFlow-oss 起動ファイル用：Rust / Go のバージョン確認・更新と、ライブラリの確認。
# start.ps1 から読み込まれます（単独では実行しません）。
#
# - Test-OssRequirements : 必要な版（rust/ffimg/Cargo.toml の rust-version、go/go.mod の go 行）を満たすか確認（オフライン）
# - Show-OssLatest       : 新しい版があるか確認（Rust は rustup check、Go は go.dev の公式一覧。1 日 1 回、結果をキャッシュ）
# - Invoke-OssUpdate     : 確認のうえ Rust（rustup update stable）と Go（winget）を更新
# - Show-OssDependencies : Rust クレートの新しい版と既知の脆弱性、Go の脆弱性を報告（更新はしない）

Set-StrictMode -Version 2.0

$script:OssRoot = Split-Path -Parent (Split-Path -Parent $PSScriptRoot)
$script:OssCache = Join-Path $script:OssRoot 'target\.launcher-version-check.json'

function Write-OssInfo([string]$msg) { Write-Host "  $msg" -ForegroundColor DarkGray }
function Write-OssOk([string]$msg) { Write-Host "  ○ $msg" -ForegroundColor Green }
function Write-OssWarn([string]$msg) { Write-Host "  △ $msg" -ForegroundColor Yellow }
function Write-OssBad([string]$msg) { Write-Host "  × $msg" -ForegroundColor Red }

# "go1.27.3" / "rustc 1.99.1 (…)" / "1.88" → [version]（解釈できなければ $null）
function ConvertTo-OssVersion([string]$text) {
    if (-not $text) { return $null }
    $m = [regex]::Match($text, '(\d+)\.(\d+)(?:\.(\d+))?')
    if (-not $m.Success) { return $null }
    $patch = if ($m.Groups[3].Success) { $m.Groups[3].Value } else { '0' }
    return [version]("$($m.Groups[1].Value).$($m.Groups[2].Value).$patch")
}

# 外部コマンドを実行して出力（文字列）と終了コードを返す。Windows PowerShell 5.1 は
# $ErrorActionPreference = 'Stop' のまま標準エラーをリダイレクトすると、コマンドが
# 何かを標準エラーに書いた時点でスクリプトが止まるため、この間だけ Continue にする。
function Invoke-OssCapture([string]$exe, [string[]]$argList, [switch]$WithStderr) {
    $prev = $ErrorActionPreference
    $ErrorActionPreference = 'Continue'
    try {
        if ($WithStderr) { $out = & $exe @argList 2>&1 | ForEach-Object { "$_" } }
        else { $out = & $exe @argList 2>$null }
        $code = $LASTEXITCODE
        return @{ Text = (($out | Out-String).Trim()); Code = $code }
    } finally {
        $ErrorActionPreference = $prev
    }
}

function Find-OssTool([string]$name, [string[]]$fallbacks) {
    $cmd = Get-Command $name -CommandType Application -ErrorAction SilentlyContinue | Select-Object -First 1
    if ($cmd) { return $cmd.Source }
    foreach ($f in $fallbacks) { if ($f -and (Test-Path -LiteralPath $f)) { return $f } }
    return $null
}

function Get-OssCargoPath { Find-OssTool 'cargo' @("$env:USERPROFILE\.cargo\bin\cargo.exe") }
function Get-OssRustupPath { Find-OssTool 'rustup' @("$env:USERPROFILE\.cargo\bin\rustup.exe") }
function Get-OssGoPath { Find-OssTool 'go' @("$env:ProgramFiles\Go\bin\go.exe", "$env:LOCALAPPDATA\Programs\Go\bin\go.exe") }

# Git for Windows の bash（scripts/leakcheck.sh 用）。System32\bash.exe は WSL なので使わない。
function Get-OssBashPath {
    $git = Find-OssTool 'git' @("$env:ProgramFiles\Git\cmd\git.exe")
    $candidates = @()
    if ($git) { $candidates += (Join-Path (Split-Path -Parent (Split-Path -Parent $git)) 'bin\bash.exe') }
    $candidates += "$env:ProgramFiles\Git\bin\bash.exe"
    foreach ($c in $candidates) { if (Test-Path -LiteralPath $c) { return $c } }
    $cmd = Get-Command 'bash' -CommandType Application -ErrorAction SilentlyContinue |
        Where-Object { $_.Source -notmatch '\\System32\\' } | Select-Object -First 1
    if ($cmd) { return $cmd.Source }
    return $null
}

# 必要な版：rust/ffimg/Cargo.toml の rust-version、go/go.mod の go 行
function Get-OssRequired {
    $req = @{ Rust = $null; Go = $null }
    $cargoToml = Join-Path $script:OssRoot 'rust\ffimg\Cargo.toml'
    if (Test-Path -LiteralPath $cargoToml) {
        $line = Select-String -LiteralPath $cargoToml -Pattern '^rust-version\s*=\s*"([^"]+)"' | Select-Object -First 1
        if ($line) { $req.Rust = ConvertTo-OssVersion $line.Matches[0].Groups[1].Value }
    }
    $gomod = Join-Path $script:OssRoot 'go\go.mod'
    if (Test-Path -LiteralPath $gomod) {
        $line = Select-String -LiteralPath $gomod -Pattern '^go\s+(\S+)' | Select-Object -First 1
        if ($line) { $req.Go = ConvertTo-OssVersion $line.Matches[0].Groups[1].Value }
    }
    return $req
}

function Get-OssInstalled {
    $inst = @{ Rust = $null; Cargo = Get-OssCargoPath; Rustup = Get-OssRustupPath; Toolchain = ''; Go = $null; GoPath = Get-OssGoPath }
    if ($inst.Cargo) {
        $rustc = Join-Path (Split-Path -Parent $inst.Cargo) 'rustc.exe'
        if (-not (Test-Path -LiteralPath $rustc)) { $rustc = Find-OssTool 'rustc' @() }
        if ($rustc) { $inst.Rust = ConvertTo-OssVersion (Invoke-OssCapture $rustc @('--version')).Text }
    }
    if ($inst.Rustup) { $inst.Toolchain = (Invoke-OssCapture $inst.Rustup @('show', 'active-toolchain')).Text }
    if ($inst.GoPath) { $inst.Go = ConvertTo-OssVersion (Invoke-OssCapture $inst.GoPath @('env', 'GOVERSION')).Text }
    return $inst
}

# 必要な版を満たすか確認する。$NeedGo=Go を使う処理か。戻り値：$true=続行可、$false=中止
function Test-OssRequirements([switch]$NeedGo) {
    Write-Host '→ 必要なツールのバージョンを確認しています…' -ForegroundColor Cyan
    $req = Get-OssRequired
    $inst = Get-OssInstalled
    $ok = $true

    # Rust（ffimg のビルドに必須）
    if (-not $inst.Cargo) {
        Write-OssBad 'Rust（cargo）が見つかりません。https://rustup.rs/ から rustup をインストールしてください（または winget install Rustlang.Rustup）。'
        $ok = $false
    } elseif ($req.Rust -and $inst.Rust -and $inst.Rust -lt $req.Rust) {
        Write-OssBad "Rust $($inst.Rust) は古すぎます（必要：$($req.Rust) 以上）。.\start.ps1 -Update で更新してください。"
        $ok = $false
    } else {
        Write-OssOk "Rust $($inst.Rust)（必要：$($req.Rust) 以上）"
        # このリポジトリは C コンパイラなしでビルドできる gnu ツールチェーンを想定（README 参照）
        if ($inst.Toolchain -match 'msvc') {
            Write-OssInfo "ツールチェーン：$($inst.Toolchain)（msvc はリンクに Visual Studio Build Tools が必要です。無い場合は rustup default stable-x86_64-pc-windows-gnu）"
        } elseif ($inst.Toolchain) {
            Write-OssInfo "ツールチェーン：$($inst.Toolchain)"
        }
    }

    # Go（Go パッケージの検査にだけ必要）
    if (-not $inst.GoPath) {
        $msg = 'Go が見つかりません。https://go.dev/dl/ からインストールしてください（または winget install GoLang.Go）。'
        if ($NeedGo) { Write-OssBad $msg; $ok = $false } else { Write-OssWarn "$msg（Go パッケージの検査に使います）" }
    } elseif ($req.Go -and $inst.Go -and $inst.Go -lt $req.Go) {
        $tc = (Invoke-OssCapture $inst.GoPath @('env', 'GOTOOLCHAIN')).Text
        if ($tc -match 'local') {
            $msg = "Go $($inst.Go) は古すぎます（必要：$($req.Go) 以上）。.\start.ps1 -Update で更新してください。"
            if ($NeedGo) { Write-OssBad $msg; $ok = $false } else { Write-OssWarn $msg }
        } else {
            Write-OssWarn "Go $($inst.Go)（必要：$($req.Go) 以上）。実行時に Go が必要な版を公式配布元から自動で取得します。"
        }
    } else {
        Write-OssOk "Go $($inst.Go)（必要：$($req.Go) 以上）"
    }

    # 任意のツール（なくても動く）
    if (Get-OssBashPath) { Write-OssOk 'Git Bash（混入検査に使います）' }
    else { Write-OssInfo 'Git Bash（任意）：なし → 混入検査（scripts/leakcheck.sh）を省略します。Git for Windows を入れると使えます。' }
    if ($NeedGo -and $inst.GoPath) {
        if (Find-OssTool 'gcc' @()) { Write-OssOk 'gcc（Go の -race 検査に使います）' }
        else { Write-OssInfo 'gcc（任意）：なし → Go の -race 検査は省略します（CI の Linux で実行されます）。' }
    }
    Write-OssInfo 'ライブラリ（Rust クレート）は Cargo.lock どおりの版をビルド時に自動で取得します（Go はサードパーティ依存なし）。'
    return $ok
}

# ---------- 最新版の確認（1 日 1 回） ----------

function Enable-OssTls {
    try {
        $p = [Net.ServicePointManager]::SecurityProtocol
        $p = $p -bor [Net.SecurityProtocolType]::Tls12
        if ([enum]::GetNames([Net.SecurityProtocolType]) -contains 'Tls13') { $p = $p -bor [Net.SecurityProtocolType]::Tls13 }
        [Net.ServicePointManager]::SecurityProtocol = $p
    } catch { }
}

function Get-OssLatestOnline {
    Enable-OssTls
    $latest = @{ CheckedAt = (Get-Date).ToUniversalTime().ToString('o'); Go = $null; RustUpdate = $null }
    # Go：安定版の一覧（新しい順）
    try {
        $go = Invoke-RestMethod -Uri 'https://go.dev/dl/?mode=json' -TimeoutSec 8 -UseBasicParsing
        $stable = @($go | Where-Object { $_.stable }) | Select-Object -First 1
        if ($stable) { $latest.Go = [string]$stable.version }
    } catch { }
    # Rust：rustup が配布元に問い合わせる（"stable-… - Update available : 1.99.1 -> 1.100.0"）
    $rustup = Get-OssRustupPath
    if ($rustup) {
        $out = (Invoke-OssCapture $rustup @('check')).Text
        $m = [regex]::Match($out, 'stable[^\r\n]*Update available\s*:\s*([\d.]+)[^\d\r\n]+([\d.]+)')
        if ($m.Success) { $latest.RustUpdate = $m.Groups[2].Value }
        elseif ($out -match 'stable[^\r\n]*Up to date') { $latest.RustUpdate = '' }
    }
    if (-not $latest.Go -and $null -eq $latest.RustUpdate) { throw 'offline' }
    return $latest
}

# キャッシュが 20 時間以内ならそれを使う。-Force で必ず問い合わせる。
function Get-OssLatest([switch]$Force) {
    if (-not $Force -and (Test-Path -LiteralPath $script:OssCache)) {
        try {
            $c = Get-Content -LiteralPath $script:OssCache -Raw -Encoding UTF8 | ConvertFrom-Json
            $age = (Get-Date).ToUniversalTime() - [datetime]::Parse($c.CheckedAt).ToUniversalTime()
            if ($age.TotalHours -lt 20) { return @{ Go = $c.Go; RustUpdate = $c.RustUpdate; Cached = $true } }
        } catch { }
    }
    try {
        $latest = Get-OssLatestOnline
        $dir = Split-Path -Parent $script:OssCache
        if (-not (Test-Path -LiteralPath $dir)) { New-Item -ItemType Directory -Path $dir | Out-Null }
        ($latest | ConvertTo-Json) | Set-Content -LiteralPath $script:OssCache -Encoding UTF8
        $latest.Cached = $false
        return $latest
    } catch {
        return $null
    }
}

# 新しい版があるか表示する。戻り値：更新候補の一覧（Rust / Go）
function Show-OssLatest([switch]$Force) {
    Write-Host '→ 新しいバージョンを確認しています（rustup / go.dev）…' -ForegroundColor Cyan
    $latest = Get-OssLatest -Force:$Force
    if (-not $latest) {
        Write-OssInfo '確認できませんでした（オフラインまたは接続先に届きません）。ビルドには影響しません。'
        return @()
    }
    $inst = Get-OssInstalled
    $updates = @()
    if ($latest.RustUpdate) {
        Write-OssWarn "Rust $($latest.RustUpdate) が出ています（現在 $($inst.Rust)）。"
        $updates += 'Rust'
    } elseif ($null -ne $latest.RustUpdate -and $inst.Rust) {
        Write-OssOk "Rust は最新です（$($inst.Rust)）。"
    }
    $goLatest = ConvertTo-OssVersion $latest.Go
    if ($inst.Go -and $goLatest -and $goLatest -gt $inst.Go) {
        Write-OssWarn "Go $goLatest が出ています（現在 $($inst.Go)）。"
        $updates += 'Go'
    } elseif ($inst.Go -and $goLatest) {
        Write-OssOk "Go は最新です（$($inst.Go)）。"
    }
    if ($updates.Count -gt 0) { Write-OssInfo '更新するには：.\start.ps1 -Update（確認してから更新します）' }
    if ($latest.Cached) { Write-OssInfo '（今日すでに確認した結果です。今すぐ確認し直すには -CheckLatest）' }
    return $updates
}

# ---------- 更新 ----------

function Test-OssWingetManaged([string]$id, [string]$winget) {
    $r = Invoke-OssCapture $winget @('list', '--id', $id, '-e', '--disable-interactivity')
    return ($r.Code -eq 0) -and ($r.Text -match [regex]::Escape($id))
}

function Invoke-OssUpdate {
    $updates = @(Show-OssLatest -Force)
    if ($updates.Count -eq 0) {
        Write-OssInfo '更新が必要なツールはありません。'
        return
    }
    Write-Host ''
    Write-Host '次のツールを更新します：' -ForegroundColor Cyan
    foreach ($u in $updates) {
        if ($u -eq 'Rust') { Write-Host '  - Rust（rustup update stable）' } else { Write-Host '  - Go（winget：GoLang.Go）' }
    }
    Write-OssInfo 'winget が利用規約の確認を求めた場合は、内容を確認して答えてください。'
    $answer = Read-Host '更新しますか？ (y/N)'
    if ($answer -notmatch '^(y|yes)$') { Write-OssInfo '更新しませんでした。'; return }
    foreach ($u in $updates) {
        if ($u -eq 'Rust') {
            $rustup = Get-OssRustupPath
            Write-Host '→ Rust を更新しています…' -ForegroundColor Cyan
            & $rustup update stable
            if ($LASTEXITCODE -eq 0) { Write-OssOk 'Rust を更新しました。' }
            else { Write-OssWarn "Rust の更新に失敗しました（終了コード $LASTEXITCODE）。rustup update stable を手動で実行してください。" }
        } else {
            $winget = Find-OssTool 'winget' @("$env:LOCALAPPDATA\Microsoft\WindowsApps\winget.exe")
            if ($winget -and (Test-OssWingetManaged 'GoLang.Go' $winget)) {
                Write-Host '→ Go を更新しています…' -ForegroundColor Cyan
                & $winget upgrade --id GoLang.Go -e
                if ($LASTEXITCODE -eq 0) { Write-OssOk 'Go を更新しました。' }
                else { Write-OssWarn "Go の更新に失敗しました（終了コード $LASTEXITCODE）。https://go.dev/dl/ から手動で更新してください。" }
            } else {
                Write-OssWarn 'Go は winget で管理されていません。https://go.dev/dl/ から最新版をインストールしてください。'
            }
        }
    }
    Write-OssInfo '更新したツールを使うには、このウィンドウを閉じて起動し直してください。'
}

# ---------- ライブラリの確認（報告のみ） ----------

function Show-OssDependencies {
    Write-Host '→ ライブラリの新しい版と既知の脆弱性を確認しています（更新はしません）…' -ForegroundColor Cyan
    Push-Location -LiteralPath $script:OssRoot
    try {
        $cargo = Get-OssCargoPath
        if ($cargo) {
            # cargo update --dry-run は Cargo.lock を変えずに「上げられる版」を表示する
            $r = Invoke-OssCapture $cargo @('update', '--dry-run') -WithStderr
            $lines = @($r.Text -split "`r?`n" | Where-Object { $_ -match '^\s*(Updating|Upgrading)\s+\S+\s+v' })
            if ($lines.Count -gt 0) { Write-OssWarn "Rust クレートに新しい版：$($lines.Count) 件"; $lines | ForEach-Object { Write-OssInfo $_.Trim() } }
            elseif ($r.Code -eq 0) { Write-OssOk 'Rust クレートは Cargo.lock の範囲で最新です。' }
            else { Write-OssWarn 'cargo update --dry-run が失敗しました（オフラインの可能性）。'; Write-Host $r.Text }
            $audit = Invoke-OssCapture $cargo @('audit', '--version')
            if ($audit.Code -eq 0 -and $audit.Text) {
                $r = Invoke-OssCapture $cargo @('audit') -WithStderr
                if ($r.Code -eq 0) { Write-OssOk 'Rust：既知の脆弱性はありません（cargo audit）。' }
                else { Write-OssWarn 'Rust：cargo audit が問題を報告しました。'; Write-Host $r.Text }
            } else {
                Write-OssInfo 'cargo-audit が未インストールのため、Rust の脆弱性確認を省略しました（cargo install cargo-audit）。'
            }
        }
        $go = Get-OssGoPath
        if ($go) {
            Write-OssOk 'Go パッケージはサードパーティ依存がありません（go.mod）。'
            $gopath = (Invoke-OssCapture $go @('env', 'GOPATH')).Text
            $vuln = Join-Path $gopath 'bin\govulncheck.exe'
            if (Test-Path -LiteralPath $vuln) {
                Push-Location -LiteralPath (Join-Path $script:OssRoot 'go')
                try { $out = (Invoke-OssCapture $vuln @('./...') -WithStderr).Text } finally { Pop-Location }
                if ($out -match 'No vulnerabilities found') { Write-OssOk 'Go：既知の脆弱性はありません（govulncheck、標準ライブラリを含む）。' }
                else { Write-OssWarn 'Go：govulncheck が問題を報告しました。'; Write-Host $out }
            } else {
                Write-OssInfo 'govulncheck が未インストールのため、Go の脆弱性確認を省略しました（go install golang.org/x/vuln/cmd/govulncheck@latest）。'
            }
        }
    } finally {
        Pop-Location
    }
    Write-OssInfo 'ライブラリの更新は、Cargo.lock を更新して検査一式（.\start.ps1 -Check）を通す開発作業として行ってください。'
}
