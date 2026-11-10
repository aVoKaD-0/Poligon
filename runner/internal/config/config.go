package config

import (
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Local, ProposedHTTP, Profiles                    bool
	NATSURL, NATSUser, NATSPassword, Durable         string
	Control, ControlToken, CA, Cert, Key, ServerName string
	Environment, Listen, MetricsToken, TempDir       string
	AgentHosts, UploadHosts                          map[string]bool
	Slots, Pending, PlanLimit, Connections           int
	MessageLimit, EvidenceLimit                      int64
	RPC, Request, Tool, Upload, Drain                time.Duration
}

func Load() (Config, error) {
	var c Config
	switch os.Getenv("RUNNER_MODE") {
	case "", "local":
		c.Local = true
	case "production":
	default:
		return c, errors.New("invalid RUNNER_MODE")
	}
	c.NATSURL = env("RUNNER_NATS_URL", "nats://127.0.0.1:4222")
	c.NATSUser = os.Getenv("RUNNER_NATS_USER")
	c.NATSPassword = os.Getenv("RUNNER_NATS_PASSWORD")
	c.Durable = env("RUNNER_DURABLE", "poligon-runner-v1")
	c.Control = env("RUNNER_CONTROL_ADDR", "127.0.0.1:9090")
	c.ControlToken = os.Getenv("RUNNER_CONTROL_TOKEN")
	c.CA, c.Cert, c.Key = os.Getenv("RUNNER_TLS_CA"), os.Getenv("RUNNER_TLS_CERT"), os.Getenv("RUNNER_TLS_KEY")
	c.ServerName = os.Getenv("RUNNER_TLS_SERVER_NAME")
	c.Environment = env("RUNNER_ENVIRONMENT_URL", "http://127.0.0.1:8081")
	c.Listen = env("RUNNER_DIAGNOSTICS_ADDR", "127.0.0.1:8091")
	c.MetricsToken = os.Getenv("RUNNER_DIAGNOSTICS_TOKEN")
	c.TempDir = env("RUNNER_TEMP_DIR", os.TempDir())
	c.AgentHosts = hosts(env("RUNNER_AGENT_HOSTS", "localhost,127.0.0.1"))
	c.UploadHosts = hosts(env("RUNNER_UPLOAD_HOSTS", "localhost:9000,127.0.0.1:9000"))
	var err error
	c.ProposedHTTP, err = boolEnv("RUNNER_HTTP_JSON_PROPOSAL")
	if err != nil {
		return c, err
	}
	c.Profiles, err = boolEnv("RUNNER_PPROF")
	if err != nil {
		return c, err
	}
	if c.Profiles && c.MetricsToken == "" {
		return c, errors.New("RUNNER_PPROF requires RUNNER_DIAGNOSTICS_TOKEN")
	}
	if (c.CA != "" || c.Cert != "" || c.Key != "") && (c.CA == "" || c.Cert == "" || c.Key == "") {
		return c, errors.New("incomplete mTLS configuration")
	}
	if c.Local && c.CA == "" && c.ControlToken == "" {
		return c, errors.New("local Control requires RUNNER_CONTROL_TOKEN")
	}
	if !c.Local && (c.CA == "" || c.ControlToken != "" || !strings.HasPrefix(c.NATSURL, "tls://") ||
		os.Getenv("RUNNER_AGENT_HOSTS") == "" || os.Getenv("RUNNER_UPLOAD_HOSTS") == "" ||
		len(c.AgentHosts) == 0 || len(c.UploadHosts) == 0) {
		return c, errors.New("production requires Control mTLS, NATS TLS and explicit endpoint allowlists")
	}
	if (c.NATSUser == "") != (c.NATSPassword == "") || !regexp.MustCompile("^[A-Za-z0-9_-]{1,80}$").MatchString(c.Durable) {
		return c, errors.New("invalid broker credentials or durable name")
	}
	for _, v := range []struct {
		key              string
		target           *int
		fallback, lo, hi int
	}{
		{"RUNNER_SLOTS", &c.Slots, 8, 1, 256},
		{"RUNNER_MAX_ACK_PENDING", &c.Pending, 128, 1, 8192},
		{"RUNNER_MAX_PLAN_BYTES", &c.PlanLimit, 1 << 20, 4096, 4 << 20},
		{"RUNNER_HTTP_CONNECTIONS", &c.Connections, 16, 1, 512},
	} {
		*v.target, err = integer(v.key, v.fallback, v.lo, v.hi)
		if err != nil {
			return c, err
		}
	}
	if c.Pending < c.Slots {
		return c, errors.New("RUNNER_MAX_ACK_PENDING must cover RUNNER_SLOTS")
	}
	message, err := integer("RUNNER_MAX_MESSAGE_BYTES", 64<<10, 1024, 64<<10)
	if err != nil {
		return c, err
	}
	c.MessageLimit = int64(message)
	evidence, err := integer("RUNNER_MAX_EVIDENCE_BYTES", 16<<20, 64<<10, 512<<20)
	if err != nil {
		return c, err
	}
	c.EvidenceLimit = int64(evidence)
	for _, v := range []struct {
		key          string
		target       *time.Duration
		fallback, hi time.Duration
	}{
		{"RUNNER_RPC_TIMEOUT", &c.RPC, 3 * time.Second, 8 * time.Second},
		{"RUNNER_AGENT_TIMEOUT", &c.Request, 15 * time.Second, 30 * time.Second},
		{"RUNNER_TOOL_TIMEOUT", &c.Tool, 5 * time.Second, 30 * time.Second},
		{"RUNNER_UPLOAD_TIMEOUT", &c.Upload, 6 * time.Second, 20 * time.Second},
		{"RUNNER_DRAIN_TIMEOUT", &c.Drain, 20 * time.Second, 2 * time.Minute},
	} {
		*v.target, err = time.ParseDuration(env(v.key, v.fallback.String()))
		if err != nil || *v.target < 100*time.Millisecond || *v.target > v.hi {
			return c, fmt.Errorf("invalid %s", v.key)
		}
	}
	return c, nil
}

func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}
func boolEnv(k string) (bool, error) {
	b, err := strconv.ParseBool(env(k, "false"))
	if err != nil {
		return false, fmt.Errorf("invalid %s", k)
	}
	return b, nil
}
func integer(k string, fallback, lo, hi int) (int, error) {
	n, err := strconv.Atoi(env(k, strconv.Itoa(fallback)))
	if err != nil || n < lo || n > hi {
		return 0, fmt.Errorf("invalid %s", k)
	}
	return n, nil
}
func hosts(raw string) map[string]bool {
	m := make(map[string]bool)
	for _, s := range strings.Split(raw, ",") {
		if s = strings.TrimSpace(s); s != "" {
			m[strings.ToLower(s)] = true
		}
	}
	return m
}
