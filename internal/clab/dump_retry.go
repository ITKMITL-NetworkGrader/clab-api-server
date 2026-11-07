package clab

import (
	"context"
	"errors"
	"math/rand/v2"
	"strings"
	"time"

	"github.com/charmbracelet/log"
	"github.com/vishvananda/netlink"
)

// NTG-439: while one lab's destroy removes its veths, another destroy's netlink link dump is
// interrupted (NLM_F_DUMP_INTR) and containerlab fails the whole destroy before removing anything
// ("results may be incomplete or inconsistent"). The kernel's contract is to retry the dump.
const (
	destroyDumpAttempts = 6
	destroyDumpWait     = 300 * time.Millisecond
)

func isDumpInterrupted(err error) bool {
	return err != nil && (errors.Is(err, netlink.ErrDumpInterrupted) ||
		strings.Contains(err.Error(), netlink.ErrDumpInterrupted.Error()))
}

// retryDumpInterrupted runs fn again, with a growing jittered wait, while it fails only because a
// netlink dump was interrupted. Any other result is returned at once.
func retryDumpInterrupted(ctx context.Context, attempts int, wait time.Duration, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		if !isDumpInterrupted(err) || attempt >= attempts {
			return err
		}
		log.Warn("netlink dump interrupted, retrying", "attempt", attempt, "of", attempts)
		select {
		case <-ctx.Done():
			return err
		case <-time.After(wait*time.Duration(attempt) + rand.N(wait)):
		}
	}
}
