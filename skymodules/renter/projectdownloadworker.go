package renter

import (
	"fmt"
	"sort"
	"time"

	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skymodules"
	"go.sia.tech/siad/types"
)

// NOTE: all of the following defined types are used by the PDC, which is
// inherently thread un-safe, that means that these types don't not need to be
// thread safe either. If fields are marked `static` it is meant to signal they
// won't change after being set.
type (
	// downloadWorker is an interface implemented by both the individual and
	// chimera workers that represents a worker that can be used for downloads.
	downloadWorker interface {
		identifier() string

		// cost returns the expected job cost for downloading a piece of data
		// with given length from the worker. If the worker has already been
		// launched, its cost will be zero.
		cost(length uint64) types.Currency

		// distribution returns the worker's read distribution, for an already
		// launched worker the distribution will have been shifted by the amount
		// of time since it was launched.
		distribution() *skymodules.Distribution

		// pieces returns all piece indices this worker can resolve
		pieces() []uint64

		// worker returns the underlying worker
		worker() *worker

		// markPieceForDownload marks a piece as the one to download
		markPieceForDownload(pieceIndex uint64)

		getPieceForDownload() (uint64, bool)

		// TODO: remove me (debugging)
		launched() bool
		chimera() bool
	}

	// chimeraWorker is a worker that's built from unresolved workers until the
	// chance it has a piece is exactly 1. At that point we can treat a chimera
	// worker exactly the same as a resolved worker in the download algorithm
	// that constructs the best worker set.
	chimeraWorker struct {
		// cachedDistribution contains a distribution that is the weighted
		// combination of all worker distrubtions in this chimera worker, it is
		// cached meaning it will only be calculated the first time the
		// distribution is requested after the chimera worker was finalized.
		cachedDistribution *skymodules.Distribution

		// remaining keeps track of how much "chance" is remaining until the
		// chimeraworker is comprised of enough to workers to be able to resolve
		// a piece. This is a helper field that avoids calculating
		// 1-SUM(weights) over and over again
		remaining float64

		distributions []*skymodules.Distribution
		weights       []float64
		workers       []*worker

		staticPieceIndices []uint64
	}

	// individualWorker is a struct that represents a single worker object, both
	// resolved and unresolved workers in the pdc can be represented by an
	// individual worker. An individual worker can be used to build a chimera
	// worker with.
	//
	// NOTE: extending this struct requires an update to the `split` method.
	individualWorker struct {
		launchedAt    time.Time
		pieceIndices  []uint64
		resolveChance float64

		currentPiece uint64

		staticLookupDistribution *skymodules.Distribution
		staticReadDistribution   *skymodules.Distribution
		staticWorker             *worker
	}

	// workerSet is a collection of workers that may or may not have been
	// launched yet in order to fulfil a download.
	workerSet struct {
		workers []downloadWorker

		staticExpectedDuration time.Duration
		staticLength           uint64
		staticMinPieces        int
	}

	// coinflips is a collection of chances where every item is the chance the
	// coin will turn up heads. We use the concept of a coin because it allows
	// to more easily reason about chance calculations.
	coinflips []float64
)

// TODO: remove me (debugging)
func (iw *individualWorker) chimera() bool {
	return false
}

// TODO: remove me (debugging)
func (cw *chimeraWorker) chimera() bool {
	return true
}

// TODO: remove me (debugging)
func (iw *individualWorker) identifier() string {
	return iw.staticWorker.staticHostPubKey.ShortString()
}

// TODO: remove me (debugging)
func (cw *chimeraWorker) identifier() string {
	return "chimera"
}

// TODO: remove me (debugging)
func (iw *individualWorker) launched() bool {
	return !iw.launchedAt.IsZero()
}

// TODO: remove me (debugging)
func (cw *chimeraWorker) launched() bool {
	return false
}

// TODO: remove me (debugging)
func (iw *individualWorker) markPieceForDownload(pieceIndex uint64) {
	iw.currentPiece = pieceIndex
}

// TODO: remove me (debugging)
func (cw *chimeraWorker) markPieceForDownload(pieceIndex uint64) {
	// this is a no-op
}

