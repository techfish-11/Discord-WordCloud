package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	"github.com/icholy/digest"
)

func callTestWAV(samples int) []byte {
	data := make([]byte, 44+samples*2)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)
	binary.LittleEndian.PutUint16(data[22:24], 1)
	binary.LittleEndian.PutUint32(data[24:28], 8000)
	binary.LittleEndian.PutUint32(data[28:32], 16000)
	binary.LittleEndian.PutUint16(data[32:34], 2)
	binary.LittleEndian.PutUint16(data[34:36], 16)
	copy(data[36:], "data")
	binary.LittleEndian.PutUint32(data[40:44], uint32(samples*2))
	for j := 44; j < len(data); j += 2 {
		binary.LittleEndian.PutUint16(data[j:], 1000)
	}
	return data
}

func TestValidateCallWAV(t *testing.T) {
	if err := validateCallWAV(callTestWAV(8000)); err != nil {
		t.Fatal(err)
	}
	if err := validateCallWAV(callTestWAV(120 * 8000)); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"empty", "truncated", "size", "rate", "stereo", "compressed", "bits", "long", "chunk-size"} {
		t.Run(name, func(t *testing.T) {
			data := callTestWAV(8000)
			switch name {
			case "empty":
				data = callTestWAV(0)
			case "truncated":
				data = data[:43]
			case "size":
				data[4] = 0
			case "rate":
				binary.LittleEndian.PutUint32(data[24:28], 24000)
			case "stereo":
				data[22] = 2
			case "compressed":
				data[20] = 3
			case "bits":
				data[34] = 8
			case "long":
				data = callTestWAV(120*8000 + 1)
			case "chunk-size":
				binary.LittleEndian.PutUint32(data[40:44], 0xffffffff)
			}
			if err := validateCallWAV(data); err == nil {
				t.Fatal("invalid WAV accepted")
			}
		})
	}
}

func TestTTSAudioWithoutFiles(t *testing.T) {
	requests := 0
	c := &callService{cfg: callConfig{speaker: 3}, http: &http.Client{Transport: callRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if strings.HasSuffix(r.URL.Path, "audio_query") {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"outputSamplingRate":24000,"outputStereo":true,"accent_phrases":[]}`))}, nil
		}
		var payload struct {
			Speaker int            `json:"speaker"`
			Query   map[string]any `json:"audioQuery"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Speaker != 3 || payload.Query["outputSamplingRate"] != float64(8000) || payload.Query["outputStereo"] != false {
			t.Errorf("incorrect synthesis format: %v", payload)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(callTestWAV(800)))}, nil
	})}}
	wav, err := c.audio(context.Background(), "hello")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wav, callTestWAV(800)) || requests != 2 {
		t.Fatal("unexpected audio or requests")
	}
}

func TestSIPConfiguration(t *testing.T) {
	for key, value := range map[string]string{"CALL_SIP_ADDRESS": "127.0.0.1:5060", "CALL_SIP_USERNAME": "200", "CALL_SIP_PASSWORD": "test-secret", "CALL_EXTENSION": "100", "CALL_SIP_BIND_IP": "127.0.0.1", "CALL_SIP_ADVERTISE_IP": "", "CALL_SIP_TRANSPORT": "udp", "CALL_SIP_BIND_PORT": "15060", "CALL_RTP_PORT_START": "20000", "CALL_RTP_PORT_END": "20100"} {
		t.Setenv(key, value)
	}
	if _, err := loadSIPConfig(); err != nil {
		t.Fatal(err)
	}
	for _, tt := range []struct{ key, value string }{{"CALL_SIP_ADDRESS", "host:0"}, {"CALL_SIP_ADDRESS", "host:5060\r\nHeader: injected"}, {"CALL_SIP_USERNAME", "200@evil"}, {"CALL_EXTENSION", "200"}, {"CALL_SIP_BIND_IP", "0.0.0.0"}, {"CALL_SIP_ADVERTISE_IP", "invalid"}, {"CALL_RTP_PORT_START", "20001"}, {"CALL_SIP_TRANSPORT", "invalid"}} {
		t.Run(tt.key+tt.value, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			if _, err := loadSIPConfig(); err == nil {
				t.Fatal("invalid configuration accepted")
			}
		})
	}
}

