package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"
)

type callConfig struct {
	sip                                        sipConfig
	users                                      map[string]bool
	location                                   *time.Location
	quietStart, quietEnd, concurrency, speaker int
	quietStartMinute, quietEndMinute           int
	cooldown, ringTimeout, totalTimeout        time.Duration
}

type callService struct {
	cfg       callConfig
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.Mutex
	active    int
	phone     callPhone
	closeOnce sync.Once
	last      map[string]time.Time
	wg        sync.WaitGroup
	http      *http.Client
}

var digitsOnly = regexp.MustCompile(`^[0-9]+$`)

func newCallService() (*callService, error) {
	enabled := os.Getenv("CALL_ENABLED")
	if enabled == "" || enabled == "false" {
		return nil, nil
	}
	if enabled != "true" {
		return nil, errors.New("CALL_ENABLED must be true or false")
	}
	c := callConfig{}
	var err error
	c.sip, err = loadSIPConfig()
	if err != nil {
		return nil, err
	}
	c.users, err = parseCallAllowedUsers(os.Getenv("CALL_ALLOWED_USERS"))
	if err != nil {
		return nil, err
	}
	c.location, err = time.LoadLocation(getenv("CALL_TIMEZONE", "Asia/Tokyo"))
	if err != nil {
		return nil, fmt.Errorf("CALL_TIMEZONE: %w", err)
	}
	ints := []struct {
		key, fallback string
		min, max      int
		dst           *int
	}{
		{"CALL_QUIET_START_HOUR", "23", 0, 23, &c.quietStart}, {"CALL_QUIET_END_HOUR", "6", 0, 23, &c.quietEnd},
		{"CALL_QUIET_START_MINUTE", "59", 0, 59, &c.quietStartMinute}, {"CALL_QUIET_END_MINUTE", "0", 0, 59, &c.quietEndMinute},
		{"CALL_MAX_CONCURRENT", "1", 1, 10, &c.concurrency}, {"CALL_SPEAKER", "3", 0, 100, &c.speaker},
	}
	for _, setting := range ints {
		v, e := strconv.Atoi(getenv(setting.key, setting.fallback))
		if e != nil || v < setting.min || v > setting.max {
			return nil, fmt.Errorf("invalid %s", setting.key)
		}
		*setting.dst = v
	}
	if c.quietStart == c.quietEnd && c.quietStartMinute == c.quietEndMinute {
		return nil, errors.New("quiet start and end times must differ")
	}
	for _, setting := range []struct {
		key, fallback string
		min, max      time.Duration
		dst           *time.Duration
	}{
		{"CALL_USER_COOLDOWN", "5m", time.Second, 24 * time.Hour, &c.cooldown}, {"CALL_RING_TIMEOUT", "30s", 5 * time.Second, 90 * time.Second, &c.ringTimeout}, {"CALL_TOTAL_TIMEOUT", "4m", time.Minute, 10 * time.Minute, &c.totalTimeout},
	} {
		v, e := time.ParseDuration(getenv(setting.key, setting.fallback))
		if e != nil || v < setting.min || v > setting.max {
			return nil, fmt.Errorf("invalid %s", setting.key)
		}
		*setting.dst = v
	}
	ctx, cancel := context.WithCancel(context.Background())
	phone, err := newSIPPhone(ctx, c.sip, c.ringTimeout)
	if err != nil {
		cancel()
		return nil, err
	}
	return &callService{cfg: c, ctx: ctx, cancel: cancel, phone: phone, last: make(map[string]time.Time), http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("TTS redirect refused") }}}, nil
}

// An absent or blank list disables user filtering. A nonblank but malformed
// list is a configuration error, never an instruction to allow everyone.
func parseCallAllowedUsers(value string) (map[string]bool, error) {
	if strings.TrimSpace(value) == "" {
		return nil, nil
	}
	users := make(map[string]bool)
	for _, user := range strings.Split(value, ",") {
		user = strings.TrimSpace(user)
		if !digitsOnly.MatchString(user) || len(user) < 17 || len(user) > 20 {
			return nil, errors.New("CALL_ALLOWED_USERS requires comma-separated Discord user IDs")
		}
		users[user] = true
	}
	return users, nil
}

func (c *callService) close() {
	c.mu.Lock()
	c.cancel()
	c.mu.Unlock()
	c.wg.Wait()
	c.closeOnce.Do(func() {
		if c.phone != nil {
			c.phone.Close()
		}
	})
}

func (c *callService) reserve(user string, now time.Time) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return nil, errors.New("Botは終了処理中です。")
	}
	if len(c.cfg.users) > 0 && !c.cfg.users[user] {
		return nil, errors.New("このコマンドを利用する権限がありません。")
	}
	if c.cfg.quiet(now) {
		return nil, errors.New("現在は深夜の発信制限時間帯です。")
	}
	if elapsed := now.Sub(c.last[user]); elapsed < c.cfg.cooldown {
		return nil, fmt.Errorf("次の発信まであと%d秒お待ちください。", int((c.cfg.cooldown-elapsed).Seconds())+1)
	}
	if c.active >= c.cfg.concurrency {
		return nil, errors.New("同時発信数の上限です。しばらくしてから再実行してください。")
	}
	c.last[user] = now
	c.active++
	c.wg.Add(1)
	return func() { c.mu.Lock(); c.active--; c.mu.Unlock(); c.wg.Done() }, nil
}

