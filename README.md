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

## Asterisk / FreePBX 音声メモ (`/call`)

`/call message:hello` で設定済みの **PJSIP内線1台**へ発信する。ホワイトリストのユーザーだけが利用でき、メッセージ管理権限は不要。サーバー内でのみ利用できる。
最初にDiscordのthinking表示を返し、同じ実行者限定Embedを「音声準備中 → 発信中 → 相手が出ました → 通話完了」へ編集する。不在・通話中・発信失敗・再生途中の切断も同じEmbedへ反映する。「完了」はPlayback成功とHangupの両方を確認した場合のみ表示する。応答は人間・留守番電話・自動応答を区別しない。

### セットアップ

1. AsteriskのAMI、PJSIP、`Playback`、`UserEvent`、`format_wav` を利用可能にする。内線のPJSIPエンドポイント名が数字の内線番号と一致する構成が対象。FreePBXの転送・Follow Me等を経由せず、`PJSIP/<内線>` を直接呼び出す。
2. [AMI設定例](asterisk/manager_custom.conf.example) を `/etc/asterisk/manager_custom.conf` に追記し、強い専用パスワードとBotの送信元IPを設定する。AMIは有効化し、`read=call,user` と `write=originate,call` を付与する。AMIアカウントには発信・切断権限があるため認証情報を厳重に管理する。
3. [ダイヤルプラン設定例](asterisk/extensions_custom.conf.example) を `/etc/asterisk/extensions_custom.conf` に追記し、`fwconsole reload` を実行する。`discord-voice-memo` は外線着信や通常の内線発信コンテキストへincludeしない。PBX側でも応答後130秒で強制切断するため、Bot停止・AMI切断時も通話が残り続けない。
4. BotホストにFFmpegをインストールする。BotとPBXが同一の音声ディレクトリを参照できるようにする。同一ホストでの運用を推奨。別ホストではプライベートな共有ファイルシステムを使い、`CALL_AUDIO_DIR` はBot側、`CALL_PBX_AUDIO_DIR` はPBX側のマウント先を指定する。音声をHTTPで公開する必要はない。
5. 下記の環境変数を設定してBotを起動する。設定ファイルの自動読込は実装していないため、シェルのexportまたはsystemdの`EnvironmentFile=`等を使う。`CALL_ENABLED=true` の場合、不正な設定・存在しないディレクトリ・FFmpeg未導入は起動エラーになる。無効時は既存機能のみ動作し、`/call` は無効と返す。

同一ホストのLinux設定例（Botの実行ユーザーを `discord-bot` とした場合）:

```bash
sudo install -d -o discord-bot -g asterisk -m 2750 /var/lib/asterisk/sounds/discord-memo
# Botプロセスの実効グループをasteriskにする（systemdならGroup=asterisk）。
# 作成される音声は0640。PBXユーザーがディレクトリを辿り、読めることを確認する。
export CALL_ENABLED=true
export CALL_AMI_ADDRESS=127.0.0.1:5038
export CALL_AMI_USERNAME=discord-memo
export CALL_AMI_SECRET='REPLACE_WITH_A_LONG_RANDOM_SECRET'
export CALL_EXTENSION=100
export CALL_ALLOWED_USERS=123456789012345678,234567890123456789
export CALL_AUDIO_DIR=/var/lib/asterisk/sounds/discord-memo
export CALL_PBX_AUDIO_DIR=/var/lib/asterisk/sounds/discord-memo
export DISCORD_TOKEN='your-bot-token'
./discord-wordcloud
```

AMIの平文接続はループバックまたは暗号化済みのプライベートトンネルで使用する。別ホストへ直接接続する場合はAsterisk側のAMI TLSを構成し、`CALL_AMI_TLS=true` とTLS待受アドレス（例 `pbx.example.internal:5039`）を指定する。証明書の信頼・ホスト名検証は必須で、省略しない。ファイアウォールでもBot以外からのAMI接続を拒否する。

### 設定と制限

