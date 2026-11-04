# アーキテクチャ / Architecture

## 1. 位置づけと依存の向き

```
private application                      FledgeFlow-oss (public)
  Go adapter ── exec ──────────────────▶  ffimg binary      (tag ffimg-vX.Y.Z)
  Go code    ── import ────────────────▶  go/fsm, go/sendguard, go/structcheck (tag go/vX.Y.Z)

  (no arrow in the opposite direction)
```

- 依存は**一方向**です。このリポジトリは利用側のコード・設定・データ・命名を一切参照しません。
- 公開対象は「どの利用者にも意味がある汎用部品」だけです。業務ロジック、実データ、秘密情報、特定の外部サービス向けのコードは含めません（`scripts/leakcheck.sh` と CI で機械的に検査）。
- テスト・例・ベンチマークの画像はすべて実行時に合成します。リポジトリには画像ファイルを置きません（leakcheck が拒否します）。

## 2. ffimg

### 2.1 処理パイプライン

```
read_limited ──▶ detect_format ──▶ header scan ──▶ decode (Limits) ──▶ [derive] to 8-bit ──▶ resize ──▶ encode ──▶ atomic write
 (max_bytes+1     (magic bytes)     (dims, metadata,  (max w/h,           (alpha → white     (Lanczos3,   (pixels    (tmp + fsync
  bytes まで)                         JPEG EOI)          max_alloc)          for JPEG)          no upscale)  only)       + rename)
```

1. **読み込み上限**: ファイルは `max_bytes + 1` バイトまでしか読みません。超えたら `too_large_bytes`（ハッシュは計算しない）。
2. **形式判定**: 先頭のマジックバイトのみ（JPEG `FF D8 FF`、PNG シグネチャ、`RIFF....WEBP`）。拡張子は見ません。
3. **ヘッダ走査**（`detect.rs`）: 割り当てを伴わない境界チェック付きの走査で、宣言された寸法とメタデータの有無を得ます。宣言値が 0（`zero_dimension`）や上限超え（`too_many_pixels`）なら**デコードせずに**拒否します。JPEG は最初の SOS 以降に EOI マーカーがなければ「途中で切れている」と判定します（JPEG デコーダには欠損部分を灰色で埋めて成功扱いにするものがあるため）。
4. **デコード**: `image::ImageReader::with_format` に `Limits`（最大幅・高さ = `max_pixels`、`max_alloc = max_pixels × 8 + 16 MiB`）を設定。デコード後の寸法も再検査します。
5. **derive**: 8bit RGB(A) に変換し、`target_dimensions`（長辺 ≤ `max_edge`、四捨五入、最小 1px、拡大しない）へ Lanczos3 で縮小。エンコーダには画素データだけを渡すため、元ファイルのメタデータは構造的に引き継がれません。
6. **アトミック書き込み**: 出力先と同じディレクトリに `create_new` で一時ファイルを作成 → 書き込み → `sync_all` → `rename`。失敗時は一時ファイルを削除し、出力先は変更しません。

### 2.2 出力契約の設計

- 標準出力は常に JSON 1 行。panic も捕捉して `internal` として JSON 化します。
- 終了コード `2`（入力が悪い）と `1`（環境・道具が悪い）を分けることで、呼び出し側は「ユーザーに差し戻す」か「再試行・アラート」かを機械的に判断できます。
- `message` は契約外です。分岐には必ず `code` を使ってください。

### 2.3 利用側アダプタの推奨実装（Go の例）

- `exec.CommandContext` でタイムアウト付きで起動し、標準出力を上限付きで読み、`json.Unmarshal` します。
- 終了コード 0 / 2 は JSON を信頼し、1 またはJSON が壊れている場合は内部エラーとして扱います。
- 未知のフィールド・未知のエラーコードは無視（または汎用エラー扱い）にします。互換性のある追加は予告なく行われ得るためです。
- バイナリはタグ `ffimg-vX.Y.Z` で固定し、`ffimg version` の出力で起動時に照合すると取り違えを防げます。

## 3. Go パッケージ

### 3.1 fsm

`fsm.Table[S comparable] map[S][]S` は「どこからどこへ遷移してよいか」だけを表すデータです。`Check` は `*TransitionError[S]` を返すので、永続化層で不正な遷移を型付きで検出できます。

