package renter

import (
	"bytes"
	"io"
	"sync"
	"time"

	"gitlab.com/NebulousLabs/Sia/build"
	"gitlab.com/NebulousLabs/Sia/modules"
	"gitlab.com/NebulousLabs/Sia/modules/renter/siafile"
	"gitlab.com/NebulousLabs/errors"
)

type (
	// streamer is a modules.Streamer that can be used to stream downloads from
	// the sia network.
	streamer struct {
		// Reader variables. The snapshot is a snapshot of the file as it
		// existed when it was opened, something that we do to give the streamer
		// a consistent view of the file even if the file is being actively
		// updated. Having this snapshot also isolates the reader from events
		// such as name changes and deletions.
		//
		// We also keep the full static file entry as it allows us to update
		// metadata items in the file such as the access time.
		//
		// TODO: Should we be updating the access times upon every call to Read
		// and Seek, instead of just when we close the file?
		staticFile      *siafile.Snapshot
		staticFileEntry *siafile.SiaFileSetEntry
		offset          int64
		r               *Renter

		// The cache itself is a []byte that is managed by threadedFillCache. The
		// 'cacheOffset' indicates the starting location of the cache within the
		// file, and all of the data in the []byte will be the actual file data
		// that follows that offset. If the cache is empty, the length will be
		// 0. We use cacheMu to make atomic updates to the cache and
		// cacheOffset, meaning that any Read call can safely use the cache so
		// long as it is holding the cacheMu mutex.
		//
		// Because the cache gets filled asynchronously, errors need to be
		// recorded and then delivered to the user later. The errors get stored
		// in readErr.
		//
		// cacheReady is a rotating channel which is used to signal to threads
		// that the cache has been updated. When a Read call is made, the first
		// action required is to grab a lock on cacheMu and then check if the
		// cache has the requested data. If not, while still holding the cacheMu
		// lock the Read thread will grab a copy of cacheReady, and then release
		// the cacheMu lock. When the threadedFillCache thread has finished
		// updating the cache, the thread will grab the cacheMu lock and then
		// the cacheReady channel will be closed and replaced with a new
		// channel. This allows any number of Read threads to simultaneously
		// block while waiting for cacheReady to be closed, and once cacheReady
		// is closed they know to check the cache again.
		cache       []byte
		cacheOffset int64
		cacheReady  chan struct{}
		readErr     error

		// cacheMu governs updates to the cache, cacheOffset, readErr, and
		// cacheReady variables. Any thread holding cacheMu can safely read or
		// modify these values without needing to worry about the other control
		// structures.
		//
		// Multiple asyncrhonous calls to fill the cache may be sent out at
		// once. To prevent race conditions, the 'cacheActive' channel is used
		// to ensure that only one instance of 'threadedFillCache' is running at
		// a time. If another instance of 'threadedFillCache' is active, the new
		// call will immediately return.
		cacheActive chan struct{}
		cacheMu     sync.Mutex
	}
)

