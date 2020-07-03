package renter

import (
	"encoding/json"
	"fmt"
	"sync/atomic"
	"time"
	"unsafe"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/types"
	"gitlab.com/NebulousLabs/errors"
)

const (
	// updatePriceTableGougingPercentageThreshold is the percentage threshold,
	// in relation to the allowance, at which we consider the cost of updating
	// the price table to be too expensive. E.g. the cost of updating the price
	// table over the total allowance period should never exceed 1% of the total
	// allowance.
	updatePriceTableGougingPercentageThreshold = .01
)

var (
	// errPriceTableGouging is returned when price gouging is detected
	errPriceTableGouging = errors.New("price table rejected due to price gouging")

	// minAcceptedPriceTableValidity is the minimum price table validity
	// the renter will accept.
	minAcceptedPriceTableValidity = build.Select(build.Var{
		Standard: 5 * time.Minute,
		Dev:      1 * time.Minute,
		Testing:  10 * time.Second,
	}).(time.Duration)
)

type (
	// workerPriceTable contains a price table and some information related to
	// retrieving the next update.
	workerPriceTable struct {
		// The account's target balance. This is defined on the price table as
		// it is influenced by the host's pricing and thus depends on the price
		// table to be able to calculate.
		staticAccountTargetBalance types.Currency

		// The actual price table.
		staticPriceTable modules.RPCPriceTable

		// The time at which the price table expires.
		staticExpiryTime time.Time

		// The next time that the worker should try to update the price table.
		staticUpdateTime time.Time

		// The number of consecutive failures that the worker has experienced in
		// trying to fetch the price table. This number is used to inform
		// staticUpdateTime, a larger number of consecutive failures will result in
		// greater backoff on fetching the price table.
		staticConsecutiveFailures uint64

		// staticRecentErr specifies the most recent error that the worker's
		// price table update has failed with.
		staticRecentErr error

		// staticRecentErrTime specifies the time at which the most recent
		// occurred
		staticRecentErrTime time.Time
	}
)

// staticNeedsPriceTableUpdate is a helper function that determines whether the
// price table should be updated.
func (w *worker) staticNeedsPriceTableUpdate() bool {
	// Check the version.
	if build.VersionCmp(w.staticCache().staticHostVersion, minAsyncVersion) < 0 {
		return false
	}
	return time.Now().After(w.staticPriceTable().staticUpdateTime)
}

// newPriceTable will initialize a price table for the worker.
func (w *worker) newPriceTable() {
	if w.staticPriceTable() != nil {
		w.renter.log.Critical("creating a new price table when a new price table already exists")
	}
	w.staticSetPriceTable(new(workerPriceTable))
}

// staticPriceTable will return the most recent price table for the worker's
// host.
func (w *worker) staticPriceTable() *workerPriceTable {
	ptr := atomic.LoadPointer(&w.atomicPriceTable)
	return (*workerPriceTable)(ptr)
}

// staticSetPriceTable will set the price table in the worker to be equal to the
// provided price table.
func (w *worker) staticSetPriceTable(pt *workerPriceTable) {
	atomic.StorePointer(&w.atomicPriceTable, unsafe.Pointer(pt))
}

// staticValid will return true if the latest price table that we have is still
// valid for the host.
//
// The price table is default invalid, because the zero time / empty time is
// before the current time, and the price table expiry defaults to the zero
// time.
func (wpt *workerPriceTable) staticValid() bool {
	return time.Now().Before(wpt.staticExpiryTime)
}

