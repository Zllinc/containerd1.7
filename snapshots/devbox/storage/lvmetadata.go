/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/containerd/containerd/errdefs"
	bolt "go.etcd.io/bbolt"
)

// LV 状态常量
const (
	LVStateNone       = "none"       // not created
	LVStateCreating   = "creating"   // creating
	LVStateCreated    = "created"    // created (not mounted or unmounted)
	LVStateMounting   = "mounting"   // mounting
	LVStateMounted    = "mounted"    // mounted
	LVStateUnmounting = "unmounting" // unmounting
	LVStateRemoving   = "removing"   // removing
	LVStateRemoved    = "removed"    // removed
	LVStateFaulty     = "faulty"     // faulty
)

var (
	// BucketKeyLVMetadata is the bucket key for LV metadata
	BucketKeyLVMetadata = []byte("lv_metadata") // lv metadata bucket
)

// LVInfo stores the state and information of an LV
type LVInfo struct {
	Name        string    `json:"name"`         // LV name (required)
	ContentID   string    `json:"content_id"`   // devbox content ID (required)
	State       string    `json:"state"`        // current state (required)
	PrevState   string    `json:"prev_state"`   // previous state (for rollback)
	MountPoint  string    `json:"mount_point"`  // mount point (required)
	IsFormatted bool      `json:"is_formatted"` // is formatted (required)
	Capacity    string    `json:"capacity"`     // capacity (required)
	CurrentKey  string    `json:"current_key"`  // current snapshot key (for debugging)
	LastError   string    `json:"last_error"`   // error information (recommended)
	CreatedTime time.Time `json:"created_time"` // created time (recommended)
	UpdatedTime time.Time `json:"updated_time"` // updated time (recommended)
}

// LVMetadataStore manages LV metadata
type LVMetadataStore struct {
	db *bolt.DB
}

// NewLVMetadataStore creates LV metadata storage
func NewLVMetadataStore(dbPath string) (*LVMetadataStore, error) {
	db, err := bolt.Open(dbPath, 0600, &bolt.Options{
		Timeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to open LV metadata database: %w", err)
	}

	// initialize bucket
	err = db.Update(func(tx *bolt.Tx) error {
		_, err := tx.CreateBucketIfNotExists(BucketKeyLVMetadata)
		return err
	})
	if err != nil {
		db.Close()
		return nil, fmt.Errorf("failed to create LV metadata bucket: %w", err)
	}

	return &LVMetadataStore{db: db}, nil
}

// Close closes the database
func (s *LVMetadataStore) Close() error {
	if s.db != nil {
		return s.db.Close()
	}
	return nil
}

// DB returns the underlying BoltDB instance (for direct access like devmapper)
func (s *LVMetadataStore) DB() *bolt.DB {
	return s.db
}

// WithTransaction executes an operation in a transaction
func (s *LVMetadataStore) WithTransaction(ctx context.Context, writable bool, fn func(ctx context.Context) error) error {
	if writable {
		return s.db.Update(func(tx *bolt.Tx) error {
			ctx = withLVTransaction(ctx, tx)
			return fn(ctx)
		})
	}
	return s.db.View(func(tx *bolt.Tx) error {
		ctx = withLVTransaction(ctx, tx)
		return fn(ctx)
	})
}

type lvTransactionKey struct{}

func withLVTransaction(ctx context.Context, tx *bolt.Tx) context.Context {
	return context.WithValue(ctx, lvTransactionKey{}, tx)
}

func getLVTransaction(ctx context.Context) (*bolt.Tx, error) {
	tx, ok := ctx.Value(lvTransactionKey{}).(*bolt.Tx)
	if !ok || tx == nil {
		return nil, ErrNoTransaction
	}
	return tx, nil
}

// AddLV adds an LV record
func AddLV(ctx context.Context, info *LVInfo) error {
	tx, err := getLVTransaction(ctx)
	if err != nil {
		return err
	}

	bkt := tx.Bucket(BucketKeyLVMetadata)
	if bkt == nil {
		return fmt.Errorf("LV metadata bucket not found")
	}

	// check if it already exists
	if existing := bkt.Get([]byte(info.Name)); existing != nil {
		return fmt.Errorf("LV %s already exists: %w", info.Name, errdefs.ErrAlreadyExists)
	}

	// set timestamps
	now := time.Now()
	if info.CreatedTime.IsZero() {
		info.CreatedTime = now
	}
	if info.UpdatedTime.IsZero() {
		info.UpdatedTime = now
	}

	// serialize
	data, err := json.Marshal(info)
	if err != nil {
		return fmt.Errorf("failed to marshal LV info: %w", err)
	}

	// save
	if err := bkt.Put([]byte(info.Name), data); err != nil {
		return fmt.Errorf("failed to save LV info: %w", err)
	}

	return nil
}

// GetLV gets an LV record
func GetLV(ctx context.Context, name string) (*LVInfo, error) {
	tx, err := getLVTransaction(ctx)
	if err != nil {
		return nil, err
	}

	bkt := tx.Bucket(BucketKeyLVMetadata)
	if bkt == nil {
		return nil, fmt.Errorf("LV metadata bucket not found")
	}

	data := bkt.Get([]byte(name))
	if data == nil {
		return nil, fmt.Errorf("LV %s not found: %w", name, errdefs.ErrNotFound)
	}

	var info LVInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return nil, fmt.Errorf("failed to unmarshal LV info: %w", err)
	}

	return &info, nil
}

