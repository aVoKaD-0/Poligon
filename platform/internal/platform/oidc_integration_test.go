package platform

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
	"golang.org/x/oauth2"
)

func TestOIDCCallbackVerifiesIdentityAndPKCE(t *testing.T) {
	pub, priv, e := ed25519.GenerateKey(rand.Reader)
	if e != nil {
		t.Fatal(e)
	}
	signer, e := jose.NewSigner(jose.SigningKey{Algorithm: jose.EdDSA, Key: jose.JSONWebKey{Key: priv, KeyID: "test"}}, nil)
	if e != nil {
		t.Fatal(e)
	}
	issuer, nonce, mode := "", "", ""
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			writeJSON(w, 200, Object{"issuer": issuer, "authorization_endpoint": issuer + "/auth", "token_endpoint": issuer + "/token", "jwks_uri": issuer + "/keys", "id_token_signing_alg_values_supported": []string{"EdDSA"}})
		case "/keys":
			_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: pub, KeyID: "test", Algorithm: "EdDSA", Use: "sig"}}})
		case "/token":
			_ = r.ParseForm()
			if r.Form.Get("code_verifier") == "" {
				t.Error("PKCE verifier missing")
			}
			claims := Object{"iss": issuer, "sub": "stable-subject", "aud": "platform", "nonce": nonce, "exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "name": "OIDC user"}
			switch mode {
			case "nonce":
				claims["nonce"] = "wrong"
			case "audience":
				claims["aud"] = "other-client"
			case "issuer":
				claims["iss"] = "https://wrong.example"
			case "expired":
				claims["exp"] = time.Now().Add(-time.Hour).Unix()
			}
			signed, _ := signer.Sign(encode(claims))
			serialized, _ := signed.CompactSerialize()
			writeJSON(w, 200, Object{"access_token": "not-stored", "token_type": "Bearer", "expires_in": 60, "id_token": serialized})
		default:
			w.WriteHeader(404)
		}
	}))
	defer provider.Close()
	issuer = provider.URL
	discovered, e := oidc.NewProvider(context.Background(), issuer)
	if e != nil {
		t.Fatal(e)
	}
	a := &App{DB: integrationDB(t), Config: Config{Env: "local", Origin: "http://platform.test", OIDCClientID: "platform"}, OIDC: discovered, OAuth: oauth2.Config{ClientID: "platform", Endpoint: discovered.Endpoint(), RedirectURL: "http://platform.test/api/v1/auth/callback", Scopes: []string{"openid"}}}
	f := &fixture{a: a, http: a.Handler()}
	for _, m := range []string{"nonce", "audience", "issuer", "expired", "valid"} {
		t.Run(m, func(t *testing.T) {
			mode = m
			login := f.request("GET", "/api/v1/auth/login", nil, nil)
			if login.Code != 302 {
				t.Fatal(login.Body.String())
			}
			location, e := url.Parse(login.Header().Get("Location"))
			if e != nil {
				t.Fatal(e)
			}
			if location.Query().Get("code_challenge_method") != "S256" || location.Query().Get("code_challenge") == "" {
				t.Fatal("missing PKCE challenge")
			}
			nonce = location.Query().Get("nonce")
			f.cookies = login.Result().Cookies()
			state := location.Query().Get("state")
			bad := f.request("GET", "/api/v1/auth/callback?state=invalid&code=test", nil, nil)
			if bad.Code != 400 {
				t.Fatal("invalid state accepted")
			}
			callback := f.request("GET", "/api/v1/auth/callback?state="+url.QueryEscape(state)+"&code=test", nil, nil)
			want := 401
			if mode == "valid" {
				want = 302
			}
			if callback.Code != want {
				t.Fatalf("callback %d wanted %d: %s", callback.Code, want, callback.Body.String())
			}
			if mode == "valid" {
				f.cookies = callback.Result().Cookies()
				profile := requireStatus(t, f.request("GET", "/api/v1/me", nil, nil), 200)
				if str(profile, "name") != "OIDC user" {
					t.Fatal("wrong identity")
				}
			}
			again := f.request("GET", "/api/v1/auth/callback?state="+url.QueryEscape(state)+"&code=test", nil, nil)
			if again.Code != 400 {
				t.Fatal("consumed state reused")
			}
		})
	}
}
