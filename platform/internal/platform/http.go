package platform

import (
	"context"

	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"bytes"
	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/cyberphone/json-canonicalization/go/src/webpki.org/jsoncanonicalizer"
	"github.com/jackc/pgx/v5"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/oauth2"
)

type Validators interface {
	ValidateScenario(context.Context, Object) error
	CheckCompatibility(context.Context, Object, []Object, Object) error
}
type App struct {
	metrics    metrics
	DB         *DB
	Config     Config
	Validators Validators
	Store      *ArtifactStore
	OIDC       *oidc.Provider
	OAuth      oauth2.Config
}
type identity struct {
	ID, Name, CSRF string
	Expires        time.Time
}
type ctxKey string

func actor(ctx context.Context) identity { u, _ := ctx.Value(ctxKey("actor")).(identity); return u }

type endpoint func(http.ResponseWriter, *http.Request) (any, error)

func (a *App) wrap(fn endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		annotateHTTP(r)
		v, e := fn(w, r)
		if e != nil {
			writeError(w, r, e)
			return
		}
		if v != nil {
			writeJSON(w, http.StatusOK, v)
		}
	}
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeError(w http.ResponseWriter, r *http.Request, e error) {
	var api *APIError
	if !errors.As(e, &api) {
		slog.Error("request failed", "request_id", r.Context().Value(ctxKey("request_id")), "error_type", fmt.Sprintf("%T", e))
		api = &APIError{500, "INTERNAL_ERROR", "Внутренняя ошибка", Object{}}
	}
	writeJSON(w, api.Status, Object{"error": Object{"code": api.Code, "message": api.Message, "request_id": r.Context().Value(ctxKey("request_id")), "details": api.Details}})
}
func body(w http.ResponseWriter, r *http.Request) (Object, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil, fail(400, "INVALID_JSON", "Требуется application/json")
	}
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	raw, e := io.ReadAll(r.Body)
	if e != nil {
		return nil, fail(400, "INVALID_JSON", "Недопустимый размер тела")
	}
	if _, e = jsoncanonicalizer.Transform(raw); e != nil {
		return nil, fail(400, "INVALID_JSON", "Некорректный JSON или повторяющиеся поля")
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var o Object
	if e := d.Decode(&o); e != nil || o == nil {
		return nil, fail(400, "INVALID_JSON", "Ожидается JSON объект")
	}
	if e := d.Decode(new(any)); e != io.EOF {
		return nil, fail(400, "INVALID_JSON", "Недопустимое тело запроса")
	}
	return o, nil
}
func (a *App) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /metrics", a.metricsHandler)
	m.HandleFunc("GET /livez", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, Object{"status": "ok"}) })
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		ctx, c := context.WithTimeout(r.Context(), 2*time.Second)
		defer c()
		var v int
		e := a.DB.Pool.QueryRow(ctx, "SELECT max(version) FROM schema_migrations").Scan(&v)
		if e != nil || v != 1 {
			writeJSON(w, 503, Object{"status": "not_ready"})
			return
		}
		var lag int
		_ = a.DB.Pool.QueryRow(ctx, "SELECT count(*) FROM outbox WHERE published_at IS NULL AND created_at < now()-interval '5 minutes'").Scan(&lag)
		writeJSON(w, 200, Object{"status": "ready", "outbox_delayed": lag, "nats_connected": a.metrics.broker.Load(), "validators_configured": a.Validators != nil})
	})
	m.HandleFunc("GET /api/v1/auth/login", a.wrap(a.login))
	m.HandleFunc("GET /api/v1/auth/callback", a.wrap(a.callback))
	api := http.NewServeMux()
	api.HandleFunc("GET /api/v1/me", a.wrap(a.me))
	api.HandleFunc("POST /api/v1/auth/logout", a.wrap(a.logout))
	api.HandleFunc("GET /api/v1/projects", a.wrap(a.projects))
	api.HandleFunc("POST /api/v1/projects", a.wrap(a.projects))
	api.HandleFunc("/api/v1/projects/{project_id}", a.wrap(a.project))
	api.HandleFunc("/api/v1/projects/{project_id}/members", a.wrap(a.members))
	api.HandleFunc("/api/v1/projects/{project_id}/members/{user_id}", a.wrap(a.members))
	api.HandleFunc("/api/v1/projects/{project_id}/credentials", a.wrap(a.credentials))
	api.HandleFunc("/api/v1/projects/{project_id}/credentials/{credential_id}", a.wrap(a.credentials))
	for _, kind := range []string{"agents", "scenarios"} {
		base := "/api/v1/projects/{project_id}/" + kind
		api.HandleFunc(base, a.wrap(a.resources))
		api.HandleFunc(base+"/{resource_id}", a.wrap(a.resources))
		api.HandleFunc(base+"/{resource_id}/versions", a.wrap(a.versions))
		api.HandleFunc(base+"/{resource_id}/versions/{version_id}", a.wrap(a.versions))
		api.HandleFunc("POST "+base+"/{resource_id}/versions/{version_id}/publish", a.wrap(a.publish))
	}
	for _, kind := range []string{"environment", "evaluator", "simulator"} {
		base := "/api/v1/projects/{project_id}/" + kind + "-versions"
		api.HandleFunc("GET "+base, a.wrap(a.catalog))
		api.HandleFunc("GET "+base+"/{version_id}", a.wrap(a.catalog))
	}
	api.HandleFunc("/api/v1/projects/{project_id}/runs", a.wrap(a.runs))
	api.HandleFunc("GET /api/v1/projects/{project_id}/runs/{run_id}", a.wrap(a.run))
	api.HandleFunc("POST /api/v1/projects/{project_id}/runs/{run_id}/cancel", a.wrap(a.cancelRun))
	api.HandleFunc("POST /api/v1/projects/{project_id}/runs/{run_id}/retry", a.wrap(a.retryRun))
	api.HandleFunc("GET /api/v1/projects/{project_id}/runs/{run_id}/sessions", a.wrap(a.sessionList))
	api.HandleFunc("GET /api/v1/projects/{project_id}/sessions/{session_id}", a.wrap(a.session))
	api.HandleFunc("GET /api/v1/projects/{project_id}/sessions/{session_id}/events", a.wrap(a.sessionEvents))
	api.HandleFunc("GET /api/v1/projects/{project_id}/sessions/{session_id}/assessment", a.wrap(a.assessment))
	api.HandleFunc("POST /api/v1/projects/{project_id}/sessions/{session_id}/reevaluate", a.wrap(a.reevaluate))
	api.HandleFunc("GET /api/v1/projects/{project_id}/sessions/{session_id}/evaluations", a.wrap(a.evaluations))
	api.HandleFunc("GET /api/v1/projects/{project_id}/sessions/{session_id}/evaluations/{evaluation_id}", a.wrap(a.evaluations))
	api.HandleFunc("POST /api/v1/projects/{project_id}/sessions/{session_id}/evaluations/{evaluation_id}/cancel", a.wrap(a.cancelEvaluation))
	api.HandleFunc("GET /api/v1/projects/{project_id}/runs/{run_id}/stream", a.stream)
	api.HandleFunc("GET /api/v1/projects/{project_id}/runs/{run_id}/report", a.wrap(a.report))
	api.HandleFunc("GET /api/v1/projects/{project_id}/comparisons", a.wrap(a.compare))
	api.HandleFunc("GET /api/v1/projects/{project_id}/artifacts/{artifact_id}/download", a.wrap(a.download))
	m.Handle("/api/", a.authenticate(api))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := ID()
		w.Header().Set("X-Request-ID", id)
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Cache-Control", "no-store")
		ctx := context.WithValue(r.Context(), ctxKey("request_id"), id)
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
		ctx = context.WithValue(ctx, ctxKey("http_trace"), &httpTrace{})
		ctx, span := otel.Tracer("poligon/platform").Start(ctx, r.Method+" HTTP")
		defer span.End()
		defer func() {
			if recover() != nil {
				writeError(w, r, errors.New("panic"))
			}
		}()
		a.observe(w, r.WithContext(ctx), m.ServeHTTP)
	})
}
func (a *App) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, e := a.identity(r)
		if e != nil {
			writeError(w, r, e)
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			csrf := r.Header.Get("X-CSRF-Token")
			if r.Header.Get("Origin") != a.Config.Origin || subtle.ConstantTimeCompare([]byte(csrf), []byte(u.CSRF)) != 1 {
				writeError(w, r, fail(403, "CSRF_REJECTED", "Недопустимый Origin или CSRF token"))
				return
			}
		}
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey("actor"), u)))
	})
}
func (a *App) identity(r *http.Request) (identity, error) {
	var u identity
	s, e := r.Cookie("poligon_session")
	c, ce := r.Cookie("poligon_csrf")
	if e != nil || ce != nil {
		return u, fail(401, "UNAUTHENTICATED", "Требуется вход")
	}
	var csrfHash string
	e = a.DB.Pool.QueryRow(r.Context(), "SELECT u.id::text,u.name,s.csrf_hash,s.expires_at FROM web_sessions s JOIN users u ON u.id=s.user_id WHERE token_hash=$1 AND expires_at>now()", hash([]byte(s.Value))).Scan(&u.ID, &u.Name, &csrfHash, &u.Expires)
	if e != nil || subtle.ConstantTimeCompare([]byte(csrfHash), []byte(hash([]byte(c.Value)))) != 1 {
		return u, fail(401, "UNAUTHENTICATED", "Сессия истекла")
	}
	u.CSRF = c.Value
	return u, nil
}
func (a *App) setCookie(w http.ResponseWriter, name, value string, age int) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: value, Path: "/", HttpOnly: true, Secure: a.Config.Env != "local", SameSite: http.SameSiteLaxMode, MaxAge: age})
}
func (a *App) newSession(ctx context.Context, w http.ResponseWriter, user string) error {
	s, c := token(), token()
	_, e := a.DB.Pool.Exec(ctx, "INSERT INTO web_sessions(token_hash,user_id,csrf_hash,expires_at) VALUES($1,$2,$3,now()+interval '8 hours')", hash([]byte(s)), user, hash([]byte(c)))
	if e == nil {
		a.setCookie(w, "poligon_session", s, 28800)
		a.setCookie(w, "poligon_csrf", c, 28800)
	}
	return e
}
func (a *App) login(w http.ResponseWriter, r *http.Request) (any, error) {
	if a.Config.Env == "local" && a.OIDC == nil {
		var id string
		e := a.DB.Pool.QueryRow(r.Context(), "INSERT INTO users(id,issuer,subject,name) VALUES($1,'local','developer','Local developer') ON CONFLICT(issuer,subject) DO UPDATE SET name=excluded.name RETURNING id::text", ID()).Scan(&id)
		if e != nil {
			return nil, e
		}
		if e = a.newSession(r.Context(), w, id); e != nil {
			return nil, e
		}
		http.Redirect(w, r, a.Config.Origin+"/", 302)
		return nil, nil
	}
	if a.OIDC == nil {
		return nil, fail(503, "AUTH_UNAVAILABLE", "OIDC не настроен")
	}
	state, nonce, verifier := token(), token(), oauth2.GenerateVerifier()
	_, e := a.DB.Pool.Exec(r.Context(), "INSERT INTO login_states(state_hash,nonce,verifier,expires_at) VALUES($1,$2,$3,now()+interval '5 minutes')", hash([]byte(state)), nonce, verifier)
	if e != nil {
		return nil, e
	}
	a.setCookie(w, "poligon_login_state", state, 300)
	http.Redirect(w, r, a.OAuth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), 302)
	return nil, nil
}
func (a *App) callback(w http.ResponseWriter, r *http.Request) (any, error) {
	if a.OIDC == nil {
		return nil, fail(503, "AUTH_UNAVAILABLE", "OIDC не настроен")
	}
	state := r.URL.Query().Get("state")
	cookie, e := r.Cookie("poligon_login_state")
	if e != nil || state == "" || subtle.ConstantTimeCompare([]byte(state), []byte(cookie.Value)) != 1 {
		return nil, fail(400, "INVALID_STATE", "Недопустимый state")
	}
	var nonce, verifier string
	e = a.DB.Pool.QueryRow(r.Context(), "DELETE FROM login_states WHERE state_hash=$1 AND expires_at>now() RETURNING nonce,verifier", hash([]byte(state))).Scan(&nonce, &verifier)
	if e != nil {
		return nil, fail(400, "INVALID_STATE", "State истёк или уже использован")
	}
	ctx, c := context.WithTimeout(r.Context(), 10*time.Second)
	defer c()
	t, e := a.OAuth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if e != nil {
		return nil, fail(401, "OIDC_REJECTED", "Не удалось завершить вход")
	}
	raw, ok := t.Extra("id_token").(string)
	if !ok {
		return nil, fail(401, "OIDC_REJECTED", "Отсутствует ID token")
	}
	verified, e := a.OIDC.Verifier(&oidc.Config{ClientID: a.Config.OIDCClientID}).Verify(ctx, raw)
	if e != nil || verified.Nonce != nonce {
		return nil, fail(401, "OIDC_REJECTED", "ID token не прошёл проверку")
	}
	var claims struct {
		Name string `json:"name"`
	}
	_ = verified.Claims(&claims)
	var id string
	e = a.DB.Pool.QueryRow(ctx, "INSERT INTO users(id,issuer,subject,name) VALUES($1,$2,$3,$4) ON CONFLICT(issuer,subject) DO UPDATE SET name=excluded.name RETURNING id::text", ID(), verified.Issuer, verified.Subject, claims.Name).Scan(&id)
	if e != nil {
		return nil, e
	}
	if e = a.newSession(ctx, w, id); e != nil {
		return nil, e
	}
	a.setCookie(w, "poligon_login_state", "", -1)
	http.Redirect(w, r, a.Config.Origin+"/", 302)
	return nil, nil
}
func (a *App) logout(w http.ResponseWriter, r *http.Request) (any, error) {
	c, _ := r.Cookie("poligon_session")
	_, e := a.DB.Pool.Exec(r.Context(), "DELETE FROM web_sessions WHERE token_hash=$1", hash([]byte(c.Value)))
	a.setCookie(w, "poligon_session", "", -1)
	a.setCookie(w, "poligon_csrf", "", -1)
	return Object{"ok": true}, e
}
func (a *App) me(w http.ResponseWriter, r *http.Request) (any, error) {
	u := actor(r.Context())
	roles, e := queryJSON(r.Context(), a.DB.Pool, "SELECT jsonb_build_object('project_id',project_id,'role',role) FROM project_members WHERE user_id=$1 ORDER BY project_id", u.ID)
	return Object{"id": u.ID, "name": u.Name, "csrf_token": u.CSRF, "session_expires_at": u.Expires, "project_roles": roles}, e
}
func (a *App) authorize(r *http.Request, write string) error {
	p := r.PathValue("project_id")
	if !validID(p) {
		return notFound()
	}
	var role string
	e := a.DB.Pool.QueryRow(r.Context(), "SELECT role FROM project_members WHERE project_id=$1 AND user_id=$2", p, actor(r.Context()).ID).Scan(&role)
	if errors.Is(e, pgx.ErrNoRows) {
		return notFound()
	}
	if e != nil {
		return e
	}
	if write == "owner" && role != "owner" || write == "editor" && role == "viewer" {
		return fail(403, "FORBIDDEN", "Недостаточная роль")
	}
	return nil
}
func method(r *http.Request, allowed ...string) error {
	for _, m := range allowed {
		if r.Method == m {
			return nil
		}
	}
	return fail(405, "METHOD_NOT_ALLOWED", "Метод не поддерживается")
}
func page(r *http.Request) (int, string, error) {
	n := 50
	if s := r.URL.Query().Get("limit"); s != "" {
		var e error
		n, e = strconv.Atoi(s)
		if e != nil || n < 1 || n > 100 {
			return 0, "", invalid("limit: 1–100")
		}
	}
	cursor := "00000000-0000-0000-0000-000000000000"
	if s := r.URL.Query().Get("cursor"); s != "" {
		b, e := base64.RawURLEncoding.DecodeString(s)
		if e != nil || !validID(string(b)) {
			return 0, "", invalid("Некорректный cursor")
		}
		cursor = string(b)
	}
	return n, cursor, nil
}
func pageResult(items []Object, limit int) Object {
	var next any
	if len(items) > limit {
		items = items[:limit]
		next = base64.RawURLEncoding.EncodeToString([]byte(str(items[len(items)-1], "id")))
	}
	return Object{"items": items, "next_cursor": next}
}
func pk(r *http.Request, key string) (string, error) {
	v := r.PathValue(key)
	if !validID(v) {
		return "", notFound()
	}
	return v, nil
}
func etag(w http.ResponseWriter, v Object) { w.Header().Set("ETag", `"`+str(v, "revision")+`"`) }
func revision(v Object) int64              { n, _ := strconv.ParseInt(str(v, "revision"), 10, 64); return n }

type httpTrace struct{ route, project, run, session string }

func annotateHTTP(r *http.Request) {
	m, _ := r.Context().Value(ctxKey("http_trace")).(*httpTrace)
	if m == nil {
		return
	}
	m.route = r.Pattern
	m.project = r.PathValue("project_id")
	m.run = r.PathValue("run_id")
	m.session = r.PathValue("session_id")
	span := trace.SpanFromContext(r.Context())
	span.SetName(r.Method + " " + r.Pattern)
	span.SetAttributes(attribute.String("project_id", m.project), attribute.String("run_id", m.run), attribute.String("session_id", m.session))
}
