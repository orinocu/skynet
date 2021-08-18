package renter

import (
	"context"
	"sync"
	"time"

	"github.com/opentracing/opentracing-go"
	"gitlab.com/SkynetLabs/skyd/build"
	"go.sia.tech/siad/crypto"
	"go.sia.tech/siad/modules"
	"go.sia.tech/siad/types"

	"gitlab.com/NebulousLabs/errors"
)

const (
	// availabilityMetricsNumBuckets is the total number of buckets we use to
	// track the sector availability metrics for a certain host. Every bucket
	// represents a range of total pieces uploaded to the network, the total
	// amount of pieces is decided by the redundancy scheme used during the
	// upload.
	availabilityMetricsNumBuckets = 16

	// availabilityMetricsBucketScale is the amount with which we scale each
	// bucket. Every bucket scales up 25%, this number was chosen because it
	// provides sufficiently granularity of coverage. E.g. using this scale the
	// buckets are the following: 1, 2, 3, 4-5, 6-7, 8-10, 11-13, 14-17, 18-22,
	// 23-28, 29-36,...
	availabilityMetricsBucketScale = 1.25

	// jobHasSectorPerformanceDecay defines how much the average performance is
	// decayed each time a new datapoint is added. The jobs use an exponential
	// weighted average.
	jobHasSectorPerformanceDecay = 0.9

	// jobHasSectorQueueMinAvailabilityRate is the minimum availability rate we
	// return when there haven't been any jobs performed yet by the queue where
	// the sector was availabile.
	jobHasSectorQueueMinAvailabilityRate = 0.001

	// hasSectorBatchSize is the number of has sector jobs batched together upon
	// calling callNext.
	// This number is the result of empirical testing which determined that 13
	// requests can be batched together without increasing the required
	// upload or download bandwidth.
	hasSectorBatchSize = 13
)

// errEstimateAboveMax is returned if a HasSector job wasn't added due to the
// estimate exceeding the max.
var errEstimateAboveMax = errors.New("can't add job since estimate is above max timeout")

type (
	// jobHasSector contains information about a hasSector query.
	jobHasSector struct {
		staticSectors      []crypto.Hash
		staticResponseChan chan *jobHasSectorResponse

		// staticNumPieces represents the redundancy with which the sectors were
		// uploaded, it is the total number of pieces meaning the sum of the
		// data and parity pieces used by the erasure coder
		//
		// NOTE: we assume that all sectors corresponding to the roots listed
		// in this HS job were uploaded using the same redundancy scheme
		staticNumPieces int

		staticPostExecutionHook func(*jobHasSectorResponse)
		once                    sync.Once

		staticSpan opentracing.Span

		*jobGeneric
	}

	// jobHasSectorBatch is a batch of has sector lookups.
	jobHasSectorBatch struct {
		staticJobs []*jobHasSector
	}

	// jobHasSectorQueue is a list of hasSector queries that have been assigned
	// to the worker.
	jobHasSectorQueue struct {
		// These variables contain an exponential weighted average of the
		// worker's recent performance for jobHasSectorQueue.
		weightedJobTime float64

		// availabilityMetrics keeps track of how often a sector was available
		// on this host, we keep track of this in a way that we take the
		// redundancy with which the sector was uploaded into account
		availabilityMetrics *availabilityMetrics

		*jobGenericQueue
	}

	// jobHasSectorResponse contains the result of a hasSector query.
	jobHasSectorResponse struct {
		staticAvailables []bool
		staticErr        error

		// The worker is included in the response so that the caller can listen
		// on one channel for a bunch of workers and still know which worker
		// successfully found the sector root.
		staticWorker *worker

		// The time it took for this job to complete is included for debugging
		// purposes.
		staticJobTime time.Duration
	}

	// availabilityMetrics is a helper struct that keeps track of sector
	// availability metrics, we keep track of these in several buckets that
	// correspond with sectors that were uploaded with a more or less similar
	// redundancy scheme
	availabilityMetrics struct {
		buckets             []*availabilityBucket
		piecesToBucketIndex map[uint64]int
		mu                  sync.Mutex
	}

	// availabilityBucket is a helper struct that keeps track of how often a
	// sector was available, every bucket holds these stats for sectors that
	// were uploaded with a similar redundancy scheme
	availabilityBucket struct {
		totalAvailable uint64
		totalJobs      uint64
	}
)