| 環境変数 | 初期値・説明 |
| --- | --- |
| `CALL_ENABLED` | `false`。有効化には `true` |
| `CALL_AMI_ADDRESS` | 有効時必須。AMIの `host:port` |
| `CALL_AMI_USERNAME` / `CALL_AMI_SECRET` | 有効時必須。AMI専用ユーザーと秘密値 |
| `CALL_AMI_TLS` | `false`。TLS使用時 `true` |
| `CALL_EXTENSION` | 有効時必須。1〜20桁の数字。宛先をDiscordから変更することはできない |
| `CALL_ALLOWED_USERS` | 有効時必須。DiscordユーザーIDのカンマ区切り。ロール名や表示名ではない |
| `CALL_AUDIO_DIR` | 有効時必須。Botから書込可能な既存の絶対パス |
| `CALL_PBX_AUDIO_DIR` | 有効時必須。PBXから同じファイルを読めるLinux絶対パス。英数字、`_`、`-`、`/` のみ |
| `CALL_FFMPEG` | `ffmpeg`。実行ファイル名またはパス |
| `CALL_SPEAKER` | `3`。APIが許可するずんだもんのspeaker ID |
| `CALL_TIMEZONE` | `Asia/Tokyo` |
| `CALL_QUIET_START_HOUR` / `CALL_QUIET_END_HOUR` | `23` / `6`。各0〜23 |
| `CALL_QUIET_START_MINUTE` / `CALL_QUIET_END_MINUTE` | `59` / `0`。各0〜59。既定ではJST 23:59以上〜翌06:00未満は発信禁止。開始・終了時刻の同値は禁止 |
| `CALL_USER_COOLDOWN` | `5m`。ユーザーごとの受付間隔。1秒〜24時間 |
| `CALL_MAX_CONCURRENT` | `1`。TTS処理を含む同時受付数。1〜10 |
| `CALL_RING_TIMEOUT` | `30s`。呼出し上限。5〜90秒 |
| `CALL_TOTAL_TIMEOUT` | `4m`。TTSを含む処理全体の上限。1〜10分、呼出し上限より135秒以上長く設定。合成後の残り時間がこの条件を満たさない場合も発信しない |

本文は1〜500文字。TTSは [指定のVOICEVOX API](https://voicevox-api.swiftlybot.com/) の `/v1/audio_query` と `/v1/synthesis` を使用する。本文は外部APIへ送信されるため、機密情報は入力しない。APIの429・エラー・不正応答・タイムアウト時には発信しない。自動再発信は行わない。クレジット `VOICEVOX:ずんだもん` をEmbedに表示する。利用規約を確認して運用する。

合成音声はFFmpegで8kHz・モノラル・16bit PCM WAVへ変換し、最大約120秒までとする。長すぎる音声は発信せずエラーにする。一時ファイルは処理終了時に削除する。強制終了やホスト障害では残る場合があるため、再起動後に古い `discord-*.wav` / `.call-*.wav` を運用側で清掃する（稼働中のファイルを削除しない）。本文・音声・AMI秘密値をアプリのログへ記録しない。

レート制限は失敗した受付にも適用する。制限と同時受付数は単一Botプロセスのメモリで管理し、再起動でリセットされる。複数Botプロセスを同時稼働させない。停止時は処理をキャンセルして終了を待つ。AMI接続断などで最終結果を確認できない場合は成功扱いせず失敗通知する。
発信要求後にAMI接続が切れて通話終了を確認できない場合は、PBX側で呼出しと通話が確実に期限切れになる時刻まで新規発信を一時停止し、同時発信数の超過を防ぐ。

### 確認

`go test ./...` で入力検証、ホワイトリスト、深夜制限、レート・同時受付制限、TTSリクエスト、模擬AMIによる応答・完了・不在・通話中・接続断・タイムアウトを確認できる。
本番導入前に設定時間帯内で `/call message:テストです` を実行し、実端末への着信、応答Embed、再生音声、完了Embedを確認する。無応答・途中切断・PBX停止の場合も確認する。Botに `Send Messages` と `Embed Links` 権限を付与する。応答後に進まない場合はAMIの `user` イベント権限とダイヤルプランを確認し、再生できない場合はPBXからのファイル読取権限・共有先・`format_wav` を確認する。

参考: [Asterisk Originate](https://docs.asterisk.org/Latest_API/API_Documentation/AMI_Actions/Originate/)、[UserEvent](https://docs.asterisk.org/Latest_API/API_Documentation/Dialplan_Applications/UserEvent/)。
