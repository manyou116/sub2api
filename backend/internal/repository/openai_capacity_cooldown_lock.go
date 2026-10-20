package repository

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
)

var capacityUnlockScript = redis.NewScript(`
if redis.call('GET',KEYS[1]) == ARGV[1] then return redis.call('DEL',KEYS[1]) end
return 0
`)

// Only DB projection and manual recovery use this lock; ordinary forwarding
// and Lua admission stay on their existing atomic path.
func (c *tempUnschedCache) LockOpenAICapacity(ctx context.Context, accountID int64) (func(), error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	key, owner := capacityKeys(accountID, "")[0]+":control_lock", uuid.NewString()
	for {
		locked, err := c.rdb.SetNX(ctx, key, owner, 10*time.Second).Result()
		if err != nil {
			return nil, err
		}
		if locked {
			return func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				_ = capacityUnlockScript.Run(cleanup, c.rdb, []string{key}, owner).Err()
			}, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