// TODO: remove me (debugging)
func (iw *individualWorker) getPieceForDownload() (uint64, bool) {
	return iw.currentPiece, false
}

// TODO: remove me (debugging)
func (cw *chimeraWorker) getPieceForDownload() (uint64, bool) {
	return 0, true
}

// NewChimeraWorker returns a new chimera worker object.
func NewChimeraWorker(numPieces int) *chimeraWorker {
	pieceIndices := make([]uint64, numPieces)
	for i := 0; i < numPieces; i++ {
		pieceIndices[i] = uint64(i)
	}
	return &chimeraWorker{remaining: 1, staticPieceIndices: pieceIndices}
}

// addWorker adds the given worker to the chimera worker.
func (cw *chimeraWorker) addWorker(w *individualWorker) *individualWorker {
	// calculate the remaining chance this chimera worker needs to be complete
	if cw.remaining == 0 {
		return w
	}

	// the given worker's chance can be higher than the remaining chance of this
	// chimera worker, in that case we have to split the worker in a part we
	// want to add, and a remainder we'll use to build the next chimera with
	toAdd := w
	var remainder *individualWorker
	if w.resolveChance > cw.remaining {
		toAdd, remainder = w.split(cw.remaining)
	}

	// update the remaining chance
	cw.remaining -= toAdd.resolveChance

	// add the worker to the chimera
	cw.distributions = append(cw.distributions, toAdd.staticReadDistribution)
	cw.weights = append(cw.weights, toAdd.resolveChance)
	cw.workers = append(cw.workers, toAdd.staticWorker)
	return remainder
}

// cost implements the downloadWorker interface.
func (cw *chimeraWorker) cost(length uint64) types.Currency {
	numWorkers := uint64(len(cw.workers))
	if numWorkers == 0 {
		return types.ZeroCurrency
	}

	var total types.Currency
	for _, w := range cw.workers {
		total = total.Add(w.staticJobReadQueue.callExpectedJobCost(length))
	}
	return total.Div64(numWorkers)
}

// distribution implements the downloadWorker interface.
func (cw *chimeraWorker) distribution() *skymodules.Distribution {
	if cw.remaining != 0 {
		build.Critical("developer error, chimera is not complete")
		return nil
	}

	if cw.cachedDistribution == nil && len(cw.distributions) > 0 {
		halfLife := cw.distributions[0].HalfLife()
		cw.cachedDistribution = skymodules.NewDistribution(halfLife)
		for i, distribution := range cw.distributions {
			cw.cachedDistribution.MergeWith(distribution, cw.weights[i])
		}
	}
	return cw.cachedDistribution
}

// pieces implements the downloadWorker interface, chimera workers return all
// pieces as we don't know yet what pieces they can resolve
func (cw *chimeraWorker) pieces() []uint64 {
	return cw.staticPieceIndices
}

// worker implements the downloadWorker interface, chimera workers return nil
// since it's comprised of multiple workers
func (cw *chimeraWorker) worker() *worker {
	return nil
}

// cost implements the downloadWorker interface.
func (iw *individualWorker) cost(length uint64) types.Currency {
	// workers that have already been launched have a zero cost
	if iw.isLaunched() {
		return types.ZeroCurrency
	}
	return iw.staticWorker.staticJobReadQueue.callExpectedJobCost(length)
}

// distribution implements the downloadWorker interface.
func (iw *individualWorker) distribution() *skymodules.Distribution {
	// if the worker has been launched already, we want to shift the
	// distribution with the time that elapsed since it was launched
	//
	// NOTE: we always shift on a clone of the original read distribution to
	// avoid shifting the same distribution multiple times
	if iw.isLaunched() {
		clone := iw.staticReadDistribution.Clone()
		clone.Shift(time.Since(iw.launchedAt))
		return clone
	}
	return iw.staticReadDistribution
}

// isLaunched returns true when this workers has been launched.
func (iw *individualWorker) isLaunched() bool {
	return !iw.launchedAt.IsZero()
}

// pieces implements the downloadWorker interface.
func (iw *individualWorker) pieces() []uint64 {
	return iw.pieceIndices
}