// newAvailabilityMetrics returns a new availabilityMetrics object
func newAvailabilityMetrics() *availabilityMetrics {
	metrics := &availabilityMetrics{
		buckets:             make([]*availabilityBucket, availabilityMetricsNumBuckets),
		piecesToBucketIndex: make(map[uint64]int),
	}

	// initialize the buckets and a map that can be used to lookup what bucket
	// corresponds to what number of pieces in constant time
	curr := uint64(1)
	for bucket := 0; bucket < availabilityMetricsNumBuckets; bucket++ {
		metrics.buckets[bucket] = &availabilityBucket{}

		next := uint64(float64(curr) * availabilityMetricsBucketScale)
		if next > curr {
			for pieces := curr; pieces <= next; pieces++ {
				metrics.piecesToBucketIndex[pieces] = bucket
			}
			curr = next + 1
			continue
		}

		metrics.piecesToBucketIndex[curr] = bucket
		curr++
	}

	return metrics
}

// bucket returns the bucket that corresponds with the given amount of pieces
// that represents the redundancy scheme with which a sector was uploaded
func (am *availabilityMetrics) bucket(numPieces uint64) *availabilityBucket {
	bucketIndex, exists := am.piecesToBucketIndex[numPieces]
	if !exists {
		return am.buckets[len(am.buckets)-1]
	}
	return am.buckets[bucketIndex]
}

// updateMetrics will update the availability metrics for the bucket
// corresponding with the given 'numPieces' parameter.
func (am *availabilityMetrics) updateMetrics(numPieces int, availables []bool) {
	if numPieces < 1 {
		build.Critical("num pieces can never be smaller than 1")
		return
	}

	bucket := am.bucket(uint64(numPieces))
	bucket.totalJobs += uint64(len(availables))
	for _, available := range availables {
		if available {
			bucket.totalAvailable++
		}
	}
}

// callNext overwrites the generic call next and batches a certain number of has
// sector jobs together.
func (jq *jobHasSectorQueue) callNext() workerJob {
	var jobs []*jobHasSector

	for {
		if len(jobs) >= hasSectorBatchSize {
			break
		}
		next := jq.jobGenericQueue.callNext()
		if next == nil {
			break
		}
		j := next.(*jobHasSector)
		jobs = append(jobs, j)
	}
	if len(jobs) == 0 {
		return nil
	}

	return &jobHasSectorBatch{
		staticJobs: jobs,
	}
}

// newJobHasSector is a helper method to create a new HasSector job.
func (w *worker) newJobHasSector(ctx context.Context, responseChan chan *jobHasSectorResponse, numPieces int, roots ...crypto.Hash) *jobHasSector {
	return w.newJobHasSectorWithPostExecutionHook(ctx, responseChan, nil, numPieces, roots...)
}

// newJobHasSectorWithPostExecutionHook is a helper method to create a new
// HasSector job with a post execution hook that is executed after the response
// is available but before sending it over the channel.
func (w *worker) newJobHasSectorWithPostExecutionHook(ctx context.Context, responseChan chan *jobHasSectorResponse, hook func(*jobHasSectorResponse), numPieces int, roots ...crypto.Hash) *jobHasSector {
	span, _ := opentracing.StartSpanFromContext(ctx, "HasSectorJob")
	return &jobHasSector{
		staticNumPieces:         numPieces,
		staticSectors:           roots,
		staticResponseChan:      responseChan,
		staticPostExecutionHook: hook,
		staticSpan:              span,
		jobGeneric:              newJobGeneric(ctx, w.staticJobHasSectorQueue, nil),
	}
}

