// main.go trae2api 入口：加载配置 → 构建 pool → 起 HTTP 服务。
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"trae2api/internal/auth"
	"trae2api/internal/pool"
	"trae2api/internal/scheduler"
	"trae2api/internal/server"
	"trae2api/internal/upstream"
)

func main() {
	cfgPath := flag.String("config", "config.json", "path to config json")
	flag.Parse()

	cfg, err := Load(*cfgPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	up := upstream.New()
	up.HTTP.Timeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	// 流式客户端无总超时，仅用首字节兜底（时长由 SSE 流本身决定）。
	if tr, ok := up.StreamHTTP.Transport.(*http.Transport); ok {
		tr.ResponseHeaderTimeout = time.Duration(cfg.Upstream.TimeoutSeconds) * time.Second
	}

	auths, err := auth.LoadDir(cfg.AuthDir)
	if err != nil {
		log.Fatalf("load auths: %v", err)
	}
	// auths/ 为空时，尝试自动导入本机 TRAE 客户端登录（对标 workbuddy2api）。
	if len(auths) == 0 {
		if a := importLocalLogin(up, cfg.AuthDir); a != nil {
			auths = append(auths, a)
		}
	}
	log.Printf("loaded %d account(s) from %s", len(auths), cfg.AuthDir)

	p := pool.New(cfg.StateFile)
	p.SyncToDir(auths) // 对齐：剔除 state.json 中已删除 auth 文件的幽灵账号

	workCfg := upstream.DefaultWorkClientConfig()
	if cfg.WorkHost != "" {
		workCfg.Host = cfg.WorkHost
	}
	if cfg.WorkBridgeURL != "" {
		workCfg.BridgeURL = cfg.WorkBridgeURL
	}
	if cfg.WorkBridgeToken != "" {
		workCfg.BridgeToken = cfg.WorkBridgeToken
	}
	if cfg.WorkMode != "" {
		workCfg.Mode = upstream.WorkMode(cfg.WorkMode)
	}
	workClient := upstream.NewWorkClient(workCfg)

	sch := scheduler.New(scheduler.Config{
		Pool:         p,
		Upstream:     up,
		CheckinHour:  cfg.Schedule.CheckinHour,
		RefreshHours: cfg.Schedule.RefreshHours,
		RefreshSkew:  24 * time.Hour,
	})

	h := server.NewHandler(server.Config{
		Pool:            p,
		Upstream:        up,
		WorkClient:      workClient,
		WorkMode:        upstream.WorkMode(cfg.WorkMode),
		APIKey:          cfg.APIKey,
		AuthDir:         cfg.AuthDir,
		PlanCooldown:    cfg.PlanCreditDur,
		SoftCooldown:    cfg.SoftRateDur,
		ErrThreshold:    cfg.Cooldown.ErrThresh,
		ErrCooldown:     cfg.ErrCooldownDur,
		DefaultModel:    cfg.DefaultModel,
		WorkBridgeURL:   cfg.WorkBridgeURL,
		WorkBridgeToken: cfg.WorkBridgeToken,
	})

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sch.Run(ctx)

	// 启动时在后台探测所有可用账号的 work_credits 积分状态
	if workClient.Mode() != upstream.WorkModeDisabled {
		go func() {
			probeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			for _, a := range auths {
				snap, err := workClient.ProbeCredits(probeCtx, a)
				if err == nil {
					p.SetWorkCredits(a.UID, snap.WorkCredits)
					log.Printf("[WorkPool] account %s (%s) work_credits: %.4f", a.UID, a.Nickname, snap.WorkCredits)
				}
			}
		}()
	}

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           h,
		ReadHeaderTimeout: 30 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	// 第二个 http.Server：监听 CallbackPort（默认 18080），只处理 /authorize 回调。
	// 复用同一 Handler（/authorize 已在主 mux 注册）。
	// cfg.CallbackPort == "0" 时不启动（纯手动粘贴模式）。
	var cbSrv *http.Server
	if cfg.CallbackPort != "" && cfg.CallbackPort != "0" {
		cbSrv = &http.Server{
			Addr:              ":" + cfg.CallbackPort,
			Handler:           h,
			ReadHeaderTimeout: 30 * time.Second,
		}
		go func() {
			<-ctx.Done()
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = cbSrv.Shutdown(sc)
		}()
		go func() {
			log.Printf("trae2api callback server on :%s (TRAE login /authorize)", cfg.CallbackPort)
			if err := cbSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				// 端口被占用（login.sh / 旧实例）不致命，降级为手动粘贴模式。
				log.Printf("callback server (:%s) failed: %v — web 登录降级为手动粘贴回调链接", cfg.CallbackPort, err)
			}
		}()
	}

	if cfg.keyGenerated {
		log.Printf("首次运行：已自动生成 API Key 并写入 %s", *cfgPath)
	}
	log.Printf("trae2api listening on %s", cfg.Listen)
	log.Printf("API Key: %s  (请求头 Authorization: Bearer <key>)", cfg.APIKey)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatalf("http: %v", err)
	}
	log.Printf("bye")
}

