package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// SIP signaling, authentication, codec negotiation and RTP are delegated to Diago.
// This client intentionally supports G.711 only, so no CGO or external encoder is needed.
type callPhone interface {
	Ready() bool
	Call(context.Context, []byte, func(string)) (string, error)
	Close()
}

type sipConfig struct {
	address, username, password, extension, bindIP, advertisedIP, transport string
	bindPort, rtpStart, rtpEnd                                              int
}

func loadSIPConfig() (sipConfig, error) {
	c := sipConfig{address: os.Getenv("CALL_SIP_ADDRESS"), username: os.Getenv("CALL_SIP_USERNAME"), password: os.Getenv("CALL_SIP_PASSWORD"), extension: os.Getenv("CALL_EXTENSION"), bindIP: os.Getenv("CALL_SIP_BIND_IP"), advertisedIP: os.Getenv("CALL_SIP_ADVERTISE_IP"), transport: getenv("CALL_SIP_TRANSPORT", "udp")}
	for name, value := range map[string]string{"CALL_SIP_ADDRESS": c.address, "CALL_SIP_USERNAME": c.username, "CALL_SIP_PASSWORD": c.password, "CALL_EXTENSION": c.extension, "CALL_SIP_BIND_IP": c.bindIP} {
		if value == "" || strings.ContainsAny(value, "\r\n\x00") || strings.TrimSpace(value) != value {
			return c, fmt.Errorf("%s is required and must contain no control delimiters or surrounding whitespace", name)
		}
	}
	host, port, err := net.SplitHostPort(c.address)
	if err != nil || host == "" || strings.ContainsAny(host, " /@;?\t\r\n") {
		return c, errors.New("CALL_SIP_ADDRESS must be host:port")
	}
	p, err := strconv.Atoi(port)
	if err != nil || p < 1 || p > 65535 {
		return c, errors.New("invalid CALL_SIP_ADDRESS port")
	}
	for name, value := range map[string]string{"CALL_SIP_USERNAME": c.username, "CALL_EXTENSION": c.extension} {
		if !digitsOnly.MatchString(value) || len(value) > 20 {
			return c, fmt.Errorf("%s must be 1–20 digits", name)
		}
	}
	if c.extension == c.username {
		return c, errors.New("CALL_EXTENSION must differ from the bot SIP extension")
	}
	if c.advertisedIP == "" {
		c.advertisedIP = c.bindIP
	}
	for name, value := range map[string]string{"CALL_SIP_BIND_IP": c.bindIP, "CALL_SIP_ADVERTISE_IP": c.advertisedIP} {
		ip := net.ParseIP(value)
		if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsMulticast() {
			return c, fmt.Errorf("%s must be a concrete unicast IPv4 address", name)
		}
	}
	if c.transport != "udp" && c.transport != "tcp" {
		return c, errors.New("CALL_SIP_TRANSPORT must be udp or tcp (use a private LAN/VPN)")
	}
	for _, setting := range []struct {
		name, fallback string
		dst            *int
	}{
		{"CALL_SIP_BIND_PORT", "15060", &c.bindPort}, {"CALL_RTP_PORT_START", "20000", &c.rtpStart}, {"CALL_RTP_PORT_END", "20100", &c.rtpEnd},
	} {
		v, e := strconv.Atoi(getenv(setting.name, setting.fallback))
		if e != nil || v < 1024 || v > 65534 {
			return c, fmt.Errorf("invalid %s", setting.name)
		}
		*setting.dst = v
	}
	if c.rtpStart%2 != 0 || c.rtpEnd%2 != 0 || c.rtpEnd-c.rtpStart < 20 {
		return c, errors.New("RTP range must use even ports with at least 20 ports (end exclusive)")
	}
	if c.bindPort >= c.rtpStart && c.bindPort < c.rtpEnd {
		return c, errors.New("SIP bind port must be outside the RTP range")
	}
	return c, nil
}

type sipPhone struct {
	cfg            sipConfig
	ringTimeout    time.Duration
	ua             *sipgo.UserAgent
	dg             *diago.Diago
	registered     atomic.Bool
	registerCancel context.CancelFunc
	serveCancel    context.CancelFunc
	wg             sync.WaitGroup
	closeOnce      sync.Once
}

