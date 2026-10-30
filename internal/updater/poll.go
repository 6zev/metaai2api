package updater

import (
	"context"
	"time"
)

// Poll runs fn once immediately and then every interval until ctx is
// cancelled. A non-positive interval disables the poller entirely, so a
// configuration that leaves updater.interval unset never spins. fn runs on the
// calling goroutine, so callers that need it detached spawn a goroutine.
func Poll(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	if interval <= 0 || fn == nil {
		return
	}
	fn(ctx)

	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}
