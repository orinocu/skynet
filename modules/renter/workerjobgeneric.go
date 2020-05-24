package renter

import (
	"sync"
	"time"

	"gitlab.com/NebulousLabs/errors"
)

type (
	// jobGeneric implements the basic functionality for a job.
	jobGeneric struct {
		staticCancelChan <-chan struct{}

		staticQueue workerJobQueue
	}

	// jobGenericQueue is a generic queue for a job. It has a mutex, references
	// a worker, tracks whether or not it has been killed, and has a cooldown
	// timer. It does not have an array of jobs that are in the queue, because
	// those are type specific.
	// uploaded.
	jobGenericQueue struct {
		jobs []workerJob

		killed bool

		cooldownUntil       time.Time
		consecutiveFailures uint64
		recentErr           error

		staticWorkerObj *worker // name conflict with staticWorker method
		mu              sync.Mutex
	}

	// workerJob defines a job that the worker is able to perform.
	workerJob interface {
		// staticCanceled returns true if the job has been canceled, false
		// otherwise.
		staticCanceled() bool

		// callDicard will discard this job, sending an error down the response
		// channel of the job. The provided error should be part of the error
		// that gets sent.
		callDiscard(error)

		// callExecute will run the actual job.
		callExecute()

		// callExpectedBandwidth will return the amount of bandwidth that a job
		// expects to consume.
		callExpectedBandwidth() (upload uint64, download uint64)
	}

	// workerJobQueue defines an interface to create a worker job queue.
	workerJobQueue interface {
		// callDiscardAll will discard all of the jobs in the queue using the
		// provided error.
		callDiscardAll(error)

		// callReportFailure should be called on the queue every time that a job
		// failes, and include the error associated with the failure.
		callReportFailure(error)

		// callReportSuccess should be called on the queue every time that a job
		// succeeds.
		callReportSuccess()

		// staticWorker will return the worker of the job queue.
		staticWorker() *worker
	}
)

// newJobGeneric returns an initialized jobGeneric. The queue that is associated
// with the job should be used as the input to this function. The job will
// cancel itself if the cancelChan is closed.
func newJobGeneric(queue workerJobQueue, cancelChan <-chan struct{}) *jobGeneric {
	return &jobGeneric{
		staticCancelChan: cancelChan,

		staticQueue: queue,
	}
}

// newJobGenericQueue will return an initialized generic job queue.
func newJobGenericQueue(w *worker) *jobGenericQueue {
	return &jobGenericQueue{
		staticWorkerObj: w,
	}
}

// staticCanceled returns whether or not the job has been canceled.
func (j *jobGeneric) staticCanceled() bool {
	select {
	case <-j.staticCancelChan:
		return true
	default:
		return false
	}
}

// callAdd will add an upload snapshot job to the queue.
func (jq *jobGenericQueue) callAdd(j workerJob) bool {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	// TODO: Should check if the account or price table is on cooldown as well.
	if jq.killed || time.Now().Before(jq.cooldownUntil) {
		return false
	}
	jq.jobs = append(jq.jobs, j)
	jq.staticWorkerObj.staticWake()
	return true
}

// callDiscardAll will discard all jobs in the queue using the provided error.
func (jq *jobGenericQueue) callDiscardAll(err error) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	jq.discardAll(err)
}

// callKill will kill the queue, discarding all jobs and ensuring no more jobs
// can be added.
func (jq *jobGenericQueue) callKill() {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	err := errors.New("worker is being killed")
	jq.discardAll(err)
	jq.killed = true
}

// callNext returns the next job in the worker queue. If there is no job in the
// queue, 'nil' will be returned.
func (jq *jobGenericQueue) callNext() workerJob {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	// Loop through the jobs, looking for the first job that hasn't yet been
	// canceled. Remove jobs from the queue along the way.
	for len(jq.jobs) > 0 {
		job := jq.jobs[0]
		jq.jobs = jq.jobs[1:]
		if job.staticCanceled() {
			continue
		}
		return job
	}

	// Job queue is empty, return nil.
	return nil
}

// callReportFailure reports that a job has failed within the queue. This will
// cause all remaining jobs in the queue to be discarded, and will put the queue
// on cooldown.
func (jq *jobGenericQueue) callReportFailure(err error) {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	err = errors.AddContext(err, "job type is going on cooldown and all jobs are being discarded")
	jq.discardAll(err)
	jq.cooldownUntil = cooldownUntil(jq.consecutiveFailures)
	jq.consecutiveFailures++
	jq.recentErr = err
}

// callReportSuccess lets the job queue know that there was a successsful job.
// Note that this will reset the consecutive failure count, but will not reset
// the recentErr value - the recentErr value is left as an error so that when
// debugging later, developers and users can see what errors had been caused by
// past issues.
func (jq *jobGenericQueue) callReportSuccess() {
	jq.mu.Lock()
	jq.consecutiveFailures = 0
	jq.mu.Unlock()
}

// discardAll will drop all jobs from the queue.
func (jq *jobGenericQueue) discardAll(err error) {
	for _, job := range jq.jobs {
		job.callDiscard(err)
	}
	jq.jobs = nil
}

// staticWorker will return the worker that is associated with this job queue.
func (jq *jobGenericQueue) staticWorker() *worker {
	return jq.staticWorkerObj
}