// threadedFillCache is a method to fill or refill the cache for the streamer.
// The function will self-enforce that only one thread is running at a time.
// While the thread is running, multiple calls to 'Read' may happen, which will
// drain the cache and require additional filling. To ensure that the cache is
// always being filled if there is a need, threadedFillCache will finish by
// calling itself in a new goroutine if it updated the cache at all.
//
// TODO: This current code will potentially fetch more than one chunk at a time,
// and will potentially fetch a small amount of data that crosses chunk
// boundaries. Is that fine?
func (s *streamer) threadedFillCache() {
	// Before grabbing the cacheActive object, check whether this thread is
	// required to exist. This check needs to be made before checking the
	// cacheActive because threadedFillCache recursively calls itself after
	// grabbing the cacheActive object, so some base case is needed to guarantee
	// termination.
	s.cacheMu.Lock()
	partialDownloadsSupported := s.staticFile.ErasureCode().SupportsPartialEncoding()
	chunkSize := s.staticFile.ChunkSize()
	cacheOffset := int64(s.cacheOffset)
	streamOffset := s.offset
	cacheLen := int64(len(s.cache))
	s.cacheMu.Unlock()
	if partialDownloadsSupported && cacheOffset == streamOffset && cacheLen > 0 {
		// If partial downloads are supported, the cache offset should start at
		// the same place as the current stream offset. If the current stream
		// offset is not the same as the cacheOffset, it means that data has
		// been read from the cache and therefore the cache needs to be
		// refilled.
		//
		// An extra check that there is any data in the cache needs to be made
		// so that the cache fill function runs immediately after
		// initialization.
		return
	}
	if !partialDownloadsSupported && cacheOffset <= streamOffset && streamOffset < cacheOffset+cacheLen && cacheLen > 0 {
		// If partial downloads are not supported, the full chunk containing the
		// current offset should be the cache. If the cache is the full chunk
		// that contains current offset, then nothing needs to be done as the
		// cache is already prepared.
		//
		// This should be functionally nearly identical to the previous cache
		// that we were using which has since been disabled.
		return
	}

	// Check cacheActive for an object. If no object exists, another
	// threadedFillCache thread is running, so this thread should terminate
	// immediately. The other thread is guaranteed to call 'threadedFillCache'
	// again upon termination (due the defer statement immediately following the
	// acquisition of the cacheActive object), meaning that any new need to
	// update the cache will eventually be satisfied.
	select {
	case <-s.cacheActive:
	default:
		return
	}
	// NOTE: The ordering here is delicate. When the function returns, the
	// first thing that happens is that the function returns the object to
	// the cacheActive channel, which allows another thread to extend the
	// cache. After this has been done, threadedFillCache is called again to
	// check whether more of the cache has been drained and whether the
	// cache needs to be topped up again.
	defer func() {
		go s.threadedFillCache()
	}()
	defer func() {
		s.cacheActive <- struct{}{}
	}()

	// If the offset is one byte beyond the current cache size, then the stream
	// data is being read faster than the cacheing has been able to supply data,
	// which means that the cache size needs to be increased. The cache size
	// should not be increased if it is already greater than or equal to the max
	// cache size.
	increaseCacheSize := false
	if streamOffset == cacheOffset+cacheLen && cacheLen < maxStreamerCacheSize && cacheLen > 0 {
		increaseCacheSize = true
	}

	// Re-fetch the variables important to gathering cache, in case they have
	// changed since the initial check.
	s.cacheMu.Lock()
	partialDownloadsSupported = s.staticFile.ErasureCode().SupportsPartialEncoding()
	chunkSize = s.staticFile.ChunkSize()
	cacheOffset = int64(s.cacheOffset)
	streamOffset = s.offset
	cacheLen = int64(len(s.cache))
	s.cacheMu.Unlock()

	// Check one more time for the conditions that indicate no cache update is
	// necessary, given that we have released and re-grabbed the lock since the
	// previous check.
	if !partialDownloadsSupported && cacheOffset <= streamOffset && streamOffset < cacheOffset+cacheLen && cacheLen > 0 {
		return
	}
	if partialDownloadsSupported && cacheOffset == streamOffset && cacheLen > 0 {
		return
	}

	// Determine what data needs to be fetched.
	//
	// If there is no support for partial downloads, a whole chunk needs to be
	// fetched, and the cache will be set equal to the chunk that currently
	// contains the stream offset. This is because that amount of data will need
	// to be fetched anyway, so we may as well use the full amount of data in
	// the cache.
	//
	// If there is support for partial downloads but the stream offset is not
	// contained within the existing cache, we need to fully replace the cache.
	// At initialization, this will be the case (cacheLen of 0 cannot contain
	// the stream offset byte within it, because it contains no bytes at all),
	// so a check for 0-size cache is made. The full cache replacement will
	// consist of a partial download the size of the cache starting from the
	// stream offset.
	//
	// The final case is that the stream offset is contained within the current
	// cache, but the stream offset is not the first byte of the cache. This
	// means that we need to drop all of the bytes prior to the stream offset
	// and then more bytes so that the cache remains the same size.
	var fetchOffset, fetchLen int64
	if !partialDownloadsSupported {
		// Request a full chunk of data.
		chunkIndex, _ := s.staticFile.ChunkIndexByOffset(uint64(streamOffset))
		fetchOffset = int64(chunkIndex * chunkSize)
		fetchLen = int64(chunkSize)
	} else if streamOffset < cacheOffset || streamOffset >= cacheOffset+cacheLen {
		// Grab enough data to fill the cache entirely starting from the current
		// stream offset.
		fetchOffset = streamOffset
		fetchLen = cacheLen

		// If there is no cache yet, the cache should be initialized to the
		// default cache size. If a previous check indicated that the cache
		// should be grown, double the size of the cache by adding the exiting
		// cache size to the fetch length.
		if fetchLen == 0 {
			fetchLen = initialStreamerCacheSize
		}
		if increaseCacheSize {
			fetchLen += cacheLen
		}
	} else {
		// Set the fetch offset to the end of the current cache, and set the
		// length equal to the number of bytes that the streamOffset has already
		// consumed, so that the cache remains the same size after we drop all
		// of the consumed bytes and extend the cache with new data.
		fetchOffset = cacheOffset + cacheLen
		fetchLen = cacheLen - (streamOffset - cacheOffset)

		// If there is a request to increase the cache size,
		if increaseCacheSize {
			fetchLen += cacheLen
		}
	}

	// Perform the actual download.
	buffer := bytes.NewBuffer([]byte{})
	ddw := newDownloadDestinationWriter(buffer)
	d, err := s.r.managedNewDownload(downloadParams{
		destination:       ddw,
		destinationType:   destinationTypeSeekStream,
		destinationString: "httpresponse",
		file:              s.staticFile,

		latencyTarget: 50 * time.Millisecond, // TODO low default until full latency suport is added.
		length:        uint64(fetchLen),
		needsMemory:   true,
		offset:        uint64(fetchOffset),
		overdrive:     5,    // TODO: high default until full overdrive support is added.
		priority:      1000, // TODO: high default until full priority support is added.
	})
	if err != nil {
		closeErr := ddw.Close()
		s.cacheMu.Lock()
		s.readErr = errors.Compose(err, closeErr)
		s.cacheMu.Unlock()
		return
	}
	// Register some cleanup for when the download is done.
	d.OnComplete(func(_ error) error {
		// close the destination buffer to avoid deadlocks.
		err := ddw.Close()
		s.cacheMu.Lock()
		if s.readErr == nil && err != nil {
			s.readErr = err
		}
		s.cacheMu.Unlock()
		return err
	})
	// Set the in-memory buffer to nil just to be safe in case of a memory
	// leak.
	defer func() {
		d.destination = nil
	}()
	// Block until the download has completed.
	select {
	case <-d.completeChan:
		err := d.Err()
		if err != nil {
			s.cacheMu.Lock()
			s.readErr = errors.AddContext(err, "download failed")
			s.cacheMu.Unlock()
		}
	case <-s.r.tg.StopChan():
		s.cacheMu.Lock()
		s.readErr = errors.New("download interrupted by shutdown")
		s.cacheMu.Unlock()
	}

	// Update the cache.
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()

	// Sanity check to verify that some other thread didn't adjust the
	// cacheOffset.
	if s.cacheOffset != cacheOffset {
		build.Critical("The stream cache offset changed while new cache data was being fetched")
	}

	// Update the cache based on whether the entire cache needs to be replaced
	// or whether only some of the cache is being replaced. The whole cache
	// needs to be replaced in the even that partial downloads are not
	// supported, and also in the event that the stream offset is complete
	// outside the previous cache.
	if !partialDownloadsSupported || streamOffset >= cacheOffset+cacheLen || streamOffset < cacheOffset {
		s.cache = buffer.Bytes()
		s.cacheOffset = fetchOffset
	} else {
		s.cache = s.cache[streamOffset-cacheOffset:]
		s.cache = append(s.cache, buffer.Bytes()...)
		s.cacheOffset = streamOffset
	}
}

