package rate

import (
	"context"
	"fmt"
	"time"

	redis "github.com/redis/go-redis/v9"
)

type Limiter struct {
	rdb *redis.Client
}

func New(addr, password string) *Limiter {
	return &Limiter{rdb: redis.NewClient(&redis.Options{Addr: addr, Password: password})}
}

func (l *Limiter) Allow(key string, perMinute, burst int) (bool, error) {
	if perMinute <= 0 { return true, nil }
	ctx := context.Background()
	windowKey := "rl:" + key + ":" + time.Now().UTC().Format("200601021504") // minute window
	max := int64(perMinute + burst)
	val, err := l.rdb.Incr(ctx, windowKey).Result()
	if err != nil { return false, fmt.Errorf("redis incr: %w", err) }
	if val == 1 {
		l.rdb.Expire(ctx, windowKey, 70*time.Second)
	}
	return val <= max, nil
}
