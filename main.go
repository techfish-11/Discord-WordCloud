package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"log"
	"math"
	"math/rand"
	"os"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/bwmarrin/discordgo"
	_ "modernc.org/sqlite"
	"golang.org/x/image/font"
	"golang.org/x/image/font/gofont/goregular"
	"golang.org/x/image/math/fixed"
	"golang.org/x/image/font/opentype"
)

const (
	defaultDB = "wordcloud.db"
	maxMessageLength = 4000
)

type App struct {
	db *sql.DB
	loc *time.Location
	mu sync.Mutex
	fonts map[int]font.Face
}

type Word struct { Text string; Count int }

var latinWord = regexp.MustCompile(`[A-Za-z][A-Za-z0-9_'-]{1,30}`)
var urlOrMention = regexp.MustCompile(`https?://\S+|<[@#!&]?\d+>`)
var stopWords = map[string]bool{
	"the":true,"and":true,"that":true,"this":true,"with":true,"from":true,"have":true,"www":true,
	"です":true,"ます":true,"した":true,"して":true,"する":true,"ある":true,"いる":true,"これ":true,"それ":true,
	"こと":true,"よう":true,"さん":true,"ちゃん":true,"ｗｗ":true,
}

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	path := getenv("WORDCLOUD_DB", defaultDB)
	db, err := sql.Open("sqlite", path)
	if err != nil { log.Fatal(err) }
	defer db.Close()
	if err := initDB(db); err != nil { log.Fatal(err) }
	loc, err := time.LoadLocation("Asia/Tokyo")
	if err != nil { log.Fatal(err) }
	app := &App{db: db, loc: loc, fonts: make(map[int]font.Face)}

	token := os.Getenv("DISCORD_TOKEN")
	if token == "" { log.Fatal("DISCORD_TOKEN is required") }
	s, err := discordgo.New("Bot " + token)
	if err != nil { log.Fatal(err) }
	s.Identify.Intents = discordgo.IntentsGuilds | discordgo.IntentsGuildMessages | discordgo.IntentMessageContent
	s.AddHandler(app.onReady)
	s.AddHandler(app.onMessage)
	s.AddHandler(app.onInteraction)
	if err := s.Open(); err != nil { log.Fatal(err) }
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
		{Name:"wordcloud", Description:"ワードクラウドの設定を管理する", Options: []*discordgo.ApplicationCommandOption{{Type:discordgo.ApplicationCommandOptionSubCommand, Name:"set", Description:"このサーバーの記録チャンネルを設定", Options: []*discordgo.ApplicationCommandOption{{Type:discordgo.ApplicationCommandOptionChannel, Name:"channel", Description:"記録するチャンネル", Required:true, ChannelTypes:[]discordgo.ChannelType{discordgo.ChannelTypeGuildText}}}}}, {Type:discordgo.ApplicationCommandOptionSubCommand, Name:"status", Description:"現在の設定を表示"}, {Type:discordgo.ApplicationCommandOptionSubCommand, Name:"disable", Description:"記録を停止"}}},
	})
	if err != nil { log.Printf("register commands: %v", err) }
}

func (a *App) onMessage(s *discordgo.Session, m *discordgo.MessageCreate) {
	if m.Author == nil || m.Author.Bot || m.GuildID == "" || strings.TrimSpace(m.Content) == "" { return }
	var configured string
	if err := a.db.QueryRow(`SELECT channel_id FROM settings WHERE guild_id=?`, m.GuildID).Scan(&configured); err != nil || configured != m.ChannelID { return }
	content := m.Content
	if len(content) > maxMessageLength { content = content[:maxMessageLength] }
	day := time.UnixMilli(m.Timestamp.UnixMilli()).In(a.loc).Format("2006-01-02")
	_, err := a.db.Exec(`INSERT OR IGNORE INTO messages(guild_id,channel_id,message_id,day,content,created_at) VALUES(?,?,?,?,?,?)`, m.GuildID, m.ChannelID, m.ID, day, content, time.Now().Unix())
	if err != nil { log.Printf("save message: %v", err) }
}

