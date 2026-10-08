package auth

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

// makeJWT 构造一个仅用于测试的 JWT（签名段为占位）。
func makeJWT(payload map[string]any) string {
	head := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	pb, _ := json.Marshal(payload)
	body := base64.RawURLEncoding.EncodeToString(pb)
	return head + "." + body + ".sig"
}

func TestParseJWTClaims(t *testing.T) {
	tok := makeJWT(map[string]any{
		"data": map[string]any{"id": "1496859252103770", "user_id": 42, "tenant_id": "t1"},
		"exp":  1753600000,
	})
	uid, exp, err := parseJWTClaims(tok)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "1496859252103770" {
		t.Errorf("uid=%q", uid)
	}
	if exp != 1753600000 {
		t.Errorf("exp=%d", exp)
	}
}

func TestParseJWTClaimsFallbackUserID(t *testing.T) {
	tok := makeJWT(map[string]any{"data": map[string]any{"user_id": 42}})
	uid, _, err := parseJWTClaims(tok)
	if err != nil {
		t.Fatal(err)
	}
	if uid != "42" {
		t.Errorf("uid=%q", uid)
	}
}

func TestParseJWTClaimsInvalid(t *testing.T) {
	if _, _, err := parseJWTClaims("not-a-jwt"); err == nil {
		t.Error("want error for non-jwt input")
	}
}