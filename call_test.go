package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAudioConversion(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("FFmpeg is not installed")
	}
	for _, seconds := range []int{1, 121} {
		t.Run(strconv.Itoa(seconds), func(t *testing.T) {
			// Generate real 24kHz PCM input, as returned by VOICEVOX.
			pcm := make([]byte, seconds*24000*2)
			var wav bytes.Buffer
			wav.WriteString("RIFF")
			binary.Write(&wav, binary.LittleEndian, uint32(36+len(pcm)))
			wav.WriteString("WAVEfmt ")
			for _, v := range []any{uint32(16), uint16(1), uint16(1), uint32(24000), uint32(48000), uint16(2), uint16(16)} {
				if err := binary.Write(&wav, binary.LittleEndian, v); err != nil {
					t.Fatal(err)
				}
			}
			wav.WriteString("data")
			binary.Write(&wav, binary.LittleEndian, uint32(len(pcm)))
			wav.Write(pcm)
			directory := t.TempDir()
			requests := 0
			c := &callService{cfg: callConfig{directory: directory, ffmpeg: ffmpeg, speaker: 3}, http: &http.Client{Transport: callRoundTripper(func(r *http.Request) (*http.Response, error) {
				requests++
				if strings.HasSuffix(r.URL.Path, "audio_query") {
					return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accent_phrases":[]}`))}, nil
				}
				var payload struct {
					Speaker int                        `json:"speaker"`
					Query   map[string]json.RawMessage `json:"audioQuery"`
				}
				if err := json.NewDecoder(r.Body).Decode(&payload); err != nil || payload.Speaker != 3 || payload.Query == nil {
					t.Errorf("invalid synthesis payload: %v %v", payload, err)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(wav.Bytes()))}, nil
			})}}
			file, err := c.audio(context.Background(), "hello", "test-audio")
			if seconds == 121 {
				if err == nil {
					t.Fatal("overlong audio accepted")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				data, err := os.ReadFile(file)
				if err != nil {
					t.Fatal(err)
				}
				if string(data[:4]) != "RIFF" || binary.LittleEndian.Uint32(data[24:28]) != 8000 || binary.LittleEndian.Uint16(data[22:24]) != 1 || binary.LittleEndian.Uint16(data[34:36]) != 16 {
					t.Fatal("not 8kHz mono 16bit WAV")
				}
				if err = os.Remove(file); err != nil {
					t.Fatal(err)
				}
			}
			entries, err := os.ReadDir(directory)
			if err != nil || len(entries) != 0 {
				t.Fatalf("temporary files left behind: %v %v", entries, err)
			}
			if requests != 2 {
				t.Fatalf("unexpected requests: %d", requests)
			}
		})
	}
}

func TestCallMessageValidation(t *testing.T) {
	for _, s := range []string{"", " \n", strings.Repeat("あ", 501), "hello\x00", string([]byte{255})} {
		if validateCallMessage(s) == nil {
			t.Errorf("accepted invalid text %q", s)
		}
	}
	if err := validateCallMessage("こんにちは\nhello"); err != nil {
		t.Fatal(err)
	}
}

func TestQuietHoursMinuteBoundaries(t *testing.T) {
	jst, err := time.LoadLocation("Asia/Tokyo")
	if err != nil {
		t.Fatal(err)
	}
	cfg := callConfig{location: jst, quietStart: 23, quietStartMinute: 59, quietEnd: 6}
	for _, tt := range []struct {
		timestamp string
		want      bool
	}{
		{"2026-11-17T23:58:59+09:00", false},
		{"2026-11-17T23:59:00+09:00", true},
		{"2026-11-18T00:00:00+09:00", true},
		{"2026-11-18T05:59:59+09:00", true},
		{"2026-11-18T06:00:00+09:00", false},
		{"2026-11-17T14:59:00Z", true},
		{"2026-11-17T21:00:00Z", false},
	} {
		now, err := time.Parse(time.RFC3339, tt.timestamp)
		if err != nil {
			t.Fatal(err)
		}
		if got := cfg.quiet(now); got != tt.want {
			t.Errorf("quiet(%s)=%v, want %v", tt.timestamp, got, tt.want)
		}
	}
	// Also support a window within one hour and a non-overnight window.
	cfg.quietStart, cfg.quietStartMinute, cfg.quietEnd, cfg.quietEndMinute = 12, 15, 12, 45
	for _, tt := range []struct {
		minute int
		want   bool
	}{{14, false}, {15, true}, {44, true}, {45, false}} {
		if got := cfg.quiet(time.Date(2026, 11, 17, 12, tt.minute, 0, 0, jst)); got != tt.want {
			t.Errorf("minute %d: %v", tt.minute, got)
		}
	}
}