// managedUpdatePriceTable performs the UpdatePriceTableRPC on the host.
func (w *worker) staticUpdatePriceTable() {
	// Sanity check - This function runs on a fairly strict schedule, the
	// control loop should not have called this function unless the price table
	// is after its updateTime.
	updateTime := w.staticPriceTable().staticUpdateTime
	if time.Now().Before(updateTime) {
		w.renter.log.Critical("price table is being updated prematurely")
	}
	// Sanity check - only one price table update should be running at a time.
	// If multiple are running at a time, there can be a race condition around
	// 'staticConsecutiveFailures'.
	if !atomic.CompareAndSwapUint64(&w.atomicPriceTableUpdateRunning, 0, 1) {
		w.renter.log.Critical("price table is being updated in two threads concurrently")
	}
	defer atomic.StoreUint64(&w.atomicPriceTableUpdateRunning, 0)

	// Create a goroutine to wake the worker when the time has come to check the
	// price table again. Make sure to grab the update time inside of the defer
	// func, after the price table has been updated.
	//
	// This defer needs to run after the defer which updates the price table.
	defer func() {
		updateTime := w.staticPriceTable().staticUpdateTime
		w.renter.tg.AfterFunc(updateTime.Sub(time.Now()), func() {
			w.staticWake()
		})
	}()

	// All remaining errors represent short term issues with the host, so the
	// price table should be updated to represent the failure, but should retain
	// the existing price table, which will allow the renter to continue
	// performing tasks even though it's having trouble getting a new price
	// table.
	var err error
	currentPT := w.staticPriceTable()
	defer func() {
		if err != nil {
			// Because of race conditions, can't modify the existing price
			// table, need to make a new one.
			pt := &workerPriceTable{
				staticPriceTable:          currentPT.staticPriceTable,
				staticExpiryTime:          currentPT.staticExpiryTime,
				staticUpdateTime:          cooldownUntil(currentPT.staticConsecutiveFailures),
				staticConsecutiveFailures: currentPT.staticConsecutiveFailures + 1,
				staticRecentErr:           err,
				staticRecentErrTime:       time.Now(),
			}
			w.staticSetPriceTable(pt)

			// If the error could be caused by a revision number mismatch,
			// signal it by setting the flag.
			if errCausedByRevisionMismatch(err) {
				w.staticSetSuspectRevisionMismatch()
				w.staticWake()
			}
		}
	}()

	// Get a stream.
	stream, err := w.staticNewStream()
	if err != nil {
		err = errors.AddContext(err, "unable to create new stream")
		return
	}
	defer func() {
		// An error closing the stream is not sufficient reason to reject the
		// price table that the host gave us. Because there is a defer checking
		// for the value of 'err', we use a different variable name here.
		streamCloseErr := stream.Close()
		if streamCloseErr != nil {
			w.renter.log.Println("ERROR: failed to close stream", streamCloseErr)
		}
	}()

	// write the specifier
	err = modules.RPCWrite(stream, modules.RPCUpdatePriceTable)
	if err != nil {
		err = errors.AddContext(err, "unable to write price table specifier")
		return
	}

	// receive the price table
	var uptr modules.RPCUpdatePriceTableResponse
	err = modules.RPCRead(stream, &uptr)
	if err != nil {
		err = errors.AddContext(err, "unable to read price table response")
		return
	}

	// decode the JSON
	var pt modules.RPCPriceTable
	err = json.Unmarshal(uptr.PriceTableJSON, &pt)
	if err != nil {
		err = errors.AddContext(err, "unable to unmarshal price table")
		return
	}

	// check for gouging before paying
	err = checkUpdatePriceTableGouging(pt, w.staticCache().staticRenterAllowance)
	if err != nil {
		err = errors.Compose(err, errors.AddContext(errPriceTableGouging, fmt.Sprintf("host %v", w.staticHostPubKeyStr)))
		w.renter.log.Println("ERROR: ", err)
		return
	}

	// calculate a target balance for the worker's account
	targetBalance := calculateTargetBalance(pt)
	if targetBalance.Cmp(w.staticCache().staticHostMaxBalance) > 0 {
		err = fmt.Errorf("calculated account target balance exceeds the host's max ephemeral account balance, host %v", w.staticHostPubKeyStr)
		w.renter.log.Println("ERROR: ", err)
		return
	}

	// provide payment
	err = w.renter.hostContractor.ProvidePayment(stream, w.staticHostPubKey, modules.RPCUpdatePriceTable, pt.UpdatePriceTableCost, w.staticAccount.staticID, w.staticCache().staticBlockHeight)
	if err != nil {
		err = errors.AddContext(err, "unable to provide payment")
		return
	}

	// The price table will not become valid until the host has received and
	// confirmed our payment. The host will signal this by sending an empty
	// response object we need to read.
	var tracked modules.RPCTrackedPriceTableResponse
	err = modules.RPCRead(stream, &tracked)
	if err != nil {
		err = errors.AddContext(err, "unable to read tracked response")
		return
	}

	// Calculate the expiry time and set the update time to be half of the
	// expiry window to ensure we update the PT before it expires
	now := time.Now()
	expiryTime := now.Add(pt.Validity)
	expiryHalfTimeInS := (expiryTime.Unix() - now.Unix()) / 2
	expiryHalfTime := time.Duration(expiryHalfTimeInS) * time.Second
	newUpdateTime := time.Now().Add(expiryHalfTime)

	// Update the price table. We preserve the recent error even though there
	// has not been an error for debugging purposes, if there has been an error
	// previously the devs like to be able to see what it was.
	wpt := &workerPriceTable{
		staticAccountTargetBalance: targetBalance,
		staticPriceTable:           pt,
		staticExpiryTime:           expiryTime,
		staticUpdateTime:           newUpdateTime,
		staticConsecutiveFailures:  0,
		staticRecentErr:            currentPT.staticRecentErr,
		staticRecentErrTime:        currentPT.staticRecentErrTime,
	}
	w.staticSetPriceTable(wpt)
}

