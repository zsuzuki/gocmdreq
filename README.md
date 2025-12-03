# gocmdreq

遠隔でコマンドラインを実行する簡易ツールです。サーバーが実行ジョブを受け付けてバックグラウンドで処理し、クライアントはジョブIDを使って状態と出力の末尾を取得できます。通信はBearerトークンとTLSで保護します。

## 特徴
- コマンド・作業ディレクトリを指定してジョブを登録
- 即座にジョブIDを返却し、サーバーは裏側で実行
- ジョブ状態: queued / running / succeeded / failed
- 直近のTTY出力数行を保持して返却
- ジョブ情報は`data/jobs.json`、出力は`data/output/<id>.log`に保存

## ビルド
```
go build ./cmd/server
go build ./cmd/client
```

## サーバーの起動例
```
go run ./cmd/server \
  --addr :8443 \
  --data-dir ./data \
  --token "my-token" \
  --tls-cert server.crt \
  --tls-key server.key
```
- `--token`が空でない場合、`Authorization: Bearer <token>`が必須になります。
- 暗号化通信が前提です。自己署名証明書を使う場合は例: `openssl req -x509 -newkey rsa:4096 -nodes -keyout server.key -out server.crt -days 365 -subj "/CN=localhost"`.
- 開発用にHTTPを許可する場合のみ`--allow-insecure-http`を付けてください。

環境変数でも設定可能: `ADDR`, `DATA_DIR`, `AUTH_TOKEN`, `TLS_CERT_FILE`, `TLS_KEY_FILE`。

## クライアントの使い方
```
# ジョブ登録
go run ./cmd/client submit \
  --server https://localhost:8443 \
  --token "my-token" \
  --command /bin/echo \
  --arg hello \
  --arg world \
  --workdir /tmp \
  --insecure   # 自己署名証明書を検証しない場合

# 状態確認
go run ./cmd/client status \
  --server https://localhost:8443 \
  --token "my-token" \
  --insecure \
  <job-id>
```

## HTTP API
- `POST /jobs` `{ "command": "ls", "args": ["-l"], "workdir": "/tmp" }` → `202 Accepted` `{ "job_id": "...", "status": "queued" }`
- `GET /jobs/{id}` → `200 OK` `{ "job": { ... }, "last_lines": ["..."], "server_time": "..." }`

## 実装メモ
- サーバーはジョブを受け取るとすぐにレスポンスを返し、ゴルーチンで実行します。
- 作業ディレクトリが存在しない場合は登録段階でエラーにします。
- サーバー再起動中に`running`だったジョブは`failed`に遷移させ、理由を記録します。
- 出力はファイルにストリーミングし、ステータス取得時に末尾数行（デフォルト20行）を返します。
