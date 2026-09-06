package main

import (
	"net/netip"
	"strings"
	"testing"

	"github.com/techfish-11/discord-wordcloud/bgpmonitor"
)

func TestFormatBGPEventEmbed(t *testing.T) {
	oldRoute := &bgpmonitor.Route{Prefix: netip.MustParsePrefix("2001:db8::/32"), OriginASN: 65001, ASPath: "64500 65001"}
	newRoute := &bgpmonitor.Route{Prefix: oldRoute.Prefix, OriginASN: 65001, ASPath: "64500 64496 65001"}
	embed := formatBGPEventEmbed(bgpmonitor.Event{Type: bgpmonitor.PathChange, Old: oldRoute, New: newRoute})

	if embed.Title != "🟡 通信経路が変更されました" {
		t.Fatalf("unexpected title: %q", embed.Title)
	}
	if embed.Color != 0xFEE75C || len(embed.Fields) != 4 {
		t.Fatalf("unexpected embed: %#v", embed)
	}
	if !strings.Contains(embed.Fields[0].Value, "2001:db8::/32") || !strings.Contains(embed.Fields[3].Value, newRoute.ASPath) {
		t.Fatalf("embed is missing route details: %#v", embed.Fields)
	}
}

func TestWithdrawEmbedExplainsMissingNewPath(t *testing.T) {
	route := &bgpmonitor.Route{Prefix: netip.MustParsePrefix("2001:db8::/32"), OriginASN: 65001, ASPath: "64500 65001"}
	embed := formatBGPEventEmbed(bgpmonitor.Event{Type: bgpmonitor.Withdraw, Old: route})
	if embed.Color != 0xED4245 || !strings.Contains(embed.Fields[3].Value, "取り消されました") {
		t.Fatalf("withdraw embed is not beginner-friendly: %#v", embed)
	}
}
