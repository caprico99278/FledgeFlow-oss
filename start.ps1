<#
.SYNOPSIS
  FledgeFlow-oss をこの PC で使える状態にします（バージョン確認 → ffimg のビルド → 合成画像での動作確認）。

.DESCRIPTION
  - Rust / Go が必要なバージョン（rust/ffimg/Cargo.toml と go/go.mod から読み取り）を満たすか、毎回確認します。
  - 新しい Rust / Go が出ていないか、1 日 1 回確認してお知らせします（rustup check / go.dev の公式一覧）。
  - ffimg をリリースビルドし（変更がなければ数秒で終わります）、バージョンの出力を Cargo.toml と照合します。
  - 実行時に生成した合成画像（実在の画像は使いません）で、inspect と derive を実際に試し、
    「メタデータが除去される」「不正な入力は拒否される」ことを確かめて結果を表示します。
  - 作った画像は samples フォルダ（Git の管理外）に置きます。リポジトリには何も追加しません。

.PARAMETER Check
  PR 前の検査一式を実行し、結果を一覧で表示します（cargo fmt / clippy / test、gofmt / go vet / go test、
  ベンチマークの 1 回実行、混入検査）。1 つでも失敗すると終了コード 1 で終わります。
.PARAMETER Image
  手元の画像ファイルを ffimg で検証し、縮小・メタデータ除去した派生画像を samples フォルダに作ります
  （元の画像は変更しません）。
.PARAMETER MaxEdge
  -Image で作る派生画像の長辺（既定 1600 px。拡大はしません）。
.PARAMETER Format
  -Image で作る派生画像の形式（jpeg / png / webp。既定 jpeg）。
.PARAMETER Bench
  ベンチマークを実行します（ffimg：合成画像 4000×3000、Go：全パッケージ）。結果の見方は docs/benchmarks.md。
.PARAMETER SkipBuild
  確認とビルドをせず、前回作った ffimg をそのまま使います。
.PARAMETER NoUpdateCheck
  新しいバージョンの確認（インターネットへの問い合わせ）をしません。
.PARAMETER CheckLatest
  1 日 1 回の制限を無視して、新しいバージョンを今すぐ確認します。
.PARAMETER Update
  ほかの処理をせず、Rust / Go に新しい版があれば確認のうえ更新します（rustup update / winget）。
.PARAMETER CheckDeps
  ほかの処理をせず、ライブラリ（Rust クレート）の新しい版と既知の脆弱性を報告します（更新はしません）。

.EXAMPLE
  .\start.ps1
.EXAMPLE
  .\start.ps1 -Check
.EXAMPLE
  .\start.ps1 -Image C:\Users\me\Pictures\photo.jpg -MaxEdge 1200
.EXAMPLE
  .\start.ps1 -Update
#>
[CmdletBinding()]
param(
    [switch]$Check,
    [string]$Image,
    [ValidateRange(1, 65535)][int]$MaxEdge = 1600,
    [ValidateSet('jpeg', 'png', 'webp')][string]$Format = 'jpeg',
    [switch]$Bench,
    [switch]$SkipBuild,
    [switch]$NoUpdateCheck,
    [switch]$CheckLatest,
    [switch]$Update,
    [switch]$CheckDeps
)

$ErrorActionPreference = 'Stop'
Set-Location -LiteralPath $PSScriptRoot
. (Join-Path $PSScriptRoot 'tools\launcher\env-check.ps1')

function Write-Step([string]$msg) { Write-Host "→ $msg" -ForegroundColor Cyan }
function Write-Note([string]$msg) { Write-Host "  $msg" -ForegroundColor DarkGray }
function Stop-WithError([string]$msg) {
    Write-Host "× $msg" -ForegroundColor Red
    exit 1
}

# 外部コマンドの終了コードで止める（PowerShell 5.1 は外部コマンドの失敗で止まらないため）
function Invoke-Native([string]$what, [scriptblock]$block) {
    & $block
    if ($LASTEXITCODE -ne 0) { Stop-WithError "$what に失敗しました（終了コード $LASTEXITCODE）。上のメッセージを確認してください。" }
}

function Format-Bytes([long]$n) {
    if ($n -ge 1MB) { return ('{0:N1} MB' -f ($n / 1MB)) }
    if ($n -ge 1KB) { return ('{0:N0} KB' -f ($n / 1KB)) }
    return "$n バイト"
}

