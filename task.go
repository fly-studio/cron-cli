package main

import (
	"context"
	"fmt"
	"github.com/robfig/cron/v3"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

type Task struct {
	Jobs            []*job
	Cron            *cron.Cron
	runningCount    int64
	stoppingTimeout int64

	wg *sync.WaitGroup

	logger   *logger
	ctx      context.Context
	cancel   context.CancelFunc
	isDocker bool
}

func NewTask(log *logger) *Task {
	return &Task{
		Cron:            cron.New(cron.WithParser(cron.NewParser(cron.SecondOptional|cron.Minute|cron.Hour|cron.Dom|cron.Month|cron.Dow|cron.Descriptor)), cron.WithLogger(log)),
		Jobs:            nil,
		wg:              &sync.WaitGroup{},
		stoppingTimeout: 3_000,
		logger:          log,
	}
}

func (t *Task) AddJob(jobs ...*job) error {
	for _, j := range jobs {
		if j.Schedule == "" {
			return fmt.Errorf("schedule required")
		}

		if len(j.Commands) < 1 {
			return fmt.Errorf("command of schedule: \"%s\" required", j.Schedule)
		}

		j.task = t
		if err := j.makeLogger(t.logger); err != nil {
			return err
		}

		if err := t.createCronJob(j); err != nil {
			return err
		}

		t.logger.Info("add job", "name", j.Name, "schedule", j.Schedule)
	}

	t.Jobs = append(t.Jobs, jobs...)

	return nil
}

func (t *Task) Start() {
	t.Cron.Start()
	t.logger.Info("cron start")
	t.wg.Add(1)

	if t.cancel != nil {
		t.cancel()
	}

	t.ctx, t.cancel = context.WithCancel(context.Background())
}

func (t *Task) Stop() {
	defer t.wg.Done()
	t.Cron.Stop()
	if t.cancel != nil {
		t.cancel()
	}

	// waiting for all job finish，force quit if timeout > stoppingTimeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.stoppingTimeout)*time.Millisecond)
	defer cancel()

for1:
	for {
		select {
		case <-ctx.Done():
			t.logger.Error(fmt.Errorf(""), "force quit")
			break for1
		default:
		}

		if atomic.LoadInt64(&t.runningCount) > 0 {
			time.Sleep(100 * time.Second)
			continue
		}
		break
	}
}

func (t *Task) ListenStopSignal() {
	go func() {
		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		<-ch
		t.Stop()
	}()
}

func (t *Task) createCronJob(job *job) error {

	var jobWrappers []cron.JobWrapper
	jobWrappers = append(jobWrappers, cron.Recover(job.logger))

	// wrap the running mode
	switch job.RunningMode {
	case "delay":
		jobWrappers = append(jobWrappers, cron.DelayIfStillRunning(job.logger))
	case "skip":
		jobWrappers = append(jobWrappers, cron.SkipIfStillRunning(job.logger))
	}

	if id, err := t.Cron.AddJob(job.Schedule, cron.NewChain(jobWrappers...).Then(job)); err != nil {
		return fmt.Errorf("invalid schedule [%s]: %w", job.Schedule, err)
	} else {
		job.id = id
	}

	return nil
}

func (t *Task) LoadSettings(configs ...string) error {

	for _, path := range configs {

		var filenames []string

		if stat, err := os.Stat(path); err != nil {
			return err
		} else if stat.IsDir() {
			if filenames, err = findYamls(path); err != nil {
				return err
			}
		} else {
			filenames = append(filenames, path)
		}

		for _, filename := range filenames {
			var config Config
			if err := loadSetting(&config, filename); err != nil {
				return err
			}
			// turn map to slice
			var jobs []*job
			for k, j := range config.Schedules {
				j.Name = k
				jobs = append(jobs, j)
			}

			if err := t.AddJob(jobs...); err != nil {
				return fmt.Errorf("config \"%s\" error: %w", filename, err)
			}
		}
	}

	return nil
}

func (t *Task) Wait() {
	t.wg.Wait()
}
