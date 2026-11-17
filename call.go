package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	address, username, secret, extension, directory, pbxDirectory, ffmpeg string
	useTLS                                                                bool
	users                                                                 map[string]bool
	location                                                              *time.Location
	quietStart, quietEnd, concurrency, speaker                            int
	quietStartMinute, quietEndMinute                                      int
	cooldown, ringTimeout, totalTimeout                                   time.Duration
}

type callService struct {
	cfg            callConfig
	ctx            context.Context
	cancel         context.CancelFunc
	mu             sync.Mutex
	active         int
	uncertainUntil time.Time
	last           map[string]time.Time
	wg             sync.WaitGroup
	http           *http.Client
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
	c := callConfig{address: os.Getenv("CALL_AMI_ADDRESS"), username: os.Getenv("CALL_AMI_USERNAME"), secret: os.Getenv("CALL_AMI_SECRET"), extension: os.Getenv("CALL_EXTENSION"), directory: os.Getenv("CALL_AUDIO_DIR"), pbxDirectory: os.Getenv("CALL_PBX_AUDIO_DIR"), ffmpeg: getenv("CALL_FFMPEG", "ffmpeg"), users: make(map[string]bool)}
	for key, value := range map[string]string{"CALL_AMI_ADDRESS": c.address, "CALL_AMI_USERNAME": c.username, "CALL_AMI_SECRET": c.secret, "CALL_EXTENSION": c.extension, "CALL_AUDIO_DIR": c.directory, "CALL_PBX_AUDIO_DIR": c.pbxDirectory} {
		if value == "" || strings.ContainsAny(value, "\r\n\x00") {
			return nil, fmt.Errorf("%s is required and must contain no control delimiters", key)
		}
	}
	if _, _, err := net.SplitHostPort(c.address); err != nil {
		return nil, fmt.Errorf("CALL_AMI_ADDRESS: %w", err)
	}
	if !digitsOnly.MatchString(c.extension) || len(c.extension) > 20 {
		return nil, errors.New("CALL_EXTENSION must be 1–20 digits (PJSIP endpoint)")
	}
	if !filepath.IsAbs(c.directory) || !regexp.MustCompile(`^/[a-zA-Z0-9_/-]+$`).MatchString(c.pbxDirectory) {
		return nil, errors.New("audio directories must be absolute; CALL_PBX_AUDIO_DIR must be a safe Linux path")
	}
	for _, user := range strings.Split(os.Getenv("CALL_ALLOWED_USERS"), ",") {
		user = strings.TrimSpace(user)
		if !digitsOnly.MatchString(user) || len(user) < 17 || len(user) > 20 {
			return nil, errors.New("CALL_ALLOWED_USERS requires comma-separated Discord user IDs")
		}
		c.users[user] = true
	}
	var err error
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
	if c.totalTimeout < c.ringTimeout+135*time.Second {
		return nil, errors.New("CALL_TOTAL_TIMEOUT must allow ringing plus at least 135s")
	}
	c.useTLS, err = strconv.ParseBool(getenv("CALL_AMI_TLS", "false"))
	if err != nil {
		return nil, fmt.Errorf("CALL_AMI_TLS: %w", err)
	}
	if _, err = exec.LookPath(c.ffmpeg); err != nil {
		return nil, fmt.Errorf("CALL_FFMPEG: %w", err)
	}
	info, err := os.Stat(c.directory)
	if err != nil {
		return nil, fmt.Errorf("CALL_AUDIO_DIR: %w", err)
	}
	if !info.IsDir() {
		return nil, errors.New("CALL_AUDIO_DIR must be a directory")
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &callService{cfg: c, ctx: ctx, cancel: cancel, last: make(map[string]time.Time), http: &http.Client{Timeout: 45 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("TTS redirect refused") }}}, nil
}

func (c *callService) close() { c.mu.Lock(); c.cancel(); c.mu.Unlock(); c.wg.Wait() }