# ffimg のエラーコード（README の表）を言葉で
$FfimgCodes = @{
    too_large_bytes    = 'ファイルが大きすぎます（--max-bytes を超えています）'
    too_many_pixels    = '画素数が多すぎます（--max-pixels を超えています）'
    unsupported_format = 'JPEG / PNG / WebP ではありません'
    decode_failed      = '画像が壊れているか、途中で切れています'
    zero_dimension     = '幅または高さが 0 です'
    limits_exceeded    = '展開に必要なメモリが上限を超えます'
    io_error           = 'ファイルを読めない、または書けません'
    encode_failed      = '出力形式への変換に失敗しました'
    usage              = '引数の誤りです'
}

# ffimg を実行し、標準出力の JSON と終了コードを返す（README の CLI 契約どおり）
function Invoke-Ffimg([string[]]$ffArgs) {
    $r = Invoke-OssCapture $script:Ffimg $ffArgs # 人間向けの補足（標準エラー）は捨てる
    $json = $null
    try { $json = $r.Text | ConvertFrom-Json } catch { }
    return @{ Code = $r.Code; Json = $json }
}

function Show-FfimgErrors($json) {
    foreach ($e in @($json.errors)) {
        $word = if ($FfimgCodes.ContainsKey([string]$e.code)) { $FfimgCodes[[string]$e.code] } else { [string]$e.message }
        Write-Host "  × $word（$($e.code)）" -ForegroundColor Red
    }
}

function Describe-Image($json) {
    $meta = if ($json.PSObject.Properties['has_metadata']) { if ($json.has_metadata) { '・メタデータあり（EXIF など）' } else { '・メタデータなし' } } else { '' }
    return "$($json.format) $($json.width)×$($json.height)・$(Format-Bytes ([long]$json.bytes))$meta"
}

# ---- 起動せずに終わる操作 ----
if ($Update) { Invoke-OssUpdate; exit 0 }
if ($CheckDeps) { Show-OssDependencies; exit 0 }

$cargo = $null
$go = $null
if (-not $SkipBuild) {
    if (-not (Test-OssRequirements -NeedGo:($Check -or $Bench))) {
        Stop-WithError '必要なツールがそろっていません。上の案内に従って準備してから、もう一度実行してください。'
    }
    if (-not $NoUpdateCheck) { [void](Show-OssLatest -Force:$CheckLatest) }
}
$cargo = Get-OssCargoPath
$go = Get-OssGoPath

