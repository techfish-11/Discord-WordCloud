package main

import (
	"bytes"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"math/rand"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"
	"golang.org/x/image/font"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"
	_ "modernc.org/sqlite"
)

const (
	defaultDB        = "wordcloud.db"
	maxMessageLength = 4000
)

var errNoWords = errors.New("集計対象の単語がありません")

type App struct {
	db    *sql.DB
	loc   *time.Location
	mu    sync.Mutex
	fonts map[int]font.Face
	text  *TextAnalyzer
	ttf   *opentype.Font
}

type Word struct {
	Text  string
	Count int
}

//go:embed NotoSansJP-VariableFont_wght.ttf
var defaultFontData []byte

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	path := getenv("WORDCLOUD_DB", defaultDB)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		log.Fatal(err)
	}
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		log.Fatal(err)
	}
	analyzer, err := NewTextAnalyzer()
	if err != nil {
		log.Fatal(err)
	}
	app := &App{db: db, loc: loc, fonts: make(map[int]font.Face), text: analyzer}

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" {
		log.Fatal("DISCORD_TOKEN is required")
	}
	s, err := discordgo.New("Bot " + token)
	if err != nil {
		log.Fatal(err)
	}
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	s.AddHandler(app.onReady)
	s.AddHandler(app.onMessage)
	s.AddHandler(app.onInteraction)
	if err := s.Open(); err != nil {
		log.Fatal(err)
	}
	defer s.Close()
	log.Println("wordcloud bot started")
	go app.scheduler(s)
	select {}
}

func initDB(db *sql.DB) error {
	_, err := db.Exec(`PRAGMA journal_mode=WAL;
CREATE TABLE IF NOT EXISTS settings (guild_id TEXT PRIMARY KEY, channel_id TEXT NOT NULL, updated_at INTEGER NOT NULL);
CREATE TABLE IF NOT EXISTS messages (id INTEGER PRIMARY KEY AUTOINCREMENT, guild_id TEXT NOT NULL, channel_id TEXT NOT NULL, message_id TEXT UNIQUE NOT NULL, day TEXT NOT NULL, content TEXT NOT NULL, created_at INTEGER NOT NULL);
CREATE INDEX IF NOT EXISTS idx_messages_channel_day ON messages(channel_id, day);`)
	return err
}

func (a *App) onReady(s *discordgo.Session, _ *discordgo.Ready) {
	_, err := s.ApplicationCommandBulkOverwrite(s.State.User.ID, "", []*discordgo.ApplicationCommand{
		{
			Name:        "wordcloud",
			Description: "ワードクラウドの設定を管理します",
			Options: []*discordgo.ApplicationCommandOption{
				{
					Type:        discordgo.ApplicationCommandOptionSubCommand,
					Name:        "set",
					Description: "記録するテキストチャンネルを設定します",
					Options: []*discordgo.ApplicationCommandOption{
						{
							Type:         discordgo.ApplicationCommandOptionChannel,
							Name:         "channel",
							Description:  "記録するテキストチャンネル",
							Required:     true,
							ChannelTypes: []discordgo.ChannelType{discordgo.ChannelTypeGuildText},
						},
					},
				},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "status", Description: "現在の設定を表示します"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "preview", Description: "今日の暫定ワードクラウドを生成します"},
				{Type: discordgo.ApplicationCommandOptionSubCommand, Name: "disable", Description: "記録を停止します"},
			},
		},
	})
	if err != nil {
		log.Printf("register commands: %v", err)
	}
}

func (a *App) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" || strings.TrimSpace(m.Content) == "" {
		return
	}
	var configured string
	if err := a.db.QueryRow(`SELECT channel_id FROM settings WHERE guild_id=?`, m.GuildID).Scan(&configured); err != nil || configured != m.ChannelID {
		return
	}
	content := m.Content
	if len(content) > maxMessageLength {
		content = content[:maxMessageLength]
	}
	day := time.UnixMilli(m.Timestamp.UnixMilli()).In(a.loc).Format("2006-01-02")
	_, err := a.db.Exec(`INSERT OR IGNORE INTO messages(guild_id,channel_id,message_id,day,content,created_at) VALUES(?,?,?,?,?,?)`, m.GuildID, m.ChannelID, m.ID, day, content, time.Now().Unix())
	if err != nil {
		log.Printf("save message: %v", err)
	}
}