// worker implements the downloadWorker interface.
func (iw *individualWorker) worker() *worker {
	return iw.staticWorker
}

// split will split the download worker into two workers, the first worker will
// have the given chance, the second worker will have the remainder as its
// chance value.
func (iw *individualWorker) split(chance float64) (*individualWorker, *individualWorker) {
	if chance >= iw.resolveChance {
		build.Critical("chance value on which we split should be strictly less than the worker's resolve chance")
		return nil, nil
	}

	main := &individualWorker{
		launchedAt:    iw.launchedAt,
		pieceIndices:  iw.pieceIndices,
		resolveChance: chance,

		staticLookupDistribution: iw.staticLookupDistribution,
		staticReadDistribution:   iw.staticReadDistribution,
		staticWorker:             iw.staticWorker,
	}
	remainder := &individualWorker{
		launchedAt:    iw.launchedAt,
		pieceIndices:  iw.pieceIndices,
		resolveChance: iw.resolveChance - chance,

		staticLookupDistribution: iw.staticLookupDistribution,
		staticReadDistribution:   iw.staticReadDistribution,
		staticWorker:             iw.staticWorker,
	}

	return main, remainder
}

// clone returns a shallow copy of the worker set.
func (ws *workerSet) clone() *workerSet {
	return &workerSet{
		workers: append([]downloadWorker{}, ws.workers...),

		staticExpectedDuration: ws.staticExpectedDuration,
		staticLength:           ws.staticLength,
		staticMinPieces:        ws.staticMinPieces,
	}
}

// cheaperSetFromCandidate returns a new worker set if the given candidate
// worker can improve the cost of the worker set. The worker that is being
// swapped by the candidate is the most expensive worker possible, which is not
// necessarily the most expensive worker in the set because we have to take into
// account the pieces the worker can download.
func (ws *workerSet) cheaperSetFromCandidate(candidate downloadWorker) *workerSet {
	// build two maps for fast lookups
	workersToIndex := make(map[string]int)
	pieceToIndex := make(map[uint64]int)
	for i, w := range ws.workers {
		// map the worker index
		key := w.worker().staticHostPubKeyStr
		workersToIndex[key] = i

		// map the piece to the worker index, if the worker is a chimera worker
		// we don't have to map the piece because a chimera worker can
		// theoretically still resolve all pieces
		piece, chimera := w.getPieceForDownload()
		if !chimera {
			pieceToIndex[piece] = i
		}
	}

	// sort the workers by cost, most expensive to cheapest
	byCostDesc := append([]downloadWorker{}, ws.workers...)
	sort.Slice(byCostDesc, func(i, j int) bool {
		wCostI := byCostDesc[i].cost(ws.staticLength)
		wCostJ := byCostDesc[j].cost(ws.staticLength)
		return wCostI.Cmp(wCostJ) > 0
	})

	// range over the workers
	swapIndex := -1
LOOP:
	for _, expensiveWorker := range byCostDesc {
		expensiveWorkerKey := expensiveWorker.worker().staticHostPubKeyStr
		expensiveWorkerIndex := workersToIndex[expensiveWorkerKey]

		// if the candidate is not cheaper than this worker we can stop looking
		// to build a cheaper set since the workers are sorted by cost
		if candidate.cost(ws.staticLength).Cmp(expensiveWorker.cost(ws.staticLength)) >= 0 {
			break
		}

		// the candidate is only useful if it can replace a worker and
		// contribute pieces to the worker set for which we don't already have
		// another worker
		for _, piece := range candidate.pieces() {
			currentWorkerIndex, exists := pieceToIndex[piece]
			if !exists || currentWorkerIndex == expensiveWorkerIndex {
				swapIndex = expensiveWorkerIndex
				break LOOP
			}
		}
	}

	if swapIndex > -1 {
		cheaperSet := ws.clone()
		cheaperSet.workers[swapIndex] = candidate
		return cheaperSet
	}
	return nil
}

