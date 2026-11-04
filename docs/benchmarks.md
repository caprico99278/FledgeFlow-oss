# ベンチマーク / Benchmarks

数値は 1 台のノート PC での参考値です。絶対値ではなく、変更前後の比較や桁の見積もりに使ってください。

## 計測条件

| 項目 | 値 |
| --- | --- |
| 計測日 | 2026-11-04 |
| CPU | 11th Gen Intel(R) Core(TM) i7-11800H @ 2.30GHz（8 コア / 16 スレッド） |
| メモリ | 31.8 GB |
| OS | Microsoft Windows 11 Home 10.0.26200 |
| 電源 | AC 接続、他の重い処理なし（電源プランなどは未調整） |
| Rust | rustc 1.99.1 (58cf8f9c3 2026-10-13)、`x86_64-pc-windows-gnu` |
| Rust ビルド | `--release`（`opt-level=3`、`lto="thin"`、`codegen-units=1`。`Cargo.toml` の `[profile.release]`） |
| 主要クレート | `image` 0.25.10（`jpeg`, `png`, `webp` のみ）、`sha2` 0.11.1 |
| Go | go1.27.3 windows/amd64 |

### 入力画像

すべて `examples/bench.rs` が実行時に生成する合成画像です（グラデーション＋ xorshift 擬似乱数ノイズ、RGB 8bit）。ノイズを含むため実写に近い圧縮率になります。

| 寸法 | JPEG（`image` 既定品質 75 でエンコード） | PNG（既定圧縮） |
| --- | --- | --- |
| 4000×3000（12.0 MP） | 4,617,296 bytes | 31,494,676 bytes |
| 1200×900（1.1 MP） | 422,168 bytes | 2,827,509 bytes |

### 方法

- `cargo run --release -p ffimg --example bench [-- <幅> <高さ> <回数>]`
- 各ケースで未計測のウォームアップ 1 回の後、N 回の壁時計時間を計測し min / median / mean を表示。
- シングルスレッド、メモリ上のバイト列に対する処理（ディスク I/O なし）。`inspect` は SHA-256・ヘッダ走査・フルデコードを含み、`derive` はそれに加えて 8bit 変換・Lanczos3 縮小・エンコード・SHA-256 を含みます。
- 依存を増やさず `x86_64-pc-windows-gnu` でビルドできるよう、criterion ではなく自前の計測ループを使っています（criterion は推移的依存の `windows-sys` が `dlltool` を要求し、この環境ではビルドできないため）。

## 結果: ffimg（ライブラリ関数、メモリ上）

### 4000×3000、n=10

| ケース | min | median | mean |
| --- | ---: | ---: | ---: |
| `inspect` JPEG | 91.1 ms | 93.1 ms | 93.1 ms |
| `inspect` PNG | 94.2 ms | 97.2 ms | 98.1 ms |
| `derive` JPEG → JPEG q85、長辺 1600（→ 1600×1200） | 432.1 ms | 451.6 ms | 454.4 ms |
| `derive` PNG → PNG、長辺 1600 | 694.1 ms | 704.2 ms | 714.4 ms |
| `derive` JPEG → WebP（ロスレス）、長辺 1600 | 378.2 ms | 398.2 ms | 391.9 ms |

### 1200×900、n=20（長辺 1600 以下のため縮小なし・再エンコードのみ）

| ケース | min | median | mean |
| --- | ---: | ---: | ---: |
| `inspect` JPEG | 8.0 ms | 8.8 ms | 8.6 ms |
| `inspect` PNG | 9.1 ms | 9.3 ms | 9.3 ms |
| `derive` JPEG → JPEG q85 | 44.1 ms | 49.4 ms | 48.5 ms |
| `derive` PNG → PNG | 134.8 ms | 138.8 ms | 140.0 ms |
| `derive` JPEG → WebP（ロスレス） | 20.0 ms | 20.6 ms | 20.6 ms |

## 結果: ffimg CLI（プロセス起動を含む）

`target/release/ffimg.exe` を Git Bash から 5 回ずつ起動し、`date +%s%N` の差で計測。入力は `examples/synth.rs` で生成した 4000×3000 JPEG（4,616,151 bytes、APP1 Exif 付き）、出力先は同じローカル SSD。

| コマンド | 5 回の値 |
| --- | --- |
| `ffimg inspect sample.jpg` | 175 / 160 / 160 / 164 / 160 ms |
| `ffimg derive sample.jpg --out d.jpg --max-edge 1600` | 510 / 517 / 512 / 521 / 525 ms |

ライブラリ計測との差（約 70 ms）は、プロセス起動、ファイル読み込み、Windows のセキュリティスキャン等によるものと考えられます。

## 結果: Go `structcheck`

`cd go && go test -run '^$' -bench . -benchmem -count=3 ./structcheck/`（GOMAXPROCS=16）。スキーマは 9 プロパティ（ネストしたオブジェクト、文字列配列＋正規表現、enum を含む）。

| ベンチマーク | ns/op（3 回） | B/op | allocs/op |
| --- | --- | ---: | ---: |
| `BenchmarkValidateValid`（違反なし） | 2326 / 2264 / 2332 | 496 | 17 |
| `BenchmarkValidateInvalid`（違反 9 件） | 2691 / 2667 / 2625 | 1522 | 36 |

## English summary

Reference numbers from one laptop (Intel i7-11800H, Windows 11, Rust 1.99.1 windows-gnu release build with thin LTO, Go 1.27.3). Inputs are synthetic gradient+noise images generated at run time. For a 12 MP image, `inspect` takes ~93 ms and `derive` to a 1600 px JPEG ~450 ms in-process (single-threaded); the CLI adds ~70 ms of process/file overhead. `structcheck.Validate` on a typical 9-property document takes ~2.3 µs. Reproduce with `cargo run --release -p ffimg --example bench` and `go test -bench . ./structcheck`.