// callDiscard will discard a job, sending the provided error.
func (j *jobHasSector) callDiscard(err error) {
	w := j.staticQueue.staticWorker()
	errLaunch := w.staticRenter.tg.Launch(func() {
		response := &jobHasSectorResponse{
			staticErr: errors.Extend(err, ErrJobDiscarded),

			staticWorker: w,
		}
		j.managedCallPostExecutionHook(response)
		select {
		case j.staticResponseChan <- response:
		case <-j.staticCtx.Done():
		case <-w.staticRenter.tg.StopChan():
		}
	})
	if errLaunch != nil {
		w.staticRenter.staticLog.Print("callDiscard: launch failed", err)
	}

	j.staticSpan.LogKV("callDiscard", err)
	j.staticSpan.SetTag("success", false)
	j.staticSpan.Finish()
}

// callDiscard discards all jobs within the batch.
func (j jobHasSectorBatch) callDiscard(err error) {
	for _, hsj := range j.staticJobs {
		hsj.callDiscard(err)
	}
}

// staticCanceled always returns false. A batched job never resides in the
// queue. It's constructed right before being executed.
func (j jobHasSectorBatch) staticCanceled() bool {
	return false
}

// staticGetMetadata return an empty struct. A batched has sector job doesn't
// contain any metadata.
func (j jobHasSectorBatch) staticGetMetadata() interface{} {
	return struct{}{}
}

// callExecute will run the has sector job.
func (j *jobHasSector) callExecute() {
	// Finish job span at the end.
	defer j.staticSpan.Finish()

	// Capture callExecute in new span.
	span := opentracing.StartSpan("callExecute", opentracing.ChildOf(j.staticSpan.Context()))
	defer span.Finish()

	batch := jobHasSectorBatch{
		staticJobs: []*jobHasSector{j},
	}
	batch.callExecute()
}

// callExecute will run the has sector job.
func (j jobHasSectorBatch) callExecute() {
	if len(j.staticJobs) == 0 {
		build.Critical("empty hasSectorBatch")
		return
	}

	start := time.Now()
	w := j.staticJobs[0].staticQueue.staticWorker()
	availables, err := j.managedHasSector()
	jobTime := time.Since(start)

	for i := range j.staticJobs {
		hsj := j.staticJobs[i]
		// Handle its span
		if err != nil {
			hsj.staticSpan.LogKV("error", err)
		}
		hsj.staticSpan.SetTag("success", err == nil)
		hsj.staticSpan.Finish()

		// Create the response.
		response := &jobHasSectorResponse{
			staticErr:     err,
			staticJobTime: jobTime,
			staticWorker:  w,
		}
		// If it was successful, attach the result.
		if err == nil {
			hsj.staticSpan.LogKV("availables", availables[i])
			response.staticAvailables = availables[i]
		}
		// Send the response.
		err2 := w.staticRenter.tg.Launch(func() {
			hsj.managedCallPostExecutionHook(response)
			select {
			case hsj.staticResponseChan <- response:
			case <-hsj.staticCtx.Done():
			case <-w.staticRenter.tg.StopChan():
			}
		})
		// Report success or failure to the queue.
		if err != nil {
			hsj.staticQueue.callReportFailure(err)
			continue
		}
		hsj.staticQueue.callReportSuccess()

		// Job was a success, update the performance and availability stats on
		// the queue.
		jq := hsj.staticQueue.(*jobHasSectorQueue)
		jq.callUpdateJobTimeMetrics(jobTime)
		jq.callUpdateAvailabilityMetrics(hsj.staticNumPieces, availables[i])
		if err2 != nil {
			w.staticRenter.staticLog.Println("callExecute: launch failed", err)
		}
	}
}

// callExpectedBandwidth returns the bandwidth that is expected to be consumed
// by the job.
func (j *jobHasSector) callExpectedBandwidth() (ul, dl uint64) {
	// sanity check
	if len(j.staticSectors) == 0 {
		build.Critical("expected bandwidth requested for a job that has no staticSectors set")
	}
	return hasSectorJobExpectedBandwidth(len(j.staticSectors))
}

