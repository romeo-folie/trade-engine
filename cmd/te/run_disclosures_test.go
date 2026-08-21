package main

import (
	"context"
	"fmt"
	"testing"
	"time"
)

type blockingDisclosureRunner struct {
	started  chan struct{}
	canceled chan struct{}
	release  chan struct{}
	closed   chan struct{}
}

func (r *blockingDisclosureRunner) Run(ctx context.Context) error {
	close(r.started)
	<-ctx.Done()
	close(r.canceled)
	<-r.release
	close(r.closed)
	return nil
}

func TestDisclosureRunnerLifecycleStopWaitsForRunnerCleanup(t *testing.T) {
	runner := &blockingDisclosureRunner{
		started: make(chan struct{}), canceled: make(chan struct{}),
		release: make(chan struct{}), closed: make(chan struct{}),
	}
	lifecycle := startDisclosureRunner(context.Background(), runner, nil)
	select {
	case <-runner.started:
	case <-time.After(time.Second):
		t.Fatal("runner did not start")
	}

	stopped := make(chan struct{})
	go func() {
		lifecycle.Stop()
		close(stopped)
	}()
	select {
	case <-runner.canceled:
	case <-time.After(time.Second):
		t.Fatal("runner context was not canceled")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned before runner cleanup completed")
	case <-time.After(25 * time.Millisecond):
	}

	close(runner.release)
	select {
	case <-runner.closed:
	case <-time.After(time.Second):
		t.Fatal("runner cleanup did not complete")
	}
	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("Stop did not join runner")
	}

	// The command may reach more than one deferred cleanup path. Repeated
	// shutdown is safe and does not wait on a second Run invocation.
	lifecycle.Stop()
}

func TestNilDisclosureRunnerLifecycleStopIsNoop(t *testing.T) {
	if lifecycle := startDisclosureRunner(context.Background(), nil, nil); lifecycle != nil {
		t.Fatalf("nil runner lifecycle = %#v, want nil", lifecycle)
	}
	var lifecycle *disclosureRunnerLifecycle
	lifecycle.Stop()
}

type errorDisclosureRunner struct{ err error }

func (r errorDisclosureRunner) Run(context.Context) error { return r.err }

func TestDisclosureRunnerLifecycleSuppressesWrappedCancellation(t *testing.T) {
	reported := make(chan error, 1)
	lifecycle := startDisclosureRunner(
		context.Background(),
		errorDisclosureRunner{err: fmt.Errorf("runner stopped: %w", context.Canceled)},
		func(err error) { reported <- err },
	)
	lifecycle.Stop()
	select {
	case err := <-reported:
		t.Fatalf("wrapped cancellation was reported: %v", err)
	default:
	}
}
