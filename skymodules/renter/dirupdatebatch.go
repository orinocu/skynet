package renter

import (
	"sync"
	"time"

	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skymodules"
)
var (
	// maxTimeBetweenBatchExectutions defines the amount of time that a batch
	// will wait before executing the queue of directories to batch. The testing
	// value is really low at 100ms to maximize the opportunity that threads
	// queue things across multiple batches (which should be safe, but
	// potentially has edge cases).
	maxTimeBetweenBatchExecutions = build.Select(build.Var{
		Dev:      30 * time.Second,
		Standard: 15 * time.Minute,
		Testing:  1 * time.Second,
	}).(time.Duration)
)

type (
	// dirUpdateBatch defines a batch of updates that should be run at the
	// same time. Performing an update on a file requires doing an update on its
	// directory and all parent directories up to the root directory. By doing
	// the updates as a batch, we can reduce the total amount of work required
	// to complete the update.
	//
	// NOTE: the health update batch depends on the mutex of the
	// dirUpdateBatcher for thread safety.
	dirUpdateBatch struct {
		// batchSet is an array of maps which contain the directories that need
		// to be updated. Each element of the array corresponds to a directory
		// of a different depth. The first element of the array just contains
		// the root directory. The second element is a map that contains only
		// direct subdirs of the root. The third element is a map that contains
		// directories which live directly in subdirs of the root, and so on.
		//
		// When performing the update on the set, the lowest level dirs are all
		// executed at once, and then their parents are added to the batchSet,
		// then the next level of dirs are executed all together, and so on.
		// This ensures that each directory is only updated a single time per
		// batch, even if it appears as a parent in dozens of directories in the
		// batchSet.
		batchSet []map[skymodules.SiaPath]struct{}

		// completeChan is a channel that gets closed when the whole batch has
		// successfully executed. It will not be closed until priorCompleteChan
		// has been closed. priorCompleteChan is the channel owned by the
		// previous batch. This ensures that when the channel is closed, all
		// updates are certain to have completed, even if those updates were
		// submitted to previous batches.
		completeChan chan struct{}
		priorCompleteChan chan struct{}

		renter *Renter
	}

	// dirUpdateBatcher receives requests to update the health of a file or
	// directory and adds them to a batch. This struct manages concurrency and
	// safety between different batches.
	dirUpdateBatcher struct {
		// nextBatch defines the next batch that will perform a health update.
		nextBatch *dirUpdateBatch

		// Utilities
		staticFlushChan chan struct{}
		mu              sync.Mutex
		staticRenter    *Renter
	}
)

// execute will execute a batch of updates.
func (batch *dirUpdateBatch) execute() {
	// iterate through the batchSet backwards.
	for i := len(batch.batchSet)-1; i >= 0; i-- {
		for dirPath , _ := range batch.batchSet[i] {
			// Update the directory metadata. Note: we don't do any updates on
			// the file healths themselves, we just use the file metadata.
			err := batch.renter.managedUpdateDirMetadata(dirPath)
			if err != nil {
				// TODO: Verbose log?
				continue
			}

			// Add the parent.
			if !dirPath.IsRoot() {
				parent, err := dirPath.Dir()
				if err != nil {
					panic("should not be getting an error when grabbing the parent of a non-root siadir")
				}
				batch.batchSet[i-1][parent] = struct{}{}
			}
		}
	}

	// Wait until the previous channel is complete.
	<-batch.priorCompleteChan
	close(batch.completeChan)
}

// callQueueUpdate will add an update to the current batch. The input needs to
// be a dir.
func (hub *dirUpdateBatcher) callQueueDirUpdate(dirPath skymodules.SiaPath) {
	hub.mu.Lock()
	defer hub.mu.Unlock()

	// Determine how many levels this dir has.
	levels := 0
	next := dirPath
	for !next.IsRoot() {
		var err error
		next, err = next.Dir()
		if err != nil {
			panic("should not get an error when parsing the dir")
		}
		levels++
	}

	// Make sure maps at each level exist.
	for i := len(hub.nextBatch.batchSet); i <= levels; i++ {
		hub.nextBatch.batchSet = append(hub.nextBatch.batchSet, make(map[skymodules.SiaPath]struct{}))
	}
	// Add the input dirPath to the final level.
	hub.nextBatch.batchSet[levels][dirPath] = struct{}{}
}

// callFlushUpdates will trigger the current batch of updates to execute, and
// will not return until all updates have compelted and are represented in the
// root directory. It will also not return until all prior batches have
// completed as well - if you have added a directory to a batch and call flush,
// you can be certain that the directory update will have executed by the time
// the flush call returns, regardless of which batch that directory was added
// to.
func (hub *dirUpdateBatcher) callFlushUpdates() {
	// Grab the complete chan for the current batch.
	hub.mu.Lock()
	completeChan := hub.nextBatch.completeChan
	hub.mu.Unlock()

	// Signal that the current batch should be flushed.
	select{
	case hub.staticFlushChan <- struct{}{}:
	default:
	}

	// Wait until the batch has completed before returning.
	<-completeChan
}

// newBatch returns a new dirUpdateBatch ready for use.
func (hub *dirUpdateBatcher) newBatch(priorCompleteChan chan struct{}) *dirUpdateBatch {
	return &dirUpdateBatch{
		completeChan: make(chan struct{}),
		priorCompleteChan: priorCompleteChan,

		renter: hub.staticRenter,
	}
}

// threadedExecuteBatchUpdates is a permanent background thread which will
// execute batched updates in the background.
func (hub *dirUpdateBatcher) threadedExecuteBatchUpdates() {
	err := hub.staticRenter.tg.Add()
	if err != nil {
		return
	}
	defer hub.staticRenter.tg.Done()

	for {
		select {
		case <-hub.staticRenter.tg.StopChan():
			return
		case <-hub.staticFlushChan:
		case <-time.After(maxTimeBetweenBatchExecutions):
		}

		// Rotate the current batch out for a new batch. This will block any
		// thread trying to add new updates to the batch, so make sure it
		// happens quickly.
		hub.mu.Lock()
		batch := hub.nextBatch
		hub.nextBatch = hub.newBatch(batch.priorCompleteChan)
		hub.mu.Unlock()

		// Execute the batch now that we aren't blocking anymore.
		batch.execute()
	}
}

// newHealthUpdateBatcher returns a health update batcher that is ready for use.
func (r *Renter) newHealthUpdateBatcher() *dirUpdateBatcher {
	hub := &dirUpdateBatcher{
		staticFlushChan: make(chan struct{}),
		staticRenter:    r,
	}

	// The next batch needs a channel which will be closed when the previous
	// batch completes. Since there is no previous batch, we provide a channel
	// that is already closed.
	initialChan := make(chan struct{})
	close(initialChan)

	hub.nextBatch = hub.newBatch(initialChan)
	go hub.threadedExecuteBatchUpdates()
	return hub
}