// callExpectedBandwidth returns the bandwidth that is expected to be consumed
// by the job.
func (j jobHasSectorBatch) callExpectedBandwidth() (ul, dl uint64) {
	var totalSectors int
	for _, hsj := range j.staticJobs {
		// sanity check
		if len(hsj.staticSectors) == 0 {
			build.Critical("expected bandwidth requested for a job that has no staticSectors set")
		}
		totalSectors += len(hsj.staticSectors)
	}
	ul, dl = hasSectorJobExpectedBandwidth(totalSectors)
	return
}

// managedHasSector returns whether or not the host has a sector with given root
func (j *jobHasSectorBatch) managedHasSector() (results [][]bool, err error) {
	if len(j.staticJobs) == 0 {
		return nil, nil
	}

	w := j.staticJobs[0].staticQueue.staticWorker()
	// Create the program.
	pt := w.staticPriceTable().staticPriceTable
	pb := modules.NewProgramBuilder(&pt, 0) // 0 duration since HasSector doesn't depend on it.
	for _, hsj := range j.staticJobs {
		for _, sector := range hsj.staticSectors {
			pb.AddHasSectorInstruction(sector)
		}
	}
	program, programData := pb.Program()
	cost, _, _ := pb.Cost(true)

	// take into account bandwidth costs
	ulBandwidth, dlBandwidth := j.callExpectedBandwidth()
	bandwidthCost := modules.MDMBandwidthCost(pt, ulBandwidth, dlBandwidth)
	cost = cost.Add(bandwidthCost)

	// Execute the program and parse the responses.
	hasSectors := make([]bool, 0, len(program))
	var responses []programResponse
	responses, _, err = w.managedExecuteProgram(program, programData, types.FileContractID{}, categoryDownload, cost)
	if err != nil {
		return nil, errors.AddContext(err, "unable to execute program for has sector job")
	}
	for _, resp := range responses {
		if resp.Error != nil {
			return nil, errors.AddContext(resp.Error, "Output error")
		}
		hasSectors = append(hasSectors, resp.Output[0] == 1)
	}
	if len(responses) != len(program) {
		return nil, errors.New("received invalid number of responses but no error")
	}

	for _, hsj := range j.staticJobs {
		results = append(results, hasSectors[:len(hsj.staticSectors)])
		hasSectors = hasSectors[len(hsj.staticSectors):]
	}
	return results, nil
}

// callAddWithEstimate will add a job to the queue and return a timestamp for
// when the job is estimated to complete. An error will be returned if the job
// is not successfully queued.
func (jq *jobHasSectorQueue) callAddWithEstimate(j *jobHasSector, maxEstimate time.Duration) (time.Time, error) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	now := time.Now()
	estimate := jq.expectedJobTime()
	if estimate > maxEstimate {
		return time.Time{}, errEstimateAboveMax
	}
	j.externJobStartTime = now
	j.externEstimatedJobDuration = estimate
	if !jq.add(j) {
		return time.Time{}, errors.New("unable to add job to queue")
	}
	return now.Add(estimate), nil
}

// callExpectedJobTime returns the expected amount of time that this job will
// take to complete.
//
// TODO: idealy we pass `numSectors` here and get the expected job time
// depending on the amount of instructions in the program.
func (jq *jobHasSectorQueue) callExpectedJobTime() time.Duration {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	return jq.expectedJobTime()
}

