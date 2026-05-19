package tasks

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/fivetwenty-io/pve-apiclient-go/v3/internal/constants"
	"github.com/fivetwenty-io/pve-apiclient-go/v3/pkg/client"
)

var (
	errUnexpectedTaskStatusFormat = errors.New("unexpected task status format")
	errTaskFailed                 = errors.New("task failed")
	errTaskInProgress             = errors.New("task is still in progress")
)

// Service defines task-related helpers.
type Service interface {
	Wait(ctx context.Context, node, upid string, opts *WaitOptions) (*Status, error)
}

// WaitOptions controls polling behavior.
type WaitOptions struct {
	TimeoutSeconds int
	IntervalMillis int
	// Backoff enables exponential backoff of the poll interval (defaults to false for BC).
	Backoff bool
	// MaxIntervalMillis caps the backoff interval; if 0 and Backoff is true, defaults to 5000ms.
	MaxIntervalMillis int
	// JitterPct adds +/- random jitter percentage to each interval (e.g., 10 => +/-10%).
	JitterPct int
}

// Status represents a Proxmox task status.
type Status struct {
	Status     string
	ExitStatus string
	UpID       string
}

// service implements Service.
type service struct {
	c client.Client
}

// New returns a new tasks service.
//
//nolint:ireturn // Factory pattern - returns interface to encapsulate implementation and enable mocking
func New(c client.Client) Service { return &service{c: c} }

// Wait polls a task until completion or timeout.
func (s *service) Wait(ctx context.Context, node, upid string, opts *WaitOptions) (*Status, error) {
	config := s.parseWaitOptions(opts)

	ctx, cancel := context.WithTimeout(ctx, time.Duration(config.timeoutSeconds)*time.Second)
	defer cancel()

	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", node, upid)
	poller := &taskPoller{
		service:        s,
		path:           path,
		upid:           upid,
		intervalMillis: config.intervalMillis,
		backoff:        config.backoff,
		maxInterval:    config.maxInterval,
		jitterPct:      config.jitterPct,
	}

	return poller.poll(ctx)
}

type waitConfig struct {
	timeoutSeconds int
	intervalMillis int
	backoff        bool
	maxInterval    int
	jitterPct      int
}

func (s *service) parseWaitOptions(opts *WaitOptions) waitConfig {
	config := waitConfig{
		timeoutSeconds: constants.DefaultTaskTimeoutSeconds,
		intervalMillis: constants.TaskIntervalMillis,
		backoff:        false,
		maxInterval:    constants.MaxTaskIntervalMillis,
		jitterPct:      0,
	}

	if opts == nil {
		return config
	}

	if opts.TimeoutSeconds > 0 {
		config.timeoutSeconds = opts.TimeoutSeconds
	}

	if opts.IntervalMillis > 0 {
		config.intervalMillis = opts.IntervalMillis
	}

	if opts.MaxIntervalMillis > 0 {
		config.maxInterval = opts.MaxIntervalMillis
	}

	if opts.JitterPct > 0 {
		config.jitterPct = opts.JitterPct
	}

	config.backoff = opts.Backoff

	return config
}

type taskPoller struct {
	service        *service
	path           string
	upid           string
	intervalMillis int
	backoff        bool
	maxInterval    int
	jitterPct      int
}

func (p *taskPoller) poll(ctx context.Context) (*Status, error) {
	cur := p.intervalMillis

	for {
		err := p.waitForInterval(ctx, cur)
		if err != nil {
			return nil, err
		}

		status, err := p.checkTaskStatus(ctx)
		if err != nil {
			if errors.Is(err, errTaskInProgress) {
				// Continue polling
			} else {
				return nil, err
			}
		} else if status != nil {
			return status, nil
		}

		cur = p.updateInterval(cur)
	}
}

func (p *taskPoller) waitForInterval(ctx context.Context, intervalMillis int) error {
	d := time.Duration(applyJitter(intervalMillis, p.jitterPct)) * time.Millisecond
	timer := time.NewTimer(d)

	select {
	case <-ctx.Done():
		if !timer.Stop() {
			<-timer.C
		}

		return context.DeadlineExceeded
	case <-timer.C:
		return nil
	}
}

func (p *taskPoller) checkTaskStatus(ctx context.Context) (*Status, error) {
	data, err := p.service.c.GetCtx(ctx, p.path, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to get task status: %w", err)
	}

	taskData, ok := data.(map[string]interface{})
	if !ok {
		return nil, errUnexpectedTaskStatusFormat
	}

	return p.parseTaskStatus(taskData)
}

func (p *taskPoller) parseTaskStatus(taskData map[string]interface{}) (*Status, error) {
	status, _ := taskData["status"].(string)
	if status != "stopped" {
		return nil, errTaskInProgress // Continue polling if not stopped
	}

	exitStatus, _ := taskData["exitstatus"].(string)
	statusObj := &Status{Status: status, ExitStatus: exitStatus, UpID: p.upid}

	if p.isSuccessExitStatus(exitStatus) {
		return statusObj, nil
	}

	return statusObj, fmt.Errorf("%w: %s", errTaskFailed, exitStatus)
}

func (p *taskPoller) isSuccessExitStatus(exitStatus string) bool {
	return exitStatus == "OK" || exitStatus == "ok" || exitStatus == ""
}

func (p *taskPoller) updateInterval(cur int) int {
	if !p.backoff {
		return cur
	}

	newInterval := cur * constants.BackoffMultiplier
	if newInterval > p.maxInterval {
		return p.maxInterval
	}

	return newInterval
}

// applyJitter increases/decreases ms by up to jitterPct%.
func applyJitter(milliseconds int, jitterPct int) int {
	if jitterPct <= 0 || milliseconds <= 0 {
		return milliseconds
	}
	// +/- jitterPct% uniformly
	delta := (milliseconds * jitterPct) / constants.JitterPercentage
	if delta == 0 {
		return milliseconds
	}
	// random in [-delta, +delta]
	randomOffset, err := rand.Int(rand.Reader, big.NewInt(int64(2*delta+1)))
	if err != nil {
		// fallback to no jitter if crypto/rand fails
		return milliseconds
	}

	off := int(randomOffset.Int64()) - delta

	v := milliseconds + off
	if v < 1 {
		v = 1
	}

	return v
}
