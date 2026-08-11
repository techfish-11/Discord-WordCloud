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

日本語を画像に描画する場合は、日本語グリフを含むTTF/OTFを用意して `WORDCLOUD_FONT=/path/to/font.ttf` を指定する。未指定時はバイナリ内蔵フォントにフォールバックするため、英数字は動作するが日本語が豆腐になる場合がある。

## Discordでの設定

- `/wordcloud set channel:#チャンネル` — 記録を開始（サーバー管理権限が必要）
- `/wordcloud status` — 現在の設定を確認
- `/wordcloud disable` — 記録を停止

メッセージ本文は日付単位で保存され、ワードクラウドの投稿に成功した後、その日のレコードを削除する。Bot自身のメッセージ、URL、メンションは集計対象外。日本語は2文字の連続語、英語は単語単位で集計する。

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
