package openaiimages

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// BootProbeOptions 启动 probe 配置。
type BootProbeOptions struct {
	Workers int           // 默认 5
	Timeout time.Duration // 单次 probe 超时；默认 30s
	Logger  *slog.Logger
}

// BootProbe 在程序启动后并发对所有 OAuth ChatGPT 账号执行一次 RefreshAccount，
// 用于快速恢复重启前的限流 / 配额 / 账号类型 / 邮箱缓存。
//
// 用法（伪代码）：
//
//	pool := openaiimages.NewImagePool(listAccounts, probe)
//	go openaiimages.BootProbe(ctx, probe, listAccounts, openaiimages.BootProbeOptions{})
//
// 不阻塞主流程；失败的账号写日志即可。
func BootProbe(ctx context.Context, probe *AccountProbe, list PoolListAccounts, opt BootProbeOptions) {
	if probe == nil || list == nil {
		return
	}
	if opt.Workers <= 0 {
		opt.Workers = 5
	}
	if opt.Timeout <= 0 {
		opt.Timeout = 30 * time.Second
	}
	logger := opt.Logger
	if logger == nil {
		logger = slog.Default()
	}

	accounts, err := list(ctx, PoolFilter{Driver: "web", AuthMode: "oauth"})
	if err != nil {
		logger.Warn("openai-image boot-probe list failed", slog.String("err", err.Error()))
		return
	}
	if len(accounts) == 0 {
		return
	}

	jobs := make(chan PoolAccount)
	var wg sync.WaitGroup
	for i := 0; i < opt.Workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for acc := range jobs {
				probeOne(ctx, probe, acc, opt.Timeout, logger)
			}
		}()
	}
	for _, a := range accounts {
		if a.AccessToken == "" {
			continue
		}
		jobs <- a
	}
	close(jobs)
	wg.Wait()
	logger.Info("openai-image boot-probe done", slog.Int("count", len(accounts)))
}

func probeOne(parent context.Context, probe *AccountProbe, acc PoolAccount, timeout time.Duration, logger *slog.Logger) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	now := time.Now()
	if !ShouldProbeNow(acc.ID, now) {
		return
	}
	res, err := probe.RefreshAccount(ctx, ProbeAccount{
		ID:          acc.ID,
		AccessToken: acc.AccessToken,
		ProxyURL:    acc.ProxyURL,
	})
	if err != nil {
		logger.Warn("openai-image boot-probe one failed",
			slog.Int64("account_id", acc.ID),
			slog.String("err", err.Error()))
		return
	}
	logger.Debug("openai-image boot-probe one ok",
		slog.Int64("account_id", acc.ID),
		slog.String("plan", res.AccountPlan),
		slog.Int("quota_remaining", res.QuotaRemaining))
}
