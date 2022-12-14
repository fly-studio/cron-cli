package main

import (
	"context"
	"errors"
	"fmt"
	"github.com/robfig/cron/v3"
	"os"
	"os/signal"
	"path/filepath"
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

	logger           *logger
	quitSignalCtx    context.Context
	quitSignalCancel context.CancelFunc
	rootPathInDocker string
	testMode         bool
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

func (t *Task) AddJob(configFile string, jobs ...*job) error {
	for _, j := range jobs {
		if j.Schedule == "" {
			return fmt.Errorf("schedule required")
		}

		if j.Command == "" {
			return fmt.Errorf("command of schedule: \"%s\" required", j.Schedule)
		}

		j.task = t
		if err := j.makeLogger(t.logger); err != nil {
			return err
		}

		if err := t.createCronJob(configFile, j); err != nil {
			return err
		}

		t.logger.Info("add job", "schedule", j.Schedule, "command", truncateText(j.Command, 40))
	}

	t.Jobs = append(t.Jobs, jobs...)

	return nil
}

func (t *Task) Start() {
	if t.testMode {
		t.startTest()
		return
	}

	t.Cron.Start()
	t.logger.Info("cron start")
	t.wg.Add(1)

	if t.quitSignalCancel != nil {
		t.quitSignalCancel()
	}

	t.quitSignalCtx, t.quitSignalCancel = context.WithCancel(context.Background())
}

func (t *Task) startTest() {
	t.logger.Info("cron start in test mode")
	t.wg.Add(1)
	if t.quitSignalCancel != nil {
		t.quitSignalCancel()
	}
	t.quitSignalCtx, t.quitSignalCancel = context.WithCancel(context.Background())

	go func() {
		defer t.stopTest()
		for _, j := range t.Jobs {
			j.Run()
		}
	}()
}

func (t *Task) InDocker() bool {
	return t.rootPathInDocker != "" && t.rootPathInDocker != "/"
}

func (t *Task) stopImpl(ctx context.Context) {
	defer t.wg.Done()
	defer func() { // delete all temporary shell files
		for _, j := range t.Jobs {
			j.deleteShellFile()
		}
	}()

	if t.quitSignalCancel != nil {
		t.quitSignalCancel()
	}

for1:
	for atomic.LoadInt64(&t.runningCount) > 0 {
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				t.logger.Error(fmt.Errorf(""), "cron jobs force quiting")
			}
			break for1
		default:
			time.Sleep(100 * time.Second)
		}
	}

	t.logger.Info("all jobs quit")
}

func (t *Task) stopTest() {
	// waiting for all job finish, force quit after stoppingTimeout
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(t.stoppingTimeout)*time.Millisecond)
	defer cancel()

	t.stopImpl(ctx)
}

func (t *Task) Stop() {
	if t.testMode {
		t.stopTest()
		return
	}

	stoppingCtx := t.Cron.Stop()

	// waiting for all job finish, force quit after stoppingTimeout
	ctx, cancel := context.WithTimeout(stoppingCtx, time.Duration(t.stoppingTimeout)*time.Millisecond)
	defer cancel()

	t.stopImpl(ctx)
}

func (t *Task) ListenStopSignal(callback func()) {
	go func() {
		ch := make(chan os.Signal)
		signal.Notify(ch, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGQUIT)
		<-ch
		callback()
	}()
}

func (t *Task) createCronJob(configFile string, job *job) error {
	var jobWrappers []cron.JobWrapper
	jobWrappers = append(jobWrappers, cron.Recover(job.logger))

	// wrap the running mode
	switch job.RunningMode {
	case "delay":
		jobWrappers = append(jobWrappers, cron.DelayIfStillRunning(job.logger))
	case "skip":
		jobWrappers = append(jobWrappers, cron.SkipIfStillRunning(job.logger))
	}

	id, err := t.Cron.AddJob(job.Schedule, cron.NewChain(jobWrappers...).Then(job))
	if err != nil {
		return fmt.Errorf("invalid schedule [%s]: %w", job.Schedule, err)
	}

	job.id = id

	// put job.command to a temporary shell file
	if configFile == "argument" {
		configFile = filepath.Join(os.TempDir(), "argument")
	}

	if t.testMode {
		job.shellFile = filepath.Join(filepath.Dir(configFile), fmt.Sprintf(".test-%s-%d.sh", filepath.Base(configFile), id))
	} else {
		job.shellFile = filepath.Join(filepath.Dir(configFile), fmt.Sprintf(".%s-%d.sh", filepath.Base(configFile), id))
	}
	job.saveShellFile()

	return nil
}

func (t *Task) Wait() {
	t.wg.Wait()
}
