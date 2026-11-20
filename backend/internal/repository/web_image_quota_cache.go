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

func webImageBackoffKey(id int64) string { return fmt.Sprintf("web_image:parent:%d:backoff", id) }
func (c *webImageQuotaCache) Get(ctx context.Context, id int64) (*service.WebImageStatus, error) {
	values, err := c.GetMany(ctx, []int64{id})
	return values[id], err
}
func (c *webImageQuotaCache) GetMany(ctx context.Context, ids []int64) (map[int64]*service.WebImageStatus, error) {
	result := make(map[int64]*service.WebImageStatus, len(ids))
	if len(ids) == 0 {
		return result, nil
	}
	keys := make([]string, 0, len(ids)*2)
	for _, id := range ids {
		keys = append(keys, webImageQuotaKey(id), webImageBackoffKey(id))
	}
	values, err := c.rdb.MGet(ctx, keys...).Result()
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		var status *service.WebImageStatus
		if raw, ok := values[2*i].(string); ok {
			status = &service.WebImageStatus{}
			if err := json.Unmarshal([]byte(raw), status); err != nil {
				return nil, err
			}
		}
		if raw, ok := values[2*i+1].(string); ok {
			var backoff service.WebImageStatus
			if err := json.Unmarshal([]byte(raw), &backoff); err != nil {
				return nil, err
			}
			if backoff.CooldownUntil != nil && backoff.CooldownUntil.After(time.Now()) {
				if status == nil {
					status = &service.WebImageStatus{ParentAccountID: id}
				}
				status.CooldownUntil = backoff.CooldownUntil
				status.CooldownReason = backoff.CooldownReason
			}
		}
		result[id] = status
	}
	return result, nil
}
func (c *webImageQuotaCache) Backoff(ctx context.Context, id int64, reason string, until time.Time) error {
	if !until.After(time.Now()) {
		return nil
	}
	raw, err := json.Marshal(service.WebImageStatus{ParentAccountID: id, CooldownUntil: &until, CooldownReason: reason})
	if err != nil {
		return err
	}
	// Preserve a longer concurrent backoff; quota and its probe lease are independent.
	ttl := max(int64(1), int64(time.Until(until).Milliseconds()))
	return c.rdb.Eval(ctx, `local ttl=redis.call('PTTL',KEYS[1]);if ttl<tonumber(ARGV[2]) then redis.call('SET',KEYS[1],ARGV[1],'PX',ARGV[2]) end;return 1`, []string{webImageBackoffKey(id)}, raw, ttl).Err()
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
