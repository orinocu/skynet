package renter

// workerjobdownloadroot.go defines the job to download a sector from a host
// using the root.

import (
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/crypto"
	"gitlab.com/NebulousLabs/Sia/modules"

	"gitlab.com/NebulousLabs/errors"
)

const (
	// jobReadSectorPerformanceDecay defines how much decay gets applied to the
	// historic performance of jobReadSector each time new data comes back.
	// Setting a low value makes the performance more volatile. If the worker
	// tends to have inconsistent performance, having the decay be a low value
	// (0.9 or lower) will be highly detrimental. A higher decay means that the
	// predictor tends to be more accurate over time, but is less responsive to
	// things like network load.
	jobReadSectorPerformanceDecay = 0.95
)

type (
	// jobReadSector contains information about a hasSector query.
	jobReadSector struct {
		canceled     chan struct{}               // Can signal that the job has been canceled
		responseChan chan *jobReadSectorResponse // Channel to send a response down

		length uint64
		offset uint64
		sector crypto.Hash
	}

	// jobReadSectorQueue is a list of hasSector queries that have been assigned
	// to the worker. The queue also tracks performance metrics, which can then
	// be used by projects to optimize job scheduling between workers.
	jobReadSectorQueue struct {
		killed bool
		jobs   []jobReadSector

		// These float64s are converted time.Duration values. They are float64
		// to get better precision on the exponential decay which gets applied
		// with each new data point.
		totalJobTime float64
		totalJobs    float64

		// TODO: This is really just for curiosity.
		fastestJob time.Duration

		staticWorker *worker
		mu           sync.Mutex
	}

	// jobReadSectorResponse contains the result of a hasSector query.
	jobReadSectorResponse struct {
		staticData []byte
		staticErr  error
	}
)

// newJobReadSectorQueue will initialize a queue for downloading sectors by
// their root for the worker. This is only meant to be run once at startup.
func (w *worker) newJobReadSectorQueue() {
	// Sanity check that there is no existing job queue.
	if w.staticJobReadSectorQueue != nil {
		w.renter.log.Critical("incorred call on newJobReadSectorQueue")
	}
	w.staticJobReadSectorQueue = &jobReadSectorQueue{
		staticWorker: w,
	}
}

// staticCanceled is a convenience function to check whether a job has been
// canceled.
func (j *jobReadSector) staticCanceled() bool {
	select {
	case <-j.canceled:
		return true
	default:
		return false
	}
}

// callAdd will add a job to the queue. False will be returned if the job cannot
// be queued because the worker has been killed.
func (jq *jobReadSectorQueue) callAdd(job jobReadSector) bool {
	defer jq.staticWorker.staticWake()
	jq.mu.Lock()
	defer jq.mu.Unlock()

	if jq.killed {
		return false
	}
	jq.jobs = append(jq.jobs, job)
	return true
}

// callNext will provide the next jobReadSector from the set of jobs.
func (jq *jobReadSectorQueue) callNext() (func(), uint64, uint64) {
	var job jobReadSector
	jq.mu.Lock()
	for {
		if len(jq.jobs) == 0 {
			jq.mu.Unlock()
			return nil, 0, 0
		}

		// Grab the next job.
		job = jq.jobs[0]
		jq.jobs = jq.jobs[1:]

		// Break out of the loop only if this job has not been canceled.
		if job.staticCanceled() {
			continue
		}
		break
	}
	jq.mu.Unlock()

	// Create the actual job that will be run by the async job launcher.
	jobFn := func() {
		// Track how long the job takes.
		start := time.Now()
		data, err := jq.staticWorker.managedReadSector(job.sector, job.offset, job.length)
		jobTime := time.Since(start)
		response := &jobReadSectorResponse{
			staticData: data,
			staticErr:  err,
		}

		// Update the metrics in the read sector queue based on the amount of
		// time the read took.
		jq.mu.Lock()
		jq.totalJobTime *= 0.9
		jq.totalJobs *= 0.9
		jq.totalJobTime += float64(jobTime)
		jq.totalJobs++
		if err == nil && len(data) > 3e6 && (jobTime < jq.fastestJob || jq.fastestJob == 0) {
			jq.fastestJob = jobTime
		}
		jq.mu.Unlock()

		// Send the response in a goroutine so that the worker resources can be
		// released faster. Need to check if the job was canceled so that the
		// memory can be released.
		go func() {
			select {
			case job.responseChan <- response:
			case <-job.canceled:
			}
		}()
	}

	// Return the job along with the bandwidth estimates for completing the job.
	ulBandwidth, dlBandwidth := programReadSectorBandwidth(job.offset, job.length)
	return jobFn, ulBandwidth, dlBandwidth
}