func (c *callService) reserve(user string, now time.Time) (func(), error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.ctx.Err() != nil {
		return nil, errors.New("Botは終了処理中です。")
	}
	if !c.cfg.users[user] {
		return nil, errors.New("このコマンドを利用する権限がありません。")
	}
	if c.cfg.quiet(now) {
		return nil, errors.New("現在は深夜の発信制限時間帯です。")
	}
	if now.Before(c.uncertainUntil) {
		return nil, errors.New("直前の通話の終了を確認できないため、安全のため発信を一時停止しています。")
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

func (c *callService) audio(ctx context.Context, message, id string) (string, error) {
	query, err := c.postTTS(ctx, "audio_query", map[string]any{"text": message, "speaker": c.cfg.speaker}, 1<<20)
	if err != nil {
		return "", err
	}
	var q map[string]json.RawMessage
	if err = json.Unmarshal(query, &q); err != nil || q == nil {
		return "", errors.New("invalid TTS audio query")
	}
	wav, err := c.postTTS(ctx, "synthesis", map[string]any{"speaker": c.cfg.speaker, "audioQuery": q}, 16<<20)
	if err != nil {
		return "", err
	}
	if len(wav) < 12 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" {
		return "", errors.New("TTS did not return WAV")
	}
	// Asterisk format_wav requires 8kHz, mono, signed 16-bit PCM.
	file, err := os.CreateTemp(c.cfg.directory, ".call-*.wav")
	if err != nil {
		return "", err
	}
	temp := file.Name()
	defer os.Remove(temp)
	if err = file.Close(); err != nil {
		return "", err
	}
	convertCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(convertCtx, c.cfg.ffmpeg, "-nostdin", "-v", "error", "-y", "-i", "pipe:0", "-t", "121", "-ar", "8000", "-ac", "1", "-c:a", "pcm_s16le", "-f", "wav", temp)
	cmd.Stdin = bytes.NewReader(wav)
	if err = cmd.Run(); err != nil {
		return "", fmt.Errorf("convert WAV: %w", err)
	}
	info, err := os.Stat(temp)
	if err != nil {
		return "", err
	}
	if info.Size() <= 44 || info.Size() > 120*8000*2+44 {
		return "", errors.New("converted audio is empty or exceeds 120 seconds")
	}
	if err = os.Chmod(temp, 0640); err != nil {
		return "", err
	}
	final := filepath.Join(c.cfg.directory, id+".wav")
	if err = os.Rename(temp, final); err != nil {
		return "", err
	}
	return final, nil
}

func (c *callService) run(ctx context.Context, message string, update func(string)) (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	id := "discord-" + hex.EncodeToString(nonce[:])
	file, err := c.audio(ctx, message, id)
	if err != nil {
		return "", err
	}
	defer func() {
		if err := os.Remove(file); err != nil {
			log.Printf("remove call audio: %v", err)
		}
	}()
	// TTS may cross the quiet-hours boundary. Recheck immediately before dialing.
	if c.cfg.quiet(time.Now()) {
		return "現在は深夜の発信制限時間帯です。発信しませんでした。", nil
	}
	// Leave room for the PBX watchdog even if the caller selected a short total timeout.
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < c.cfg.ringTimeout+135*time.Second {
		return "", errors.New("insufficient time remaining to safely originate")
	}
	return c.originate(ctx, id, update)
}

// AMI uses one authenticated connection per call, isolating events and failures.
// All reads are bounded; cancellation closes the socket to unblock pending I/O.
type amiReader struct{ scanner *bufio.Scanner }

func newAMIReader(r io.Reader) *amiReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 4096), 64<<10)
	return &amiReader{s}
}
func (r *amiReader) frame() (map[string]string, error) {
	m := make(map[string]string)
	size := 0
	for r.scanner.Scan() {
		line := r.scanner.Text()
		size += len(line)
		if size > 256<<10 {
			return nil, errors.New("AMI frame too large")
		}
		if line == "" {
			if len(m) > 0 {
				return m, nil
			}
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		if ok {
			m[strings.ToLower(key)] = strings.TrimSpace(value)
		}
	}
	if err := r.scanner.Err(); err != nil {
		return nil, err
	}
	return nil, io.EOF
}
func amiWrite(conn net.Conn, fields ...string) error {
	for _, field := range fields {
		if strings.ContainsAny(field, "\r\n\x00") {
			return errors.New("invalid AMI header")
		}
	}
	if err := conn.SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return err
	}
	_, err := io.WriteString(conn, strings.Join(fields, "\r\n")+"\r\n\r\n")
	return err
}

