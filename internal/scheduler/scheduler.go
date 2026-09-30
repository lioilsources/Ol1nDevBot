// Package scheduler runs pollers on cron schedules (Europe/Prague) with
// overlap protection and health tracking.
package scheduler

import (
	"context"
	"log/slog"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/lioilsources/Ol1nDevBot/internal/health"
)

// Job is one scheduled unit of work.
type Job struct {
	Name string
	// Spec is a cron spec ("0 9 * * 1") or "@every 30m".
	Spec string
	// RunAtStart runs the job once shortly after startup.
	RunAtStart bool
	Timeout    time.Duration
	Fn         func(ctx context.Context) error
}

// Scheduler wraps robfig/cron.
type Scheduler struct {
	c       *cron.Cron
	tracker *health.Tracker
	log     *slog.Logger
	jobs    []Job
}

// New creates a scheduler in the given location.
func New(loc *time.Location, tracker *health.Tracker, log *slog.Logger) *Scheduler {
	c := cron.New(cron.WithLocation(loc), cron.WithChain(cron.Recover(cronLogger{log}), cron.SkipIfStillRunning(cronLogger{log})))
	return &Scheduler{c: c, tracker: tracker, log: log}
}

// Add registers a job.
func (s *Scheduler) Add(ctx context.Context, j Job) error {
	if j.Timeout == 0 {
		j.Timeout = 10 * time.Minute
	}
	if _, err := s.c.AddFunc(j.Spec, func() { s.run(ctx, j) }); err != nil {
		return err
	}
	s.jobs = append(s.jobs, j)
	s.log.Info("job scheduled", "job", j.Name, "spec", j.Spec)
	return nil
}

func (s *Scheduler) run(ctx context.Context, j Job) {
	if ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, j.Timeout)
	defer cancel()
	start := time.Now()
	err := s.tracker.Run(ctx, j.Name, j.Fn)
	s.log.Debug("job finished", "job", j.Name, "took", time.Since(start), "err", err)
}

// Start begins scheduling; jobs with RunAtStart are kicked off after delay,
// sequentially, so startup does not burst every API at once.
func (s *Scheduler) Start(ctx context.Context, delay time.Duration) {
	s.c.Start()
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		for _, j := range s.jobs {
			if j.RunAtStart {
				s.run(ctx, j)
			}
		}
	}()
}

// Stop stops scheduling and waits for running jobs.
func (s *Scheduler) Stop() { <-s.c.Stop().Done() }

type cronLogger struct{ l *slog.Logger }

func (c cronLogger) Info(msg string, kv ...any) { c.l.Debug("cron: "+msg, kv...) }
func (c cronLogger) Error(err error, msg string, kv ...any) {
	c.l.Error("cron: "+msg, append(kv, "err", err)...)
}