// UpdateLV updates an LV record
func UpdateLV(ctx context.Context, name string, updateFn func(*LVInfo)) error {
	tx, err := getLVTransaction(ctx)
	if err != nil {
		return err
	}

	bkt := tx.Bucket(BucketKeyLVMetadata)
	if bkt == nil {
		return fmt.Errorf("LV metadata bucket not found")
	}

	// get existing record
	data := bkt.Get([]byte(name))
	if data == nil {
		return fmt.Errorf("LV %s not found: %w", name, errdefs.ErrNotFound)
	}

	var info LVInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return fmt.Errorf("failed to unmarshal LV info: %w", err)
	}

	// apply update
	updateFn(&info)

	// update timestamps
	info.UpdatedTime = time.Now()

	// serialize
	newData, err := json.Marshal(&info)
	if err != nil {
		return fmt.Errorf("failed to marshal LV info: %w", err)
	}

	// save
	if err := bkt.Put([]byte(name), newData); err != nil {
		return fmt.Errorf("failed to update LV info: %w", err)
	}

	return nil
}

// RemoveLV removes an LV record
func RemoveLV(ctx context.Context, name string) error {
	tx, err := getLVTransaction(ctx)
	if err != nil {
		return err
	}

	bkt := tx.Bucket(BucketKeyLVMetadata)
	if bkt == nil {
		return fmt.Errorf("LV metadata bucket not found")
	}

	// check if it exists
	if data := bkt.Get([]byte(name)); data == nil {
		return fmt.Errorf("LV %s not found: %w", name, errdefs.ErrNotFound)
	}

	// delete
	if err := bkt.Delete([]byte(name)); err != nil {
		return fmt.Errorf("failed to delete LV info: %w", err)
	}

	return nil
}

// WalkLVs walks all LV records
func WalkLVs(ctx context.Context, fn func(*LVInfo) error) error {
	tx, err := getLVTransaction(ctx)
	if err != nil {
		return err
	}

	bkt := tx.Bucket(BucketKeyLVMetadata)
	if bkt == nil {
		return fmt.Errorf("LV metadata bucket not found")
	}

	return bkt.ForEach(func(k, v []byte) error {
		var info LVInfo
		if err := json.Unmarshal(v, &info); err != nil {
			return fmt.Errorf("failed to unmarshal LV info: %w", err)
		}
		return fn(&info)
	})
}
