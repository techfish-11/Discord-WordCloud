package main

import (
	"bytes"
	"database/sql"
	"image/png"
	"slices"
	"testing"
	"time"

	"golang.org/x/image/font"
)

func TestTextAnalyzerTokenizeJapaneseContentWords(t *testing.T) {
	a, err := NewTextAnalyzer()
	if err != nil {
		t.Fatal(err)
	}
	got := a.Tokenize("新しいゲームを遊んだけど、映像がすごく綺麗だった！ゲームは楽しいね")
	for _, want := range []string{"新しい", "ゲーム", "遊んだ", "映像", "綺麗だった", "楽しい"} {
		if !slices.Contains(got, want) {
			t.Errorf("Tokenize() = %q, missing %q", got, want)
		}
	}
	for _, noise := range []string{"を", "ん", "けど", "が", "だっ", "た", "は", "ね"} {
		if slices.Contains(got, noise) {
			t.Errorf("Tokenize() = %q, contains noise %q", got, noise)
		}
	}
}

func TestTextAnalyzerNormalizesAndRemovesDiscordNoise(t *testing.T) {
	a, err := NewTextAnalyzer()
	if err != nil {
		t.Fatal(err)
	}
	got := a.Tokenize("ＡＩ AI https://example.com <@12345> `const value = 1` :party_parrot:")
	if count(got, "ai") != 2 {
		t.Fatalf("Tokenize() = %q, want two normalized ai tokens", got)
	}
	for _, noise := range []string{"example", "const", "value", "party_parrot"} {
		if slices.Contains(got, noise) {
			t.Errorf("Tokenize() = %q, contains removed content %q", got, noise)
		}
	}
}

func TestTextAnalyzerPreservesInflectionAndSpelling(t *testing.T) {
	a, err := NewTextAnalyzer()
	if err != nil {
		t.Fatal(err)
	}
	got := a.Tokenize("昨日はわからなかった。今はわかる。理由は分からなかった")
	for _, want := range []string{"わからなかった", "わかる", "分からなかった"} {
		if !slices.Contains(got, want) {
			t.Errorf("Tokenize() = %q, missing %q", got, want)
		}
	}
	for _, collapsed := range []string{"分かる", "わから", "分から"} {
		if slices.Contains(got, collapsed) {
			t.Errorf("Tokenize() = %q, contains collapsed expression %q", got, collapsed)
		}
	}
}

func count(words []string, target string) int {
	n := 0
	for _, word := range words {
		if word == target {
			n++
		}
	}
	return n
}

func TestScaledFontSize(t *testing.T) {
	if got := scaledFontSize(2, 2, 20); got != 28 {
		t.Errorf("scaledFontSize(min) = %d, want 28", got)
	}
	if got := scaledFontSize(20, 2, 20); got != 124 {
		t.Errorf("scaledFontSize(max) = %d, want 124", got)
	}
	if low, high := scaledFontSize(4, 2, 20), scaledFontSize(10, 2, 20); low >= high {
		t.Errorf("scaledFontSize is not monotonic: low=%d high=%d", low, high)
	}
}

func TestRenderProducesPNGWithEmbeddedJapaneseFont(t *testing.T) {
	app := &App{fonts: make(map[int]font.Face)}
	data, err := app.render([]Word{{Text: "ゲーム", Count: 12}, {Text: "楽しい", Count: 4}}, "2026-08-22")
	if err != nil {
		t.Fatal(err)
	}
	img, err := png.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode rendered PNG: %v", err)
	}
	if got := img.Bounds().Size(); got.X != 1600 || got.Y != 900 {
		t.Fatalf("image size = %v, want 1600x900", got)
	}
}

func TestGeneratePreviewKeepsSourceMessages(t *testing.T) {
	db, err := sql.Open("sqlite", "file:preview-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	analyzer, err := NewTextAnalyzer()
	if err != nil {
		t.Fatal(err)
	}
	app := &App{db: db, loc: time.UTC, fonts: make(map[int]font.Face), text: analyzer}
	for id, content := range []string{"ゲームが楽しい", "新しいゲームは楽しい"} {
		if _, err := db.Exec(`INSERT INTO messages(guild_id,channel_id,message_id,day,content,created_at) VALUES(?,?,?,?,?,?)`, "guild", "channel", id, "2026-08-23", content, time.Now().Unix()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := app.generate("guild", "channel", "2026-08-23")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		t.Fatalf("decode preview PNG: %v", err)
	}
	var remaining int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE guild_id=? AND channel_id=? AND day=?`, "guild", "channel", "2026-08-23").Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 2 {
		t.Fatalf("remaining messages = %d, want 2", remaining)
	}
}
