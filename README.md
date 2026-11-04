# FledgeFlow-oss

作品を公開・発信するためのワークフローで使う、**汎用的で再利用可能な Go / Rust コンポーネント**を集めたリポジトリです。
ライセンスは [Apache License 2.0](LICENSE) です。

- 単体でビルド・テストできます（非公開リポジトリへの依存はありません）。
- 依存の向きは一方向です。利用側（非公開の本体）が、このリポジトリの**バージョン固定された成果物**（特に `ffimg` のバイナリ）に依存します。このリポジトリから利用側を参照することはありません。
- テスト・サンプル・ベンチマークで使う画像は**すべて実行時にプログラムで生成した合成データ**（グラデーション＋擬似乱数ノイズ）です。実在の作品・写真・秘密情報は含みません。

English summary: [below](#english).

## 構成

| パス | 言語 | 内容 |
| --- | --- | --- |
| [`rust/ffimg/`](rust/ffimg/) | Rust | `ffimg`: 画像の検証（inspect）と派生画像の生成（derive）を行う CLI ＋ライブラリ。純 Rust、JPEG / PNG / WebP のみ |
| [`go/fsm/`](go/fsm/) | Go | 汎用の有限状態遷移テーブル `fsm.Table[S]` |
| [`go/sendguard/`](go/sendguard/) | Go | 「外部システムに届いたか分からない」送信を**最大 1 回**に抑えるプロトコル（CAS ストア＋ `Guard`） |
| [`go/structcheck/`](go/structcheck/) | Go | デコード済み JSON（`map[string]any`）を小さな宣言的スキーマで検証。LLM の構造化出力の検査向け |
| [`docs/architecture.md`](docs/architecture.md) | – | 設計・不変条件・利用側との境界 |
| [`docs/benchmarks.md`](docs/benchmarks.md) | – | 計測結果と計測条件 |
| [`scripts/leakcheck.sh`](scripts/leakcheck.sh) | Bash | 公開してはいけない語・鍵・画像ファイルの混入検査 |

Go モジュールは `github.com/caprico99278/FledgeFlow-oss/go`（`go 1.27`、サードパーティ依存なし）です。

## ffimg の CLI 契約

利用側はこの CLI をサブプロセスとして呼び出し、標準出力の JSON と終了コードだけを見ます。**以下は安定した契約**です（変更規則は「バージョニング」を参照）。

### 共通ルール

- 標準出力には **常に 1 行・1 個の JSON オブジェクト**を出力します（`--help` / `--version` を除く）。人間向けの補足は標準エラー出力へ出ます。
- 終了コード: `0` = 正常（入力が妥当で処理成功） / `2` = 入力が不正（JSON は出力される） / `1` = 内部エラー・I/O エラー・使い方の誤り（可能な限り `ok:false` の JSON を出力）。
- 形式は**拡張子ではなくマジックバイト**で判定します（JPEG / PNG / WebP 以外は `unsupported_format`）。
- 既定の上限: `--max-bytes 52428800`（50 MiB）、`--max-pixels 100000000`（1 億画素）。
- デコードは `image::ImageReader` ＋ `Limits`（最大幅・高さ、最大確保量 = `max_pixels × 8 + 16 MiB`）で行い、解凍爆弾に備えます。さらにデコード前にヘッダだけを走査し、0 サイズや巨大な寸法を宣言する入力はデコードせずに拒否します。

### `ffimg inspect <path> [--max-bytes N] [--max-pixels N]`

```json
{"ok":true,"format":"jpeg","width":4000,"height":3000,"bytes":4616151,
 "sha256":"14afb5…","has_metadata":true,"errors":[]}
```

| フィールド | 型 | 説明 |
| --- | --- | --- |
| `ok` | bool | `errors` が空なら `true` |
| `format` | `"jpeg"｜"png"｜"webp"｜"unknown"` | マジックバイトから判定 |
| `width`, `height` | u32 | 不明な場合は 0 |
| `bytes` | u64 | 入力のバイト数 |
| `sha256` | string | 入力の SHA-256（小文字 16 進）。`too_large_bytes` と I/O エラー時は入力を読まないため空文字 |
| `has_metadata` | bool | EXIF / XMP / ICC 等の検出（ベストエフォート）。JPEG: APP1 / APP2 / APP13 / COM、PNG: `eXIf` `iTXt` `tEXt` `zTXt` `iCCP`、WebP: `EXIF` `XMP ` `ICCP` チャンクと VP8X フラグ |
| `errors` | `[{"code","message"}]` | `code` は下表。`message` は人間向けで契約外 |

### `ffimg derive <in> --out <path> --max-edge N [--format jpeg|png|webp] [--quality 1-100] [--max-bytes N] [--max-pixels N]`

入力を inspect と同じ規則で検証し、長辺が `--max-edge` 以下になるようアスペクト比を保って縮小（**拡大はしない**）、指定形式で**再エンコード**します。再エンコードで画素だけを書き出すため、メタデータ（EXIF の位置情報など）はすべて除去されます。出力は同じディレクトリの一時ファイルに書いて `fsync` 後に rename する**アトミック書き込み**です（失敗時に出力先は変更されません）。

- 既定: `--format jpeg`、`--quality 85`。`--max-edge` は 1〜65535。
- JPEG 出力ではアルファを白背景に合成します。
- **WebP 出力はロスレスのみ**です（純 Rust の `image` クレートのエンコーダにロッシーモードがないため）。PNG / WebP では `--quality` は無視されます。

```json
{"ok":true,"format":"jpeg","width":1600,"height":1200,"bytes":514591,
 "sha256":"828db4…","source_sha256":"14afb5…","errors":[]}
```

失敗時は `ok:false`、`width`/`height`/`bytes` は 0、`sha256` は空、`source_sha256` は入力を読めた場合のみ設定されます。

### `ffimg version`

```json
{"name":"ffimg","version":"0.1.0"}
```

### エラーコード

| code | 終了コード | 意味 |
| --- | --- | --- |
| `too_large_bytes` | 2 | 入力が `--max-bytes` を超える |
| `too_many_pixels` | 2 | 幅×高さが `--max-pixels` を超える（ヘッダ宣言値でも判定） |
| `unsupported_format` | 2 | JPEG / PNG / WebP ではない |
| `decode_failed` | 2 | 破損・途中で切れている（JPEG は EOI マーカー欠落も検出）など |
| `zero_dimension` | 2 | 幅または高さが 0 |
| `limits_exceeded` | 2 | デコーダの確保上限に達した |
| `io_error` | 1 | 入力を読めない・出力を書けない |
| `encode_failed` | 1 | 出力形式へのエンコードに失敗 |
| `usage` | 1 | 引数の誤り |
| `internal` | 1 | 想定外の内部エラー（panic を捕捉） |

## ビルドとテスト

必要なもの: Rust（stable、1.88 以上）、Go 1.27。

```sh
# Rust（リポジトリ直下の Cargo ワークスペース）
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
cargo build --release            # => target/release/ffimg(.exe)

# 合成画像を作って試す
cargo run --release -p ffimg --example synth -- /tmp/s.jpg 4000 3000 --exif
./target/release/ffimg inspect /tmp/s.jpg
./target/release/ffimg derive /tmp/s.jpg --out /tmp/d.jpg --max-edge 1600

# ベンチマーク（条件と結果は docs/benchmarks.md）
cargo run --release -p ffimg --example bench

# Go
cd go
go vet ./...
go test ./...                    # CI（Linux）では go test -race ./...
go test -run '^$' -bench . -benchmem ./structcheck

# 公開前の混入検査
bash scripts/leakcheck.sh
```

Windows では `x86_64-pc-windows-gnu` ツールチェーンだけで（C コンパイラや追加ツールなしで）ビルドできるよう、依存は純 Rust のクレートに限定しています（`clap` の `color` 機能も無効化しています）。

## バージョニングとタグ

| 対象 | タグ | 例 |
| --- | --- | --- |
| Rust クレート / `ffimg` CLI | `ffimg-vX.Y.Z` | `ffimg-v0.1.0` |
| Go モジュール（`go/` サブディレクトリ） | `go/vX.Y.Z` | `go/v0.1.0` |

- [Semantic Versioning](https://semver.org/) に従います。Go のタグはサブディレクトリモジュールの規約（`go/` 接頭辞）に従います。
- CLI 契約（JSON のフィールド名・型・意味、エラーコード、終了コード、既定値）の**削除・意味変更は破壊的変更**です。`0.x` の間はマイナー版、`1.0` 以降はメジャー版を上げます。フィールドやエラーコードの**追加**は互換変更として扱うため、利用側は未知のフィールドとコードを無視してください。
- 利用側はタグ（またはそのコミット）を固定して取り込み、任意のブランチ先端には依存しないでください。`Cargo.lock` はバイナリの再現性のためにコミットします。

## ライセンス

Apache License 2.0。[LICENSE](LICENSE) と [NOTICE](NOTICE) を参照してください。貢献方法は [CONTRIBUTING.md](CONTRIBUTING.md)、脆弱性の報告は [SECURITY.md](SECURITY.md) へ。

---

## English

**FledgeFlow-oss** is a small collection of generic, reusable Go and Rust components for publishing workflows, licensed under Apache-2.0. It builds and tests standalone and has no dependency on any private code; a private application consumes version-pinned artifacts of this repository (one-way dependency). All images used by tests, examples and benchmarks are synthetic and generated at run time.

- **`rust/ffimg`** — pure-Rust CLI + library that validates JPEG/PNG/WebP files (format from magic bytes, byte and pixel limits, header pre-scan and limit-guarded decoding against decompression bombs, best-effort metadata detection) and derives resized, metadata-free copies written atomically. Each run prints exactly one JSON object to stdout. Exit codes: 0 = ok, 2 = invalid input, 1 = internal/I/O/usage error. WebP output is lossless only. See the contract above (field and code tables are language-neutral).
- **`go/fsm`** — generic transition table `fsm.Table[S]` with `Can`, `Check` (typed `*TransitionError[S]`) and `Terminal`.
- **`go/sendguard`** — at-most-once protocol for side effects whose delivery may be ambiguous: CAS claim, `Submitting` persisted before sending, outcomes mapped to `Succeeded` / `Failed` (definite rejection) / `Unknown` (everything ambiguous, never retried automatically; reconcile with `Resolve`). Includes an in-memory reference store.
- **`go/structcheck`** — validates decoded JSON (`map[string]any`) against a tiny declarative schema and returns deterministically ordered violations; meant for checking LLM structured output.

Build/test: `cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test`, `cd go && go vet ./... && go test ./...`, `bash scripts/leakcheck.sh`.
Tags: `ffimg-vX.Y.Z` for the Rust crate/CLI, `go/vX.Y.Z` for the Go module.