// Close closes the streamer and let's the fileSet know that the SiaFile is no
// longer in use.
func (s *streamer) Close() error {
	err1 := s.staticFileEntry.SiaFile.UpdateAccessTime()
	err2 := s.staticFileEntry.Close()
	return errors.Compose(err1, err2)
}

// Read will check the stream cache for the data that is being requested. If the
// data is fully or partially there, Read will return what data is available
// without error. If the data is not there, Read will issue a call to fill the
// cache and then block until the data is at least partially available.
func (s *streamer) Read(p []byte) (n int, err error) {
	// Wait in a loop until the requested data is available, or until an error
	// is recovered. The loop needs to release the lock between iterations, but
	// the lock that it grabs needs to be held after the loops termination if
	// the right conditions are met, resulting in an ugly/complex locking
	// strategy.
	for {
		// Get the file's size and check for EOF.
		fileSize := int64(s.staticFile.Size())
		if s.offset >= fileSize {
			return 0, io.EOF
		}

		// Grab the lock and check that the cache has data which we want. If the
		// cache does have data that we want, we will keep the lock and exit the
		// loop. If there's an error, we will drop the lock and return the
		// error. If the cache does not have the data we want but there is no
		// error, we will drop the lock and spin up a thread to fill the cache,
		// and then block until the cache has been updated.
		s.cacheMu.Lock()
		// If there is a cache error, drop the lock and return. This check
		// should happen before anything else.
		if s.readErr != nil {
			err := s.readErr
			s.cacheMu.Unlock()
			return 0, err
		}

		// Check if the cache continas data that we are interested in. If so,
		// break out of the cache-fetch loop while still holding the lock.
		if s.cacheOffset <= s.offset && s.offset < s.cacheOffset+int64(len(s.cache)) {
			break
		}

		// There is no error, but the data that we want is also unavailable.
		// Grab the cacheReady channel to detect when the cache has been
		// updated, and then drop the lock and block until there has been a
		// cache update.
		//
		// Notably, it should not be necessary to spin up a new cache thread.
		// There are four conditions which may cause the stream offset to be
		// located outside of the existing cache, and all conditions will result
		// with a thread being spun up regardless. The first condition is
		// initialization, where no cache exists. A fill cache thread is spun up
		// upon initialization. The second condition is after a Seek, which may
		// move the offset outside of the current cache. The call to Seek also
		// spins up a cache filling thread. The third condition is after a read,
		// which adjusts the stream offset. A new cache fill thread gets spun up
		// in this case as well, immediately after the stream offset is
		// adjusted. Finally, there is the case where a cache fill thread was
		// spun up, but then immediately spun down due to another cache fill
		// thread already running. But this case is handled as well, because a
		// cache fill thread will spin up another cache fill thread when it
		// finishes specifically to cover this case.
		cacheReady := s.cacheReady
		s.cacheMu.Unlock()
		<-cacheReady

		// Upon iterating, the lock is not held, so the call to grab the lock at
		// the top of the function should not cause a deadlock.
	}
	// This code should only be reachable if the lock is still being held and
	// there is also data in the cache for us. Defer releasing the lock.
	defer s.cacheMu.Unlock()

	dataStart := s.offset - s.cacheOffset
	dataEnd := dataStart + int64(len(p))
	// If the read request extends beyond the cache, truncate it to include
	// only up to where the cache ends.
	cacheEnd := s.cacheOffset + int64(len(s.cache))
	if dataEnd > cacheEnd {
		dataEnd = cacheEnd
	}
	copy(p, s.cache[dataStart:dataEnd])
	s.offset += dataEnd - dataStart
	go s.threadedFillCache() // Now that some data is consumed, fetch more data.
	return int(dataEnd - dataStart), nil
}