func TestCallReservations(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &callService{ctx: ctx, cancel: cancel, cfg: callConfig{users: map[string]bool{"alice": true, "bob": true}, location: time.UTC, quietStart: 22, quietEnd: 8, concurrency: 1, cooldown: time.Minute}, last: make(map[string]time.Time)}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	if _, err := c.reserve("mallory", now); err == nil {
		t.Fatal("unauthorized user accepted")
	}
	for _, hour := range []int{22, 23, 0, 7} {
		if _, err := c.reserve("alice", time.Date(2026, 1, 1, hour, 0, 0, 0, time.UTC)); err == nil {
			t.Fatalf("quiet hour %d accepted", hour)
		}
	}
	release, err := c.reserve("alice", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.reserve("bob", now); err == nil {
		t.Fatal("concurrent call accepted")
	}
	release()
	if _, err := c.reserve("alice", now.Add(time.Second)); err == nil {
		t.Fatal("rate limit bypassed")
	}
	release, err = c.reserve("alice", now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	release()
	c.uncertainUntil = now.Add(2 * time.Minute)
	if _, err := c.reserve("bob", now.Add(time.Minute)); err == nil {
		t.Fatal("call accepted with an unconfirmed remote call")
	}
	c.close()
	if _, err := c.reserve("bob", now); err == nil {
		t.Fatal("call accepted during shutdown")
	}
}

func TestConcurrentCallLimit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c := &callService{ctx: ctx, cancel: cancel, cfg: callConfig{users: map[string]bool{"u": true}, location: time.UTC, quietStart: 22, quietEnd: 8, concurrency: 1, cooldown: time.Minute}, last: make(map[string]time.Time)}
	var wg sync.WaitGroup
	accepted := make(chan func(), 20)
	for j := 0; j < 20; j++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if release, err := c.reserve("u", time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)); err == nil {
				accepted <- release
			}
		}()
	}
	wg.Wait()
	close(accepted)
	count := 0
	for release := range accepted {
		count++
		release()
	}
	if count != 1 {
		t.Fatalf("accepted %d calls", count)
	}
}

func TestAMIFrames(t *testing.T) {
	r := newAMIReader(strings.NewReader("Asterisk Call Manager/9.0\r\nResponse: Success\r\nActionID: login\r\n\r\nEvent: Hangup\r\nUniqueid: abc\r\n\r\n"))
	f, err := r.frame()
	if err != nil || f["actionid"] != "login" {
		t.Fatalf("%v %v", f, err)
	}
	f, err = r.frame()
	if err != nil || f["uniqueid"] != "abc" {
		t.Fatalf("%v %v", f, err)
	}
	if _, err = newAMIReader(strings.NewReader("Event: " + strings.Repeat("x", 70<<10))).frame(); err == nil {
		t.Fatal("oversized frame accepted")
	}
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	if amiWrite(a, "Secret: x\r\nAction: Originate") == nil {
		t.Fatal("AMI injection accepted")
	}
}