// adjustedDuration returns the cost adjusted expected duration of the worker
// set using the given price per ms.
func (ws *workerSet) adjustedDuration(ppms types.Currency) time.Duration {
	// calculate the total cost of the worker set
	var totalCost types.Currency
	for _, w := range ws.workers {
		totalCost = totalCost.Add(w.cost(ws.staticLength))
	}

	// calculate the cost penalty using the given price per ms and apply it to
	// the worker set's expected duration.
	return addCostPenalty(ws.staticExpectedDuration, totalCost, ppms)
}

// chancesAfter is a small helper function that returns a list of every worker's
// chance it's completed after the given duration.
func (ws workerSet) chancesAfter(dur time.Duration) coinflips {
	chances := make(coinflips, len(ws.workers))
	for i, w := range ws.workers {
		chances[i] = w.distribution().ChanceAfter(dur)
	}
	return chances
}

// chanceGreaterThanHalf returns whether the total chance this worker set
// completes the download before the given duration is more than 50%.
//
// NOTE: this function abstracts the chance a worker resolves after the given
// duration as a coinflip to make it easier to reason about the problem given
// that the workerset consists out of one or more overdrive workers.
func (ws workerSet) chanceGreaterThanHalf(dur time.Duration) bool {
	// convert every worker into a coinflip
	coinflips := ws.chancesAfter(dur)

	var chance float64
	switch ws.numOverdriveWorkers() {
	case 0:
		// if we don't have to consider any overdrive workers, the chance it's
		// all heads is the chance that needs to be greater than half
		chance = coinflips.chanceAllHeads()
	case 1:
		// if there is 1 overdrive worker, we can essentially have one of the
		// coinflips come up as tails, as long as all the others are heads
		chance = coinflips.chanceHeadsAllowOneTails()
	case 2:
		// if there are 2 overdrive workers, we can have two of them come up as
		// tails, as long as all the others are heads
		chance = coinflips.chanceHeadsAllowTwoTails()
	default:
		// if there are a lot of overdrive workers, we use an approximation by
		// summing all coinflips to see whether we are expected to be able to
		// download min pieces within the given duration
		return coinflips.chanceSum() > float64(ws.staticMinPieces)
	}

	return chance > 0.5
}

// numOverdriveWorkers returns the number of overdrive workers in the worker
// set.
func (ws workerSet) numOverdriveWorkers() int {
	numWorkers := len(ws.workers)
	if numWorkers < ws.staticMinPieces {
		return 0
	}
	return numWorkers - ws.staticMinPieces
}

// chanceAllHeads returns the chance all coins show heads.
func (cf coinflips) chanceAllHeads() float64 {
	if len(cf) == 0 {
		return 0
	}

	chanceAllHeads := float64(1)
	for _, chanceHead := range cf {
		chanceAllHeads *= chanceHead
	}
	return chanceAllHeads
}

// chanceHeadsAllowOneTails returns the chance at least n-1 coins show heads
// where n is the amount of coins.
func (cf coinflips) chanceHeadsAllowOneTails() float64 {
	chanceAllHeads := cf.chanceAllHeads()

	totalChance := chanceAllHeads
	for _, chanceHead := range cf {
		chanceTails := 1 - chanceHead
		totalChance += (chanceAllHeads / chanceHead * chanceTails)
	}
	return totalChance
}

// chanceHeadsAllowTwoTails returns the chance at least n-2 coins show heads
// where n is the amount of coins.
func (cf coinflips) chanceHeadsAllowTwoTails() float64 {
	chanceAllHeads := cf.chanceAllHeads()
	totalChance := cf.chanceHeadsAllowOneTails()

	for i := 0; i < len(cf)-1; i++ {
		chanceIHeads := cf[i]
		chanceITails := 1 - chanceIHeads
		chanceOnlyITails := chanceAllHeads / chanceIHeads * chanceITails
		for jj := i + 1; jj < len(cf); jj++ {
			chanceJHeads := cf[jj]
			chanceJTails := 1 - chanceJHeads
			chanceOnlyIAndJJTails := chanceOnlyITails / chanceJHeads * chanceJTails
			totalChance += chanceOnlyIAndJJTails
		}
	}
	return totalChance
}

// chanceSum returns the sum of all chances
func (cf coinflips) chanceSum() float64 {
	var sum float64
	for _, flip := range cf {
		sum += flip
	}
	return sum
}

