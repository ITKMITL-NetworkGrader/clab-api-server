package clab

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
)

func TestRetryDumpInterrupted(t *testing.T) {
	ctx := context.Background()
	other := errors.New("lab not found")
	// containerlab logs and returns the error as text in some paths, so the wrapped form must match too.
	wrapped := fmt.Errorf("destroy failed: %s", netlink.ErrDumpInterrupted.Error())

	cases := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{"succeeds after interrupted dumps", []error{netlink.ErrDumpInterrupted, wrapped, nil}, 3, nil},
		{"other error is not retried", []error{other}, 1, other},
		{"gives up after the last attempt", []error{wrapped, wrapped, wrapped}, 3, wrapped},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			err := retryDumpInterrupted(ctx, 3, time.Millisecond, func() error {
				calls++
				return tc.results[calls-1]
			})
			if calls != tc.wantCalls || !errors.Is(err, tc.wantErr) && err != tc.wantErr {
				t.Fatalf("calls=%d err=%v, want calls=%d err=%v", calls, err, tc.wantCalls, tc.wantErr)
			}
		})
	}

	t.Run("stops when the context ends", func(t *testing.T) {
		cancelled, cancel := context.WithCancel(ctx)
		cancel()
		calls := 0
		err := retryDumpInterrupted(cancelled, 5, time.Hour, func() error { calls++; return netlink.ErrDumpInterrupted })
		if calls != 1 || !errors.Is(err, netlink.ErrDumpInterrupted) {
			t.Fatalf("calls=%d err=%v", calls, err)
		}
	})
}
