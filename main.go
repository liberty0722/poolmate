// PoolMate 账号池网关：把本人 WorkBuddy 账号聚合成统一 API 网关 + Web 管理面板。
// 用法：poolmate [-config config.json]
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"poolmate/internal/auth"
	"poolmate/internal/config"
	"poolmate/internal/gateway"
	"poolmate/internal/keys"
	"poolmate/internal/notify"
	"poolmate/internal/panel"
	"poolmate/internal/pool"
	"poolmate/internal/scheduler"
	"poolmate/internal/stats"
	"poolmate/internal/upstream"
)

func main() {
	configPath := flag.String("config", "config.json", "配置文件路径")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	c := cfg.Snapshot()
	abs, _ := filepath.Abs(*configPath)
	log.Printf("[poolmate] 配置: %s", abs)

	// 凭证目录：优先配置，其次可执行文件旁的 auths（绿色包形态）
	authDir := c.AuthDir
	if !filepath.IsAbs(authDir) {
		if exe, err := os.Executable(); err == nil {
			authDir = filepath.Join(filepath.Dir(exe), authDir)
		}
	}
	dataDir := c.DataDir
	if !filepath.IsAbs(dataDir) {
		if exe, err := os.Executable(); err == nil {
			dataDir = filepath.Join(filepath.Dir(exe), dataDir)
		}
	}
	_ = cfg.Update(func(cc *config.Config) { cc.DataDir = dataDir })

	// vault 主密码（仅内存）
	var vaultMu sync.RWMutex
	vaultPass := ""
	getPass := func() string {
		vaultMu.RLock()
		defer vaultMu.RUnlock()
		return vaultPass
	}
	setPass := func(p string) {
		vaultMu.Lock()
		vaultPass = p
		vaultMu.Unlock()
	}

	// 核心组件
	client := upstream.New(c.Gateway.UpstreamTimeout.Duration)
	notifier := notify.New(cfg)
	p := pool.New(cfg, func(e pool.Event) {
		level := "warn"
		if e.Kind == pool.EvAuthFailed {
			level = "critical"
		}
		notifier.Emit(notify.Event{
			Kind: string(e.Kind), Level: level, UID: e.UID,
			Title: fmt.Sprintf("账号「%s」%s", firstNonEmpty(e.Name, e.UID[:min(8, len(e.UID))]), eventText(e.Kind)),
			Body:  e.Detail,
		})
	})
	keyStore, err := keys.Load(filepath.Join(dataDir, "keys.json"))
	if err != nil {
		log.Fatalf("加载密钥库失败: %v", err)
	}
	statStore := stats.New(filepath.Join(dataDir, "request-logs"))

	// 凭证加载与热重载
	var reloadMu sync.Mutex
	reloadAuths := func() {
		reloadMu.Lock()
		defer reloadMu.Unlock()
		auths, errs := auth.LoadDir(authDir, getPass())
		for _, e := range errs {
			log.Printf("[poolmate] 凭证加载警告: %v", e)
		}
		p.SyncAccounts(auths)
	}
	reloadAuths()
	ready, total, _, _ := p.Summary()
	log.Printf("[poolmate] 账号池: %d 个账号（%d 可用）", total, ready)

	// 首次运行且无任何密钥时创建默认密钥
	if len(keyStore.List()) == 0 {
		k := keyStore.Create("默认密钥（全部模型）", nil, 0, 0, 0, nil)
		log.Printf("[poolmate] 已创建默认 API 密钥: %s", k.Key)
	}

	gw := gateway.New(cfg, p, keyStore, statStore, client, notifier)
	sched := scheduler.New(scheduler.Deps{
		Cfg: cfg, Pool: p, Client: client, Notifier: notifier,
		ReloadAuths: reloadAuths,
		AuthDir:     authDir,
		StateFile:   filepath.Join(dataDir, "state.json"),
	})
	pn := panel.New(panel.Deps{
		Cfg: cfg, Pool: p, Keys: keyStore, Stats: statStore, Client: client,
		Notifier: notifier, Scheduler: sched, Gateway: gw,
		AuthDir: authDir, StateFile: filepath.Join(dataDir, "state.json"),
		ReloadAuths: reloadAuths, VaultPassword: getPass, SetVaultPass: setPass,
	})

	mux := http.NewServeMux()
	mux.Handle("/v1/", gw.Routes())
	mux.Handle("/healthz", gw.Routes())
	mux.Handle("/status", gw.Routes())
	mux.Handle("/", pn.Handler())

	srv := &http.Server{
		Addr: c.Listen, Handler: mux,
		ReadHeaderTimeout: 30 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go sched.Start(ctx)

	// 启动后先刷一轮余额与模型目录
	go func() {
		for _, a := range p.List() {
			acc := p.Get(a.UID)
			if acc == nil || acc.Auth == nil {
				continue
			}
			if acc.Auth.NeedsRefresh(10 * time.Minute) {
				if err := client.RefreshToken(acc.Auth); err != nil {
					log.Printf("[poolmate] %s token 刷新失败: %v", a.Name, err)
					continue
				}
			}
			remain, packs, err := client.Balance(acc.Auth)
			p.SetBalance(acc, remain, packs, err)
			if err != nil {
				log.Printf("[poolmate] %s 余额刷新失败: %v", a.Name, err)
			}
		}
		gw.InvalidateCatalog()
	}()

	go func() {
		log.Printf("[poolmate] 网关: http://%s/v1  面板: http://%s", c.Listen, c.Listen)
		if c.AdminKey != "" {
			log.Printf("[poolmate] 面板管理密钥: %s", c.AdminKey)
		}
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("服务退出: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("[poolmate] 正在关闭…")
	p.Save()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutdownCtx)
}

func eventText(k pool.EventKind) string {
	switch k {
	case pool.EvAccountDisabled:
		return "已被禁用"
	case pool.EvBreakerOpen:
		return "触发熔断"
	case pool.EvAuthFailed:
		return "凭证失效"
	}
	return string(k)
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
