# Security Policy

脆弱性を見つけたら、公開 issue ではなく GitHub の **Security → Report a vulnerability**(private vulnerability reporting)から知らせてください。

- 対象: このリポジトリのコード(backend / frontend / scripts / Claude Code の hooks)。
- 対象外: 利用者自身の環境・設定(`.env` の値、loopback の外への公開、GMO コインや Anthropic のサービス自体)。
- これは個人の研究用プロジェクトで、対応の期限や報奨金はありません。修正は main への commit として公開します。

運用する人へ: API キーは取引に必要な権限だけで発行し(出金権限を付けない)、GMO 側で接続元 IP を制限してください。
キーが漏れた可能性があるときは、bot を止めて GMO 側でキーを失効させてから再発行してください。
