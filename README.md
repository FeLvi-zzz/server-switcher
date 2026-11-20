# server-switcher

unix domain socket や `host:port` で公開されている複数のローカルサーバーを束ねるリバースプロキシ。

- instance（例: `hoge`, `foo`）ごとに独立したプロキシを持ち、同時に起動できる
- ルーティング用 header（default: `X-Backend`）が無ければ instance の default 接続先へ
- header があれば、その値と同じ名前で登録された接続先へ（未登録なら 502）
- header が無くルーティング用 cookie（default: `X-Backend`）があれば、その値の接続先へ（header が優先）
- Host ヘッダは元のまま転送し、ルーティング用 header / cookie は除去する
- 設定の変更は serve 中にも自動で反映される（再起動不要）

## Install

```sh
go install .   # or: go build -o server-switcher .
```

## Usage

```sh
# 接続先の登録（instance の最初の登録が自動で default になる）
server-switcher add hoge app1 unix:/tmp/hoge-app1.sock
server-switcher add hoge app2 127.0.0.1:3001
server-switcher add foo  app1 127.0.0.1:4001

# 起動（-listen / -header / -cookie は instance 設定に保存され、次回以降は省略可）
server-switcher serve hoge -listen 127.0.0.1:8080
server-switcher serve foo  -listen 127.0.0.1:8081   # unix:/path.sock も可

curl localhost:8080/                       # -> hoge の default (app1)
curl -H 'X-Backend: app2' localhost:8080/  # -> hoge の app2
curl -b 'X-Backend=app2' localhost:8080/   # -> hoge の app2（cookie でも可）

server-switcher default hoge app2
server-switcher rm hoge app1
server-switcher list          # 全 instance（running/stopped も表示）
server-switcher list hoge
server-switcher delete foo    # instance ごと削除
```

設定ファイル: `$SERVER_SWITCHER_CONFIG_DIR/<instance>.json`（未指定時は `~/.config/server-switcher/`）