# ================= 検査一式（-Check） =================
if ($Check) {
    $results = New-Object System.Collections.Generic.List[object]
    function Invoke-CheckStep([string]$name, [string]$dir, [scriptblock]$block) {
        Write-Host ''
        Write-Step $name
        $sw = [Diagnostics.Stopwatch]::StartNew()
        Push-Location -LiteralPath (Join-Path $PSScriptRoot $dir)
        try {
            # The block writes the tools' output to the screen (Out-Host) and returns
            # only $true / $false / $null (skipped).
            $ok = @(& $block) | Select-Object -Last 1
        } catch {
            Write-Host "  $($_.Exception.Message)" -ForegroundColor Red
            $ok = $false
        } finally {
            Pop-Location
        }
        $sw.Stop()
        $result = if ($null -eq $ok) { 'skip' } elseif ($ok -eq $true) { 'ok' } else { 'ng' }
        $results.Add([pscustomobject]@{ Name = $name; Result = $result; Seconds = [math]::Round($sw.Elapsed.TotalSeconds, 1) })
    }

    Invoke-CheckStep 'Rust：整形（cargo fmt --check）' '.' { & $cargo fmt --all --check | Out-Host; $LASTEXITCODE -eq 0 }
    Invoke-CheckStep 'Rust：lint（cargo clippy -D warnings）' '.' {
        # clippy は前回の結果を使い回すことがあり、Rust を更新して増えた指摘を見逃す（CI は毎回まっさら）。
        # このリポジトリの Rust ソースの更新時刻だけを新しくして、自前のクレートを検査し直させる
        # （依存ライブラリの結果はそのまま使うので速い）。
        $now = Get-Date
        Get-ChildItem -LiteralPath (Join-Path $PSScriptRoot 'rust') -Recurse -Filter '*.rs' | ForEach-Object { $_.LastWriteTime = $now }
        & $cargo clippy --locked --all-targets -- -D warnings | Out-Host
        $LASTEXITCODE -eq 0
    }
    Invoke-CheckStep 'Rust：テスト（cargo test）' '.' { & $cargo test --locked | Out-Host; $LASTEXITCODE -eq 0 }
    Invoke-CheckStep 'Go：整形（gofmt -l）' 'go' {
        $gofmt = Join-Path (Split-Path -Parent $go) 'gofmt.exe'
        if (-not (Test-Path -LiteralPath $gofmt)) { $gofmt = 'gofmt' }
        $bad = @(& $gofmt -l . | Where-Object { $_ })
        if ($bad.Count) { Write-Host '  整形されていないファイル：' -ForegroundColor Red; $bad | ForEach-Object { Write-Host "    $_" } }
        $bad.Count -eq 0
    }
    Invoke-CheckStep 'Go：静的検査（go vet）' 'go' { & $go vet ./... | Out-Host; $LASTEXITCODE -eq 0 }
    Invoke-CheckStep 'Go：テスト（go test）' 'go' {
        # -race は cgo（C コンパイラ）が必要。無ければ通常のテストにする（CI の Linux では -race で実行）
        $race = [bool](Find-OssTool 'gcc' @())
        $prev = $env:CGO_ENABLED
        try {
            if ($race) { $env:CGO_ENABLED = '1'; & $go test -race -count=1 ./... | Out-Host }
            else { Write-Note '（C コンパイラが無いため -race なしで実行します）'; & $go test -count=1 ./... | Out-Host }
            $LASTEXITCODE -eq 0
        } finally {
            $env:CGO_ENABLED = $prev # この PowerShell の環境変数を元に戻す
        }
    }
    Invoke-CheckStep 'Go：ベンチマークの 1 回実行' 'go' { & $go test -run '^$' -bench . -benchtime 1x ./... | Out-Null; $LASTEXITCODE -eq 0 }
    Invoke-CheckStep '混入検査（scripts/leakcheck.sh）' '.' {
        $bash = Get-OssBashPath
        if (-not $bash) { Write-Note 'Git Bash が無いため省略しました。'; return $null }
        & $bash scripts/leakcheck.sh | Out-Host
        $LASTEXITCODE -eq 0
    }

    Write-Host ''
    Write-Host '検査の結果' -ForegroundColor Cyan
    foreach ($r in $results) {
        switch ($r.Result) {
            'ok' { Write-Host ("  ○ {0}（{1} 秒）" -f $r.Name, $r.Seconds) -ForegroundColor Green }
            'ng' { Write-Host ("  × {0}（{1} 秒）" -f $r.Name, $r.Seconds) -ForegroundColor Red }
            default { Write-Host ("  － {0}（省略）" -f $r.Name) -ForegroundColor DarkGray }
        }
    }
    $failed = @($results | Where-Object { $_.Result -eq 'ng' }).Count
    if ($failed) { Stop-WithError "$failed 件の検査が失敗しました。上のメッセージを確認してください。" }
    Write-Host '○ すべての検査が通りました。PR を出せる状態です。' -ForegroundColor Green
    exit 0
}

# ================= ffimg のビルド =================
$script:Ffimg = Join-Path $PSScriptRoot 'target\release\ffimg.exe'
if (-not $SkipBuild) {
    Write-Step 'ffimg をビルドしています（リリース版）…'
    Invoke-Native 'ffimg のビルド' { & $cargo build --locked --release -p ffimg }
} elseif (-not (Test-Path -LiteralPath $script:Ffimg)) {
    Stop-WithError "$($script:Ffimg) がありません。-SkipBuild を付けずに実行してください。"
}

# バージョンの照合（取り違え防止：docs/architecture.md の推奨どおり）
$want = (Select-String -LiteralPath (Join-Path $PSScriptRoot 'rust\ffimg\Cargo.toml') -Pattern '^version\s*=\s*"([^"]+)"' | Select-Object -First 1).Matches[0].Groups[1].Value
$ver = Invoke-Ffimg @('version')
if ($ver.Code -ne 0 -or -not $ver.Json -or [string]$ver.Json.version -ne $want) {
    Stop-WithError "ffimg のバージョンが一致しません（実行ファイル：$($ver.Json.version)、Cargo.toml：$want）。ビルドし直してください。"
}
Write-Note "ffimg $want：$($script:Ffimg)"

$samples = Join-Path $PSScriptRoot 'samples'
if (-not (Test-Path -LiteralPath $samples)) { New-Item -ItemType Directory -Path $samples | Out-Null }

# ================= ベンチマーク（-Bench） =================
if ($Bench) {
    Write-Step 'ffimg のベンチマーク（合成画像 4000×3000、10 回）…'
    Invoke-Native 'ffimg のベンチマーク' { & $cargo run --locked --release -q -p ffimg --example bench }
    Write-Step 'Go のベンチマーク…'
    Push-Location -LiteralPath (Join-Path $PSScriptRoot 'go')
    try { Invoke-Native 'Go のベンチマーク' { & $go test -run '^$' -bench . -benchmem ./... } } finally { Pop-Location }
    Write-Note '計測条件と過去の結果は docs/benchmarks.md にあります。'
    exit 0
}

