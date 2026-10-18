package repository

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/redis/go-redis/v9"
)

const (
	oauthTokenKeyPrefix       = "oauth:token:"
	oauthRefreshLockKeyPrefix = "oauth:refresh_lock:"
)

type geminiTokenCache struct {
	rdb *redis.Client
}

func NewGeminiTokenCache(rdb *redis.Client) service.GeminiTokenCache {
	return &geminiTokenCache{rdb: rdb}
}

func (c *geminiTokenCache) GetAccessToken(ctx context.Context, cacheKey string) (string, error) {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Get(ctx, key).Result()
}

func (c *geminiTokenCache) SetAccessToken(ctx context.Context, cacheKey string, token string, ttl time.Duration) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Set(ctx, key, token, ttl).Err()
}

func (c *geminiTokenCache) DeleteAccessToken(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthTokenKeyPrefix, cacheKey)
	return c.rdb.Del(ctx, key).Err()
}

func (c *geminiTokenCache) AcquireRefreshLock(ctx context.Context, cacheKey string, ttl time.Duration) (bool, error) {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.SetNX(ctx, key, 1, ttl).Result()
}

func (c *geminiTokenCache) ReleaseRefreshLock(ctx context.Context, cacheKey string) error {
	key := fmt.Sprintf("%s%s", oauthRefreshLockKeyPrefix, cacheKey)
	return c.rdb.Del(ctx, key).Err()
}

// AcquireRefreshLease 使用随机 owner 占用同一个刷新键，返回空 owner 表示已被占用。
func (c *geminiTokenCache) AcquireRefreshLease(ctx context.Context, cacheKey string, ttl time.Duration) (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	owner := hex.EncodeToString(raw[:])
	acquired, err := c.rdb.SetNX(ctx, oauthRefreshLockKeyPrefix+cacheKey, owner, ttl).Result()
	if err != nil || !acquired {
		return "", err
	}
	return owner, nil
}

// OwnsRefreshLease 写凭据前确认租约，过期执行者不得继续提交。
func (c *geminiTokenCache) OwnsRefreshLease(ctx context.Context, cacheKey, owner string) (bool, error) {
	value, err := c.rdb.Get(ctx, oauthRefreshLockKeyPrefix+cacheKey).Result()
	if err == redis.Nil {
		return false, nil
	}
	return value == owner, err
}

// ReleaseRefreshLease 只释放自己的 owner，不能删除过期后被新执行者取得的锁。
func (c *geminiTokenCache) ReleaseRefreshLease(ctx context.Context, cacheKey, owner string) error {
	return c.rdb.Eval(ctx, `if redis.call('GET', KEYS[1]) == ARGV[1] then return redis.call('DEL', KEYS[1]) end return 0`,
		[]string{oauthRefreshLockKeyPrefix + cacheKey}, owner).Err()
}