// checkUpdatePriceTableGouging verifies the cost of updating the price table is
// reasonable, if deemed unreasonable we will reject it and this worker will be
// put into cooldown.
func checkUpdatePriceTableGouging(pt modules.RPCPriceTable, allowance modules.Allowance) error {
	// If there is no allowance, price gouging checks have to be disabled,
	// because there is no baseline for understanding what might count as price
	// gouging.
	if allowance.Funds.IsZero() {
		return nil
	}

	// Verify the validity is reasonable
	if pt.Validity < minAcceptedPriceTableValidity {
		return fmt.Errorf("update price table validity %v is considered too low, the minimum accepted validity is %v", pt.Validity, minAcceptedPriceTableValidity)
	}

	// In order to decide whether or not the update price table cost is too
	// expensive, we first have to calculate how many times we'll need to update
	// the price table over the entire allowance period
	durationInS := int64(pt.Validity.Seconds())
	periodInS := int64(allowance.Period) * 10 * 60 // period times 10m blocks
	numUpdates := periodInS / durationInS

	// The cost of updating is considered too expensive if the total cost is
	// above a certain % of the allowance.
	totalUpdateCost := pt.UpdatePriceTableCost.Mul64(uint64(numUpdates))
	if totalUpdateCost.Cmp(allowance.Funds.MulFloat(updatePriceTableGougingPercentageThreshold)) > 0 {
		return fmt.Errorf("update price table cost %v is considered too high, the total cost over the entire duration of the allowance periods exceeds %v%% of the allowance - price gouging protection enabled", pt.UpdatePriceTableCost, updatePriceTableGougingPercentageThreshold)
	}

	return nil
}

// calculateTargetBalance calculates a target balance for the worker account. We
// want to set the balance to twice the amount of money required to download
// 'minDownloadBeforeRefill' amount of data in 64 KiB download jobs. Twice
// because we refill at 50% of the target balance.
func calculateTargetBalance(pt modules.RPCPriceTable) types.Currency {
	// Calculate the cost of a read sector job, we use StreamDownloadSize as an
	// average download size here which is 64 KiB.
	pb := modules.NewProgramBuilder(&pt, 0)
	mr := crypto.Hash{}
	pb.AddReadSectorInstruction(modules.StreamDownloadSize, 0, mr, true)
	programCost, _, _ := pb.Cost(true)

	// Calculate the expected bandwidth costs for the program, arriving at a
	// cost per read job.
	ulbw, dlbw := readSectorJobExpectedBandwidth(modules.StreamDownloadSize)
	bandwidthCost := modules.MDMBandwidthCost(pt, ulbw, dlbw)
	costPerJob := programCost.Add(bandwidthCost)

	// Calculate the balance target, which is twice the amount of money
	// necessary to run enough jobs to download 'minDownloadBeforeRefill' worth
	// of data.
	numDownloadJobs := minDownloadBeforeRefill / modules.StreamDownloadSize
	return costPerJob.Mul64(numDownloadJobs).Mul64(2)
}
