package main

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/bwmarrin/discordgo"
	"github.com/techfish-11/discord-wordcloud/bgpmonitor"
)

const (
	defaultBGPLocalAddress = "192.168.1.5"
	defaultBGPNeighbor     = "192.168.1.4"
	defaultBGPASN          = uint32(65010)
)

func newBGPMonitor(ctx context.Context, app *App) (*bgpmonitor.Monitor, error) {
	localASN, err := uint32Env("BGP_LOCAL_ASN", defaultBGPASN)
	if err != nil {
		return nil, err
	}
	neighborASN, err := uint32Env("BGP_NEIGHBOR_ASN", defaultBGPASN)
	if err != nil {
		return nil, err
	}
	localAddress := getenv("BGP_LOCAL_ADDRESS", defaultBGPLocalAddress)
	config := bgpmonitor.Config{
		LocalASN: localASN, NeighborASN: neighborASN,
		LocalAddress: localAddress, RouterID: getenv("BGP_ROUTER_ID", localAddress),
		NeighborAddress: getenv("BGP_NEIGHBOR_ADDRESS", defaultBGPNeighbor),
	}
	return bgpmonitor.New(config, func(event bgpmonitor.Event) {
		// Backpressure is deliberate: silently dropping routing events would make
		// the monitor's reported state unreliable during a burst.
		select {
		case app.bgpEvents <- event:
		case <-ctx.Done():
		}
	})
}

func uint32Env(name string, fallback uint32) (uint32, error) {
	value := getenv(name, strconv.FormatUint(uint64(fallback), 10))
	parsed, err := strconv.ParseUint(value, 10, 32)
	if err != nil || parsed == 0 {
		return 0, fmt.Errorf("%s must be an ASN between 1 and 4294967295", name)
	}
	return uint32(parsed), nil
}

func (a *App) handleASCommand(s *discordgo.Session, i *discordgo.InteractionCreate) {
	options := i.ApplicationCommandData().Options
	if len(options) != 1 {
		respond(s, i, "サブコマンドを指定してください。", true)
		return
	}
	op := options[0]
	switch op.Name {
	case "add", "remove":
		if len(op.Options) != 1 {
			respond(s, i, "監視するAS番号を指定してください（例: 65001）。", true)
			return
		}
		asn64 := op.Options[0].IntValue()
		if asn64 < 1 || asn64 > int64(^uint32(0)) {
			respond(s, i, "ASNは1〜4294967295の範囲で指定してください。", true)
			return
		}
		if op.Name == "add" {
			result, err := a.db.Exec(`INSERT OR IGNORE INTO bgp_watched_as(guild_id,asn,created_at) VALUES(?,?,?)`, i.GuildID, asn64, time.Now().Unix())
			if err != nil {
				log.Printf("add watched ASN for guild %s: %v", i.GuildID, err)
				respond(s, i, "AS番号を保存できませんでした。しばらくしてから、もう一度お試しください。", true)
				return
			}
			rows, _ := result.RowsAffected()
			if rows == 0 {
				respond(s, i, fmt.Sprintf("AS%dは、すでに監視リストに入っています。", asn64), true)
			} else {
				respond(s, i, fmt.Sprintf("AS%dを監視リストに追加しました。このASが発信する経路に変化があるとお知らせします。", asn64), false)
			}
			return
		}
		result, err := a.db.Exec(`DELETE FROM bgp_watched_as WHERE guild_id=? AND asn=?`, i.GuildID, asn64)
		if err != nil {
			log.Printf("remove watched ASN for guild %s: %v", i.GuildID, err)
			respond(s, i, "AS番号を監視リストから削除できませんでした。しばらくしてから、もう一度お試しください。", true)
			return
		}
		rows, _ := result.RowsAffected()
		if rows == 0 {
			respond(s, i, fmt.Sprintf("AS%dは監視リストに入っていません。", asn64), true)
		} else {
			respond(s, i, fmt.Sprintf("AS%dを監視リストから削除しました。", asn64), false)
		}
	case "list":
		rows, err := a.db.Query(`SELECT asn FROM bgp_watched_as WHERE guild_id=? ORDER BY asn`, i.GuildID)
		if err != nil {
			log.Printf("list watched ASNs for guild %s: %v", i.GuildID, err)
			respond(s, i, "監視AS一覧の取得に失敗しました。", true)
			return
		}
		defer rows.Close()
		var asns []uint32
		for rows.Next() {
			var asn uint32
			if err := rows.Scan(&asn); err != nil {
				log.Printf("scan watched ASN for guild %s: %v", i.GuildID, err)
				respond(s, i, "監視AS一覧の取得に失敗しました。", true)
				return
			}
			asns = append(asns, asn)
		}
		if err := rows.Err(); err != nil {
			log.Printf("read watched ASNs for guild %s: %v", i.GuildID, err)
			respond(s, i, "監視AS一覧の取得に失敗しました。", true)
			return
		}
		if len(asns) == 0 {
			respond(s, i, "現在、監視リストに登録されているAS番号はありません。`/as add`で追加できます。", true)
			return
		}
		values := make([]string, len(asns))
		for n, asn := range asns {
			values[n] = fmt.Sprintf("AS%d", asn)
		}
		respond(s, i, truncateBGPField("現在監視しているAS番号: "+strings.Join(values, ", "), 1900), true)
	case "notify-channel-set":
		if len(op.Options) != 1 {
			respond(s, i, "通知先チャンネルを指定してください。", true)
			return
		}
		channel := op.Options[0].ChannelValue(s)
		if channel == nil || channel.GuildID != i.GuildID || channel.Type != discordgo.ChannelTypeGuildText {
			respond(s, i, "このサーバーのテキストチャンネルを指定してください。", true)
			return
		}
		_, err := a.db.Exec(`INSERT INTO bgp_settings(guild_id,channel_id,updated_at) VALUES(?,?,?) ON CONFLICT(guild_id) DO UPDATE SET channel_id=excluded.channel_id,updated_at=excluded.updated_at`, i.GuildID, channel.ID, time.Now().Unix())
		if err != nil {
			log.Printf("set BGP channel for guild %s: %v", i.GuildID, err)
			respond(s, i, "通知先の保存に失敗しました。", true)
			return
		}
		respond(s, i, fmt.Sprintf("経路変更のお知らせを<#%s>へ送るように設定しました。", channel.ID), false)
	default:
		respond(s, i, "不明なサブコマンドです。", true)
	}
}

