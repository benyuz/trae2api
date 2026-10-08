// config.go 加载 JSON 配置 + TW2A_* 环境变量覆盖。
// API Key 优先级：env TW2A_API_KEY > config.json 的 api_key > 首次运行自动生成并回写。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Config 顶层配置。
type Config struct {
	Listen          string `json:"listen"`            // ":7864"
	CallbackPort    string `json:"callback_port"`     // "18080"（TRAE 登录回调监听端口，0 = 不起）
	APIKey          string `json:"api_key,omitempty"` // 首次运行自动生成并落盘本地 config.json
	AuthDir         string `json:"auth_dir"`          // "./auths"
	StateFile       string `json:"state_file"`        // "./data/state.json"
	DefaultModel    string `json:"default_model"`     // "glm-5.2"
	WorkMode        string `json:"work_mode"`         // "auto" | "native" | "bridge" | "disabled" (默认 auto)
	WorkHost        string `json:"work_host"`         // 官方 Work 专属端点 (默认 https://api5-normal.mchost.guru)
	WorkBridgeURL   string `json:"work_bridge_url"`   // "http://host-gateway:7865" (本地 Work 积分桥接端点)
	WorkBridgeToken string `json:"work_bridge_token"` // 访问 WorkBridge 的 Bearer token（空 = 不鉴权）

	Cooldown struct {
		PlanCredit  string `json:"plan_credit"`   // "12h"
		SoftRate    string `json:"soft_rate"`     // "60s"
		ErrThresh   int    `json:"err_threshold"` // 3
		ErrCooldown string `json:"err_cooldown"`  // "10m"
	} `json:"cooldown"`

	Schedule struct {
		CheckinHour  int   `json:"checkin_hour"`  // 9
		RefreshHours []int `json:"refresh_hours"` // [3]
	} `json:"schedule"`

	Upstream struct {
		TimeoutSeconds int `json:"timeout_seconds"` // 120
	} `json:"upstream"`

	// 解析后的 duration。
	PlanCreditDur  time.Duration `json:"-"`
	SoftRateDur    time.Duration `json:"-"`
	ErrCooldownDur time.Duration `json:"-"`

	// keyGenerated 标记本次 API Key 是否为自动生成（供启动日志提示，不落盘）。
	keyGenerated bool
}

// Default 返回默认配置。
func Default() *Config {
	c := &Config{
		Listen:          ":7864",
		CallbackPort:    "18080",
		APIKey:          "",
		AuthDir:         "./auths",
		StateFile:       "./data/state.json",
		DefaultModel:    "glm-5.2",
		WorkMode:        "auto",
		WorkHost:        "",
		WorkBridgeURL:   "http://127.0.0.1:7865",
		WorkBridgeToken: "",
	}
	c.Cooldown.PlanCredit = "12h"
	c.Cooldown.SoftRate = "60s"
	c.Cooldown.ErrThresh = 3
	c.Cooldown.ErrCooldown = "10m"
	c.Schedule.CheckinHour = 9
	c.Schedule.RefreshHours = []int{3}
	c.Upstream.TimeoutSeconds = 120
	return c
}

// Load 从 path 读配置。首次运行会落盘一份默认 config.json；缺少 API Key 时自动生成并回写；
// 最后用 TW2A_* env 覆盖（env 仅内存生效，不落盘）。path 为空时纯默认 + env。
func Load(path string) (*Config, error) {
	// 1) 首次运行：配置文件不存在则先落盘一份默认配置。
	if path != "" {
		if _, err := os.Stat(path); os.IsNotExist(err) {
			if werr := writeConfig(path, Default()); werr != nil {
				log.Printf("write default config %s: %v", path, werr)
			}
		}
	}

	// 2) 读取配置文件（文件值；env 稍后覆盖）。
	c := Default()
	if path != "" {
		raw, err := os.ReadFile(path)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("read config: %w", err)
			}
		} else if err := json.Unmarshal(raw, c); err != nil {
			return nil, fmt.Errorf("parse config: %w", err)
		}
	}

	// 3) API Key：env 优先；否则用文件内的；都没有则自动生成并回写（不写入 env 值）。
	if os.Getenv("TW2A_API_KEY") == "" && c.APIKey == "" {
		key, err := randomKey()
		if err != nil {
			return nil, fmt.Errorf("gen api key: %w", err)
		}
		c.APIKey = key
		c.keyGenerated = true
		if path != "" {
			if werr := writeConfig(path, c); werr != nil {
				log.Printf("persist api key to %s: %v", path, werr)
			}
		}
	}

	// 4) env 覆盖（最高优先级，仅内存生效）。
	applyEnv(c)
	if err := c.normalize(); err != nil {
		return nil, err
	}
	return c, nil
}