func (c *callService) originate(ctx context.Context, id string, update func(string)) (string, error) {
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	var conn net.Conn
	var err error
	if c.cfg.useTLS {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: &tls.Config{MinVersion: tls.VersionTLS12}}).DialContext(ctx, "tcp", c.cfg.address)
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", c.cfg.address)
	}
	if err != nil {
		return "", fmt.Errorf("AMI connect: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	if err = conn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
		return "", err
	}
	r := newAMIReader(conn)
	if err = amiWrite(conn, "Action: Login", "ActionID: login", "Username: "+c.cfg.username, "Secret: "+c.cfg.secret, "Events: on"); err != nil {
		return "", err
	}
	for {
		f, e := r.frame()
		if e != nil {
			return "", fmt.Errorf("AMI login: %w", e)
		}
		if f["actionid"] == "login" {
			if f["response"] != "Success" {
				return "", errors.New("AMI authentication rejected")
			}
			break
		}
	}
	deadline, _ := ctx.Deadline()
	if err = conn.SetReadDeadline(deadline); err != nil {
		return "", err
	}
	// PBX-side absolute timeout remains effective even if this connection is lost.
	started, settled := time.Now(), false
	defer func() {
		if !settled {
			// A lost AMI connection cannot prove that the remote channel stopped.
			// Block new calls until even the longest possible remote call expires.
			until := started.Add(c.cfg.ringTimeout + 135*time.Second)
			c.mu.Lock()
			if until.After(c.uncertainUntil) {
				c.uncertainUntil = until
			}
			c.mu.Unlock()
		}
	}()
	if err = amiWrite(conn, "Action: Originate", "ActionID: "+id, "Channel: PJSIP/"+c.cfg.extension, "ChannelId: "+id, "Context: discord-voice-memo", "Exten: s", "Priority: 1", "Async: true", "Timeout: "+strconv.FormatInt(c.cfg.ringTimeout.Milliseconds(), 10), "Variable: MEMO_ID="+id, "Variable: MEMO_FILE="+strings.TrimRight(c.cfg.pbxDirectory, "/")+"/"+id); err != nil {
		return "", err
	}
	defer func() {
		if err := amiWrite(conn, "Action: Hangup", "Channel: "+id); err != nil && ctx.Err() == nil {
			log.Printf("AMI cleanup hangup: %v", err)
		}
	}()
	update("発信中…")
	answered, played := false, false
	for {
		f, err := r.frame()
		if err != nil {
			return "", fmt.Errorf("AMI event stream interrupted: %w", err)
		}
		if f["actionid"] == id && f["response"] == "Error" {
			settled = true
			return "", errors.New("AMI originate rejected")
		}
		if f["event"] == "OriginateResponse" && f["actionid"] == id && f["response"] == "Failure" {
			settled = true
			if f["reason"] == "3" || f["reason"] == "5" {
				return "相手が応答しませんでした（不在または通話中）。", nil
			}
			return "", fmt.Errorf("originate failed (reason %s)", f["reason"])
		}
		if f["event"] == "UserEvent" && f["userevent"] == "DiscordMemo" && f["memoid"] == id {
			switch f["status"] {
			case "answered":
				if !answered {
					answered = true
					update("相手が出ました。音声メモを再生しています…")
				}
			case "played":
				played = f["playbackstatus"] == "SUCCESS"
			}
		}
		if f["event"] == "Hangup" && f["uniqueid"] == id {
			settled = true
			if played {
				return "通話が完了しました。音声メモを最後まで再生しました。", nil
			}
			if answered {
				return "通話が終了しました。音声メモは最後まで再生されませんでした。", nil
			}
			// OriginateResponse follows Hangup on failed attempts; wait for its reason.
		}
	}
}
