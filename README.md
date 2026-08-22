# Discord Word Cloud Bot

指定したDiscordチャンネルのメッセージをSQLiteへ保存し、JSTの毎日0:00に前日分をPNGワードクラウドとして同じチャンネルへ投稿するGo製Bot。

## 必要なもの

- Go 1.22以上
- Discord Bot Token
- Developer Portalで `Message Content Intent` を有効化
- Botに `View Channel`、`Read Message History`、`Send Messages`、`Attach Files` 権限

## ビルドと起動

```bash
go mod tidy
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags='-s -w' -o discord-wordcloud .
DISCORD_TOKEN='ここにBot Token' ./discord-wordcloud
```

DBは同じディレクトリの `wordcloud.db` に作られる。場所を変える場合は `WORDCLOUD_DB=/var/lib/discord-wordcloud/wordcloud.db` を指定する。

日本語フォント（Noto Sans JP）はバイナリへ内蔵される。別のTTF/OTFを使う場合のみ `WORDCLOUD_FONT=/path/to/font.ttf` を指定する。不正なパスやフォントはエラーとして扱われる。

## Discordでの設定

- `/wordcloud set channel:#チャンネル` — 記録を開始（メッセージの管理権限が必要）
- `/wordcloud status` — 現在の設定を確認
- `/wordcloud preview` — 当日分の暫定ワードクラウドを実行者だけに表示（保存データは削除しない）
- `/wordcloud disable` — 記録を停止

メッセージ本文は日付単位で保存され、ワードクラウドの投稿に成功した後、その日のレコードを削除する。Bot自身のメッセージ、URL、メンション、コード、Discord絵文字名は集計対象外。日本語は形態素解析を行い、名詞と、否定・過去などの助動詞を含む動詞・形容詞の活用表現を入力時の表記のまま集計する。1メッセージ内の過剰な反復は単語ごとに3回までに制限する。

描画時は最大140語を頻度順に配置し、横書きと縦書きを混在させる。文字の矩形ではなく描画ピクセルを使って衝突を判定し、文字同士を重ねずに空き領域へ詰める。大きい語が収まらない場合は段階的に縮小して再配置する。

## systemd例

```ini
[Unit]
After=network-online.target

[Service]
WorkingDirectory=/opt/discord-wordcloud
Environment=DISCORD_TOKEN=xxxxx
Environment=WORDCLOUD_DB=/var/lib/discord-wordcloud/wordcloud.db
ExecStart=/opt/discord-wordcloud/discord-wordcloud
Restart=always
RestartSec=5

[Install]
WantedBy=multi-user.target
```