func (c callConfig) quiet(now time.Time) bool {
	local := now.In(c.location)
	minute := local.Hour()*60 + local.Minute()
	start := c.quietStart*60 + c.quietStartMinute
	end := c.quietEnd*60 + c.quietEndMinute
	if start > end {
		return minute >= start || minute < end
	}
	return minute >= start && minute < end
}

func validateCallMessage(message string) error {
	if strings.TrimSpace(message) == "" || !utf8.ValidString(message) || utf8.RuneCountInString(message) > 500 {
		return errors.New("メッセージは1〜500文字で指定してください。")
	}
	for _, r := range message {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return errors.New("メッセージに使用できない制御文字が含まれています。")
		}
	}
	return nil
}

func (a *App) handleCall(s *discordgo.Session, i *discordgo.InteractionCreate) {
	if a.calls == nil {
		respond(s, i, "音声メモ機能は無効です。", true)
		return
	}
	if i.Member == nil || i.Member.User == nil {
		respond(s, i, "ユーザー情報を取得できません。", true)
		return
	}
	opts := i.ApplicationCommandData().Options
	if len(opts) != 1 || opts[0].Name != "message" || opts[0].Type != discordgo.ApplicationCommandOptionString {
		respond(s, i, "messageを指定してください。", true)
		return
	}
	message, ok := opts[0].Value.(string)
	if !ok {
		respond(s, i, "messageが不正です。", true)
		return
	}
	if err := validateCallMessage(message); err != nil {
		respond(s, i, err.Error(), true)
		return
	}
	release, err := a.calls.reserve(i.Member.User.ID, time.Now())
	if err != nil {
		respond(s, i, err.Error(), true)
		return
	}
	defer release()
	if err = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseDeferredChannelMessageWithSource, Data: &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral}}); err != nil {
		log.Printf("defer call: %v", err)
		return
	}
	update := func(status string) {
		embeds := []*discordgo.MessageEmbed{{Title: "音声メモ", Description: status, Color: 0x5865F2, Footer: &discordgo.MessageEmbedFooter{Text: "VOICEVOX:ずんだもん"}, Timestamp: time.Now().UTC().Format(time.RFC3339)}}
		if _, err := s.InteractionResponseEdit(i.Interaction, &discordgo.WebhookEdit{Embeds: &embeds, AllowedMentions: &discordgo.MessageAllowedMentions{}}); err != nil {
			log.Printf("edit call response: %v", err)
		}
	}
	update("音声を準備しています…")
	ctx, cancel := context.WithTimeout(a.calls.ctx, a.calls.cfg.totalTimeout)
	defer cancel()
	status, err := a.calls.run(ctx, message, update)
	if err != nil {
		log.Printf("call failed user=%s: %v", i.Member.User.ID, err)
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			update("処理がタイムアウトしました。通話の完了は確認できませんでした。")
			return
		}
		update("発信または音声再生に失敗しました。管理者にお問い合わせください。")
		return
	}
	update(status)
}

// postTTS intentionally omits remote response bodies and message text from errors.
func (c *callService) postTTS(ctx context.Context, endpoint string, payload any, limit int64) ([]byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://voicevox-api.swiftlybot.com/v1/"+endpoint, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS %s request failed", endpoint)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("TTS %s HTTP %d", endpoint, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, fmt.Errorf("read TTS %s: %w", endpoint, err)
	}
	if int64(len(body)) > limit {
		return nil, errors.New("TTS response too large")
	}
	return body, nil
}

func (c *callService) audio(ctx context.Context, message string) ([]byte, error) {
	query, err := c.postTTS(ctx, "audio_query", map[string]any{"text": message, "speaker": c.cfg.speaker}, 1<<20)
	if err != nil {
		return nil, err
	}
	var q map[string]json.RawMessage
	if err = json.Unmarshal(query, &q); err != nil || q == nil {
		return nil, errors.New("invalid TTS audio query")
	}
	q["outputSamplingRate"] = json.RawMessage(`8000`)
	q["outputStereo"] = json.RawMessage(`false`)
	wav, err := c.postTTS(ctx, "synthesis", map[string]any{"speaker": c.cfg.speaker, "audioQuery": q}, 16<<20)
	if err != nil {
		return nil, err
	}
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return nil, errors.New("TTS did not return WAV")
	}
	if err := validateCallWAV(wav); err != nil {
		return nil, err
	}
	return wav, nil
}

func (c *callService) run(ctx context.Context, message string, update func(string)) (string, error) {
	if !c.phone.Ready() {
		return "", errors.New("SIP registration is not ready")
	}
	wav, err := c.audio(ctx, message)
	if err != nil {
		return "", err
	}
	if c.cfg.quiet(time.Now()) {
		return "現在は深夜の発信制限時間帯です。発信しませんでした。", nil
	}
	return c.phone.Call(ctx, wav, update)
}
