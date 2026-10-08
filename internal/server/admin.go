// 管理面板（/admin）：只读查询，无鉴权（本地面板）；CLI 操作留待开发。
package server

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"trae2api/internal/pool"
	"trae2api/internal/upstream"
)

//go:embed admin.html
var adminPageHTML []byte

// adminKeyPlaceholder admin.html 中 API Key 注入占位符（JS 变量）。
const adminKeyPlaceholder = "__TW2A_ADMIN_KEY__"

// adminPage 返回内嵌 HTML 面板（深色简洁风，无外部依赖）。
// 打开时把当前 API Key 注入页面，免去手动粘贴（本地面板，localhost 自用）。
func (h *Handler) adminPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	key, _ := json.Marshal(h.cfg.APIKey) // JSON 字符串（含引号），安全注入 JS
	html := bytes.Replace(adminPageHTML, []byte(adminKeyPlaceholder), key, 1)
	_, _ = w.Write(html)
}

// adminOverview 流程概览（只读，无鉴权）：账号数 / 模型表来源 / 是否就绪，
// 供面板顶部「流程状态条」渲染。
func (h *Handler) adminOverview(w http.ResponseWriter, r *http.Request) {
	st := h.cfg.Pool.List()
	total, healthy := len(st), 0
	for _, s := range st {
		if s.Enabled && !s.Disabled && !s.Cooling {
			healthy++
		}
	}
	modelN, modelSrc := h.ModelSummary()
	writeJSON(w, http.StatusOK, map[string]any{
		"accounts_total":   total,
		"accounts_healthy": healthy,
		"models_total":     modelN,
		"models_source":    modelSrc, // "dynamic" | "static"
		"ready":            healthy > 0,
	})
}

// adminCredits 查询全部账号的实时额度 + 签到状态（并发拉取上游）。
func (h *Handler) adminCredits(w http.ResponseWriter, r *http.Request) {
	type acct struct {
		UID            string  `json:"uid"`
		Nickname       string  `json:"nickname"`
		Remain         int64   `json:"remain"`
		Limit          int64   `json:"limit"`
		Used           int64   `json:"used"`
		Packs          int     `json:"packs"`
		WorkCredits    float64 `json:"work_credits"`
		CheckedIn      bool    `json:"checked_in"`
		CheckinCredits int64   `json:"checkin_credits"`
		CheckinEnable  bool    `json:"checkin_enable"`
		Cooling        bool    `json:"cooling"`
		WorkCooling    bool    `json:"work_cooling"`
		Disabled       bool    `json:"disabled"`
		Error          string  `json:"error,omitempty"`
	}

	st := h.cfg.Pool.List()
	out := make([]acct, len(st))
	var wg sync.WaitGroup
	for i, s := range st {
		wg.Add(1)
		go func(i int, s pool.Status) {
			defer wg.Done()
			a := h.cfg.Pool.AuthByUID(s.UID)
			if a == nil {
				out[i] = acct{UID: s.UID, Nickname: s.Nickname, Error: "no auth found"}
				return
			}
			var ac acct
			ac.UID = s.UID
			ac.Nickname = s.Nickname
			ac.Cooling = s.Cooling
			ac.WorkCooling = s.WorkCooling
			ac.WorkCredits = s.WorkCredits
			ac.Disabled = s.Disabled
			remain, limit, used, packs, err := h.cfg.Upstream.EntUsage(a)
			if err != nil {
				ac.Error = "ent_usage: " + err.Error()
			} else {
				ac.Remain, ac.Limit, ac.Used, ac.Packs = remain, limit, used, packs
			}
			checkedIn, credits, enable, cerr := h.cfg.Upstream.CheckinStatus(a)
			if cerr != nil {
				if ac.Error != "" {
					ac.Error += "; "
				}
				ac.Error += "checkin: " + cerr.Error()
			} else {
				ac.CheckedIn, ac.CheckinCredits, ac.CheckinEnable = checkedIn, credits, enable
			}
			if h.cfg.WorkClient != nil && h.cfg.WorkMode != upstream.WorkModeDisabled {
				if snap, werr := h.cfg.WorkClient.ProbeCredits(r.Context(), a); werr == nil {
					ac.WorkCredits = snap.WorkCredits
					h.cfg.Pool.SetWorkCredits(s.UID, snap.WorkCredits)
				}
			}
			out[i] = ac
		}(i, s)
	}
	wg.Wait()

	writeJSON(w, http.StatusOK, map[string]any{
		"fetched_at": time.Now().Format("2006-01-02 15:04:05"),
		"accounts":   out,
	})
}
