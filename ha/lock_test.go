//go:build !race
// +build !race

package ha

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/nomad/helper/testlog"
	"github.com/shoenig/test/must"
)

var testLease = 10 * time.Millisecond

type mockLock struct {
	locked        bool
	acquiresCalls map[string]int
	renewsCounter int
	mu            sync.Mutex

	leaseStartTime time.Time
}

func (ml *mockLock) Acquire(_ context.Context, callerID string) (string, error) {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	ml.acquiresCalls[callerID] += 1
	if ml.locked {
		return "", nil
	}

	ml.locked = true
	ml.leaseStartTime = time.Now()
	return "lockID", nil
}

func (ml *mockLock) Release(_ context.Context) error {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if !ml.locked {
		return errors.New("error")
	}

	ml.locked = false
	ml.renewsCounter = 0
	return nil
}

// The behavior of renew is not an exact replication of
// the lock work, its intended to test the behavior of the
// multiple instances running.
func (ml *mockLock) Renew(_ context.Context) error {
	ml.mu.Lock()
	defer ml.mu.Unlock()

	if !ml.locked {
		return errors.New("error")
	}

	if time.Since(ml.leaseStartTime) > testLease {
		ml.locked = false
		return errors.New("lease lost")
	}

	ml.leaseStartTime = time.Now()
	ml.renewsCounter += 1
	return nil
}

type mockService struct {
	startsCounter int
	starterID     string
}

func (ms *mockService) Run(callerID string, ctx context.Context) func(ctx context.Context) {
	return func(ctx context.Context) {

		ms.startsCounter += 1
		ms.starterID = callerID

		<-ctx.Done()
		ms.starterID = ""
	}
}

func TestAcquireLock_MultipleInstances(t *testing.T) {
	l := mockLock{
		acquiresCalls: map[string]int{},
	}

	s := mockService{}

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()

	// Set up independent contexts to test the switch when one controller stops
	hac1Ctx, hac1Cancel := context.WithCancel(testCtx)
	defer hac1Cancel()

	// Wait time on hac1 is 0, it should always get the lock.
	hac1 := HALockController{
		ID:            "hac1",
		lock:          &l,
		logger:        testlog.HCLogger(t),
		renewalPeriod: time.Duration(float64(testLease) * renewalFactor),
		waitPeriod:    time.Duration(float64(testLease) * waitFactor),
		randomDelay:   0,
	}

	hac2 := HALockController{
		ID:            "hac2",
		lock:          &l,
		logger:        testlog.HCLogger(t),
		renewalPeriod: time.Duration(float64(testLease) * renewalFactor),
		waitPeriod:    time.Duration(float64(testLease) * waitFactor),
		randomDelay:   6 * time.Millisecond,
	}

	must.False(t, l.locked)

	go hac1.Start(hac1Ctx, s.Run(hac1.ID, hac1Ctx))
	go hac2.Start(testCtx, s.Run(hac2.ID, testCtx))

	time.Sleep(4 * time.Millisecond)
	/*
		After 4 ms more (4 ms total):
		* hac2 should  not have tried to acquire the lock.
		* hac1 should have the lock and the service should be running.
		* The first lease is not over yet, no calls to renew should have been made.
	*/
	must.True(t, l.locked)
	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 0, l.acquiresCalls[hac2.ID])

	must.Eq(t, 0, l.renewsCounter)

	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac1.ID, s.starterID)

	time.Sleep(6 * time.Millisecond)
	/*
		After 6 ms more (10 ms total):
		* hac2 should have tried to acquire the lock at least once.
		* hc1 should have renewed once the lease and still hold the lock.
	*/
	must.True(t, l.locked)
	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 1, l.acquiresCalls[hac2.ID])

	must.One(t, l.renewsCounter)

	must.One(t, s.startsCounter)
	must.StrContains(t, hac1.ID, s.starterID)

	time.Sleep(5 * time.Millisecond)
	/*
		After 5 ms more (15 ms total):
		* hac2 should have tried to acquire the lock still just once:
				initialDelay(6) + waitTime(11) = 18.
		* hac1 should have renewed the lease 2 times and still hold the lock:
				initialDelay(0) + renewals(2) * renewalPeriod(7) = 14.
	*/

	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 1, l.acquiresCalls[hac2.ID])

	must.True(t, l.locked)

	must.Eq(t, 2, l.renewsCounter)
	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac1.ID, s.starterID)

	time.Sleep(15 * time.Millisecond)
	/*
		After 15 ms more (30 ms total):
		* hac2 should have tried to acquire the lock 3 times:
				initialDelay(6) + calls(2)* waitTime(11) = 28.
		* hac1 should have renewed the lease 4 times and still hold the lock:
				initialDelay(0) + renewals(4) * renewalPeriod(7) = 28.
	*/

	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 3, l.acquiresCalls[hac2.ID])

	must.True(t, l.locked)

	must.Eq(t, 4, l.renewsCounter)
	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac1.ID, s.starterID)

	// Start a new instance of the service with ha running, initial delay of 1ms
	hac3 := HALockController{
		ID:            "hac3",
		lock:          &l,
		logger:        testlog.HCLogger(t),
		renewalPeriod: time.Duration(float64(testLease) * renewalFactor),
		waitPeriod:    time.Duration(float64(testLease) * waitFactor),
		randomDelay:   1 * time.Millisecond,
	}

	go hac3.Start(testCtx, s.Run(hac3.ID, testCtx))
	time.Sleep(15 * time.Millisecond)
	/*
		After 15 ms more (45 ms total):
		* hac3 should have tried to acquire the lock twice, once on start and
			once after waitTime(11).
		* hac2 should have tried to acquire the lock 4 times:
				initialDelay(6) + calls(3) * waitTime(11) = 39.
		* hac1 should have renewed the lease 4 times and still hold the lock:
				initialDelay(0) + renewals(6) * renewalPeriod(7) = 42.
	*/

	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 4, l.acquiresCalls[hac2.ID])
	must.Eq(t, 2, l.acquiresCalls[hac3.ID])

	must.True(t, l.locked)

	must.Eq(t, 6, l.renewsCounter)
	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac1.ID, s.starterID)

	// Stop hac1 and release the lock
	hac1Cancel()

	l.mu.Lock()
	l.locked = false
	l.renewsCounter = 0
	l.mu.Unlock()

	time.Sleep(10 * time.Millisecond)
	/*
		After 10 ms more (55 ms total):
		* hac3 should have tried to acquire the lock 3 times.
		* hac2 should have tried to acquire the lock 5 times and succeeded on the
		 the fifth, is currently holding the lock and Run the service.
		* hc1 is stopped.
	*/
	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 5, l.acquiresCalls[hac2.ID])
	must.Eq(t, 3, l.acquiresCalls[hac3.ID])

	must.True(t, l.locked)

	must.Eq(t, 0, l.renewsCounter)
	must.Eq(t, 2, s.startsCounter)
	must.StrContains(t, hac2.ID, s.starterID)

	time.Sleep(5 * time.Millisecond)
	/*
		After 5 ms more (60 ms total):
		* hac3 should have tried to acquire the lock 3 times.
		* hac2 should have renewed the lock once.
		* hc1 is stopped.
	*/
	must.Eq(t, 1, l.acquiresCalls[hac1.ID])
	must.Eq(t, 5, l.acquiresCalls[hac2.ID])
	must.Eq(t, 3, l.acquiresCalls[hac3.ID])

	must.True(t, l.locked)

	must.Eq(t, 1, l.renewsCounter)
	must.Eq(t, 2, s.startsCounter)
	must.StrContains(t, hac2.ID, s.starterID)
}