// importLocalLogin 在 auths/ 为空时尝试自动导入本机 TRAE 客户端登录。
// 读取本机 trae-jwt-token → GetUserInfo 换权威 uid → 落盘 auths/ 并返回。
// 任一步失败都返回 nil（仅记日志），不影响服务启动。
func importLocalLogin(up *upstream.Client, authDir string) *auth.Auth {
	lt, err := auth.DiscoverLocalToken()
	if err != nil {
		log.Printf("[LocalImport] 发现本机登录失败: %v", err)
		return nil
	}
	if lt == nil {
		return nil
	}
	machineID, err := auth.NewMachineID()
	if err != nil {
		log.Printf("[LocalImport] 生成 machine id 失败: %v", err)
		return nil
	}
	a := &auth.Auth{
		AccessToken: lt.AccessToken,
		Domain:      "trae.cn",
		ApiHost:     "https://api.trae.com.cn",
		MachineID:   machineID,
		ExpiresAt:   lt.ExpiresAt,
	}
	if _, derr := a.EnsureCheckinDeviceID(); derr != nil {
		log.Printf("[LocalImport] 生成 device id 失败: %v", derr)
		return nil
	}
	// 用 token 换权威 uid / nickname（失败则回退 JWT payload 里的 uid）。
	if uid, nick, ent, gerr := up.GetUserInfo(a); gerr == nil && uid != "" {
		a.UID = uid
		a.Nickname = nick
		a.EnterpriseID = ent
	} else {
		if gerr != nil {
			log.Printf("[LocalImport] GetUserInfo: %v", gerr)
		}
		a.UID = lt.UID
	}
	if a.UID == "" {
		log.Printf("[LocalImport] 已找到 %s，但无法解析 uid，跳过自动导入", lt.Path)
		return nil
	}
	// 落盘前校验凭证是否被 SOLO 通道接受（GetUserInfo 只过 oauth 主机，不足以证明可用）。
	if _, ferr := up.FetchModels(a); ferr != nil {
		log.Printf("[LocalImport] 本机 token 不被上游接受（%v），跳过自动导入；请改用 Web 登录导入", ferr)
		return nil
	}
	if err := os.MkdirAll(authDir, 0o755); err != nil {
		log.Printf("[LocalImport] 创建 %s 失败: %v", authDir, err)
		return nil
	}
	a.FilePath = auth.FilePathFor(authDir, a.UID)
	if err := a.SaveAtomic(); err != nil {
		log.Printf("[LocalImport] 落盘失败: %v", err)
		return nil
	}
	log.Printf("[LocalImport] 已从本机登录自动导入账号 uid=%s (%s)，来源 %s", a.UID, a.Nickname, lt.Path)
	return a
}