// updateWorkers
func (pdc *projectDownloadChunk) updateWorkers(workers []*individualWorker) {
	ws := pdc.workerState
	ws.mu.Lock()
	defer ws.mu.Unlock()

	// create two maps to help update the workers
	resolved := make(map[string]int)
	unresolved := make(map[string]int)
	for i, w := range workers {
		if w.resolveChance == 1 {
			resolved[w.staticWorker.staticHostPubKeyStr] = i
		} else {
			unresolved[w.staticWorker.staticHostPubKeyStr] = i
		}
	}

	// now iterate over all resolved workers, if we did not have that worker as
	// resolved before, it became resolved and we want to update it
	for _, rw := range ws.resolvedWorkers {
		rwIndex, rwExists := resolved[rw.worker.staticHostPubKeyStr]
		uwIndex, uwExists := unresolved[rw.worker.staticHostPubKeyStr]

		// handle the edge case where both don't exist, this might happen when a
		// worker is not part of the worker array because it was deemed unfit
		// for downloading
		if !rwExists && !uwExists {
			continue
		}

		// if it became resolved, update the worker accordingly
		if !rwExists && uwExists {
			if cap(workers[uwIndex].pieceIndices) != cap(rw.pieceIndices) {
				fmt.Printf("CAP WRONG %v != %v\n", cap(workers[uwIndex].pieceIndices), cap(rw.pieceIndices))
			}
			workers[uwIndex].pieceIndices = rw.pieceIndices
			workers[uwIndex].resolveChance = 1
			continue
		}

		// if it was resolved already, we want to update a couple of fields
		if rwExists {
			// update the launched at
			if workers[rwIndex].launchedAt.IsZero() {
				workers[rwIndex].launchedAt = pdc.launchedAt(rw.worker)
			}

			// update the piece indices
			workers[uwIndex].pieceIndices = pdc.filterCompletedPieceIndices(rw.worker, workers[uwIndex].pieceIndices)
		}
	}
}

// workers returns the resolved and unresolved workers as separate slices
func (pdc *projectDownloadChunk) workers() []*individualWorker {
	ws := pdc.workerState
	ws.mu.Lock()
	defer ws.mu.Unlock()

	var workers []*individualWorker

	// convenience variables
	ec := pdc.workerSet.staticErasureCoder
	length := pdc.pieceLength

	// add all resolved workers that are deemed good for downloading
	for _, rw := range ws.resolvedWorkers {
		if !isGoodForDownload(rw.worker) {
			continue
		}

		stats := rw.worker.staticJobReadQueue.staticStats
		rdt := stats.distributionTrackerForLength(length)
		ldt := rw.worker.staticJobHasSectorQueue.staticDT
		workers = append(workers, &individualWorker{
			launchedAt:    pdc.launchedAt(rw.worker),
			pieceIndices:  rw.pieceIndices,
			resolveChance: 1,

			staticLookupDistribution: ldt.Distribution(0),
			staticReadDistribution:   rdt.Distribution(0),
			staticWorker:             rw.worker,
		})
	}

	// add all unresolved workers that are deemed good for downloading
	for _, uw := range ws.unresolvedWorkers {
		w := uw.staticWorker
		stats := w.staticJobReadQueue.staticStats
		rdt := stats.distributionTrackerForLength(length)
		ldt := w.staticJobHasSectorQueue.staticDT

		// exclude workers that are not useful
		if !isGoodForDownload(w) {
			continue
		}

		// unresolved workers can still have all pieces
		pieceIndices := make([]uint64, ec.MinPieces())
		for i := 0; i < len(pieceIndices); i++ {
			pieceIndices[i] = uint64(i)
		}

		workers = append(workers, &individualWorker{
			pieceIndices:  pieceIndices,
			resolveChance: w.staticJobHasSectorQueue.callAvailabilityRate(ec.NumPieces()),

			staticLookupDistribution: ldt.Distribution(0),
			staticReadDistribution:   rdt.Distribution(0),
			staticWorker:             w,
		})
	}

	return workers
}

