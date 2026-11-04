# Contributing / 貢献ガイド

FledgeFlow-oss への貢献を歓迎します。Issues / Pull Requests は日本語・英語どちらでも構いません。

## 方針

- **汎用部品だけ**を扱います。特定の企業・個人・外部サービス向けのコード、業務ロジック、実データは受け付けません。
- **実在の画像を追加しないでください。** テスト・例・ベンチマークの画像は実行時にプログラムで生成します（`rust/ffimg/tests/common/mod.rs` を参照）。
- 秘密情報（鍵、トークン、`.env`）を含めないでください。
- `ffimg` の CLI 契約（JSON・エラーコード・終了コード）を変える場合は README の表を更新し、PR に互換性への影響を明記してください。
- 依存は最小限に。Rust は純 Rust のクレートのみ（Windows の `x86_64-pc-windows-gnu` で追加ツールなしにビルドできること）、Go はサードパーティ依存なしを維持します。

## PR 前のチェック

```sh
cargo fmt --check
cargo clippy --all-targets -- -D warnings
cargo test
(cd go && gofmt -l . && go vet ./... && go test ./...)
bash scripts/leakcheck.sh
```

## ライセンス

貢献物は [Apache License 2.0](LICENSE) の下で提供されるものとします（Apache-2.0 第 5 条）。

## English

Contributions are welcome in English or Japanese. Keep changes generic (no product-, company- or service-specific code), never add real images (generate them in tests), never commit secrets, keep dependencies minimal (pure Rust; no third-party Go modules), and run the checks above before opening a PR. Contributions are licensed under Apache-2.0.