func TestFailedRenewal(t *testing.T) {
	l := mockLock{
		acquiresCalls: map[string]int{},
	}

	s := mockService{}

	testCtx, testCancel := context.WithCancel(context.Background())
	defer testCancel()

	// Set the renewal period to 1.5  * testLease (15 ms) to force and error.
	hac := HALockController{
		ID:            "hac1",
		lock:          &l,
		logger:        testlog.HCLogger(t),
		renewalPeriod: time.Duration(float64(testLease) * 1.5),
		waitPeriod:    time.Duration(float64(testLease) * waitFactor),
		randomDelay:   0,
	}

	must.False(t, l.locked)

	go hac.Start(testCtx, s.Run(hac.ID, testCtx))

	time.Sleep(5 * time.Millisecond)
	/*
		After 5ms, the service should be running and the lock held,
		no renewals needed or performed yet.
	*/

	must.Eq(t, 1, l.acquiresCalls[hac.ID])
	must.True(t, l.locked)

	must.Eq(t, 0, l.renewsCounter)
	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac.ID, s.starterID)

	time.Sleep(15 * time.Millisecond)
	/*
		After 15ms (20ms total) hac should have tried and failed at renewing the
		lock, causing the service to return, no new calls to acquire the lock yet
		either.
	*/
	must.Eq(t, 1, l.acquiresCalls[hac.ID])
	must.False(t, l.locked)

	must.Eq(t, 0, l.renewsCounter)
	must.Eq(t, 1, s.startsCounter)
	must.StrContains(t, hac.ID, "")

	time.Sleep(10 * time.Millisecond)
	/*
		After 10ms (30ms total) hac should have tried and succeeded at getting
		the lock and the service should be running again.
	*/
	must.Eq(t, 2, l.acquiresCalls[hac.ID])
	must.True(t, l.locked)

	must.Eq(t, 0, l.renewsCounter)
	must.Eq(t, 2, s.startsCounter)
	must.StrContains(t, hac.ID, s.starterID)
}