// launchedAt returns the time at which this worker was launched.
func (pdc *projectDownloadChunk) launchedAt(w *worker) time.Time {
	for _, lw := range pdc.launchedWorkers {
		if lw.staticWorker.staticHostPubKeyStr == w.staticHostPubKeyStr {
			return lw.staticLaunchTime
		}
	}
	return time.Time{}
}
func (pdc *projectDownloadChunk) filterCompletedPieceIndices(w *worker, pieceIndices []uint64) []uint64 {
	completed := make(map[uint64]struct{})
	for _, lw := range pdc.launchedWorkers {
		if lw.staticWorker.staticHostPubKeyStr == w.staticHostPubKeyStr && !lw.completeTime.IsZero() {
			completed[lw.staticPieceIndex] = struct{}{}
		}
	}

	var filtered []uint64
	for _, pi := range pieceIndices {
		if _, exists := completed[pi]; !exists {
			filtered = append(filtered, pi)
		}
	}
	return filtered
}

// workerIsDownloading returns true if the given worker is launched and has not
// returned yet.
func (pdc *projectDownloadChunk) isLaunched(w *worker, piece uint64) bool {
	for _, lw := range pdc.launchedWorkers {
		fmt.Printf("launched worker %v is downloading piece %v and is complete: %v\n", lw.staticWorker.staticHostPubKey.ShortString(), lw.staticPieceIndex, lw.completeTime)
		// check if piece matches
		if lw.staticPieceIndex != piece {
			continue
		}
		// check if worker matches
		if lw.staticWorker.staticHostPubKeyStr != w.staticHostPubKeyStr {
			continue
		}
		return true
	}
	return false
}

// launchWorkerSet will try to launch every wo
func (pdc *projectDownloadChunk) launchWorkerSet(ws *workerSet) {
	fmt.Println("launching set")
	// convenience variables
	minPieces := pdc.workerSet.staticErasureCoder.MinPieces()

	// range over all workers in the set and launch if possible
	for _, w := range ws.workers {
		// continue if the worker is a chimera worker
		piece, chimera := w.getPieceForDownload()
		if chimera {
			fmt.Println("skip because chimera")
			continue
		}

		worker := w.worker()
		workerStr := w.identifier()

		// continue if worker is still downloading
		if pdc.isLaunched(worker, piece) {
			fmt.Println("skip because downloading")
			continue
		}

		// launch the piece
		isOverdrive := len(pdc.launchedWorkers) >= minPieces
		_, launched := pdc.launchWorker(worker, piece, isOverdrive)
		if launched {
			fmt.Printf("launched worker %v for piece %v\n", workerStr, piece)
		}
	}
	return
}

// launchWorkers performs the main download loop, every iteration we update the
// pdc's available pieces, construct a new worker set and launch every worker
// that can be launched from that set. Every iteration we check whether the
// download was finished.
func (pdc *projectDownloadChunk) launchWorkers() {
	// register for a worker update chan
	ws := pdc.workerState
	ws.mu.Lock()
	workerUpdateChan := ws.registerForWorkerUpdate()
	ws.mu.Unlock()

	// update the available pieces
	pdc.updateAvailablePieces()

	// grab the workers, this set of workers will not change but rather get
	// updated to avoid needless performing gouging checks on every iteration
	workers := pdc.workers()

	for {
		// create a worker set and launch it
		workerSet, err := pdc.createWorkerSet(workers, maxOverdriveWorkers)
		if err != nil {
			pdc.fail(err)
			return
		}
		if workerSet != nil {
			pdc.launchWorkerSet(workerSet)
		}

		// iterate
		select {
		case <-time.After(time.Second): // TODO update to 20 * time.Millisecond
			// recreate the workerset every 20ms
		case <-workerUpdateChan:
			// update the available pieces list
			pdc.updateAvailablePieces()

			// register for another update chan
			ws := pdc.workerState
			ws.mu.Lock()
			workerUpdateChan = ws.registerForWorkerUpdate()
			ws.mu.Unlock()
		case jrr := <-pdc.workerResponseChan:
			pdc.handleJobReadResponse(jrr)

			// check whether the download is completed
			completed, err := pdc.finished()
			if completed {
				pdc.finalize()
				return
			}
			if err != nil {
				pdc.fail(err)
				return
			}
		case <-pdc.ctx.Done():
			pdc.fail(errors.New("download timed out"))
			return
		case <-time.After(time.Minute):
			build.Critical("download timed out after 1m, a download should always have a reasonable timeout")
			return
		}

		// update the workers on every iteration
		pdc.updateWorkers(workers)
	}
}