// callAvailabilityRate returns the percentage of jobs that came back having the
// sector for this queue's worker.
func (jq *jobHasSectorQueue) callAvailabilityRate(numPieces int) float64 {
	jq.mu.Lock()
	defer jq.mu.Unlock()

	// assert the given value for num pieces makes sense, we throw a critical
	// here as this can only be caused by developer error
	if numPieces < 1 {
		build.Critical("num pieces can never be smaller than 1")
		return 0
	}

	// fetch the bucket that corresponds with the given redundancy
	bucket := jq.availabilityMetrics.bucket(uint64(numPieces))

	// if there haven't been any jobs yet where the sector was available on the
	// host, we return a minimum rate of .1% to avoid multiplication by zero in
	// our download code algorithms.
	if bucket.totalAvailable == 0 || bucket.totalJobs == 0 {
		return jobHasSectorQueueMinAvailabilityRate
	}

	return float64(bucket.totalAvailable) / float64(bucket.totalJobs)
}

// callUpdateAvailabilityMetrics updates the fields on the has sector queue that
// keep track of how many jobs were executed successfully, and how many jobs had
// the sector be available.
func (jq *jobHasSectorQueue) callUpdateAvailabilityMetrics(numPieces int, availables []bool) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	jq.availabilityMetrics.updateMetrics(numPieces, availables)
}

// callUpdateJobTimeMetrics takes a duration it took to fulfil that job and uses
// it to update the job performance metrics on the queue.
func (jq *jobHasSectorQueue) callUpdateJobTimeMetrics(jobTime time.Duration) {
	jq.mu.Lock()
	defer jq.mu.Unlock()
	jq.weightedJobTime = expMovingAvgHotStart(jq.weightedJobTime, float64(jobTime), jobHasSectorPerformanceDecay)
}

// expectedJobTime will return the amount of time that a job is expected to
// take, given the current conditions of the queue.
func (jq *jobHasSectorQueue) expectedJobTime() time.Duration {
	return time.Duration(jq.weightedJobTime)
}

// initJobHasSectorQueue will init the queue for the has sector jobs.
func (w *worker) initJobHasSectorQueue() {
	// Sanity check that there is no existing job queue.
	if w.staticJobHasSectorQueue != nil {
		w.staticRenter.staticLog.Critical("incorret call on initJobHasSectorQueue")
		return
	}

	w.staticJobHasSectorQueue = &jobHasSectorQueue{
		availabilityMetrics: newAvailabilityMetrics(),
		jobGenericQueue:     newJobGenericQueue(w),
	}
}

// managedCallPostExecutionHook calls a post execution hook if registered. The
// hook will only be called the first time this method is executed. Subsequent
// calls are no-ops.
func (j *jobHasSector) managedCallPostExecutionHook(resp *jobHasSectorResponse) {
	if j.staticPostExecutionHook == nil {
		return // nothing to do
	}
	j.once.Do(func() {
		j.staticPostExecutionHook(resp)
	})
}

// hasSectorJobExpectedBandwidth is a helper function that returns the expected
// bandwidth consumption of a has sector job. This helper function enables
// getting at the expected bandwidth without having to instantiate a job.
func hasSectorJobExpectedBandwidth(numRoots int) (ul, dl uint64) {
	// closestMultipleOf is a small helper function that essentially rounds up
	// 'num' to the closest multiple of 'multipleOf'.
	closestMultipleOf := func(num, multipleOf int) int {
		mod := num % multipleOf
		if mod != 0 {
			num += (multipleOf - mod)
		}
		return num
	}

	// A HS job consumes more than one packet on download as soon as it contains
	// 13 roots or more. In terms of upload bandwidth that threshold is at 17.
	// To be conservative we use 10 and 15 as cutoff points.
	downloadMultiplier := closestMultipleOf(numRoots, 10) / 10
	uploadMultiplier := closestMultipleOf(numRoots, 15) / 15

	// A base of 1500 is used for the packet size. On ipv4, it is technically
	// smaller, but siamux is general and the packet size is the Ethernet MTU
	// (1500 bytes) minus any protocol overheads. It's possible if the renter is
	// connected directly over an interface to a host that there is no overhead,
	// which means siamux could use the full 1500 bytes. So we use the most
	// conservative value here as well.
	ul = uint64(1500 * uploadMultiplier)
	dl = uint64(1500 * downloadMultiplier)
	return
}
