package main

import (
	"fmt"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ikawaha/kagome-dict/ipa"
	"github.com/ikawaha/kagome/v2/tokenizer"
	"golang.org/x/text/unicode/norm"
)

var (
	urlMentionCode = regexp.MustCompile("(?is)https?://\\S+|<[@#!&]?\\d+>|```.*?```|`[^`]*`|:[A-Za-z0-9_+-]+:")
	latinToken     = regexp.MustCompile(`^[a-z][a-z0-9_+#.-]{1,31}$`)
)

// Content words are deliberately conservative. A word cloud is more useful
// with fewer meaningful terms than with many particles and sentence fragments.
var contentStopWords = map[string]struct{}{
	"する": {}, "いる": {}, "ある": {}, "なる": {}, "できる": {}, "思う": {}, "言う": {}, "見る": {},
	"これ": {}, "それ": {}, "あれ": {}, "ここ": {}, "そこ": {}, "ため": {}, "もの": {}, "こと": {},
	"よう": {}, "そう": {}, "さん": {}, "ちゃん": {}, "くん": {}, "今日": {}, "昨日": {}, "明日": {},
	"the": {}, "and": {}, "that": {}, "this": {}, "with": {}, "from": {}, "have": {}, "www": {},
	"http": {}, "https": {}, "discord": {},
}

type TextAnalyzer struct {
	tokenizer *tokenizer.Tokenizer
}

func NewTextAnalyzer() (*TextAnalyzer, error) {
	t, err := tokenizer.New(ipa.Dict(), tokenizer.OmitBosEos())
	if err != nil {
		return nil, fmt.Errorf("initialize Japanese tokenizer: %w", err)
	}
	return &TextAnalyzer{tokenizer: t}, nil
}

func (a *TextAnalyzer) Tokenize(text string) []string {
	if a == nil || a.tokenizer == nil || strings.TrimSpace(text) == "" {
		return nil
	}
	text = norm.NFKC.String(urlMentionCode.ReplaceAllString(text, " "))
	tokens := a.tokenizer.Tokenize(text)
	words := make([]string, 0, len(tokens))
	for _, token := range tokens {
		word, ok := contentWord(token)
		if ok {
			words = append(words, word)
		}
	}
	return words
}

func contentWord(token tokenizer.Token) (string, bool) {
	pos := token.POS()
	if len(pos) == 0 {
		return "", false
	}
	word := token.Surface
	if base, ok := token.BaseForm(); ok && base != "" && base != "*" {
		word = base
	}
	word = strings.ToLower(strings.TrimSpace(word))
	if word == "" || utf8.RuneCountInString(word) > 32 {
		return "", false
	}
	if _, blocked := contentStopWords[word]; blocked {
		return "", false
	}

	switch pos[0] {
	case "名詞":
		if len(pos) > 1 {
			switch pos[1] {
			case "非自立", "代名詞", "数", "接尾", "副詞可能":
				return "", false
			}
		}
	case "動詞", "形容詞":
		if len(pos) > 1 && pos[1] == "非自立" {
			return "", false
		}
	default:
		return "", false
	}

	if latinToken.MatchString(word) {
		return word, true
	}
	if !containsLetter(word) {
		return "", false
	}
	// Short all-hiragana tokens are overwhelmingly conversational noise. A
	// single kanji or katakana term (猫, AI) can still be meaningful.
	if onlyHiragana(word) && utf8.RuneCountInString(word) < 3 {
		return "", false
	}
	return word, true
}

func containsLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

func onlyHiragana(s string) bool {
	for _, r := range s {
		if !unicode.In(r, unicode.Hiragana) {
			return false
		}
	}
	return s != ""
}