func (a *App) onInteraction(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if i.Type != discordgo.InteractionApplicationCommand || i.GuildID == "" { return }
	if !hasManageGuild(i.Member) { respond(s, i, "この設定を変更するにはサーバー管理権限が必要だよ", true); return }
	op := i.ApplicationCommandData().Options[0]
	switch op.Name {
	case "set":
		ch := op.Options[0].ChannelValue(s)
		_, err := a.db.Exec(`INSERT INTO settings(guild_id,channel_id,updated_at) VALUES(?,?,?) ON CONFLICT(guild_id) DO UPDATE SET channel_id=excluded.channel_id,updated_at=excluded.updated_at`, i.GuildID, ch.ID, time.Now().Unix())
		if err != nil { respond(s,i,"設定に失敗した: "+err.Error(),true); return }
		respond(s,i,fmt.Sprintf("記録チャンネルを <#%s> に設定したよ。毎日JST 0:00に前日のワードクラウドを送信する", ch.ID), false)
	case "status":
		var ch string
		if err := a.db.QueryRow(`SELECT channel_id FROM settings WHERE guild_id=?`, i.GuildID).Scan(&ch); err != nil { respond(s,i,"このサーバーは未設定だよ",true) } else { respond(s,i,"記録チャンネル: <#"+ch+">",true) }
	case "disable":
		_, err := a.db.Exec(`DELETE FROM settings WHERE guild_id=?`, i.GuildID)
		if err != nil { respond(s,i,"停止に失敗した: "+err.Error(),true); return }
		respond(s,i,"記録を停止したよ。保存済みの過去データは削除していない",false)
	}
}

func (a *App) scheduler(s *discordgo.Session) {
	for {
		now := time.Now().In(a.loc)
		next := time.Date(now.Year(),now.Month(),now.Day()+1,0,0,5,0,a.loc)
		time.Sleep(time.Until(next))
		yesterday := next.AddDate(0,0,-1).Format("2006-01-02")
		rows, err := a.db.Query(`SELECT guild_id,channel_id FROM settings`)
		if err != nil { log.Printf("settings: %v",err); continue }
		var configs [][2]string
		for rows.Next() { var g,c string; if rows.Scan(&g,&c)==nil { configs=append(configs,[2]string{g,c}) } }; rows.Close()
		for _, cfg := range configs { if err := a.publish(s,cfg[0],cfg[1],yesterday); err != nil { log.Printf("publish %s: %v",cfg[0],err) } }
	}
}

func (a *App) publish(s *discordgo.Session, guildID, channelID, day string) error {
	rows, err := a.db.Query(`SELECT content FROM messages WHERE guild_id=? AND channel_id=? AND day=?`,guildID,channelID,day); if err != nil{return err}; defer rows.Close()
	counts := map[string]int{}
	for rows.Next(){var text string; if rows.Scan(&text)==nil {for _,w:=range tokenize(text){counts[w]++}}}
	words:=make([]Word,0,len(counts)); for w,c:=range counts {if c>=2 {words=append(words,Word{w,c})}}
	sort.Slice(words,func(i,j int)bool{return words[i].Count>words[j].Count}); if len(words)>80 {words=words[:80]}
	if len(words)==0 { _,err=s.ChannelMessageSend(channelID,"昨日（"+day+"）は、集計できるメッセージがなかったよ"); if err==nil {_,err=a.db.Exec(`DELETE FROM messages WHERE guild_id=? AND channel_id=? AND day=?`,guildID,channelID,day)}; return err }
	pngData,err:=a.render(words,day); if err!=nil{return err}
	_,err=s.ChannelFileSend(channelID,"wordcloud-"+day+".png","image/png",bytes.NewReader(pngData)); if err==nil {_,err=a.db.Exec(`DELETE FROM messages WHERE guild_id=? AND channel_id=? AND day=?`,guildID,channelID,day)}; return err
}