// TODO: Have this take a size of a file as input.
func (jq *jobReadSectorQueue) callAverageJobTime() time.Duration {
	jq.mu.Lock()
	avg := time.Duration(jq.totalJobTime / jq.totalJobs)
	jq.mu.Unlock()
	return avg
}

// programReadSectorBandwidth returns the bandwidth that gets consumed by a
// ReadSector program.
//
// TODO: These values are overly conservative, once we've got the protocol more
// optimized we can bring these down.
func programReadSectorBandwidth(offset, length uint64) (ulBandwidth, dlBandwidth uint64) {
	ulBandwidth = 1 << 15                              // 32 KiB
	dlBandwidth = uint64(float64(length)*1.01) + 1<<14 // (readSize * 1.01 + 16 KiB)
	return
}

// managedReadSector returns the sector data for given root
func (w *worker) managedReadSector(sectorRoot crypto.Hash, offset, length uint64) ([]byte, error) {
	// create the program
	pt := w.staticPriceTable().staticPriceTable
	pb := modules.NewProgramBuilder(&pt)
	pb.AddReadSectorInstruction(length, offset, sectorRoot, true)
	program, programData := pb.Program()
	cost, _, _ := pb.Cost(true)

	// take into account bandwidth costs
	ulBandwidth, dlBandwidth := programReadSectorBandwidth(offset, length)
	bandwidthCost := modules.MDMBandwidthCost(pt, ulBandwidth, dlBandwidth)
	cost = cost.Add(bandwidthCost)

	// exeucte it
	//
	// TODO: for this program we don't actually need the file contract - v149
	// only.
	responses, err := w.managedExecuteProgram(program, programData, w.staticCache().staticContractID, cost)
	if err != nil {
		return nil, err
	}

	// return the response
	var sectorData []byte
	for _, resp := range responses {
		if resp.Error != nil {
			return nil, resp.Error
		}
		sectorData = resp.Output
		break
	}
	return sectorData, nil
}

// managedDumpJobsReadSector will release all remaining ReadSector jobs as failed.
func (w *worker) managedDumpJobsReadSector() {
	jq := w.staticJobReadSectorQueue // Convenience variable
	jq.mu.Lock()
	defer jq.mu.Unlock()
	for _, job := range jq.jobs {
		// Send the response in a goroutine so that the worker resources can be
		// released faster.
		go func(j jobReadSector) {
			response := &jobReadSectorResponse{
				staticErr: errors.New("worker is dumping all read sector jobs"),
			}
			j.responseChan <- response
		}(job)
	}
	jq.jobs = nil
}

// managedKillJobsReadSector will release all remaining ReadSector jobs as failed.
func (w *worker) managedKillJobsReadSector() {
	jq := w.staticJobReadSectorQueue // Convenience variable
	jq.mu.Lock()
	defer jq.mu.Unlock()
	for _, job := range jq.jobs {
		// Send the response in a goroutine so that the worker resources can be
		// released faster.
		go func(j jobReadSector) {
			response := &jobReadSectorResponse{
				staticErr: errors.New("worker killed"),
			}
			j.responseChan <- response
		}(job)
	}
	jq.killed = true
	jq.jobs = nil
}