### 3.2 sendguard — 最大 1 回の送信

外部 API への送信は「タイムアウトしたが実は届いていた」が起こり得ます。sendguard は**二重送信より停止を選ぶ**プロトコルです。

```
None ──Enqueue──▶ Ready ──claim(CAS)──▶ Claimed ──persist──▶ Submitting ──send──▶ Succeeded
                    │                     │                      │                Failed   (Definite() == true)
                    └──Cancel──▶ Cancelled ◀──Cancel──┘           └──────────────▶ Unknown  (それ以外すべて)
Claimed (stale)    ──Recover──▶ Ready      （まだ送っていない）
Submitting (stale) ──Recover──▶ Unknown    （送ったかもしれない）
Unknown            ──Resolve──▶ Succeeded | Failed | Ready（届いていないと確認できた場合のみ）
```

不変条件:

1. `send` を呼ぶ前に、必ず `Submitting` が永続化されている。
2. `Ready → Claimed` の CAS に勝った 1 者だけが `send` を呼べる（バージョン付き CAS）。
3. 結果の判定: 成功 → `Succeeded`、`Definite() == true` のエラー → `Failed`、タイムアウト・キャンセル・不明なエラー・panic → `Unknown`。
4. `Unknown` は自動では再試行されない。抜け出す手段は `Resolve` のみ。
5. 結果の永続化は `context.WithoutCancel` で行い、呼び出し元のキャンセルで `Submitting` のまま取り残されないようにする。
6. `Recover` は古い `Claimed` を `Ready` に、古い `Submitting` を `Unknown` に移す。遅いだけで生きているワーカーは最終 CAS に失敗し `ErrLostUpdate` を受け取る（記録は `Unknown` のまま、照合が必要）。

`Store` は `Get` と `CompareAndSwap` だけを要求します。RDB なら `UPDATE … SET … , version = version + 1 WHERE id = ? AND state = ? AND version = ?` の影響行数で実装できます。存在しないレコードは `(None, 0)` として扱い、そこからの CAS で作成します。`MemStore` は契約の実行可能な仕様でもあり、`Transitions` にない遷移を拒否します。

### 3.3 structcheck

LLM などの信頼できない生成元の構造化出力を、`json.Unmarshal` 後の `map[string]any` に対して検証します。

- 型: `string` / `number` / `integer` / `boolean` / `array-of-string` / `object`（＋何でも許す `Any`）。
- 制約: `Required`、`Enum`、`MinLength` / `MaxLength`（**ルーン数**）、`MaxItems`、`Pattern`（RE2、キャッシュ）、`AdditionalProperties`。
- 違反は `Violation{Path, Code, Message}` で、順序は決定的（深さ優先、オブジェクト内は 型 → 必須キー → 宣言済みプロパティ → 追加プロパティ、各キーは辞書順、配列は添字順）。同じ入力から常に同じ出力が得られるため、ログ比較や生成元へのフィードバックに使えます。
- JSON の `null` は「存在しない」として扱います。

## 4. 品質ゲート

| 項目 | ローカル | CI（ubuntu-latest） |
| --- | --- | --- |
| Rust 整形 / lint / テスト | `cargo fmt --check`、`cargo clippy --all-targets -- -D warnings`、`cargo test` | 同左＋ `--locked`、release ビルドとスモークテスト |
| Go | `go vet ./...`、`go test ./...` | `gofmt`、`go vet`、`go test -race`、ベンチ 1 回実行 |
| 混入検査 | `bash scripts/leakcheck.sh` | 同左（追加パターンは secret `LEAKCHECK_EXTRA`） |

## English summary

The private application depends one-way on version-pinned artifacts of this repository. `ffimg` reads at most `max_bytes+1` bytes, detects the format from magic bytes, pre-scans headers (dimensions, metadata, JPEG EOI) without allocating, decodes under `image::Limits`, and for `derive` converts to 8-bit, resizes with Lanczos3 without upscaling, re-encodes pixels only (so metadata cannot survive), and writes via temp file + fsync + rename. `sendguard` guarantees at-most-once sends: CAS claim, `Submitting` persisted before sending, ambiguous outcomes become `Unknown`, which only `Resolve` can leave. `structcheck` validates decoded JSON with deterministic violation ordering.