func tokenize(text string) []string {
	text=urlOrMention.ReplaceAllString(text," "); out:=make([]string,0)
	for _,w:=range latinWord.FindAllString(strings.ToLower(text),-1){if !stopWords[w] {out=append(out,w)}}
	run:=[]rune{}; flush:=func(){for i:=0;i+1<len(run);i++{w:=string(run[i:i+2]);if !stopWords[w]{out=append(out,w)}};run=nil}
	for _,r:=range text {if unicode.In(r,unicode.Han,unicode.Hiragana,unicode.Katakana) {run=append(run,r)} else {flush()}};flush();return out
}

func (a *App) render(words []Word, day string) ([]byte,error) {
	img:=image.NewRGBA(image.Rect(0,0,1600,900)); draw.Draw(img,img.Bounds(),&image.Uniform{color.RGBA{248,250,252,255}},image.Point{},draw.Src)
	r:=rand.New(rand.NewSource(int64(len(words))*7919)); placed:=[]image.Rectangle{}; palette:=[]color.RGBA{{38,99,235,255},{14,116,144,255},{124,58,237,255},{5,150,105,255},{219,39,119,255},{71,85,105,255}}
	for idx,w:=range words {size:=24+int(math.Sqrt(float64(w.Count))*12);if size>116{size=116};face,err:=a.face(size);if err!=nil{return nil,err}; d:=font.Drawer{Face:face}; tw:=d.MeasureString(w.Text).Ceil(); th:=size+8; var rect image.Rectangle; found:=false
		for n:=0;n<3000;n++ {ang:=float64(n)*0.34;rad:=float64(n)*0.65;x:=800+int(math.Cos(ang)*rad)-tw/2;y:=460+int(math.Sin(ang)*rad)-th/2;rect=image.Rect(x-8,y-4,x+tw+8,y+th+4);if rect.Min.X<20||rect.Max.X>1580||rect.Min.Y<70||rect.Max.Y>870{continue};ok:=true;for _,p:=range placed{if rect.Overlaps(p){ok=false;break}};if ok{found=true;break}}
		if !found{continue}; placed=append(placed,rect); x,y:=rect.Min.X+8,rect.Min.Y+th-8; d.Dst=img; d.Dot=fixed.P(x,y); col:=palette[(idx+int(r.Int31n(3)))%len(palette)]; d.Src=image.NewUniform(col);d.DrawString(w.Text)
	}
	face,_:=a.face(28);d:=font.Drawer{Dst:img,Src:image.NewUniform(color.RGBA{30,41,59,255}),Face:face,Dot:fixed.P(42,52)};d.DrawString("Word Cloud  ·  "+day)
	var b bytes.Buffer;err:=png.Encode(&b,img);return b.Bytes(),err
}

func (a *App) face(size int)(font.Face,error){
	a.mu.Lock(); defer a.mu.Unlock()
	if f:=a.fonts[size]; f!=nil{return f,nil}
	data:=goregular.TTF
	if path:=os.Getenv("WORDCLOUD_FONT"); path!="" {if b,err:=os.ReadFile(path); err==nil {data=b}}
	tt,err:=opentype.Parse(data); if err!=nil{return nil,err}
	f,err:=opentype.NewFace(tt,&opentype.FaceOptions{Size:float64(size),DPI:72,Hinting:font.HintingFull}); if err==nil{a.fonts[size]=f}; return f,err
}
func hasManageGuild(m *discordgo.Member)bool{return m!=nil && (m.Permissions&discordgo.PermissionManageGuild)!=0}
func respond(s *discordgo.Session,i *discordgo.InteractionCreate,msg string,ephemeral bool){flags:=discordgo.MessageFlags(0);if ephemeral{flags=discordgo.MessageFlagsEphemeral};_ = s.InteractionRespond(i.Interaction,&discordgo.InteractionResponse{Type:discordgo.InteractionResponseChannelMessageWithSource,Data:&discordgo.InteractionResponseData{Content:msg,Flags:flags}})}
func getenv(k,d string)string{if v:=os.Getenv(k);v!=""{return v};return d}
