# Discord Word Cloud Bot

指定したDiscordチャンネルのメッセージをSQLiteへ保存し、JSTの毎日0:00に前日分をPNGワードクラウドとして同じチャンネルへ投稿するGo製Bot。

## 必要なもの

- Go 1.24以上
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

## FreePBXにSIP電話として登録する音声メモ (`/call`)

Bot自身がSIPクライアントとしてFreePBXへREGISTERし、`/call message:hello` で指定した内線へINVITEを送る。相手が応答すると、VOICEVOXの音声をG.711（PCMU / PCMA）のRTPとしてBotから送信し、送信終了後にBYEで切断する。SIP・認証・RTP処理には [Diago](https://github.com/emiago/diago) を使用する。

Discordにはthinking表示の後、同じ実行者限定Embedを「音声準備中 → 発信中 → 相手が出ました → 送信・通話完了」へ編集する。不在、通話中、拒否、途中切断、タイムアウト、接続失敗も通知する。RTPは受信側が実際に音を聞いたことを保証できないため、完了表示は音声送信とSIP切断応答の確認を意味する。留守番電話・自動応答と人間の応答は区別しない。

**FFmpeg、音声共有フォルダ、AMIユーザー、音声再生用カスタムダイヤルプランは不要。** 音声はメモリ内だけで扱う。以前の `CALL_AMI_*`、`CALL_AUDIO_DIR`、`CALL_PBX_AUDIO_DIR`、`CALL_FFMPEG` は使わない。

### FreePBX側の設定

1. Bot専用のPJSIP内線（例 `200`）を作り、認証パスワード（Secret）を設定する。発信先（例 `100`）とは別の内線にする。Bot専用内線の認証ユーザー名と内線番号が同じ構成にする。
2. Bot専用内線でG.711 `ulaw` / `alaw` を許可し、Direct Mediaを無効にする。着信を受ける機能はなく、Botへの着信は486で拒否する。
3. FreePBXからBotのSIP待受IPとRTPポートへ到達できるようにする。既定のSIP待受は `15060`、RTP/RTCPはUDP `20000〜20099`。PBX側のSIPポートはFreePBXの実設定を指定する（例 `5060`）。
4. Bot専用内線の発信権限・コンテキストを、許可した宛先だけに制限する。Botは設定された数字の宛先だけをダイヤルするが、PBXの転送・Follow Me・外線ルートによって外線へ転送される場合があるため、PBX側でも制限する。

同一LANまたはVPN内のIPv4接続を対象とする。SIPはUDP/TCP、音声は平文RTPであり、インターネットへ直接公開しない。BotとPBXが別マシンでも音声ディレクトリの共有は不要。NAT越しの場合は `CALL_SIP_ADVERTISE_IP` をPBXから到達できるIPにし、SIPポートとRTP範囲を同じ番号で転送する。複雑なNAT構成ではVPNを推奨する。

### systemd設定例（全体）

例ではBotのIPが `192.168.1.5`、FreePBXが `192.168.1.10:5060`、Bot内線が `200`、発信先が `100`。実環境の値に変更する。`discord-bot` は実際のLinux実行ユーザーに置き換える。既存のサービスに別機能の環境変数がある場合はそれも残す。

`/etc/systemd/system/discord-wordcloud.service`:

```ini
[Unit]
Description=Discord WordCloud Bot
Wants=network-online.target
After=network-online.target

[Service]
Type=simple
User=discord-bot
WorkingDirectory=/opt/discord-wordcloud
ExecStart=/opt/discord-wordcloud/discord-wordcloud
Environment="DISCORD_TOKEN=REPLACE_WITH_BOT_TOKEN"
Environment="WORDCLOUD_DB=/var/lib/discord-wordcloud/wordcloud.db"

Environment="CALL_ENABLED=true"
Environment="CALL_SIP_ADDRESS=192.168.1.10:5060"
Environment="CALL_SIP_USERNAME=200"
Environment="CALL_SIP_PASSWORD=REPLACE_WITH_PJSIP_SECRET"
Environment="CALL_EXTENSION=100"
Environment="CALL_SIP_TRANSPORT=udp"
Environment="CALL_SIP_BIND_IP=192.168.1.5"
Environment="CALL_SIP_BIND_PORT=15060"
Environment="CALL_SIP_ADVERTISE_IP=192.168.1.5"
Environment="CALL_RTP_PORT_START=20000"
Environment="CALL_RTP_PORT_END=20100"
# 未設定・空ならユーザー制限なし。制限する場合のみIDを指定する。
Environment="CALL_ALLOWED_USERS="
# Environment="CALL_ALLOWED_USERS=123456789012345678,234567890123456789"
Environment="CALL_SPEAKER=3"

Environment="CALL_TIMEZONE=Asia/Tokyo"
Environment="CALL_QUIET_START_HOUR=23"
Environment="CALL_QUIET_START_MINUTE=59"
Environment="CALL_QUIET_END_HOUR=6"
Environment="CALL_QUIET_END_MINUTE=0"
Environment="CALL_USER_COOLDOWN=5m"
Environment="CALL_MAX_CONCURRENT=1"
Environment="CALL_RING_TIMEOUT=30s"
Environment="CALL_TOTAL_TIMEOUT=4m"

Restart=on-failure
RestartSec=5
TimeoutStopSec=90
UMask=0027

[Install]
WantedBy=multi-user.target
```

BotにはSQLite用ディレクトリへの書込権限を付ける。`asterisk` グループへの所属は不要。

```bash
sudo install -d -o discord-bot -g discord-bot -m 0750 /var/lib/discord-wordcloud
sudo systemctl daemon-reload
sudo systemctl enable --now discord-wordcloud.service
sudo systemctl restart discord-wordcloud.service
sudo journalctl -u discord-wordcloud.service -f
```

秘密値をサービスファイルに直接書かず、rootのみ読めるファイルを `EnvironmentFile=/etc/discord-wordcloud.env` で読み込んでもよい。その場合は `CALL_SIP_PASSWORD=...` のような形式で記載する。`.env` の自動読込は行わない。以前のAMI設定から移行する場合は、不要になったAMIアカウント・カスタムコンテキストをPBX側から削除し、再ビルドしたBotへ差し替える。

### 設定一覧

| 環境変数 | 初期値・説明 |
| --- | --- |
| `CALL_ENABLED` | `false`。`true` で有効化 |
| `CALL_SIP_ADDRESS` | 必須。FreePBXのSIP接続先 `host:port` |
| `CALL_SIP_USERNAME` | 必須。Bot専用PJSIP内線番号兼認証ユーザー名。1〜20桁の数字 |
| `CALL_SIP_PASSWORD` | 必須。Bot専用PJSIP内線のSecret。AMIのパスワードではない |
| `CALL_EXTENSION` | 必須。発信先の1〜20桁の数字。Bot内線とは別番号 |
| `CALL_SIP_BIND_IP` | 必須。Bot自身の具体的なIPv4アドレス。`0.0.0.0` は不可 |
| `CALL_SIP_BIND_PORT` | `15060`。BotのSIP待受ポート。1024〜65534、RTP範囲外 |
| `CALL_SIP_ADVERTISE_IP` | `CALL_SIP_BIND_IP` と同じ。PBXから到達できるBotのIPv4アドレス |
| `CALL_SIP_TRANSPORT` | `udp`。`udp` または `tcp` |
| `CALL_RTP_PORT_START` / `CALL_RTP_PORT_END` | `20000` / `20100`。開始を含み終了を含まないUDP範囲。両端は偶数、20ポート以上 |
| `CALL_ALLOWED_USERS` | 任意。未設定・空（空白のみも含む）ならユーザー制限なし。IDのカンマ区切りを指定すると、そのユーザーだけ発信可能。不正なIDや空の区切り要素は起動エラー |
| `CALL_SPEAKER` | `3`。APIが許可するずんだもんのspeaker ID |
| `CALL_TIMEZONE` | `Asia/Tokyo` |
| `CALL_QUIET_START_HOUR` / `CALL_QUIET_START_MINUTE` | `23` / `59`。発信禁止開始 |
| `CALL_QUIET_END_HOUR` / `CALL_QUIET_END_MINUTE` | `6` / `0`。発信禁止終了。既定はJST 23:59〜翌06:00未満 |
| `CALL_USER_COOLDOWN` | `5m`。ユーザーごとの受付間隔。1秒〜24時間 |
| `CALL_MAX_CONCURRENT` | `1`。TTSを含む同時受付数。1〜10 |
| `CALL_RING_TIMEOUT` | `30s`。応答待ち時間。5〜90秒 |
| `CALL_TOTAL_TIMEOUT` | `4m`。TTSから通話終了までの上限。1〜10分 |

SIP登録が完了するとログに `SIP registration ready` が出る。PBX停止・認証エラー時は発信せず、10秒間隔で登録を再試行する。登録更新・登録解除もBotが処理する。停止時とタイムアウト時はSIPライブラリでCANCEL/BYEを送る。PBXへ到達できないと即時切断を保証できないため、FreePBX側にもRTP inactivity timeoutや最大通話時間を設定する。

本文は1〜500文字、音声は120秒まで。[VOICEVOX API](https://voicevox-api.swiftlybot.com/) の `/v1/audio_query` と `/v1/synthesis` を使い、`outputSamplingRate=8000`、`outputStereo=false` で合成する。返されたWAVの形式・長さを検証し、不正な音声やAPIエラー時は発信しない。クレジット `VOICEVOX:ずんだもん` をEmbedへ表示する。本文は外部APIへ送信されるため、入力内容と利用規約を確認して運用する。

サーバー内で利用でき、メッセージ管理権限は不要。`CALL_ALLOWED_USERS` が未設定・空なら全ユーザーが利用可能で、IDを指定した場合だけホワイトリストで制限する。深夜制限・レート制限・同時受付制限はホワイトリストの有無にかかわらず適用する。レート制限は失敗した受付にも適用し、再起動でリセットされる。同時受付数も単一プロセス内で管理するため、Botを複数プロセスで稼働させない。自動再発信はしない。

### テストとトラブルシューティング

`go test ./...` で制限・JST時刻境界・TTS形式・WAV入力検証と、ループバックSIP/RTPによるDigest認証、発信、音声送信、切断、不在、通話中、拒否、キャンセルを検証する。Go 1.24以上が必要。`CGO_ENABLED=0` でLinuxバイナリをビルドできる。

本番では `/call message:テストです` を実行し、実端末の着信・音声・Embed更新を確認する。登録できない場合はPJSIPのSecret・ポート・FreePBXのファイアウォールを確認する。相手が出ても無音なら、BotがSDPで通知するIP (`CALL_SIP_ADVERTISE_IP`)、RTP範囲、PCMU/PCMA許可、Direct Mediaの無効化を確認する。BotにDiscordの `Send Messages` と `Embed Links` 権限を付与する。
