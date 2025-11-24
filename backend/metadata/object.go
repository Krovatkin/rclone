package metadata

import (
	"context"
	"io"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
)

// Object represents a wrapped object with metadata caching
type Object struct {
	fs.Object // Embedded wrapped object
	f         *Fs
}

// newObject creates a new wrapped object
func (f *Fs) newObject(o fs.Object) *Object {
	return &Object{
		Object: o,
		f:      f,
	}
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.f
}

// Hash returns the selected checksum of the file
// If metadata is cached and valid, return from cache
// Otherwise fall back to wrapped object's Hash()
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	// Try to get metadata from cache
	if o.f.db != nil {
		record, err := getMetadata(ctx, o.f.db, o.Remote())
		if err == nil {
			// Validate fingerprint (size + modtime)
			if record.IsValid(o.Size(), o.ModTime(ctx)) {
				// Get hash from cache
				if hashValue, ok := record.Hashes[ht.String()]; ok && hashValue != "" {
					fs.Debugf(o.f, "Hash cache hit for %s: %s = %s", o.Remote(), ht.String(), hashValue)
					return hashValue, nil
				}
			} else {
				fs.Debugf(o.f, "Hash cache miss for %s: fingerprint mismatch", o.Remote())
			}
		}
	}

	// Fall back to underlying object
	hashValue, err := o.Object.Hash(ctx, ht)
	if err != nil {
		return "", err
	}

	// If we got a hash from underlying object, cache it
	if o.f.db != nil && hashValue != "" {
		// Get existing metadata or create new
		record, err := getMetadata(ctx, o.f.db, o.Remote())
		if err != nil {
			// Create new metadata record
			hashes := make(map[string]string)
			hashes[ht.String()] = hashValue
			record = NewMetadataRecord(o.Size(), o.ModTime(ctx), hashes)
		} else {
			// Update existing record
			if record.Hashes == nil {
				record.Hashes = make(map[string]string)
			}
			record.Hashes[ht.String()] = hashValue
		}

		if err := putMetadata(ctx, o.f.db, o.Remote(), record); err != nil {
			fs.Debugf(o.f, "Failed to cache hash: %v", err)
		} else {
			fs.Debugf(o.f, "Cached hash for %s: %s = %s", o.Remote(), ht.String(), hashValue)
		}
	}

	return hashValue, nil
}

// ModTime returns the modification time of the object
// Check cache first, then fall back to wrapped object
func (o *Object) ModTime(ctx context.Context) time.Time {
	// Try to get modtime from cache
	if o.f.db != nil {
		record, err := getMetadata(ctx, o.f.db, o.Remote())
		if err == nil {
			// Validate fingerprint (size must match)
			if record.Size == o.Size() {
				fs.Debugf(o.f, "ModTime from cache for %s: %v", o.Remote(), record.ModTime)
				return record.ModTime
			}
		}
	}

	// Fall back to underlying object
	return o.Object.ModTime(ctx)
}

// SetModTime sets the modification time of the object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	// Set modtime on underlying object
	err := o.Object.SetModTime(ctx, modTime)
	if err != nil {
		return err
	}

	// Update metadata cache
	if o.f.db != nil {
		// Get existing metadata or create new
		record, err := getMetadata(ctx, o.f.db, o.Remote())
		if err != nil {
			// Create new metadata record
			record = NewMetadataRecord(o.Size(), modTime, make(map[string]string))
		} else {
			// Update existing record
			record.ModTime = modTime
		}

		if err := putMetadata(ctx, o.f.db, o.Remote(), record); err != nil {
			fs.Debugf(o.f, "Failed to update modtime in cache: %v", err)
			// Don't fail the operation if cache update fails
		} else {
			fs.Debugf(o.f, "Updated modtime in cache for %s: %v", o.Remote(), modTime)
		}
	}

	return nil
}

// Update updates the object with new content
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// Create multi-hasher for configured hash types
	hashSet := o.f.Hashes()
	var hasher *hash.MultiHasher
	if hashSet != hash.Set(hash.None) {
		var err error
		hasher, err = hash.NewMultiHasherTypes(hashSet)
		if err != nil {
			fs.Debugf(o.f, "Failed to create hasher: %v", err)
		} else {
			// Wrap input with hash calculation
			in = io.TeeReader(in, hasher)
		}
	}

	// Update on underlying object
	err := o.Object.Update(ctx, in, src, options...)
	if err != nil {
		return err
	}

	// Store new metadata in DB if hashes were calculated
	if o.f.db != nil && hasher != nil {
		hashes := make(map[string]string)
		for hashType, hashValue := range hasher.Sums() {
			hashes[hashType.String()] = hashValue
		}

		record := NewMetadataRecord(src.Size(), src.ModTime(ctx), hashes)
		if err := putMetadata(ctx, o.f.db, src.Remote(), record); err != nil {
			fs.Debugf(o.f, "Failed to store metadata after update: %v", err)
			// Don't fail the update if metadata storage fails
		} else {
			fs.Debugf(o.f, "Stored metadata after update for %s with hashes: %v", src.Remote(), hashes)
		}
	}

	return nil
}

// Remove removes the object
func (o *Object) Remove(ctx context.Context) error {
	// Remove from underlying fs
	err := o.Object.Remove(ctx)
	if err != nil {
		return err
	}

	// Delete metadata from cache
	if o.f.db != nil {
		if err := deleteMetadata(ctx, o.f.db, o.Remote()); err != nil {
			fs.Debugf(o.f, "Failed to delete metadata: %v", err)
			// Don't fail the remove if metadata deletion fails
		}
	}

	return nil
}

// UnWrap returns the wrapped Object
func (o *Object) UnWrap() fs.Object {
	return o.Object
}

// Check the interfaces are satisfied
var (
	_ fs.Object = (*Object)(nil)
)
