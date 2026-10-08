// local.go 本机 TRAE 客户端登录态发现：auths/ 为空时用于自动导入。
//
// 仅读取本机 trae-jwt-token（Trae CN 客户端登录后写入），不访问网络；
// token 只在本机进程内使用，不外发。对标 workbuddy2api 的本地凭证读取。
package auth

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LocalToken 本机发现的 TRAE 登录 access token。
type LocalToken struct {
	AccessToken string // Cloud-IDE-JWT
	UID         string // JWT payload 解析出的 uid（可能为空）
	ExpiresAt   int64  // JWT exp（Unix 秒，0 表示未知）
	Path        string // token 文件路径
}

// LocalTokenPaths 返回 trae-jwt-token 的候选路径（Trae CN / Trae）。
func LocalTokenPaths() []string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil
	}
	return []string{
		filepath.Join(home, ".trae-cn", "trae-jwt-token"),
		filepath.Join(home, ".trae", "trae-jwt-token"),
	}
}

// DiscoverLocalToken 读取本机登录 token。未找到返回 (nil, nil)。
func DiscoverLocalToken() (*LocalToken, error) {
	for _, p := range LocalTokenPaths() {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		tok := strings.TrimSpace(string(raw))
		if tok == "" {
			continue
		}
		lt := &LocalToken{AccessToken: tok, Path: p}
		if uid, exp, perr := parseJWTClaims(tok); perr == nil {
			lt.UID = uid
			lt.ExpiresAt = exp
		}
		return lt, nil
	}
	return nil, nil
}

// NewMachineID 生成 32 位十六进制 machine id（与登录闭环一致）。
func NewMachineID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// parseJWTClaims 解析 JWT payload，取 uid（data.id 优先，回退 data.user_id）与 exp。
func parseJWTClaims(token string) (uid string, exp int64, err error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", 0, fmt.Errorf("not a jwt")
	}
	seg := parts[1]
	raw, derr := base64.RawURLEncoding.DecodeString(seg)
	if derr != nil {
		if pad := len(seg) % 4; pad != 0 {
			seg += strings.Repeat("=", 4-pad)
		}
		if raw, derr = base64.URLEncoding.DecodeString(seg); derr != nil {
			return "", 0, derr
		}
	}
	var payload struct {
		Data struct {
			ID     json.RawMessage `json:"id"`
			UserID json.RawMessage `json:"user_id"`
		} `json:"data"`
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return "", 0, err
	}
	uid = rawToScalar(payload.Data.ID)
	if uid == "" {
		uid = rawToScalar(payload.Data.UserID)
	}
	return uid, payload.Exp, nil
}

// rawToScalar 把 JSON RawMessage（字符串或数字）规整为字符串。
func rawToScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	return strings.Trim(string(raw), `"`)
}