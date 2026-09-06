package main

import (
	"context"
	"database/sql"
	"net/netip"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
	"github.com/techfish-11/discord-wordcloud/bgpmonitor"
)

func TestBGPNotificationChannelTypes(t *testing.T) {
	allowed := []discordgo.ChannelType{
		discordgo.ChannelTypeGuildText,
		discordgo.ChannelTypeGuildNewsThread,
		discordgo.ChannelTypeGuildPublicThread,
		discordgo.ChannelTypeGuildPrivateThread,
	}
	for _, channelType := range allowed {
		if !isBGPNotificationChannel(channelType) {
			t.Errorf("channel type %d should be allowed", channelType)
		}
	}
	if isBGPNotificationChannel(discordgo.ChannelTypeGuildVoice) {
		t.Fatal("voice channels must not be allowed")
	}
}

func TestResolveBGPNotificationThreadFromInteraction(t *testing.T) {
	const threadID = "123456789"
	thread := &discordgo.Channel{ID: threadID, Type: discordgo.ChannelTypeGuildPrivateThread}
	interaction := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{
		Type:    discordgo.InteractionApplicationCommand,
		GuildID: "987654321",
		Data: discordgo.ApplicationCommandInteractionData{Resolved: &discordgo.ApplicationCommandInteractionDataResolved{
			Channels: map[string]*discordgo.Channel{threadID: thread},
		}},
	}}
	option := &discordgo.ApplicationCommandInteractionDataOption{Type: discordgo.ApplicationCommandOptionChannel, Value: threadID}
	got, err := resolveBGPNotificationChannel(nil, interaction, option)
	if err != nil {
		t.Fatal(err)
	}
	if got != thread {
		t.Fatalf("resolved channel = %#v, want thread from interaction", got)
	}
}

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

func TestDailyReportTimeValidation(t *testing.T) {
	hour, minute, err := parseDailyReportTime("09:05")
	if err != nil || hour != 9 || minute != 5 {
		t.Fatalf("parseDailyReportTime = %d:%d, %v", hour, minute, err)
	}
	for _, invalid := range []string{"9:05", "24:00", "09:60", "noon"} {
		if _, _, err := parseDailyReportTime(invalid); err == nil {
			t.Errorf("parseDailyReportTime(%q) unexpectedly succeeded", invalid)
		}
	}
}

func TestDailyChangedASesAreCounted(t *testing.T) {
	db, err := sql.Open("sqlite", "file:bgp-daily-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	app := &App{db: db}
	targets := []bgpNotificationTarget{{guildID: "guild", channelID: "channel", asns: []uint32{65001, 65001, 65002}}}
	if err := app.recordChangedASes(context.Background(), targets, "2026-09-06"); err != nil {
		t.Fatal(err)
	}
	changes, err := app.changedASes(context.Background(), "guild", "2026-09-06")
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 || changes[0] != (dailyASChange{ASN: 65001, Count: 2}) || changes[1] != (dailyASChange{ASN: 65002, Count: 1}) {
		t.Fatalf("changed ASes = %#v", changes)
	}
	embed := formatDailyBGPReportEmbed("2026-09-06", changes)
	if !strings.Contains(embed.Fields[0].Value, "2 AS") || !strings.Contains(embed.Fields[1].Value, "AS65001 — 2回") {
		t.Fatalf("daily embed has wrong count: %#v", embed)
	}
}

func TestDailyReportShowsOnlyTopFive(t *testing.T) {
	changes := []dailyASChange{{ASN: 1, Count: 9}, {ASN: 2, Count: 8}, {ASN: 3, Count: 7}, {ASN: 4, Count: 6}, {ASN: 5, Count: 5}, {ASN: 6, Count: 4}}
	embed := formatDailyBGPReportEmbed("2026-09-06", changes)
	if strings.Contains(embed.Fields[1].Value, "AS6") || !strings.Contains(embed.Fields[1].Value, "AS5") {
		t.Fatalf("top-five field = %q", embed.Fields[1].Value)
	}
}

func TestInitDBMigratesLegacyDailyChangeTable(t *testing.T) {
	db, err := sql.Open("sqlite", "file:bgp-daily-migration-test?mode=memory&cache=shared")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE bgp_daily_changed_as (guild_id TEXT NOT NULL, day TEXT NOT NULL, asn INTEGER NOT NULL, PRIMARY KEY(guild_id,day,asn))`); err != nil {
		t.Fatal(err)
	}
	if err := initDB(db); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO bgp_daily_changed_as(guild_id,day,asn,change_count) VALUES('g','2026-09-06',65001,2)`); err != nil {
		t.Fatalf("migrated change_count column is unavailable: %v", err)
	}
}
