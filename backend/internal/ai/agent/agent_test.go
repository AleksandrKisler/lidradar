package agent_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"lidradar/backend/internal/ai/agent"
	"lidradar/backend/internal/ai/domain"
)

type recordingCloud struct {
	mu             sync.Mutex
	claimed        bool
	heartbeatSlots []int
	completed      chan struct{}
	failed         chan string
}

func (cloud *recordingCloud) Heartbeat(_ context.Context, status domain.NodeStatus, _ string, slots int) error {
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if status == domain.NodeReady {
		cloud.heartbeatSlots = append(cloud.heartbeatSlots, slots)
	}
	return nil
}

func (cloud *recordingCloud) Claim(context.Context) (domain.Job, bool, error) {
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	if cloud.claimed {
		return domain.Job{}, false, nil
	}
	cloud.claimed = true
	return domain.Job{ID: "job", Prompt: "private prompt"}, true, nil
}

func (*recordingCloud) Started(context.Context, string) (string, error) { return "run", nil }

func (cloud *recordingCloud) Complete(context.Context, string, string, string) error {
	select {
	case <-cloud.completed:
	default:
		close(cloud.completed)
	}
	return nil
}

func (cloud *recordingCloud) Failed(_ context.Context, _, _, errorCode string) error {
	if cloud.failed != nil {
		select {
		case cloud.failed <- errorCode:
		default:
		}
	}
	return nil
}

type slowProvider struct{ delay time.Duration }

func (slowProvider) Ready(context.Context) error { return nil }
func (provider slowProvider) Infer(ctx context.Context, _ string) (string, error) {
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(provider.delay):
		return `{}`, nil
	}
}

type failingProvider struct{ err error }

func (failingProvider) Ready(context.Context) error { return nil }
func (provider failingProvider) Infer(context.Context, string) (string, error) {
	return "", provider.err
}

func TestRunnerRenewsHeartbeatWhileInferenceIsBusy(t *testing.T) {
	cloud := &recordingCloud{completed: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (agent.Runner{
			Cloud: cloud, Provider: slowProvider{delay: 45 * time.Millisecond},
			ModelVersion: "test-model", PollInterval: 5 * time.Millisecond,
			HeartbeatInterval: 5 * time.Millisecond,
		}).Run(ctx)
	}()
	select {
	case <-cloud.completed:
	case <-time.After(time.Second):
		t.Fatal("agent did not complete a job")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("agent did not stop")
	}
	cloud.mu.Lock()
	defer cloud.mu.Unlock()
	busyHeartbeats := 0
	for _, slots := range cloud.heartbeatSlots {
		if slots == 0 {
			busyHeartbeats++
		}
	}
	if busyHeartbeats < 2 {
		t.Fatalf("busy heartbeats = %d, all = %#v", busyHeartbeats, cloud.heartbeatSlots)
	}
}

func TestRunnerReportsProviderTimeoutAsFailedRun(t *testing.T) {
	cloud := &recordingCloud{completed: make(chan struct{}), failed: make(chan string, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- (agent.Runner{
			Cloud: cloud, Provider: failingProvider{err: context.DeadlineExceeded},
			ModelVersion: "test-model", PollInterval: time.Millisecond,
			HeartbeatInterval: 5 * time.Millisecond,
		}).Run(ctx)
	}()

	select {
	case code := <-cloud.failed:
		if code != "PROVIDER_INFERENCE_FAILED" {
			t.Fatalf("код ошибки = %q", code)
		}
	case <-time.After(time.Second):
		t.Fatal("агент не сообщил Cloud Core об ошибке провайдера")
	}
	select {
	case <-cloud.completed:
		t.Fatal("ошибочный результат был отмечен как успешно завершённый")
	default:
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("завершение агента = %v", err)
	}
}