# ================= 手元の画像を試す（-Image） =================
if ($Image) {
    if (-not (Test-Path -LiteralPath $Image -PathType Leaf)) { Stop-WithError "ファイルが見つかりません：$Image" }
    $src = (Resolve-Path -LiteralPath $Image).Path
    Write-Step "検証しています：$src"
    $r = Invoke-Ffimg @('inspect', $src)
    if ($r.Code -ne 0) { Show-FfimgErrors $r.Json; Stop-WithError 'この画像は使えません（元の画像は変更していません）。' }
    Write-Host "  ○ $(Describe-Image $r.Json)" -ForegroundColor Green
    $ext = @{ jpeg = 'jpg'; png = 'png'; webp = 'webp' }[$Format]
    $out = Join-Path $samples ("{0}-{1}.{2}" -f [IO.Path]::GetFileNameWithoutExtension($src), $MaxEdge, $ext)
    Write-Step "派生画像を作っています（長辺 $MaxEdge px 以下・$Format・メタデータ除去）…"
    $d = Invoke-Ffimg @('derive', $src, '--out', $out, '--max-edge', "$MaxEdge", '--format', $Format)
    if ($d.Code -ne 0) { Show-FfimgErrors $d.Json; Stop-WithError '派生画像を作れませんでした。' }
    $c = Invoke-Ffimg @('inspect', $out)
    Write-Host "  ○ $(Describe-Image $c.Json)" -ForegroundColor Green
    Write-Note "出力：$out（元の画像は変更していません。samples フォルダは Git の管理外です）"
    exit 0
}

# ================= 合成画像での動作確認（既定） =================
$demo = Join-Path $samples 'demo'
if (-not (Test-Path -LiteralPath $demo)) { New-Item -ItemType Directory -Path $demo | Out-Null }
$synth = Join-Path $demo 'synthetic.jpg'
$derived = Join-Path $demo 'synthetic-1200.jpg'
$broken = Join-Path $demo 'not-an-image.jpg'

Write-Step '合成画像を作っています（2400×1600、EXIF 付き。実在の画像は使いません）…'
Invoke-Native '合成画像の作成' { & $cargo run --locked --release -q -p ffimg --example synth -- $synth 2400 1600 --exif }

$pass = 0
$fail = 0
function Write-Result([bool]$ok, [string]$msg) {
    if ($ok) { $script:pass++; Write-Host "  ○ $msg" -ForegroundColor Green } else { $script:fail++; Write-Host "  × $msg" -ForegroundColor Red }
}

Write-Step 'inspect：画像を検証します'
$a = Invoke-Ffimg @('inspect', $synth)
Write-Result ($a.Code -eq 0 -and $a.Json.ok -and $a.Json.has_metadata) "元の画像：$(Describe-Image $a.Json)"

Write-Step 'derive：長辺 1200 px に縮小し、メタデータを除去します'
$b = Invoke-Ffimg @('derive', $synth, '--out', $derived, '--max-edge', '1200')
$c = Invoke-Ffimg @('inspect', $derived)
Write-Result ($b.Code -eq 0 -and $c.Code -eq 0 -and [int]$c.Json.width -eq 1200 -and -not $c.Json.has_metadata) "派生画像：$(Describe-Image $c.Json)"

Write-Step '不正な入力：画像ではないファイルを渡します'
Set-Content -LiteralPath $broken -Value 'this is plain text, not an image' -Encoding ASCII
$e = Invoke-Ffimg @('inspect', $broken)
$codes = @($e.Json.errors | ForEach-Object { [string]$_.code })
Write-Result ($e.Code -eq 2 -and $codes -contains 'unsupported_format') "終了コード $($e.Code)・$($codes -join ', ')（$($FfimgCodes['unsupported_format'])）として拒否されました"

Write-Host ''
if ($fail) { Stop-WithError "動作確認で $fail 件が期待どおりではありませんでした。" }
Write-Host "○ ffimg は正しく動いています（$pass 件の確認がすべて通りました）。" -ForegroundColor Green
Write-Note "実行ファイル：$($script:Ffimg)"
Write-Note '利用側のアプリから使うには、アプリの設定で上の実行ファイルの場所を指定するか、PATH の通ったフォルダに置いてください。'
Write-Note "試した画像：$demo（Git の管理外）"
Write-Note '手元の画像を試す：.\start.ps1 -Image <画像ファイル>　検査一式：.\start.ps1 -Check（または check.cmd）'
exit 0
