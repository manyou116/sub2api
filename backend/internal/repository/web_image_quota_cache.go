package repository

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/redis/go-redis/v9"
)

const webImageQuotaTTLSeconds = 600

type webImageQuotaCache struct{ rdb *redis.Client }

func NewWebImageQuotaCache(rdb *redis.Client) service.WebImageQuotaCache {
	return &webImageQuotaCache{rdb: rdb}
}
func webImageQuotaKey(id int64) string { return fmt.Sprintf("web_image:parent:%d:quota", id) }
func webImageProbeKey(id int64) string { return fmt.Sprintf("web_image:parent:%d:probe", id) }

func (c *webImageQuotaCache) Get(ctx context.Context, parentID int64) (*service.WebImageStatus, error) {
	raw, err := c.rdb.Get(ctx, webImageQuotaKey(parentID)).Bytes()
	if err == redis.Nil {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var status service.WebImageStatus
	if err = json.Unmarshal(raw, &status); err != nil {
		return nil, err
	}
	return &status, nil
}
func (c *webImageQuotaCache) AcquireProbe(ctx context.Context, parentID int64, owner string, ttl time.Duration) (bool, error) {
	return c.rdb.SetNX(ctx, webImageProbeKey(parentID), owner, ttl).Result()
}
func (c *webImageQuotaCache) PublishProbe(ctx context.Context, parentID int64, owner string, status *service.WebImageStatus) (bool, error) {
	raw, err := json.Marshal(status)
	if err != nil {
		return false, err
	}
	written, err := c.rdb.Eval(ctx, `if redis.call('GET',KEYS[1])~=ARGV[1] then return 0 end redis.call('SET',KEYS[2],ARGV[2],'EX',ARGV[3]);return 1`, []string{webImageProbeKey(parentID), webImageQuotaKey(parentID)}, owner, raw, webImageQuotaTTLSeconds).Int()
	return written == 1, err
}
func (c *webImageQuotaCache) ReleaseProbe(ctx context.Context, parentID int64, owner string) error {
	return c.rdb.Eval(ctx, `if redis.call('GET',KEYS[1])==ARGV[1] then return redis.call('DEL',KEYS[1]) end return 0`, []string{webImageProbeKey(parentID)}, owner).Err()
}
func (c *webImageQuotaCache) Exhaust(ctx context.Context, parentID int64, status *service.WebImageStatus) error {
	raw, err := json.Marshal(status)
	if err != nil {
		return err
	}
	return c.rdb.Eval(ctx, `redis.call('DEL',KEYS[1]);redis.call('SET',KEYS[2],ARGV[1],'EX',ARGV[2]);return 1`, []string{webImageProbeKey(parentID), webImageQuotaKey(parentID)}, raw, webImageQuotaTTLSeconds).Err()
}
func (c *webImageQuotaCache) Delete(ctx context.Context, parentID int64) error {
	return c.rdb.Del(ctx, webImageQuotaKey(parentID)).Err()
}
