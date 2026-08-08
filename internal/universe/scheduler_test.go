package universe

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/romeo-folie/trade-engine/internal/config"
	"github.com/romeo-folie/trade-engine/internal/engine"
	"github.com/romeo-folie/trade-engine/internal/store"
)

type fakeSessionBuilder struct {
	run func(context.Context, engine.USMarketSession) (*store.SwingUniverseVersion, error)
}

func (f fakeSessionBuilder) RunSession(
	ctx context.Context, session engine.USMarketSession,
) (*store.SwingUniverseVersion, error) {
	return f.run(ctx, session)
}

type fakeApprovedJob struct {
	name string
	run  func(context.Context, engine.USMarketSession, *store.SwingUniverseVersion, time.Time) error
}

func (f fakeApprovedJob) Name() string { return f.name }

func (f fakeApprovedJob) RunSessionWithUniverse(
	ctx context.Context, session engine.USMarketSession,
	version *store.SwingUniverseVersion, decisionAt time.Time,
) error {
	return f.run(ctx, session, version, decisionAt)
}

func TestSessionSchedulerBuildsBeforeExactUniverseHandoff(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 8, 7, 17, 0, 0, 0, location)
	session := engine.MostRecentCompletedMarketSession(now, 30*time.Minute)
	completedAt := now.UTC().Add(-time.Minute)
	version := &store.SwingUniverseVersion{
		ID: "exact-version", Status: "completed", QualityStatus: "approved",
		SessionDate: &session.Date, CompletedAt: &completedAt,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var mu sync.Mutex
	var order []string
	jobCheck := make(chan error, 1)
	builder := fakeSessionBuilder{run: func(
		_ context.Context, got engine.USMarketSession,
	) (*store.SwingUniverseVersion, error) {
		mu.Lock()
		defer mu.Unlock()
		order = append(order, "builder")
		if !got.Date.Equal(session.Date) {
			t.Fatalf("builder session = %v, want %v", got.Date, session.Date)
		}
		return version, nil
	}}
	job := fakeApprovedJob{name: "dependent", run: func(
		_ context.Context, gotSession engine.USMarketSession,
		gotVersion *store.SwingUniverseVersion, decisionAt time.Time,
	) error {
		mu.Lock()
		order = append(order, "job")
		mu.Unlock()
		if gotVersion != version || !gotSession.Date.Equal(session.Date) {
			jobCheck <- fmt.Errorf("handoff = %p / %v", gotVersion, gotSession.Date)
			cancel()
			return nil
		}
		if decisionAt.Before(completedAt) || !decisionAt.Equal(now.UTC()) {
			jobCheck <- fmt.Errorf("decision_at = %v, completed_at = %v", decisionAt, completedAt)
			cancel()
			return nil
		}
		jobCheck <- nil
		cancel()
		return nil
	}}
	scheduler := SessionScheduler{
		Cfg:     config.UniverseConfig{Enabled: true, AfterCloseDelayMinutes: 30},
		Builder: builder, Jobs: []ApprovedUniverseJob{job},
		Now: func() time.Time { return now },
	}
	if err := scheduler.Run(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-jobCheck; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "builder" || order[1] != "job" {
		t.Fatalf("execution order = %v", order)
	}
}

func TestSessionSchedulerRunsSiblingJobsAndNamesFailures(t *testing.T) {
	session := engine.USMarketSession{Date: time.Now()}
	version := &store.SwingUniverseVersion{ID: "approved"}
	var mu sync.Mutex
	called := map[string]bool{}
	jobs := []ApprovedUniverseJob{
		fakeApprovedJob{name: "broken", run: func(context.Context, engine.USMarketSession, *store.SwingUniverseVersion, time.Time) error {
			mu.Lock()
			called["broken"] = true
			mu.Unlock()
			return errors.New("provider unavailable")
		}},
		fakeApprovedJob{name: "healthy", run: func(context.Context, engine.USMarketSession, *store.SwingUniverseVersion, time.Time) error {
			mu.Lock()
			called["healthy"] = true
			mu.Unlock()
			return nil
		}},
	}
	err := (&SessionScheduler{Jobs: jobs}).runJobs(context.Background(), session, version, time.Now())
	if err == nil || !strings.Contains(err.Error(), "broken: provider unavailable") {
		t.Fatalf("runJobs error = %v", err)
	}
	if !called["broken"] || !called["healthy"] {
		t.Fatalf("job calls = %+v", called)
	}
}

func TestUniverseConfigEvidenceExcludesOperationalTiming(t *testing.T) {
	cfg := config.Defaults().Scanner.Universe
	snapshot, hash, err := UniverseConfigEvidence(cfg)
	if err != nil {
		t.Fatal(err)
	}
	changed := cfg
	changed.AfterCloseDelayMinutes++
	changed.RunLeaseMinutes++
	_, timingHash, err := UniverseConfigEvidence(changed)
	if err != nil {
		t.Fatal(err)
	}
	if timingHash != hash {
		t.Fatalf("operational timing changed identity: %s != %s", timingHash, hash)
	}
	var recorded config.UniverseConfig
	if err := json.Unmarshal(snapshot, &recorded); err != nil {
		t.Fatal(err)
	}
	if recorded.AfterCloseDelayMinutes != cfg.AfterCloseDelayMinutes ||
		recorded.RunLeaseMinutes != cfg.RunLeaseMinutes {
		t.Fatalf("operational config missing from snapshot: %+v", recorded)
	}
	changed.MinMarketDataCoverage = 0.95
	_, qualityHash, err := UniverseConfigEvidence(changed)
	if err != nil {
		t.Fatal(err)
	}
	if qualityHash == hash || len(hash) != 64 || len(snapshot) == 0 {
		t.Fatalf("build identity not captured: hash=%q qualityHash=%q snapshot=%s", hash, qualityHash, snapshot)
	}
}

func TestLatestRetryAtTraversesWrappedSiblingErrors(t *testing.T) {
	now := time.Now()
	err := errors.Join(
		fmt.Errorf("first job: %w", &BuildLeaseActiveError{RetryAt: now.Add(10 * time.Minute)}),
		fmt.Errorf("second job: %w", &BuildLeaseActiveError{RetryAt: now.Add(time.Hour)}),
	)
	if got := latestRetryAt(err); !got.Equal(now.Add(time.Hour)) {
		t.Fatalf("latest retry = %v, want %v", got, now.Add(time.Hour))
	}
}