func TestOriginateLifecycle(t *testing.T) {
	for _, scenario := range []string{"success", "no-answer", "busy", "failure", "interrupted", "disconnect", "auth", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				conn, err := listener.Accept()
				if err != nil {
					done <- err
					return
				}
				defer conn.Close()
				conn.SetDeadline(time.Now().Add(3 * time.Second))
				r := newAMIReader(conn)
				if _, err = r.frame(); err != nil {
					done <- err
					return
				}
				response := "Success"
				if scenario == "auth" {
					response = "Error"
				}
				if err = amiWrite(conn, "Response: "+response, "ActionID: login"); err != nil {
					done <- err
					return
				}
				if scenario == "auth" {
					done <- nil
					return
				}
				f, err := r.frame()
				if err != nil {
					done <- err
					return
				}
				if f["channel"] != "PJSIP/100" || f["channelid"] != "test-call" || f["context"] != "discord-voice-memo" {
					done <- errors.New("wrong originate target")
					return
				}
				if scenario == "disconnect" {
					done <- nil
					return
				}
				if scenario == "timeout" {
					_, _ = r.frame()
					done <- nil
					return
				}
				if scenario == "no-answer" || scenario == "busy" || scenario == "failure" {
					reason := "3"
					if scenario == "busy" {
						reason = "5"
					}
					if scenario == "failure" {
						reason = "0"
					}
					err = amiWrite(conn, "Event: Hangup", "Uniqueid: test-call")
					if err == nil {
						err = amiWrite(conn, "Event: OriginateResponse", "ActionID: test-call", "Response: Failure", "Reason: "+reason)
					}
					done <- err
					return
				}
				frames := [][]string{
					{"Event: UserEvent", "UserEvent: DiscordMemo", "MemoID: other-call", "Status: answered"},
					{"Event: UserEvent", "UserEvent: DiscordMemo", "MemoID: test-call", "Status: answered"},
				}
				if scenario != "interrupted" {
					frames = append(frames, []string{"Event: UserEvent", "UserEvent: DiscordMemo", "MemoID: test-call", "Status: played", "PlaybackStatus: SUCCESS"})
				}
				frames = append(frames, []string{"Event: Hangup", "Uniqueid: test-call"})
				for _, fields := range frames {
					if err = amiWrite(conn, fields...); err != nil {
						done <- err
						return
					}
				}
				_, _ = r.frame()
				done <- nil
			}()
			c := &callService{cfg: callConfig{address: listener.Addr().String(), username: "bot", secret: "secret", extension: "100", pbxDirectory: "/audio", ringTimeout: time.Second}}
			timeout := 2 * time.Second
			if scenario == "timeout" {
				timeout = 100 * time.Millisecond
			}
			ctx, cancel := context.WithTimeout(context.Background(), timeout)
			defer cancel()
			var updates []string
			status, err := c.originate(ctx, "test-call", func(s string) { updates = append(updates, s) })
			wantError := scenario == "failure" || scenario == "disconnect" || scenario == "auth" || scenario == "timeout"
			if (err != nil) != wantError {
				t.Fatalf("status=%q err=%v", status, err)
			}
			if (scenario == "disconnect" || scenario == "timeout") && !c.uncertainUntil.After(time.Now()) {
				t.Fatal("unconfirmed remote call did not block new calls")
			}
			if scenario == "success" && (!strings.Contains(status, "最後まで再生しました") || len(updates) != 2) {
				t.Fatalf("wrong lifecycle: %q %v", status, updates)
			}
			if scenario == "interrupted" && !strings.Contains(status, "再生されませんでした") {
				t.Fatalf("unexpected success: %s", status)
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}

type callRoundTripper func(*http.Request) (*http.Response, error)

func (f callRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestTTSRequests(t *testing.T) {
	c := &callService{http: &http.Client{Transport: callRoundTripper(func(r *http.Request) (*http.Response, error) {
		if r.URL.String() != "https://voicevox-api.swiftlybot.com/v1/audio_query" || r.Method != "POST" || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("incorrect API request: %v", r)
		}
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload["text"] != "hello" || payload["speaker"] != float64(3) {
			t.Errorf("incorrect payload: %v", payload)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"accent_phrases":[]}`))}, nil
	})}}
	if _, err := c.postTTS(context.Background(), "audio_query", map[string]any{"text": "hello", "speaker": 3}, 1024); err != nil {
		t.Fatal(err)
	}
	if _, err := c.postTTS(context.Background(), "audio_query", map[string]any{"text": "hello", "speaker": 3}, 2); err == nil {
		t.Fatal("size limit ignored")
	}
	c.http.Transport = callRoundTripper(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 429, Body: io.NopCloser(strings.NewReader("private details"))}, nil
	})
	if _, err := c.postTTS(context.Background(), "synthesis", nil, 100); err == nil || strings.Contains(err.Error(), "private details") {
		t.Fatal("HTTP error mishandled")
	}
}

func TestCallDisabledAndInvalidConfig(t *testing.T) {
	t.Setenv("CALL_ENABLED", "")
	c, err := newCallService()
	if c != nil || err != nil {
		t.Fatalf("disabled: %v %v", c, err)
	}
	t.Setenv("CALL_ENABLED", "yes")
	if _, err = newCallService(); err == nil {
		t.Fatal("invalid switch accepted")
	}
	t.Setenv("CALL_ENABLED", "true")
	t.Setenv("CALL_AMI_ADDRESS", "127.0.0.1:5038\r\nAction: Originate")
	if _, err = newCallService(); err == nil {
		t.Fatal("invalid config accepted")
	}
}
