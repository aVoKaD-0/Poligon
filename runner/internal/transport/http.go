package transport

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/poligon/runner/internal/execution"
	"go.opentelemetry.io/otel/propagation"
)

var errSize = errors.New("HTTP response size exceeded")

func HTTPClient(connections int, allowLocal bool, agent bool) *http.Client {
	dialer := &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}
	dial := dialer.DialContext
	if agent && !allowLocal {
		dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, err
			}
			ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
			if err != nil {
				return nil, err
			}
			for _, ip := range ips {
				if !publicIP(ip) {
					return nil, errors.New("agent DNS resolved to a restricted network")
				}
			}
			for _, ip := range ips {
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
			}
			return nil, errors.New("agent connection failed")
		}
	}
	return &http.Client{Transport: &http.Transport{
		DialContext: dial, TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 5 * time.Second, ForceAttemptHTTP2: true, MaxResponseHeaderBytes: 32 << 10,
		MaxConnsPerHost: connections, MaxIdleConnsPerHost: connections, MaxIdleConns: connections,
		IdleConnTimeout: 60 * time.Second, ResponseHeaderTimeout: 30 * time.Second,
	}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

var special = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"),
	netip.MustParsePrefix("3fff::/20"),
}
var globalV6 = netip.MustParsePrefix("2000::/3")

func publicIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || (ip.Is6() && !globalV6.Contains(ip)) {
		return false
	}
	for _, p := range special {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}

func Endpoint(raw string, local bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.Fragment != "" ||
		(u.Scheme != "https" && !(local && u.Scheme == "http")) {
		return nil, errors.New("invalid HTTP endpoint")
	}
	return u, nil
}

func ReadBody(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errSize
	}
	return b, nil
}

func send(ctx context.Context, c *http.Client, method, target string, raw []byte, headers http.Header, limit int64) (int, []byte, error) {
	res, err := sendRequest(ctx, c, method, target, raw, headers)
	if err != nil {
		return 0, nil, err
	}
	defer res.Body.Close()
	b, err := ReadBody(res.Body, limit)
	return res.StatusCode, b, err
}

func sendRequest(ctx context.Context, c *http.Client, method, target string, raw []byte, headers http.Header) (*http.Response, error) {
	if err := execution.CheckLease(ctx); err != nil {
		return nil, err
	}
	var reader io.Reader
	if raw != nil {
		reader = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, err
	}
	// A write with an Idempotency-Key must still reach the CRM at most once per call.
	if method != http.MethodGet {
		req.GetBody = nil
	}
	req.Header = headers
	if raw != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	propagation.TraceContext{}.Inject(ctx, propagation.HeaderCarrier(req.Header))
	return c.Do(req)
}

func errCode(ctx context.Context, err error) string {
	if errors.Is(err, errSize) {
		return "response_too_large"
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return "cancelled"
	}
	var netErr net.Error
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.As(err, &netErr) && netErr.Timeout() {
		return "timeout"
	}
	return "transport_error"
}

func headerValue(v string) bool {
	return !strings.ContainsFunc(v, func(r rune) bool { return r < 32 || r > 126 })
}

func objectJSON(raw []byte) bool {
	var object json.RawMessage
	return execution.DecodeStrict(raw, &object) == nil
}
