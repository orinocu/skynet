package renter

import (
	"context"
	"os"
	"sync"
	"time"

	lock "github.com/square/mongo-lock"
	"github.com/tus/tusd/pkg/handler"
	"github.com/tus/tusd/pkg/memorylocker"
	"gitlab.com/NebulousLabs/errors"
	"gitlab.com/SkynetLabs/skyd/build"
	"gitlab.com/SkynetLabs/skyd/skymodules"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/readconcern"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

const (
	// mongoLockTTL is the time-to-live in seconds for a lock in the
	// mongodb. After that time passes, an entry is no longer considered
	// locked. This avoids deadlocks in case a server locks an entry and
	// then crashes before unlocking it.
	mongoLockTTL = 300 // 5 minutes

	tusDBName                     = "tus"
	tusUploadsMongoCollectionName = "uploads"
)

type (
	skynetTUSMongoUploadStore struct {
		staticClient         *mongo.Client
		staticPortalHostname string
	}

	mongoTUSUpload struct {
		ID     string `bson:"_id"`
		LockID string `bson:"lockid"`
	}

	skynetMongoLock struct {
		staticClient         *lock.Client
		staticPortalHostname string
		staticUploadID       string
	}
)

func (us *skynetTUSMongoUploadStore) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	return us.staticClient.Disconnect(ctx)
}

// NewLock creates a new lock for the upload with the given ID.
func (us *skynetTUSMongoUploadStore) NewLock(uploadID string) (handler.Lock, error) {
	client := lock.NewClient(us.staticClient.Database(tusDBName).Collection(tusUploadsMongoCollectionName))
	err := client.CreateIndexes(context.Background())
	if err != nil {
		return nil, err
	}
	return &skynetMongoLock{
		staticClient:         client,
		staticPortalHostname: us.staticPortalHostname,
		staticUploadID:       uploadID,
	}, nil
}

// Lock exclusively locks the lock. It returns handler.ErrFileLocked if the
// upload is already locked and it will put an expiration time on the lock in
// case the server dies while the file is locked. That way uploads won't remain
// locked forever.
func (l *skynetMongoLock) Lock() error {
	client := l.staticClient
	ld := lock.LockDetails{
		Owner: "TUS",
		Host:  l.staticPortalHostname,
		TTL:   mongoLockTTL,
	}
	err := client.XLock(context.Background(), l.staticUploadID, l.staticUploadID, ld)
	if err == lock.ErrAlreadyLocked {
		return handler.ErrFileLocked
	}
	return err
}

// Unlock attempts to unlock an upload. It will retry doing so for a certain
// time before giving up.
func (l *skynetMongoLock) Unlock() error {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	var err error
LOOP:
	for {
		_, err = l.staticClient.Unlock(context.Background(), l.staticUploadID)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			break LOOP
		case <-time.After(time.Second):
		}
	}
	build.Critical("Failed to unlock the lock", err)
	return err
}

func (us *skynetTUSMongoUploadStore) ToPrune() ([]skymodules.SkynetTUSUpload, error) {
	panic("not implemented yet")
}

func (us *skynetTUSMongoUploadStore) Prune(skymodules.SkynetTUSUpload) error {
	panic("not implemented yet")
}

func (us *skynetTUSMongoUploadStore) SaveUpload(id string, upload skymodules.SkynetTUSUpload) error {
	panic("not implemented yet")
}

func (us *skynetTUSMongoUploadStore) Upload(id string) (skymodules.SkynetTUSUpload, error) {
	panic("not implemented yet")
}

// NewSkynetTUSInMemoryUploadStore creates a new skynetTUSInMemoryUploadStore.
func NewSkynetTUSInMemoryUploadStore() skymodules.SkynetTUSUploadStore {
	return &skynetTUSInMemoryUploadStore{
		uploads:      make(map[string]*skynetTUSUpload),
		staticLocker: memorylocker.New(),
	}
}

