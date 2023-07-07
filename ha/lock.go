package ha

import (
	"context"
	"math/rand"
	"time"

	log "github.com/hashicorp/go-hclog"
	"github.com/hashicorp/nomad-autoscaler/sdk/helper/uuid"
)

const (
	renewalFactor = 0.7
	waitFactor    = 1.1
)

type lock interface {
	Acquire(ctx context.Context, callerID string) (string, error)
	Release(ctx context.Context) error
	Renew(ctx context.Context) error
}

type HALockController struct {
	ID            string
	renewalPeriod time.Duration
	waitPeriod    time.Duration
	randomDelay   time.Duration

	logger log.Logger
	lock   lock
}

func NewHALockController(l lock, logger log.Logger, lease time.Duration) *HALockController {
	ID := uuid.Generate()
	logger = logger.Named("ha_mode").With("id", ID)

	rn := rand.New(rand.NewSource(time.Now().Unix())).Intn(100)
	hac := HALockController{
		lock:          l,
		logger:        logger,
		renewalPeriod: time.Duration(float64(lease) * renewalFactor),
		waitPeriod:    time.Duration(float64(lease) * waitFactor),
		ID:            ID,
		randomDelay:   time.Duration(rn) * time.Millisecond,
	}

	return &hac
}

func (hc *HALockController) Start(ctx context.Context, protectedFunc func(ctx context.Context)) error {
	hc.logger.Named(hc.ID)

	// To avoid collisions if all the instances start at the same time, wait
	// a random time before making the first call.
	hc.wait(ctx)

	waitTicker := time.NewTicker(hc.waitPeriod)
	defer waitTicker.Stop()

	for {
		hc.logger.Debug("attempting to acquire lock")
		lockID, err := hc.lock.Acquire(ctx, hc.ID)
		if err != nil {
			// TODO: What to do with fatal errors?
			hc.logger.Error("unable to get lock", err)
		}

		if lockID != "" {
			hc.logger.Debug("lock acquired, ID", lockID)
			funcCtx, cancel := context.WithCancel(ctx)
			defer cancel()

			// Start running the lock protected function
			go protectedFunc(funcCtx)

			// Maintain lease is a blocking function, will only return in case
			// the lock is lost or the context is canceled.
			err := hc.maintainLease(ctx)
			if err != nil {
				hc.logger.Debug("lease lost", err)
				cancel()
				// Give the protected function some time to return before potentially
				// running it again.
				hc.wait(ctx)
			}
		}

		waitTicker.Stop()
		waitTicker = time.NewTicker(hc.waitPeriod)

		select {
		case <-ctx.Done():
			hc.logger.Debug("context canceled, returning")
			return nil

		case <-waitTicker.C:
		}
	}
}

func (hc *HALockController) maintainLease(ctx context.Context) error {
	renewTicker := time.NewTicker(hc.renewalPeriod)
	defer renewTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			hc.logger.Debug("context canceled, returning")
			return nil

		case <-renewTicker.C:
			hc.logger.Debug("renewing lease")
			err := hc.lock.Renew(ctx)
			if err != nil {
				return err
			}
		}
	}
}

func (hc *HALockController) wait(ctx context.Context) {
	t := time.NewTimer(hc.randomDelay)
	defer t.Stop()

	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