// randomKey 生成 32 位十六进制随机 API Key（128 bit 熵）。
func randomKey() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// writeConfig 将配置以 0600 权限原子落盘（含自动生成的 API Key）。
func writeConfig(path string, c *Config) error {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func applyEnv(c *Config) {
	if v := os.Getenv("TW2A_API_KEY"); v != "" {
		c.APIKey = v
	}
	if v := os.Getenv("TW2A_LISTEN"); v != "" {
		c.Listen = v
	}
	if v := os.Getenv("TW2A_CALLBACK_PORT"); v != "" {
		c.CallbackPort = v
	}
	if v := os.Getenv("TW2A_AUTH_DIR"); v != "" {
		c.AuthDir = v
	}
	if v := os.Getenv("TW2A_STATE_FILE"); v != "" {
		c.StateFile = v
	}
	if v := os.Getenv("TW2A_DEFAULT_MODEL"); v != "" {
		c.DefaultModel = v
	}
	if v := os.Getenv("TW2A_WORK_MODE"); v != "" {
		c.WorkMode = v
	}
	if v := os.Getenv("TW2A_WORK_HOST"); v != "" {
		c.WorkHost = v
	}
	if v := os.Getenv("TW2A_WORK_BRIDGE_URL"); v != "" {
		c.WorkBridgeURL = v
	}
	if v := os.Getenv("TW2A_WORK_BRIDGE_TOKEN"); v != "" {
		c.WorkBridgeToken = v
	}
	if v := os.Getenv("TW2A_PLAN_CREDIT"); v != "" {
		c.Cooldown.PlanCredit = v
	}
	if v := os.Getenv("TW2A_SOFT_RATE"); v != "" {
		c.Cooldown.SoftRate = v
	}
	if v := os.Getenv("TW2A_ERR_THRESHOLD"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Cooldown.ErrThresh = n
		}
	}
	if v := os.Getenv("TW2A_ERR_COOLDOWN"); v != "" {
		c.Cooldown.ErrCooldown = v
	}
	if v := os.Getenv("TW2A_CHECKIN_HOUR"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Schedule.CheckinHour = n
		}
	}
	if v := os.Getenv("TW2A_TIMEOUT_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			c.Upstream.TimeoutSeconds = n
		}
	}
}

func (c *Config) normalize() error {
	var err error
	if c.PlanCreditDur, err = time.ParseDuration(c.Cooldown.PlanCredit); err != nil {
		return fmt.Errorf("cooldown.plan_credit: %w", err)
	}
	if c.SoftRateDur, err = time.ParseDuration(c.Cooldown.SoftRate); err != nil {
		return fmt.Errorf("cooldown.soft_rate: %w", err)
	}
	if c.ErrCooldownDur, err = time.ParseDuration(c.Cooldown.ErrCooldown); err != nil {
		return fmt.Errorf("cooldown.err_cooldown: %w", err)
	}
	if c.Cooldown.ErrThresh <= 0 {
		c.Cooldown.ErrThresh = 3
	}
	if c.Upstream.TimeoutSeconds <= 0 {
		c.Upstream.TimeoutSeconds = 120
	}
	if c.DefaultModel == "" {
		c.DefaultModel = "glm-5.2"
	}
	c.WorkMode = strings.ToLower(strings.TrimSpace(c.WorkMode))
	if c.WorkMode == "" {
		c.WorkMode = "auto"
	}
	if c.Listen == "" {
		c.Listen = ":7864"
	}
	if !strings.HasPrefix(c.Listen, ":") && !strings.Contains(c.Listen, ":") {
		c.Listen = ":" + c.Listen
	}
	// CallbackPort：空/未设 → 默认 18080；显式 "0" → 不起回调 server（纯手动粘贴模式）
	if c.CallbackPort == "" {
		c.CallbackPort = "18080"
	}
	return nil
}
