package platform

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Object = map[string]any

type Config struct {
	Env, HTTPAddr, GRPCAddr, DatabaseURL, NATSURL, NATSUser, NATSPassword string
	Origin, OIDCIssuer, OIDCClientID, OIDCClientSecret                    string
	S3Endpoint, S3PublicEndpoint, S3AccessKey, S3SecretKey, S3Bucket      string
	S3TLS                                                                 bool
	MasterKey                                                             []byte
	ServiceTokens                                                         map[string]string
	TLSCert, TLSKey, TLSCA                                                string
	AllowedHosts                                                          []string
	AllowPrivate                                                          bool
	GlobalConcurrency, GlobalEvaluations                                  int
}

func LoadConfig() (Config, error) {
	c := Config{Env: os.Getenv("APP_ENV"), HTTPAddr: env("HTTP_ADDR", ":8080"), GRPCAddr: env("GRPC_ADDR", ":9090"), DatabaseURL: os.Getenv("DATABASE_URL"), NATSURL: env("NATS_URL", "nats://localhost:4222"), NATSUser: os.Getenv("NATS_USER"), NATSPassword: os.Getenv("NATS_PASSWORD"), Origin: env("PUBLIC_ORIGIN", "http://localhost:8080"), OIDCIssuer: os.Getenv("OIDC_ISSUER"), OIDCClientID: os.Getenv("OIDC_CLIENT_ID"), OIDCClientSecret: os.Getenv("OIDC_CLIENT_SECRET"), S3Endpoint: env("S3_ENDPOINT", "localhost:9000"), S3PublicEndpoint: env("S3_PUBLIC_ENDPOINT", env("S3_ENDPOINT", "localhost:9000")), S3AccessKey: os.Getenv("S3_ACCESS_KEY"), S3SecretKey: os.Getenv("S3_SECRET_KEY"), S3Bucket: env("S3_BUCKET", "poligon-evidence"), S3TLS: os.Getenv("S3_TLS") == "true", TLSCert: os.Getenv("GRPC_TLS_CERT"), TLSKey: os.Getenv("GRPC_TLS_KEY"), TLSCA: os.Getenv("GRPC_TLS_CA"), AllowedHosts: strings.FieldsFunc(os.Getenv("AGENT_ALLOWED_HOSTS"), func(r rune) bool { return r == ',' }), AllowPrivate: os.Getenv("ALLOW_PRIVATE_AGENTS") == "true", GlobalConcurrency: 50, GlobalEvaluations: 20, ServiceTokens: map[string]string{"runner": os.Getenv("RUNNER_SERVICE_TOKEN"), "environment": os.Getenv("ENVIRONMENT_SERVICE_TOKEN"), "intelligence": os.Getenv("INTELLIGENCE_SERVICE_TOKEN")}}
	var err error
	for key, target := range map[string]*int{"GLOBAL_CONCURRENCY": &c.GlobalConcurrency, "GLOBAL_EVALUATION_CONCURRENCY": &c.GlobalEvaluations} {
		if raw := os.Getenv(key); raw != "" {
			n, e := strconv.Atoi(raw)
			if e != nil || n < 1 || n > 1000 {
				return c, errors.New(key + " must be 1-1000")
			}
			*target = n
		}
	}
	c.MasterKey, err = base64.StdEncoding.DecodeString(os.Getenv("CREDENTIAL_MASTER_KEY"))
	if err != nil || len(c.MasterKey) != 32 {
		return c, errors.New("CREDENTIAL_MASTER_KEY must be a base64 32-byte key")
	}
	if c.Env != "local" && c.Env != "production" {
		return c, errors.New("APP_ENV must be explicitly local or production")
	}
	if c.DatabaseURL == "" {
		return c, errors.New("DATABASE_URL is required")
	}
	u, e := url.Parse(c.Origin)
	if e != nil || u.Host == "" || u.Path != "" || u.RawQuery != "" || u.Fragment != "" || u.User != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return c, errors.New("PUBLIC_ORIGIN must be an origin without path")
	}
	for _, v := range c.ServiceTokens {
		if c.Env == "local" && len(v) < 24 {
			return c, errors.New("local service tokens must be at least 24 characters")
		}
	}
	if c.Env == "production" && (u.Scheme != "https" || c.AllowPrivate || c.TLSCA == "" || c.TLSCert == "" || c.TLSKey == "" || c.OIDCIssuer == "" || !c.S3TLS) {
		return c, errors.New("production requires HTTPS, OIDC, mTLS, S3 TLS and public agent networks")
	}
	return c, nil
}
func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}
func ID() string {
	b := make([]byte, 16)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}