// Real loopback SIP/RTP integration: digest REGISTER and INVITE, ACK, audio,
// remote BYE, local BYE and cancellation. No PBX, external service or FFmpeg.
func TestSIPLoopback(t *testing.T) {
	for _, scenario := range []string{"success", "busy", "unavailable", "rejected", "interrupted", "timeout", "cancelled"} {
		t.Run(scenario, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			ua, err := sipgo.NewUA()
			if err != nil {
				t.Fatal(err)
			}
			defer ua.Close()
			srv, err := sipgo.NewServer(ua)
			if err != nil {
				t.Fatal(err)
			}
			challenge := digest.Challenge{Realm: "test-pbx", Nonce: "loopback-test-nonce", Algorithm: "MD5"}
			authenticate := func(req *sip.Request, tx sip.ServerTransaction) bool {
				h := req.GetHeader("Authorization")
				if h == nil {
					res := sip.NewResponseFromRequest(req, 401, "Unauthorized", nil)
					res.AppendHeader(sip.NewHeader("WWW-Authenticate", challenge.String()))
					if err := tx.Respond(res); err != nil {
						t.Error(err)
					}
					return false
				}
				cred, e := digest.ParseCredentials(h.Value())
				if e != nil {
					t.Error(e)
					return false
				}
				want, e := digest.Digest(&challenge, digest.Options{Method: req.Method.String(), URI: cred.URI, Username: "200", Password: "test-secret"})
				if e != nil || cred.Username != "200" || cred.Response != want.Response {
					t.Error("bad digest credentials")
					tx.Respond(sip.NewResponseFromRequest(req, 403, "Forbidden", nil))
					return false
				}
				return true
			}
			srv.OnRegister(func(req *sip.Request, tx sip.ServerTransaction) {
				if authenticate(req, tx) {
					if err := tx.Respond(sip.NewResponseFromRequest(req, 200, "OK", nil)); err != nil {
						t.Error(err)
					}
				}
			})
			pbx := diago.NewDiago(ua, diago.WithServer(srv), diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "127.0.0.1", BindPort: 0}), diago.WithMediaConfig(diago.MediaConfig{Codecs: []media.Codec{media.CodecAudioUlaw, media.CodecAudioAlaw}}), diago.WithServerRequestMiddleware(func(next sipgo.RequestHandler) sipgo.RequestHandler {
				return func(req *sip.Request, tx sip.ServerTransaction) {
					if req.Method == sip.INVITE && !authenticate(req, tx) {
						return
					}
					next(req, tx)
				}
			}))
			received := make(chan int, 1)
			pbxCtx, stopPBX := context.WithCancel(context.Background())
			defer stopPBX()
			if err = pbx.ServeBackground(pbxCtx, func(d *diago.DialogServerSession) {
				if d.FromUser() != "200" || d.ToUser() != "100" {
					t.Error("incorrect caller or recipient")
				}
				switch scenario {
				case "busy":
					d.Respond(486, "Busy Here", nil)
					return
				case "unavailable":
					d.Respond(480, "Unavailable", nil)
					return
				case "rejected":
					d.Respond(603, "Decline", nil)
					return
				case "timeout":
					d.Respond(180, "Ringing", nil)
					select {
					case <-d.Context().Done():
					case <-time.After(time.Second):
					}
					return
				}
				med, err := d.Answer(diago.AnswerOptions{})
				if err != nil {
					t.Error(err)
					return
				}
				defer med.Close()
				reader, err := med.AudioReader()
				if err != nil {
					t.Error(err)
					return
				}
				buf := make([]byte, 160)
				n, err := reader.Read(buf)
				if err != nil {
					t.Error(err)
					return
				}
				received <- n
				if scenario == "interrupted" {
					hangCtx, c := context.WithTimeout(context.Background(), time.Second)
					defer c()
					if err = d.Hangup(hangCtx); err != nil {
						t.Error(err)
					}
					return
				}
				for {
					if _, err = reader.Read(buf); err != nil {
						break
					}
				}
			}); err != nil {
				t.Fatal(err)
			}
			// ListenReady precedes transport-layer bookkeeping. Read through its
			// synchronized accessor and wait until the listener is published.
			ports := 0
			for ports == 0 {
				if listeners := ua.TransportLayer().ListenPorts("udp"); len(listeners) > 0 {
					ports = listeners[0]
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("PBX listener unavailable")
				case <-time.After(time.Millisecond):
				}
			}
			conn, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			clientPort := conn.LocalAddr().(*net.UDPAddr).Port
			conn.Close()
			cfg := sipConfig{address: fmt.Sprintf("127.0.0.1:%d", ports), username: "200", password: "test-secret", extension: "100", bindIP: "127.0.0.1", advertisedIP: "127.0.0.1", transport: "udp", bindPort: clientPort, rtpStart: 20000, rtpEnd: 20100}
			ringTimeout := 2 * time.Second
			if scenario == "timeout" {
				ringTimeout = 100 * time.Millisecond
			}
			phone, err := newSIPPhone(ctx, cfg, ringTimeout)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { cancel(); phone.Close() }()
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for !phone.Ready() {
				select {
				case <-ctx.Done():
					t.Fatal("registration timed out")
				case <-ticker.C:
				}
			}
			callCtx, callCancel := context.WithCancel(ctx)
			defer callCancel()
			var updates []string
			status, err := phone.Call(callCtx, callTestWAV(4000), func(s string) {
				updates = append(updates, s)
				if scenario == "cancelled" && len(updates) == 2 {
					callCancel()
				}
			})
			if scenario == "cancelled" {
				if err == nil {
					t.Fatal("cancelled call succeeded")
				}
				return
			}
			if err != nil {
				t.Fatalf("call failed: %v", err)
			}
			switch scenario {
			case "success":
				if !strings.Contains(status, "送信が完了") || len(updates) != 2 {
					t.Fatalf("%s %v", status, updates)
				}
				select {
				case n := <-received:
					if n == 0 {
						t.Fatal("empty RTP")
					}
				default:
					t.Fatal("no RTP received")
				}
			case "interrupted":
				if !strings.Contains(status, "最後まで送信されませんでした") {
					t.Fatal(status)
				}
			case "busy":
				if !strings.Contains(status, "通話中") {
					t.Fatal(status)
				}
			case "unavailable", "timeout":
				if !strings.Contains(status, "応答しませんでした") {
					t.Fatal(status)
				}
			case "rejected":
				if !strings.Contains(status, "拒否") {
					t.Fatal(status)
				}
			}
		})
	}
}