func (a *App) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand || i.GuildID == "" {
		return
	}
	if !hasManageMessages(i.Member) {
		respond(s, i, "この設定を変更するにはメッセージの管理権限が必要だよ", true)
		return
	}
	options := i.ApplicationCommandData().Options
	if len(options) != 1 {
		respond(s, i, "サブコマンドを指定してね", true)
		return
	}
	op := options[0]
	switch op.Name {
	case "set":
		ch := op.Options[0].ChannelValue(s)
		_, err := a.db.Exec(`INSERT INTO settings(guild_id,channel_id,updated_at) VALUES(?,?,?) ON CONFLICT(guild_id) DO UPDATE SET channel_id=excluded.channel_id,updated_at=excluded.updated_at`, i.GuildID, ch.ID, time.Now().Unix())
		if err != nil {
			respond(s, i, "設定に失敗した: "+err.Error(), true)
			return
		}
		respond(s, i, fmt.Sprintf("記録チャンネルを <#%s> に設定したよ。毎日JST 0:00に前日のワードクラウドを送信する", ch.ID), false)
	case "status":
		var ch string
		if err := a.db.QueryRow(`SELECT channel_id FROM settings WHERE guild_id=?`, i.GuildID).Scan(&ch); err != nil {
			respond(s, i, "このサーバーは未設定だよ", true)
		} else {
			respond(s, i, "記録チャンネル: <#"+ch+">", true)
		}
	case "preview":
		a.preview(s, i)
	case "disable":
		_, err := a.db.Exec(`DELETE FROM settings WHERE guild_id=?`, i.GuildID)
		if err != nil {
			respond(s, i, "停止に失敗した: "+err.Error(), true)
			return
		}
		respond(s, i, "記録を停止したよ。保存済みの過去データは削除していない", false)
	}
}

func (a *App) preview(s *discordgo.Session, i *discordgo.InteractionCreate) {
	var channelID string
	if err := a.db.QueryRow(`SELECT channel_id FROM settings WHERE guild_id=?`, i.GuildID).Scan(&channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			respond(s, i, "このサーバーは未設定だよ。先に `/wordcloud set` を実行してね", true)
		} else {
			log.Printf("preview settings %s: %v", i.GuildID, err)
			respond(s, i, "設定の読み込みに失敗したよ", true)
		}
		return
	}
	if err := deferResponse(s, i); err != nil {
		log.Printf("defer preview %s: %v", i.GuildID, err)
		return
	}

	day := time.Now().In(a.loc).Format("2006-01-02")
	pngData, err := a.generate(i.GuildID, channelID, day)
	if errors.Is(err, errNoWords) {
		editResponse(s, i, "今日（"+day+"）は、まだ集計できる単語がないよ", nil)
		return
	}
	if err != nil {
		log.Printf("preview %s: %v", i.GuildID, err)
		editResponse(s, i, "暫定ワードクラウドの生成に失敗したよ", nil)
		return
	}
	editResponse(s, i, "今日（"+day+"）の暫定ワードクラウドです。日次投稿用のデータは削除していません。", &discordgo.File{
		Name:        "wordcloud-preview-" + day + ".png",
		ContentType: "image/png",
		Reader:      bytes.NewReader(pngData),
	})
}

func (a *App) scheduler(s *discordgo.Session) {
	for {
		now := time.Now().In(a.loc)
		next := time.Date(now.Year(), now.Month(), now.Day()+1, 0, 0, 5, 0, a.loc)
		time.Sleep(time.Until(next))
		yesterday := next.AddDate(0, 0, -1).Format("2006-01-02")
		rows, err := a.db.Query(`SELECT guild_id,channel_id FROM settings`)
		if err != nil {
			log.Printf("settings: %v", err)
			continue
		}
		var configs [][2]string
		for rows.Next() {
			var g, c string
			if rows.Scan(&g, &c) == nil {
				configs = append(configs, [2]string{g, c})
			}
		}
		rows.Close()
		for _, cfg := range configs {
			if err := a.publish(s, cfg[0], cfg[1], yesterday); err != nil {
				log.Printf("publish %s: %v", cfg[0], err)
			}
		}
	}
}

func (a *App) publish(s *discordgo.Session, guildID, channelID, day string) error {
	pngData, err := a.generate(guildID, channelID, day)
	if errors.Is(err, errNoWords) {
		_, err = s.ChannelMessageSend(channelID, "昨日（"+day+"）は、集計できるメッセージがなかったよ")
	} else if err == nil {
		_, err = s.ChannelFileSend(channelID, "wordcloud-"+day+".png", bytes.NewReader(pngData))
	}
	if err != nil {
		return err
	}
	_, err = a.db.Exec(`DELETE FROM messages WHERE guild_id=? AND channel_id=? AND day=?`, guildID, channelID, day)
	return err
}