// Seek sets the offset for the next Read to offset, interpreted
// according to whence: SeekStart means relative to the start of the file,
// SeekCurrent means relative to the current offset, and SeekEnd means relative
// to the end. Seek returns the new offset relative to the start of the file
// and an error, if any.
func (s *streamer) Seek(offset int64, whence int) (int64, error) {
	var newOffset int64
	switch whence {
	case io.SeekStart:
		newOffset = 0
	case io.SeekCurrent:
		newOffset = s.offset
	case io.SeekEnd:
		newOffset = int64(s.staticFile.Size())
	}
	newOffset += offset
	if newOffset < 0 {
		return s.offset, errors.New("cannot seek to negative offset")
	}

	// Update the offset of the stream and immediately send a thread to update
	// the cache.
	s.offset = newOffset
	go s.threadedFillCache()
	return s.offset, nil
}

// Streamer creates a modules.Streamer that can be used to stream downloads from
// the sia network.
//
// TODO: Why do we return entry.SiaPath() as a part of the call that opens the
// stream?
func (r *Renter) Streamer(siaPath string) (string, modules.Streamer, error) {
	// Lookup the file associated with the nickname.
	entry, err := r.staticFileSet.Open(siaPath)
	if err != nil {
		return "", nil, err
	}

	// Create the streamer
	s := &streamer{
		staticFile:      entry.Snapshot(),
		staticFileEntry: entry,
		r:               r,

		cacheActive: make(chan struct{}, 1),
		cacheReady:  make(chan struct{}),
	}

	// Put an object into the cacheActive to indicate that there is no cache
	// thread running at the moment, and then spin up a cache thread to fill the
	// cache (the cache thread will consume the cacheActive object itself).
	s.cacheActive <- struct{}{}
	go s.threadedFillCache()

	return entry.SiaPath(), s, nil
}