// createWorkerSet tries to create a worker set from the pdc's resolved and
// unresolved workers, the maximum amount of overdrive workers in the set is
// defined by the given 'maxOverdriveWorkers' argument.
func (pdc *projectDownloadChunk) createWorkerSet(allWorkers []*individualWorker, maxOverdriveWorkers int) (*workerSet, error) {
	// convenience variables
	ppms := pdc.pricePerMS
	length := pdc.pieceLength
	minPieces := pdc.workerSet.staticErasureCoder.MinPieces()
	numPieces := pdc.workerSet.staticErasureCoder.NumPieces()

	// split the given workers in resolved and unresolved workers
	var resolvedWorkers []*individualWorker
	var unresolvedWorkers []*individualWorker
	for _, iw := range allWorkers {
		if iw.resolveChance == 1 {
			resolvedWorkers = append(resolvedWorkers, iw)
		} else {
			unresolvedWorkers = append(unresolvedWorkers, iw)
		}
	}

	fmt.Printf("creating worker set, resolved %v unresolved %v\n", len(resolvedWorkers), len(unresolvedWorkers))

	// verify we have enough workers to complete the download
	if len(allWorkers) < minPieces {
		return nil, errors.Compose(ErrRootNotFound, errors.AddContext(errNotEnoughWorkers, fmt.Sprintf("%v < %v", len(allWorkers), minPieces)))
	}

	// sort unresolved workers by expected resolve time
	sort.Slice(unresolvedWorkers, func(i, j int) bool {
		dI := unresolvedWorkers[i].staticLookupDistribution
		dJ := unresolvedWorkers[j].staticLookupDistribution
		return dI.ExpectedDuration() < dJ.ExpectedDuration()
	})

	// combine unresolved workers into a set of chimera workers
	var chimeraWorkers []*chimeraWorker
	current := NewChimeraWorker(numPieces)
	for _, uw := range unresolvedWorkers {
		remainder := current.addWorker(uw)
		if remainder == nil {
			// chimera is not complete yet
			continue
		}

		// chimera is complete, so we add it and reset the "current" worker
		// using the remainder worker
		chimeraWorkers = append(chimeraWorkers, current)
		current = NewChimeraWorker(numPieces)
		current.addWorker(remainder)
	}

	fmt.Printf("built %v chimera workers from the unresolved workers\n", len(chimeraWorkers))
	// note that we ignore the "current" worker as it is not complete

	// combine all workers
	workers := make([]downloadWorker, len(resolvedWorkers)+len(chimeraWorkers))
	for rwi, rw := range resolvedWorkers {
		workers[rwi] = rw
	}
	for cwi, cw := range chimeraWorkers {
		workers[len(resolvedWorkers)+cwi] = cw
	}
	if len(workers) == 0 {
		return nil, nil
	}

	// loop state
	var bestSet *workerSet
	var bestSetFound bool

OUTER:
	for numOverdrive := 0; numOverdrive <= maxOverdriveWorkers; numOverdrive++ {
		workersNeeded := minPieces + numOverdrive
		for bI := 0; bI < skymodules.DistributionTrackerTotalBuckets; bI++ {
			bDur := skymodules.DistributionDurationForBucketIndex(bI)
			fmt.Printf("= = = = = \nduration in focus %v \n", bDur)
			// exit early if ppms in combination with the bucket duration
			// already exceeds the adjusted cost of the current best set,
			// workers would be too slow by definition
			if bestSetFound && bDur > bestSet.adjustedDuration(ppms) {
				fmt.Println("breaking OUTER, best set found and dur is larger than adjusted best set duration")
				break OUTER
			}

			// sort the workers by percentage chance they complete after the
			// current bucket duration
			sort.Slice(workers, func(i, j int) bool {
				chanceI := workers[i].distribution().ChanceAfter(bDur)
				chanceJ := workers[j].distribution().ChanceAfter(bDur)
				return chanceI > chanceJ
			})

			// TODO: remove me (debug logging)
			msg := "\nsortedWorkers:\n"
			for i, w := range workers {
				msg += fmt.Sprintf("%d) %v datapoints: %v chance: %v cost: %v chimera: %t launched: %v pieces: %v\n", i+1, w.identifier(), w.distribution().DataPoints(), w.distribution().ChanceAfter(bDur), w.cost(length), w.chimera(), w.launched(), w.pieces())
			}
			fmt.Println(msg)

			// group the most likely workers to complete in the current duration
			// in a way that we ensure no two workers are going after the same
			// piece
			var mostLikely []downloadWorker
			var lessLikely []downloadWorker
			pieces := make(map[uint64]struct{})
			for _, w := range workers {
				for _, pieceIndex := range w.pieces() {
					_, exists := pieces[pieceIndex]
					if exists {
						continue
					}
					w.markPieceForDownload(pieceIndex)
					pieces[pieceIndex] = struct{}{}

					if len(mostLikely) < workersNeeded {
						mostLikely = append(mostLikely, w)
					} else {
						lessLikely = append(lessLikely, w)
					}
					break // only use a worker once
				}
			}

			mostLikelySet := &workerSet{
				workers: mostLikely,

				staticExpectedDuration: bDur,
				staticLength:           length,
				staticMinPieces:        minPieces,
			}

			msg = "mostLikely:\n"
			for _, w := range mostLikelySet.workers {
				msg += w.identifier() + " "
			}
			fmt.Println(msg + "\n")

			// if the chance of the most likely set does not exceed 50%, it is
			// not high enough to continue, no need to continue this iteration,
			// we need to try a slower and thus more likely bucket
			if !mostLikelySet.chanceGreaterThanHalf(bDur) {
				fmt.Println("mostLikely is NOT greater than half for", bDur)
				continue
			}
			fmt.Println("mostLikely IS greater than half for", bDur)

			// now loop the remaining workers and try and swap them with the
			// most expensive workers in the most likely set
			for _, w := range lessLikely {
				cheaperSet := mostLikelySet.cheaperSetFromCandidate(w)
				if cheaperSet == nil {
					continue
				}
				if !cheaperSet.chanceGreaterThanHalf(bDur) {
					break
				}
				msg := "cheaperSet: "
				for _, w := range mostLikelySet.workers {
					msg += w.identifier() + ", "
				}
				fmt.Println(msg)
				mostLikelySet = cheaperSet
			}

			// perform price per ms comparison
			if !bestSetFound {
				fmt.Println("best set not found, is now equal to most likely")
				fmt.Println(len(mostLikelySet.workers))
				bestSet = mostLikelySet
				bestSetFound = true
			} else {
				fmt.Println("best set existed already")
				if mostLikelySet.adjustedDuration(ppms) < bestSet.adjustedDuration(ppms) {
					fmt.Println("best set updated")
					bestSet = mostLikelySet
				}
			}
		}
	}

	if bestSet != nil {
		msg := "bestSet: "
		for _, w := range bestSet.workers {
			msg += w.identifier() + ", "
		}
		fmt.Println(msg)
	}

	return bestSet, nil
}

// isGoodForDownload is a helper function that returns true if and only if the
// worker meets a certain set of criteria that make it useful for downloads.
// It's only useful if it is not on any type of cooldown, if it's async ready
// and if it's not price gouging.
func isGoodForDownload(w *worker) bool {
	// workers on cooldown or that are non async ready are not useful
	if w.managedOnMaintenanceCooldown() || !w.managedAsyncReady() {
		return false
	}

	// workers with a read queue on cooldown
	hsq := w.staticJobHasSectorQueue
	rjq := w.staticJobReadQueue
	if hsq.onCooldown() || rjq.onCooldown() {
		return false
	}

	// workers that are price gouging are not useful
	pt := w.staticPriceTable().staticPriceTable
	allowance := w.staticCache().staticRenterAllowance
	if err := checkProjectDownloadGouging(pt, allowance); err != nil {
		return false
	}

	return true
}