func (a *App) generate(guildID, channelID, day string) ([]byte, error) {
	rows, err := a.db.Query(`SELECT content FROM messages WHERE guild_id=? AND channel_id=? AND day=?`, guildID, channelID, day)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]int{}
	for rows.Next() {
		var text string
		if err := rows.Scan(&text); err != nil {
			return nil, fmt.Errorf("scan message: %w", err)
		}
		perMessage := make(map[string]int)
		for _, w := range a.text.Tokenize(text) {
			// Limit repetition in a single message so pasted text or spam cannot
			// dominate an entire day's conversation.
			if perMessage[w] < 3 {
				counts[w]++
				perMessage[w]++
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read messages: %w", err)
	}
	words := make([]Word, 0, len(counts))
	for w, c := range counts {
		if c >= 2 {
			words = append(words, Word{w, c})
		}
	}
	sort.Slice(words, func(i, j int) bool {
		if words[i].Count != words[j].Count {
			return words[i].Count > words[j].Count
		}
		return words[i].Text < words[j].Text
	})
	if len(words) > 80 {
		words = words[:80]
	}
	if len(words) == 0 {
		return nil, errNoWords
	}
	return a.render(words, day)
}

func (a *App) render(words []Word, day string) ([]byte, error) {
	img := image.NewRGBA(image.Rect(0, 0, 1600, 900))
	draw.Draw(img, img.Bounds(), &image.Uniform{color.RGBA{248, 250, 252, 255}}, image.Point{}, draw.Src)
	r := rand.New(rand.NewSource(int64(len(words)) * 7919))
	placed := []image.Rectangle{}
	palette := []color.RGBA{{38, 99, 235, 255}, {14, 116, 144, 255}, {124, 58, 237, 255}, {5, 150, 105, 255}, {219, 39, 119, 255}, {71, 85, 105, 255}}
	minCount, maxCount := words[len(words)-1].Count, words[0].Count
	for idx, w := range words {
		size := scaledFontSize(w.Count, minCount, maxCount)
		face, err := a.face(size)
		if err != nil {
			return nil, err
		}
		d := font.Drawer{Face: face}
		tw := d.MeasureString(w.Text).Ceil()
		th := size + 8
		var rect image.Rectangle
		found := false
		for n := 0; n < 3000; n++ {
			ang := float64(n) * 0.34
			rad := float64(n) * 0.65
			x := 800 + int(math.Cos(ang)*rad) - tw/2
			y := 460 + int(math.Sin(ang)*rad) - th/2
			rect = image.Rect(x-8, y-4, x+tw+8, y+th+4)
			if rect.Min.X < 20 || rect.Max.X > 1580 || rect.Min.Y < 70 || rect.Max.Y > 870 {
				continue
			}
			ok := true
			for _, p := range placed {
				if rect.Overlaps(p) {
					ok = false
					break
				}
			}
			if ok {
				found = true
				break
			}
		}
		if !found {
			continue
		}
		placed = append(placed, rect)
		x, y := rect.Min.X+8, rect.Min.Y+th-8
		d.Dst = img
		d.Dot = fixed.P(x, y)
		col := palette[(idx+int(r.Int31n(3)))%len(palette)]
		d.Src = image.NewUniform(col)
		d.DrawString(w.Text)
	}
	face, _ := a.face(28)
	d := font.Drawer{Dst: img, Src: image.NewUniform(color.RGBA{30, 41, 59, 255}), Face: face, Dot: fixed.P(42, 52)}
	d.DrawString("Word Cloud  ·  " + day)
	var b bytes.Buffer
	err := png.Encode(&b, img)
	return b.Bytes(), err
}

func (a *App) face(size int) (font.Face, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if f := a.fonts[size]; f != nil {
		return f, nil
	}
	if a.ttf == nil {
		data := defaultFontData
		if path := os.Getenv("WORDCLOUD_FONT"); path != "" {
			var err error
			data, err = os.ReadFile(path)
			if err != nil {
				return nil, fmt.Errorf("read WORDCLOUD_FONT %q: %w", path, err)
			}
		}
		var err error
		a.ttf, err = opentype.Parse(data)
		if err != nil {
			return nil, fmt.Errorf("parse word-cloud font: %w", err)
		}
	}
	f, err := opentype.NewFace(a.ttf, &opentype.FaceOptions{Size: float64(size), DPI: 72, Hinting: font.HintingFull})
	if err == nil {
		a.fonts[size] = f
	}
	return f, err
}

func scaledFontSize(count, minCount, maxCount int) int {
	const minSize, maxSize = 28, 124
	if maxCount <= minCount {
		return (minSize + maxSize) / 2
	}
	// Log scaling prevents one highly repeated word from dwarfing every other
	// term while preserving the ordering of frequencies.
	ratio := math.Log1p(float64(count-minCount)) / math.Log1p(float64(maxCount-minCount))
	return minSize + int(math.Round(ratio*(maxSize-minSize)))
}
func hasManageMessages(m *discordgo.Member) bool {
	return m != nil && (m.Permissions&discordgo.PermissionManageMessages) != 0
}
func respond(s *discordgo.Session, i *discordgo.InteractionCreate, msg string, ephemeral bool) {
	flags := discordgo.MessageFlags(0)
	if ephemeral {
		flags = discordgo.MessageFlagsEphemeral
	}
	_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Content: msg, Flags: flags}})
}

func deferResponse(s *discordgo.Session, i *discordgo.InteractionCreate) error {
	return s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{
		Type: discordgo.InteractionResponseDeferredChannelMessageWithSource,
	})
}

func editResponse(s *discordgo.Session, i *discordgo.InteractionCreate, content string, file *discordgo.File) {
	edit := &discordgo.WebhookEdit{Content: &content}
	if file != nil {
		edit.Files = []*discordgo.File{file}
	}
	if _, err := s.InteractionResponseEdit(i.Interaction, edit); err != nil {
		log.Printf("edit interaction response %s: %v", i.ID, err)
	}
}
func getenv(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
