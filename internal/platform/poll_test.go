package platform

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPoll(t *testing.T) {
	calls := 0
	value, err := Poll(context.Background(), func(context.Context) (int, bool, time.Duration, error) {
		calls++
		return calls, calls == 3, time.Millisecond, nil
	})
	if err != nil || value != 3 || calls != 3 {
		t.Fatal(value, calls, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = Poll(ctx, func(context.Context) (int, bool, time.Duration, error) {
		t.Fatal("read after cancellation")
		return 0, true, 0, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	want := errors.New("read failed")
	_, err = Poll(context.Background(), func(context.Context) (int, bool, time.Duration, error) { return 0, false, 0, want })
	if err != want {
		t.Fatal(err)
	}
	_, err = Poll(context.Background(), func(context.Context) (int, bool, time.Duration, error) { return 0, false, 0, nil })
	if err == nil {
		t.Fatal("accepted zero poll interval")
	}
	ctx, cancel = context.WithCancel(context.Background())
	defer cancel()
	_, err = Poll(ctx, func(context.Context) (int, bool, time.Duration, error) { cancel(); return 1, true, 0, nil })
	if !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	ctx, stop := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer stop()
	calls = 0
	_, err = Poll(ctx, func(context.Context) (int, bool, time.Duration, error) { calls++; return 0, false, time.Hour, nil })
	if !errors.Is(err, context.DeadlineExceeded) || calls != 1 {
		t.Fatal(calls, err)
	}
}

func TestWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Wait(ctx, time.Hour); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if err := Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
}
