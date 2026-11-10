package transport

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/poligon/runner/internal/execution"
)

type PCMFormat struct {
	SampleRate int    `json:"sample_rate"`
	Channels   int    `json:"channels"`
	Encoding   string `json:"encoding"`
}

type pcmHello struct {
	Version int       `json:"schema_version"`
	Type    string    `json:"type"`
	Format  PCMFormat `json:"format"`
}

// PCM is the bounded transport for the proposed voice protocol.
// The dialogue engine requires a voice simulator contract before it can use it.
type PCM struct {
	conn   *websocket.Conn
	format PCMFormat
	max    int
	idle   time.Duration
}

func (a *Agents) OpenPCM(ctx context.Context, endpoint, token string, f PCMFormat, frameLimit int) (*PCM, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "wss" && !(a.Local && u.Scheme == "ws")) ||
		f.Encoding != "s16le" || f.Channels < 1 || f.Channels > 2 || f.SampleRate < 8000 ||
		f.SampleRate > 48000 || frameLimit < 2*f.Channels || int64(frameLimit) > a.Limit || !headerValue(token) {
		return nil, execution.ErrProtocol
	}
	if u.Scheme == "wss" {
		u.Scheme = "https"
	} else {
		u.Scheme = "http"
	}
	safe, err := a.agentEndpoint(u.String())
	if err != nil {
		return nil, err
	}
	token = strings.Trim(token, " ")
	h := make(http.Header)
	if token != "" {
		h.Set("Authorization", "Bearer "+token)
	}
	ctx, cancel := context.WithTimeout(ctx, a.Timeout)
	defer cancel()
	if err := execution.CheckLease(ctx); err != nil {
		return nil, err
	}
	conn, _, err := websocket.Dial(ctx, safe.String(), &websocket.DialOptions{
		HTTPClient: a.Client, HTTPHeader: h, Subprotocols: []string{"poligon.ws-pcm.v1"},
	})
	if err != nil {
		return nil, errors.New("WebSocket connection failed")
	}
	if conn.Subprotocol() != "poligon.ws-pcm.v1" {
		conn.CloseNow()
		return nil, execution.ErrProtocol
	}
	conn.SetReadLimit(int64(max(frameLimit, 512)))
	hello, _ := json.Marshal(pcmHello{1, "open", f})
	if err = execution.CheckLease(ctx); err != nil {
		conn.CloseNow()
		return nil, err
	}
	if err = conn.Write(ctx, websocket.MessageText, hello); err != nil {
		conn.CloseNow()
		return nil, errors.New("WebSocket handshake failed")
	}
	kind, b, err := conn.Read(ctx)
	var accepted pcmHello
	if err != nil || kind != websocket.MessageText || execution.DecodeStrict(b, &accepted) != nil ||
		accepted.Version != 1 || accepted.Type != "accepted" || accepted.Format != f {
		conn.CloseNow()
		return nil, execution.ErrProtocol
	}
	conn.SetReadLimit(int64(frameLimit))
	return &PCM{conn, f, frameLimit, a.Timeout}, nil
}

func (p *PCM) valid(frame []byte) bool {
	return len(frame) > 0 && len(frame) <= p.max && len(frame)%(2*p.format.Channels) == 0
}

func (p *PCM) Write(ctx context.Context, frame []byte) error {
	if !p.valid(frame) {
		return execution.ErrProtocol
	}
	ctx, cancel := context.WithTimeout(ctx, p.idle)
	defer cancel()
	if err := execution.CheckLease(ctx); err != nil {
		return err
	}
	return p.conn.Write(ctx, websocket.MessageBinary, frame)
}

func (p *PCM) Read(ctx context.Context) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, p.idle)
	defer cancel()
	if err := execution.CheckLease(ctx); err != nil {
		return nil, err
	}
	kind, frame, err := p.conn.Read(ctx)
	if err != nil {
		return nil, err
	}
	if kind != websocket.MessageBinary || !p.valid(frame) {
		p.conn.CloseNow()
		return nil, execution.ErrProtocol
	}
	return frame, nil
}

func (p *PCM) Close() error {
	err := p.conn.Close(websocket.StatusNormalClosure, "")
	if err != nil && !strings.Contains(err.Error(), "closed") {
		return err
	}
	return nil
}