// NewSkynetTUSMongoUploadStore creates a new upload store using a mongodb as
// the storage backend.
func NewSkynetTUSMongoUploadStore(ctx context.Context, uri, portalName string, creds options.Credential) (skymodules.SkynetTUSUploadStore, error) {
	return newSkynetTUSMongoUploadStore(ctx, uri, portalName, creds)
}

// newSkynetTUSMongoUploadStore creates a new upload store using a mongodb as
// the storage backend.
func newSkynetTUSMongoUploadStore(ctx context.Context, uri, portalName string, creds options.Credential) (*skynetTUSMongoUploadStore, error) {
	opts := options.Client().
		ApplyURI(uri).
		SetAuth(creds).
		SetReadConcern(readconcern.Majority()).
		SetWriteConcern(writeconcern.New(writeconcern.WMajority()))
	client, err := mongo.Connect(ctx, opts)
	return &skynetTUSMongoUploadStore{
		staticClient:         client,
		staticPortalHostname: portalName,
	}, err
}

// skynetTUSInMemoryUploadStore is an in-memory skynetTUSUploadStore
// implementation.
type skynetTUSInMemoryUploadStore struct {
	uploads      map[string]*skynetTUSUpload
	mu           sync.Mutex
	staticLocker *memorylocker.MemoryLocker
}

func (u *skynetTUSUpload) SiaPath() skymodules.SiaPath {
	return u.staticSUP.SiaPath
}

func (u *skynetTUSUpload) Skylink() (skymodules.Skylink, bool) {
	u.mu.Lock()
	defer u.mu.Unlock()
	_, exists := u.fi.MetaData["Skylink"]
	return u.sl, exists
}

// Close implements the io.Closer and is a no-op for the in-memory store.
func (us *skynetTUSInMemoryUploadStore) Close() error { return nil }

// NewLock implements handler.Locker by forwarding the call to an in-memory
// locker.
func (us *skynetTUSInMemoryUploadStore) NewLock(id string) (handler.Lock, error) {
	return us.staticLocker.NewLock(id)
}

// SaveUpload saves an upload.
func (us *skynetTUSInMemoryUploadStore) SaveUpload(id string, u skymodules.SkynetTUSUpload) error {
	us.mu.Lock()
	defer us.mu.Unlock()
	upload, ok := u.(*skynetTUSUpload)
	if !ok {
		err := errors.New("SaveUpload: can't store a non *skynetTUSUpload")
		build.Critical(err)
		return err
	}
	us.uploads[id] = upload
	return nil
}

// Upload returns an upload by ID.
func (us *skynetTUSInMemoryUploadStore) Upload(id string) (skymodules.SkynetTUSUpload, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	upload, exists := us.uploads[id]
	if !exists {
		return nil, os.ErrNotExist
	}
	return upload, nil
}

// Prune removes uploads that have been idle for too long.
func (us *skynetTUSInMemoryUploadStore) ToPrune() ([]skymodules.SkynetTUSUpload, error) {
	us.mu.Lock()
	defer us.mu.Unlock()
	var toDelete []skymodules.SkynetTUSUpload
	for _, u := range us.uploads {
		u.mu.Lock()
		lastWrite := u.lastWrite
		complete := u.complete
		u.mu.Unlock()
		if time.Since(lastWrite) < PruneTUSUploadTimeout {
			continue // nothing to do
		}
		// If the upload wasn't completed, delete the files on disk.
		if !complete {
			toDelete = append(toDelete, u)
		}
	}
	return toDelete, nil
}

// Prune removes uploads that have been idle for too long.
func (us *skynetTUSInMemoryUploadStore) Prune(toPrune skymodules.SkynetTUSUpload) error {
	us.mu.Lock()
	defer us.mu.Unlock()
	upload := toPrune.(*skynetTUSUpload)
	_ = upload.Close()
	delete(us.uploads, upload.fi.ID)
	return nil
}