func newSIPPhone(ctx context.Context, cfg sipConfig, ringTimeout time.Duration) (*sipPhone, error) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(cfg.username), sipgo.WithUserAgentHostname(cfg.advertisedIP))
	if err != nil {
		return nil, fmt.Errorf("create SIP user agent: %w", err)
	}
	// Diago's RTP port range is process-global and is set once, before serving.
	media.RTPPortStart, media.RTPPortEnd = cfg.rtpStart, cfg.rtpEnd
	p := &sipPhone{cfg: cfg, ringTimeout: ringTimeout, ua: ua}
	registerCtx, registerCancel := context.WithCancel(ctx)
	p.registerCancel = registerCancel
	p.dg = diago.NewDiago(ua, diago.WithTransport(diago.Transport{Transport: cfg.transport, BindHost: cfg.bindIP, BindPort: cfg.bindPort, ExternalHost: cfg.advertisedIP, ExternalPort: cfg.bindPort, MediaExternalIP: net.ParseIP(cfg.advertisedIP)}), diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}}))
	// Keep signaling alive until active calls and unregister have finished.
	serveCtx, serveCancel := context.WithCancel(context.Background())
	p.serveCancel = serveCancel
	if err = p.dg.ServeBackground(serveCtx, func(d *diago.DialogServerSession) {
		defer d.Close()
		if err := d.Respond(sip.StatusBusyHere, "Outbound calls only", nil); err != nil {
			log.Printf("reject incoming SIP call failed")
		}
	}); err != nil {
		registerCancel()
		serveCancel()
		ua.Close()
		return nil, fmt.Errorf("start SIP listener: %w", err)
	}
	p.wg.Add(1)
	go func() { defer p.wg.Done(); p.registrationLoop(registerCtx) }()
	return p, nil
}

func (p *sipPhone) recipient(user string) sip.Uri {
	host, port, _ := net.SplitHostPort(p.cfg.address)
	n, _ := strconv.Atoi(port)
	return sip.Uri{Scheme: "sip", User: user, Host: host, Port: n, UriParams: sip.HeaderParams{{K: "transport", V: p.cfg.transport}}}
}