func (a *App) bgpNotifier(ctx context.Context, s *discordgo.Session) {
	for {
		select {
		case <-ctx.Done():
			return
		case event := <-a.bgpEvents:
			channels, err := a.notificationChannels(event)
			if err != nil {
				log.Printf("resolve BGP notification channels: %v", err)
				continue
			}
			embed := formatBGPEventEmbed(event)
			for _, channelID := range channels {
				if _, err := s.ChannelMessageSendEmbed(channelID, embed); err != nil {
					log.Printf("send BGP notification to channel %s: %v", channelID, err)
				}
			}
		}
	}
}

func (a *App) notificationChannels(event bgpmonitor.Event) ([]string, error) {
	asns := eventASNs(event)
	if len(asns) == 0 {
		return nil, nil
	}
	query := `SELECT DISTINCT s.channel_id FROM bgp_settings s JOIN bgp_watched_as w ON w.guild_id=s.guild_id WHERE w.asn IN (` + strings.TrimSuffix(strings.Repeat("?,", len(asns)), ",") + `)`
	args := make([]any, len(asns))
	for i, asn := range asns {
		args[i] = asn
	}
	rows, err := a.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var channels []string
	for rows.Next() {
		var channel string
		if err := rows.Scan(&channel); err != nil {
			return nil, err
		}
		channels = append(channels, channel)
	}
	return channels, rows.Err()
}

func eventASNs(event bgpmonitor.Event) []uint32 {
	set := make(map[uint32]struct{}, 2)
	if event.Old != nil {
		set[event.Old.OriginASN] = struct{}{}
	}
	if event.New != nil {
		set[event.New.OriginASN] = struct{}{}
	}
	values := make([]uint32, 0, len(set))
	for asn := range set {
		if asn != 0 {
			values = append(values, asn)
		}
	}
	sort.Slice(values, func(i, j int) bool { return values[i] < values[j] })
	return values
}

func formatBGPEventEmbed(event bgpmonitor.Event) *discordgo.MessageEmbed {
	var prefix, oldPath, newPath, origin string
	if event.Old != nil {
		prefix = event.Old.Prefix.String()
		oldPath = event.Old.ASPath
		origin = fmt.Sprintf("AS%d", event.Old.OriginASN)
	}
	if event.New != nil {
		prefix = event.New.Prefix.String()
		newPath = event.New.ASPath
		if event.Old != nil && event.Old.OriginASN != event.New.OriginASN {
			origin = fmt.Sprintf("AS%d → AS%d", event.Old.OriginASN, event.New.OriginASN)
		} else {
			origin = fmt.Sprintf("AS%d", event.New.OriginASN)
		}
	}
	if oldPath == "" {
		oldPath = "なし（今回初めて受信）"
	}
	if newPath == "" {
		newPath = "なし（経路は取り消されました）"
	}
	oldPath = truncateBGPField(oldPath, 900)
	newPath = truncateBGPField(newPath, 900)

	title := "🟡 通信経路が変更されました"
	description := "同じネットワークへ到達するまでに通るASの並びが変わりました。"
	color := 0xFEE75C
	switch event.Type {
	case bgpmonitor.Announce:
		title = "🟢 新しい経路を受信しました"
		description = "これまで見えていなかったネットワークへの経路が新しく届きました。"
		color = 0x57F287
	case bgpmonitor.Withdraw:
		title = "🔴 経路が取り消されました"
		description = "このネットワークへの経路情報が取り消されました。一時的に到達できない可能性があります。"
		color = 0xED4245
	}

	return &discordgo.MessageEmbed{
		Title:       title,
		Description: description,
		Color:       color,
		Fields: []*discordgo.MessageEmbedField{
			{Name: "対象ネットワーク（Prefix）", Value: "`" + prefix + "`", Inline: false},
			{Name: "発信元のAS番号（Origin ASN）", Value: "`" + origin + "`", Inline: false},
			{Name: "これまでの経路（旧AS_PATH）", Value: "`" + oldPath + "`", Inline: false},
			{Name: "現在の経路（新AS_PATH）", Value: "`" + newPath + "`", Inline: false},
		},
		Footer:    &discordgo.MessageEmbedFooter{Text: "AS_PATHは、経路情報が通ってきたAS番号の並びです。"},
		Timestamp: time.Now().UTC().Format(time.RFC3339),
	}
}

func truncateBGPField(value string, max int) string {
	if len(value) <= max {
		return value
	}
	return value[:max-3] + "..."
}
