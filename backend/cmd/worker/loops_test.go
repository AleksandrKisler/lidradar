package main

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type recordedLog struct {
	mu     sync.Mutex
	events []string
}

func (l *recordedLog) Error(_ string, arguments ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := 0; i+1 < len(arguments); i += 2 {
		if arguments[i] == "event" {
			l.events = append(l.events, arguments[i+1].(string))
		}
	}
}

func (l *recordedLog) seen() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.events...)
}

var fastPace = pacing{idle: 30 * time.Millisecond, failure: 60 * time.Millisecond}

func TestALoopThatBlocksDoesNotDelayTheOthers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	var fast atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		runLoops(ctx, &recordedLog{}, fastPace,
			// доставка в Telegram, которая отвечает по таймауту
			loop{event: "slow", step: func(ctx context.Context) (bool, error) {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return true, nil
			}},
			loop{event: "fast", step: func(context.Context) (bool, error) { fast.Add(1); time.Sleep(time.Millisecond); return true, nil }},
		)
	}()
	time.Sleep(300 * time.Millisecond)
	if got := fast.Load(); got < 50 {
		t.Fatalf("the jobs loop made %d steps in 300 ms while the delivery loop was blocked; independent loops must not wait for each other", got)
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runLoops must return after the context is cancelled and every loop has stopped")
	}
}

func TestEachLoopPausesAccordingToItsOwnOutcome(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type result struct {
		progressed bool
		err        error
	}
	script := []result{{true, nil}, {true, nil}, {false, nil}, {false, errors.New("boom")}, {true, nil}}
	var mu sync.Mutex
	var at []time.Time
	step := func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) > len(script) {
			cancel()
			return true, nil
		}
		next := script[len(at)-1]
		return next.progressed, next.err
	}
	runLoops(ctx, &recordedLog{}, fastPace, loop{event: "x", step: step})
	mu.Lock()
	defer mu.Unlock()
	if len(at) != len(script)+1 {
		t.Fatalf("steps: %d", len(at))
	}
	gap := func(i int) time.Duration { return at[i+1].Sub(at[i]) }
	if gap(0) >= fastPace.idle {
		t.Errorf("after progress the next step starts at once, gap %v", gap(0))
	}
	if gap(1) >= fastPace.idle {
		t.Errorf("after progress the next step starts at once, gap %v", gap(1))
	}
	if gap(2) < fastPace.idle {
		t.Errorf("an empty queue waits the idle pause, gap %v", gap(2))
	}
	if gap(3) < fastPace.failure {
		t.Errorf("an error waits the failure pause, gap %v", gap(3))
	}
}

func TestErrorsAreLoggedByTheLoopThatFailedAndNotAfterShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	logger := &recordedLog{}
	var calls atomic.Int32
	runLoops(ctx, logger, fastPace,
		loop{event: "job.failed", message: "m", step: func(context.Context) (bool, error) {
			if calls.Add(1) == 1 {
				return false, errors.New("one real failure")
			}
			// ошибка из-за остановки процесса не журналируется как сбой задания
			cancel()
			return false, context.Canceled
		}},
		loop{event: "other", message: "m", step: func(ctx context.Context) (bool, error) { <-ctx.Done(); return false, nil }},
	)
	if got := logger.seen(); len(got) != 1 || got[0] != "job.failed" {
		t.Fatalf("logged events: %v", got)
	}
}

func TestAFixedIntervalLoopRunsAtOnceAndThenOnlyEveryInterval(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	var at []time.Time
	runLoops(ctx, &recordedLog{}, fastPace, loop{event: "diagnostics.failed", every: 80 * time.Millisecond, step: func(context.Context) (bool, error) {
		mu.Lock()
		defer mu.Unlock()
		at = append(at, time.Now())
		if len(at) == 3 {
			cancel()
		}
		// даже «есть что делать» и ошибка не ускоряют диагностику
		if len(at)%2 == 1 {
			return true, nil
		}
		return false, errors.New("boom")
	}})
	mu.Lock()
	defer mu.Unlock()
	if len(at) != 3 {
		t.Fatalf("steps: %d", len(at))
	}
	for i := 0; i < 2; i++ {
		if gap := at[i+1].Sub(at[i]); gap < 80*time.Millisecond {
			t.Errorf("the interval loop waits its own interval after every step, gap %v", gap)
		}
	}
}
