package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"
)

const (
	// schedulerInterval: how often the worker looks for sources that need a collection.
	schedulerInterval = time.Hour
	// schedulerFirstDelay: the first look comes this long after start, not
	// at once, so a crash-looping worker does not request on every restart.
	schedulerFirstDelay = time.Minute
)

// schedulerEnabled parses SCHEDULER_ENABLED: empty means true; true, false,
// 1 and 0 are accepted, anything else is an error.
func schedulerEnabled(v string) (bool, error) {
	switch v {
	case "", "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("SCHEDULER_ENABLED must be true, false, 1 or 0, got %q", v)
}

// startScheduler runs tick after first and then every interval until ctx is
// cancelled, in the background. Tick errors are logged and never stop the
// loop. The returned channel is closed when the loop has ended (at once when
// disabled, in which case tick is never called).
func startScheduler(ctx context.Context, enabled bool, tick func(context.Context) ([]string, error), first, interval time.Duration) <-chan struct{} {
	done := make(chan struct{})
	if !enabled {
		slog.Info("scheduler disabled (SCHEDULER_ENABLED=false)")
		close(done)
		return done
	}
	go func() {
		defer close(done)
		timer := time.NewTimer(first)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			requested, err := tick(ctx)
			for _, key := range requested {
				slog.Info("collection requested by scheduler", "source", key)
			}
			if err != nil && ctx.Err() == nil {
				slog.Error("scheduler tick failed", "error", err)
			}
			timer.Reset(interval)
		}
	}()
	return done
}