func validID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
func token() string {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		panic(e)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func hash(b []byte) string { s := sha256.Sum256(b); return hex.EncodeToString(s[:]) }
func canonical(v any) ([]byte, string, error) {
	b, e := json.Marshal(v)
	if e != nil {
		return nil, "", e
	}
	b, e = jsoncanonicalizer.Transform(b)
	if e != nil {
		return nil, "", e
	}
	return b, hash(b), nil
}
func encode(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func obj(b []byte) Object {
	var o Object
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	_ = d.Decode(&o)
	return o
}
func str(o Object, k string) string { v, _ := o[k].(string); return v }
func integer(o Object, k string, d int) int {
	v, ok := o[k]
	if !ok {
		return d
	}
	switch n := v.(type) {
	case float64:
		if n == float64(int(n)) {
			return int(n)
		}
	case json.Number:
		i, e := strconv.Atoi(string(n))
		if e == nil {
			return i
		}
	case int:
		return n
	}
	return -1
}
func stringsOf(v any) []string {
	a, _ := v.([]any)
	r := make([]string, 0, len(a))
	for _, s := range a {
		v, ok := s.(string)
		if !ok {
			return nil
		}
		r = append(r, v)
	}
	return r
}
func sub(o Object, k string) Object { v, _ := o[k].(map[string]any); return v }
func encrypt(key []byte, plain string) ([]byte, error) {
	b, e := aes.NewCipher(key)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	n := make([]byte, a.NonceSize())
	if _, e = rand.Read(n); e != nil {
		return nil, e
	}
	return a.Seal(n, n, []byte(plain), []byte("poligon.credentials.v1")), nil
}
func decrypt(key, b []byte) (string, error) {
	c, e := aes.NewCipher(key)
	if e != nil {
		return "", e
	}
	a, e := cipher.NewGCM(c)
	if e != nil || len(b) < a.NonceSize() {
		return "", errors.New("invalid ciphertext")
	}
	p, e := a.Open(nil, b[:a.NonceSize()], b[a.NonceSize():], []byte("poligon.credentials.v1"))
	return string(p), e
}

type APIError struct {
	Status        int
	Code, Message string
	Details       any
}

func (e *APIError) Error() string             { return e.Code }
func fail(status int, code, msg string) error { return &APIError{status, code, msg, Object{}} }
func invalid(msg string) error                { return fail(422, "VALIDATION_ERROR", msg) }
func conflict(msg string) error               { return fail(409, "VERSION_CONFLICT", msg) }
func notFound() error                         { return fail(404, "NOT_FOUND", "Ресурс не найден") }
func requireFields(o Object, allowed []string, required ...string) error {
	set := map[string]bool{}
	for _, k := range allowed {
		set[k] = true
	}
	for k := range o {
		if !set[k] {
			return invalid("Неизвестное поле: " + k)
		}
	}
	for _, k := range required {
		if o[k] == nil {
			return invalid("Обязательное поле: " + k)
		}
	}
	return nil
}
func checkETag(r *http.Request, revision int64) error {
	if r.Header.Get("If-Match") != fmt.Sprintf(`"%d"`, revision) {
		return fail(412, "PRECONDITION_FAILED", "Требуется актуальный If-Match")
	}
	return nil
}

type DB struct{ Pool *pgxpool.Pool }
type Queryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

func queryJSON(ctx context.Context, q Queryer, sql string, args ...any) ([]Object, error) {
	rows, e := q.Query(ctx, sql, args...)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	out := []Object{}
	for rows.Next() {
		var b []byte
		if e = rows.Scan(&b); e != nil {
			return nil, e
		}
		out = append(out, dbObject(b))
	}
	return out, rows.Err()
}
func oneJSON(ctx context.Context, q Queryer, sql string, args ...any) (Object, error) {
	var b []byte
	e := q.QueryRow(ctx, sql, args...).Scan(&b)
	if errors.Is(e, pgx.ErrNoRows) {
		return nil, notFound()
	}
	if e != nil {
		return nil, e
	}
	return dbObject(b), nil
}

// Short state transitions share a transaction-scoped lock. It also serializes global quotas
// across replicas. No network I/O runs while this lock is held.
func (d *DB) Tx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, e := d.Pool.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(ctx)
	if _, e = tx.Exec(ctx, "SELECT pg_advisory_xact_lock(70697001)"); e != nil {
		return e
	}
	if e = fn(tx); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func audit(ctx context.Context, tx pgx.Tx, p, actor, action, id string) error {
	_, e := tx.Exec(ctx, "INSERT INTO audit_log(id,project_id,actor,action,resource_id) VALUES($1,$2,$3,$4,$5)", ID(), p, actor, action, id)
	return e
}
func (c Config) ValidateEndpoint(ctx context.Context, raw string) error {
	u, e := url.Parse(raw)
	if e != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || !(u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "wss" || u.Scheme == "ws") {
		return invalid("Некорректный endpoint")
	}
	if c.Env != "local" && u.Scheme != "https" && u.Scheme != "wss" {
		return invalid("Endpoint требует TLS")
	}
	allowed := false
	for _, h := range c.AllowedHosts {
		if strings.EqualFold(h, u.Hostname()) {
			allowed = true
		}
	}
	if !allowed {
		return invalid("Endpoint отсутствует в allowlist")
	}
	ips, e := net.DefaultResolver.LookupIPAddr(ctx, u.Hostname())
	if e != nil {
		return fail(503, "DEPENDENCY_UNAVAILABLE", "Не удалось разрешить endpoint")
	}
	for _, ip := range ips {
		if !c.AllowPrivate && (ip.IP.IsPrivate() || ip.IP.IsLoopback() || ip.IP.IsLinkLocalUnicast() || ip.IP.IsUnspecified() || ip.IP.IsMulticast()) {
			return invalid("Endpoint использует запрещённую сеть")
		}
	}
	return nil
}
func terminal(s string) bool { return s == "completed" || s == "cancelled" || s == "failed" }
func utc() time.Time         { return time.Now().UTC() }

func dbObject(b []byte) Object {
	o := obj(b)
	if n, ok := o["revision"].(json.Number); ok {
		o["revision"] = string(n)
	}
	return o
}
