# Security Policy / セキュリティポリシー

## 報告方法 / Reporting

脆弱性は**公開 Issue に書かず**、GitHub の [Private vulnerability reporting](https://docs.github.com/en/code-security/security-advisories/guidance-on-reporting-and-writing-information-about-vulnerabilities/privately-reporting-a-security-vulnerability)（リポジトリの *Security* タブ → *Report a vulnerability*）で報告してください。再現手順、影響を受けるバージョン（タグ）、可能なら最小の再現入力を添えてください。再現用の画像は合成したものを使い、個人情報や実在の作品を含めないでください。

Please report vulnerabilities privately via GitHub's *Report a vulnerability* button (Security tab), not in public issues. Include the affected tag, reproduction steps and, if possible, a minimal synthetic input.

## 対象 / Scope

- 最新のリリースタグ（`ffimg-vX.Y.Z`、`go/vX.Y.Z`）が対象です。
- 特に関心のある問題: `ffimg` の解凍爆弾・過大な資源消費、上限の回避、メタデータ除去の漏れ、非アトミックな書き込み、panic による JSON 契約の破れ。`sendguard` の二重送信につながる競合。`structcheck` の panic。

Of particular interest: resource-exhaustion or limit bypasses in `ffimg`, metadata surviving `derive`, non-atomic writes, broken JSON contract on panic; races in `sendguard` that could cause a duplicate send; panics in `structcheck`.

## 対応 / Handling

受領の連絡、修正、修正版タグの公開、アドバイザリの公開の順に対応します。このプロジェクトは小規模なため、対応時間の保証はありません。

We will acknowledge, fix, tag a release and publish an advisory. This is a small project; no response-time guarantees are made.
