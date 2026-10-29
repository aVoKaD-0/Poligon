package platform

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCanonicalJSONAndCredentialRestore(t *testing.T) {
	_, a, e := canonical(Object{"b": json.Number("1.0"), "a": "тест"})
	if e != nil {
		t.Fatal(e)
	}
	_, b, e := canonical(Object{"a": "тест", "b": 1})
	if e != nil || a != b {
		t.Fatal("equivalent JSON must have the same JCS hash")
	}
	key := make([]byte, 32)
	cipher, e := encrypt(key, "provider-secret")
	if e != nil {
		t.Fatal(e)
	}
	plain, e := decrypt(key, cipher)
	if e != nil || plain != "provider-secret" {
		t.Fatal("credential restore failed")
	}
	wrong := make([]byte, 32)
	wrong[0] = 1
	if _, e = decrypt(wrong, cipher); e == nil {
		t.Fatal("restore without the original master key must fail")
	}
	cipher[len(cipher)-1] ^= 1
	if _, e = decrypt(key, cipher); e == nil {
		t.Fatal("tampered ciphertext accepted")
	}
}
func TestAssessmentValidityAndRequiredAggregation(t *testing.T) {
	for _, tc := range []struct {
		name, validity, verdict string
		statuses                []string
		wantError               bool
	}{{"pass", "valid", "pass", []string{"pass"}, false}, {"agent defect", "valid", "fail", []string{"pass", "fail"}, false}, {"missing evidence", "valid", "inconclusive", []string{"inconclusive"}, false}, {"invalid simulation", "invalid", "inconclusive", []string{"fail"}, false}, {"unknown simulation cannot fail agent", "unknown", "fail", []string{"fail"}, true}, {"no checks cannot pass", "valid", "pass", nil, true}, {"evaluator error cannot pass", "valid", "pass", []string{"error"}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			rows := []any{}
			for _, s := range tc.statuses {
				rows = append(rows, Object{"required": true, "status": s})
			}
			d := Object{"schema_version": 1, "evaluation_id": ID(), "evaluator_version_id": ID(), "simulation_validity": tc.validity, "verdict": tc.verdict, "assertions": rows}
			e := ValidateAssessment(d)
			if (e != nil) != tc.wantError {
				t.Fatalf("assessment validation: %v", e)
			}
		})
	}
}
func TestInvalidLocalAuthConfig(t *testing.T) {
	t.Setenv("APP_ENV", "production")
	t.Setenv("CREDENTIAL_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("DATABASE_URL", "postgres://unused")
	t.Setenv("ALLOW_PRIVATE_AGENTS", "true")
	if _, e := LoadConfig(); e == nil {
		t.Fatal("production must reject insecure local configuration")
	}
}

func TestStrictJSONBody(t *testing.T) {
	for _, raw := range []string{`{"name":"first","name":"second"}`, `{"name":"x"} {"name":"y"}`, `[]`, `null`} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
		if _, e := body(httptest.NewRecorder(), r); e == nil {
			t.Fatalf("invalid JSON body accepted: %s", raw)
		}
	}
}
