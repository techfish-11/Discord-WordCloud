package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

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
	c.close()
	if _, err := c.reserve("bob", now); err == nil {
		t.Fatal("call accepted during shutdown")
	}
}

func TestOptionalCallWhitelist(t *testing.T) {
	const allowed = "123456789012345678"
	const other = "234567890123456789"
	for _, tt := range []struct {
		name, value           string
		wantError, restricted bool
	}{
		{"unset or empty", "", false, false},
		{"whitespace", " \t\n", false, false},
		{"one user", allowed, false, true},
		{"spaces and duplicate", " " + allowed + ", " + allowed + " ", false, true},
		{"invalid ID", "invalid", true, false},
		{"only separator", ",", true, false},
		{"trailing separator", allowed + ",", true, false},
		{"invalid among valid", allowed + ",invalid", true, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			users, err := parseCallAllowedUsers(tt.value)
			if (err != nil) != tt.wantError {
				t.Fatalf("parse error=%v, want error=%v", err, tt.wantError)
			}
			if err != nil {
				return
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			c := &callService{ctx: ctx, cancel: cancel, cfg: callConfig{users: users, location: time.UTC, quietStart: 23, quietStartMinute: 59, quietEnd: 6, concurrency: 1, cooldown: time.Minute}, last: make(map[string]time.Time)}
			now := time.Date(2026, 11, 17, 12, 0, 0, 0, time.UTC)
			release, err := c.reserve(other, now)
			if tt.restricted {
				if err == nil {
					release()
					t.Fatal("unlisted user accepted")
				}
			} else {
				if err != nil {
					t.Fatalf("unrestricted user rejected: %v", err)
				}
				if _, err := c.reserve(allowed, now); err == nil {
					t.Fatal("concurrency limit bypassed without whitelist")
				}
				release()
				if _, err := c.reserve(other, now.Add(time.Second)); err == nil {
					t.Fatal("rate limit bypassed without whitelist")
				}
			}
			release, err = c.reserve(allowed, now)
			if err != nil {
				t.Fatalf("allowed user rejected: %v", err)
			}
			release()
			if _, err := c.reserve(other, time.Date(2026, 11, 17, 23, 59, 0, 0, time.UTC)); err == nil {
				t.Fatal("quiet hours bypassed")
			}
		})
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
	t.Setenv("CALL_SIP_ADDRESS", "127.0.0.1:5038\r\nAction: Originate")
	if _, err = newCallService(); err == nil {
		t.Fatal("invalid config accepted")
	}
}