func (p *sipPhone) registrationLoop(ctx context.Context) {
	for ctx.Err() == nil {
		tx, err := p.dg.RegisterTransaction(ctx, p.recipient(p.cfg.username), diago.RegisterOptions{Username: p.cfg.username, Password: p.cfg.password, Expiry: 5 * time.Minute})
		if err == nil {
			regCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			err = tx.Register(regCtx)
			cancel()
			if err == nil {
				p.registered.Store(true)
				log.Printf("SIP registration ready")
				err = tx.QualifyLoop(ctx)
				p.registered.Store(false)
				unregisterCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				if e := tx.Unregister(unregisterCtx); e != nil {
					log.Printf("SIP unregister failed")
				}
				cancel()
			}
		}
		if ctx.Err() != nil {
			return
		}
		// Do not log SIP packets or authentication material from library errors.
		var response *diago.RegisterResponseError
		if errors.As(err, &response) {
			log.Printf("SIP registration failed status=%d; retrying in 10s", response.StatusCode())
		} else {
			log.Printf("SIP registration unavailable; retrying in 10s")
		}
		timer := time.NewTimer(10 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (p *sipPhone) Ready() bool { return p.registered.Load() }
func (p *sipPhone) Close() {
	p.closeOnce.Do(func() {
		p.registerCancel()
		p.wg.Wait()
		p.serveCancel()
		if err := p.ua.Close(); err != nil {
			log.Printf("close SIP user agent failed")
		}
	})
}

func (p *sipPhone) Call(ctx context.Context, wav []byte, update func(string)) (string, error) {
	if !p.Ready() {
		return "", errors.New("SIP registration is not ready")
	}
	d, err := p.dg.NewDialog(p.recipient(p.cfg.extension), diago.NewDialogOptions{Transport: p.cfg.transport})
	if err != nil {
		return "", errors.New("create SIP dialog failed")
	}
	defer d.Close()
	update("発信中…")
	ringCtx, cancel := context.WithTimeout(ctx, p.ringTimeout)
	med, err := d.Invite(ringCtx, diago.InviteClientOptions{Username: p.cfg.username, Password: p.cfg.password, Headers: []sip.Header{}})
	timedOut := errors.Is(ringCtx.Err(), context.DeadlineExceeded)
	cancel()
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if timedOut {
			return "相手が応答しませんでした（呼出し時間切れ）。", nil
		}
		var response *sipgo.ErrDialogResponse
		if errors.As(err, &response) {
			switch response.Res.StatusCode {
			case 408, 480:
				return "相手が応答しませんでした（不在）。", nil
			case 486, 600:
				return "相手は通話中です。", nil
			case 603:
				return "相手が着信を拒否しました。", nil
			}
			return "", fmt.Errorf("SIP invite failed status=%d", response.Res.StatusCode)
		}
		return "", errors.New("SIP invite failed (transport or media negotiation)")
	}
	defer med.Close()
	// Cleanup uses a fresh context so shutdown/timeout can still send BYE.
	hungup := false
	defer func() {
		if !hungup && d.Context().Err() == nil {
			hangCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := d.Hangup(hangCtx); err != nil {
				log.Printf("SIP cleanup hangup failed")
			}
		}
	}()
	if err = d.Ack(ctx); err != nil {
		return "", errors.New("SIP ACK failed")
	}
	update("相手が出ました。音声メモを再生しています…")
	stop := context.AfterFunc(ctx, func() { med.Close() })
	defer stop()
	playback, err := med.PlaybackCreate()
	if err != nil {
		return "", errors.New("create RTP playback failed")
	}
	if _, err = playback.Play(bytes.NewReader(wav), "audio/wav"); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		if d.Context().Err() != nil {
			return "通話が終了しました。音声メモは最後まで送信されませんでした。", nil
		}
		return "", errors.New("RTP audio playback failed")
	}
	// Allow the last RTP frame and the recipient's jitter buffer to drain before BYE.
	timer := time.NewTimer(200 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-d.Context().Done():
		return "通話が終了しました。音声送信後の切断を確認しました。", nil
	case <-timer.C:
	}
	hangCtx, hangCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer hangCancel()
	if err = d.Hangup(hangCtx); err != nil {
		return "", errors.New("audio sent but SIP hangup was not confirmed")
	}
	hungup = true
	return "音声メモの送信が完了し、通話を終了しました。", nil
}

// Validate the exact format requested from VOICEVOX before placing a call.
// Reject malformed RIFF sizes, compressed WAVs, unexpected formats and overlong audio.
func validateCallWAV(wav []byte) error {
	bad := errors.New("TTS must return a valid 8kHz mono 16bit PCM WAV of at most 120 seconds")
	if len(wav) < 44 || string(wav[:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || uint64(binary.LittleEndian.Uint32(wav[4:8]))+8 != uint64(len(wav)) {
		return bad
	}
	foundFormat, foundData := false, false
	for pos := 12; pos < len(wav); {
		if len(wav)-pos < 8 {
			return bad
		}
		name := string(wav[pos : pos+4])
		size := uint64(binary.LittleEndian.Uint32(wav[pos+4 : pos+8]))
		pos += 8
		if size > uint64(len(wav)-pos) {
			return bad
		}
		chunk := wav[pos : pos+int(size)]
		switch name {
		case "fmt ":
			if foundFormat || len(chunk) < 16 {
				return bad
			}
			foundFormat = true
			if binary.LittleEndian.Uint16(chunk[0:2]) != 1 || binary.LittleEndian.Uint16(chunk[2:4]) != 1 || binary.LittleEndian.Uint32(chunk[4:8]) != 8000 || binary.LittleEndian.Uint32(chunk[8:12]) != 16000 || binary.LittleEndian.Uint16(chunk[12:14]) != 2 || binary.LittleEndian.Uint16(chunk[14:16]) != 16 {
				return bad
			}
		case "data":
			if !foundFormat || foundData || size == 0 || size%2 != 0 || size > 120*8000*2 {
				return bad
			}
			foundData = true
		}
		pos += int(size)
		if size%2 != 0 {
			if pos >= len(wav) {
				return bad
			}
			pos++
		}
	}
	if !foundFormat || !foundData {
		return bad
	}
	return nil
}
