package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/cache"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/fspath"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/kv"
)

// Register with rclone
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "metadata",
		Description: "Cache file metadata (hashes and modtime) locally",
		NewFs:       NewFs,
		Options: []fs.Option{
			{
				Name:     "remote",
				Help:     "Remote to cache metadata for.\n\nThis should be in the form of 'remote:path'",
				Required: true,
			},
			{
				Name:     "metadata_hashes",
				Help:     "Comma separated list of hashes to calculate and store (md5, sha256, sha1)",
				Default:  fs.CommaSepList{"md5", "sha256"},
				Advanced: true,
			},
			{
				Name:     "metadata_db_path",
				Help:     "Path to metadata database (empty for automatic: ~/.cache/rclone/kv/<remote>~metadata.bolt)",
				Default:  "",
				Advanced: true,
			},
		},
	})
}

// Options defines the configuration for this remote
type Options struct {
	Remote         string          `config:"remote"`
	MetadataHashes fs.CommaSepList `config:"metadata_hashes"`
	MetadataDbPath string          `config:"metadata_db_path"`
}

// Fs represents a metadata caching filesystem
type Fs struct {
	fs.Fs            // Embedded wrapped filesystem
	wrapper  fs.Fs   // What's wrapping us (for multi-level wrapping)
	name     string  // Name of this remote
	root     string  // Root path
	opt      Options // Configuration options
	features *fs.Features
	db       *kv.DB // Metadata database
}

// NewFs constructs an Fs from the path, container:path
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	// Parse config
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	// Check for circular reference
	if strings.HasPrefix(opt.Remote, name+":") {
		return nil, errors.New("can't point metadata remote at itself - check the value of the remote setting")
	}

	// Get the wrapped filesystem
	remotePath := fspath.JoinRootPath(opt.Remote, root)
	wrappedFs, wrapErr := cache.Get(ctx, remotePath)

	// Handle both normal operation and ErrorIsFile
	switch wrapErr {
	case nil:
		// Normal case
	case fs.ErrorIsFile:
		// This is OK - root points to a file
	default:
		// Real error
		return nil, fmt.Errorf("failed to get wrapped filesystem: %w", wrapErr)
	}

	// Create the Fs
	f := &Fs{
		Fs:   wrappedFs,
		name: name,
		root: root,
		opt:  *opt,
	}

	// Pin the wrapped fs to prevent garbage collection
	cache.PinUntilFinalized(f.Fs, f)

	// Initialize metadata database
	db, err := kv.Start(ctx, "metadata", f)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize metadata database: %w", err)
	}
	f.db = db
	fs.Debugf(f, "Metadata caching enabled with hashes: %v", opt.MetadataHashes)

	// Setup features
	f.features = (&fs.Features{
		// We don't modify data, so we can pass through most features
		// The wrapped fs capabilities will be masked
	}).Fill(ctx, f).Mask(ctx, wrappedFs).WrapsFs(f, wrappedFs)

	return f, wrapErr
}

// Name of the remote (as passed into NewFs)
func (f *Fs) Name() string {
	return f.name
}

// Root of the remote (as passed into NewFs)
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("Metadata cache for %s:%s", f.Fs.Name(), f.Fs.Root())
}

// Precision of the ModTimes in this Fs
func (f *Fs) Precision() time.Duration {
	return f.Fs.Precision()
}

// Hashes returns the supported hash types - pass through from wrapped fs
func (f *Fs) Hashes() hash.Set {
	// We don't modify data, so pass through the wrapped fs hashes
	// Plus add the hashes we can calculate
	hashSet := f.Fs.Hashes()

	// Add configured hashes that we can calculate
	for _, hashName := range f.opt.MetadataHashes {
		switch strings.ToLower(hashName) {
		case "md5":
			hashSet = hashSet.Add(hash.MD5)
		case "sha1":
			hashSet = hashSet.Add(hash.SHA1)
		case "sha256":
			hashSet = hashSet.Add(hash.SHA256)
		}
	}

	return hashSet
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return f.features
}

// List the objects and directories in dir into entries
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	entries, err = f.Fs.List(ctx, dir)
	if err != nil {
		return nil, err
	}

	// Wrap each object entry
	return f.wrapEntries(entries), nil
}

// wrapEntries wraps object entries with our Object wrapper
func (f *Fs) wrapEntries(entries fs.DirEntries) fs.DirEntries {
	for i, entry := range entries {
		if o, ok := entry.(fs.Object); ok {
			entries[i] = f.newObject(o)
		}
		// Directories are passed through unchanged
	}
	return entries
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	o, err := f.Fs.NewObject(ctx, remote)
	if err != nil {
		return nil, err
	}
	return f.newObject(o), nil
}

// Put uploads a new file
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	// Create multi-hasher for configured hash types
	hashSet := f.Hashes()
	var hasher *hash.MultiHasher
	if hashSet != hash.Set(hash.None) {
		var err error
		hasher, err = hash.NewMultiHasherTypes(hashSet)
		if err != nil {
			fs.Debugf(f, "Failed to create hasher: %v", err)
		} else {
			// Wrap input with hash calculation
			in = io.TeeReader(in, hasher)
		}
	}

	// Upload to wrapped fs
	o, err := f.Fs.Put(ctx, in, src, options...)
	if err != nil {
		return nil, err
	}

	// Store metadata in DB if hashes were calculated
	if hasher != nil {
		hashes := make(map[string]string)
		for hashType, hashValue := range hasher.Sums() {
			hashes[hashType.String()] = hashValue
		}

		record := NewMetadataRecord(src.Size(), src.ModTime(ctx), hashes)
		if err := putMetadata(ctx, f.db, src.Remote(), record); err != nil {
			fs.Debugf(f, "Failed to store metadata: %v", err)
			// Don't fail the upload if metadata storage fails
		} else {
			fs.Debugf(f, "Stored metadata for %s with hashes: %v", src.Remote(), hashes)
		}
	}

	return f.newObject(o), nil
}

// Mkdir makes the directory
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	return f.Fs.Mkdir(ctx, dir)
}

// Rmdir removes the directory
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	return f.Fs.Rmdir(ctx, dir)
}

// Shutdown the backend, closing any background tasks and any cached connections
func (f *Fs) Shutdown(ctx context.Context) error {
	if f.db != nil {
		fs.Debugf(f, "Closing metadata database")
		if err := f.db.Stop(false); err != nil {
			return err
		}
	}
	// Also shutdown the wrapped fs if it supports it
	if do := f.Fs.Features().Shutdown; do != nil {
		return do(ctx)
	}
	return nil
}

// UnWrap returns the Fs that this Fs is wrapping
func (f *Fs) UnWrap() fs.Fs {
	return f.Fs
}

// WrapFs returns the Fs that is wrapping this Fs
func (f *Fs) WrapFs() fs.Fs {
	return f.wrapper
}

// SetWrapper sets the Fs that is wrapping this Fs
func (f *Fs) SetWrapper(wrapper fs.Fs) {
	f.wrapper = wrapper
}

// Check the interfaces are satisfied
var (
	_ fs.Fs              = (*Fs)(nil)
	_ fs.Shutdowner      = (*Fs)(nil)
	_ fs.UnWrapper       = (*Fs)(nil)
	_ fs.Wrapper         = (*Fs)(nil)
)
