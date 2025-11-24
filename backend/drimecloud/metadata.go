package drime

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/rclone/rclone/lib/kv"
)

// Current metadata version
const CurrentMetadataVersion = 1

// MetadataEnvelope wraps versioned metadata for forward/backward compatibility
type MetadataEnvelope struct {
	Version int             `json:"version"`
	Data    json.RawMessage `json:"data"`
}

// MetadataRecord_v1 is version 1 of the metadata schema
type MetadataRecord_v1 struct {
	// Fingerprint for validation
	Size    int64     `json:"size"`
	ModTime time.Time `json:"modtime"`

	// Cached metadata
	Hashes map[string]string `json:"hashes"` // e.g., {"md5": "abc123", "sha256": "def456"}

	// When this record was created
	Created time.Time `json:"created"`

	// Optional: API metadata for reference
	EntryID   int64  `json:"entry_id,omitempty"`
	Hash      string `json:"api_hash,omitempty"`    // Drime's base64 hash
	UpdatedAt string `json:"updated_at,omitempty"` // Drime's UpdatedAt timestamp
}

// MetadataRecord is the current version (alias to latest)
type MetadataRecord = MetadataRecord_v1

// NewMetadataRecord creates a new metadata record with current version
func NewMetadataRecord(size int64, modTime time.Time, hashes map[string]string) *MetadataRecord {
	return &MetadataRecord{
		Size:    size,
		ModTime: modTime,
		Hashes:  hashes,
		Created: time.Now(),
	}
}

// Encode marshals the metadata record into versioned JSON bytes
func (r *MetadataRecord_v1) Encode() ([]byte, error) {
	// Marshal the record data
	data, err := json.Marshal(r)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal metadata: %w", err)
	}

	// Wrap in versioned envelope
	envelope := MetadataEnvelope{
		Version: CurrentMetadataVersion,
		Data:    data,
	}

	// Marshal the envelope
	return json.Marshal(envelope)
}

// DecodeMetadata decodes versioned JSON bytes into the appropriate metadata struct
// This static constructor handles version-based unmarshalling
func DecodeMetadata(data []byte) (*MetadataRecord, error) {
	if len(data) == 0 {
		return nil, fmt.Errorf("empty metadata")
	}

	// First, decode the envelope to check version
	var envelope MetadataEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, fmt.Errorf("failed to unmarshal envelope: %w", err)
	}

	// Switch on version and unmarshal into appropriate struct
	switch envelope.Version {
	case 1:
		var record MetadataRecord_v1
		if err := json.Unmarshal(envelope.Data, &record); err != nil {
			return nil, fmt.Errorf("failed to unmarshal v1 metadata: %w", err)
		}
		return &record, nil

	// Future versions would go here:
	// case 2:
	//     var record MetadataRecord_v2
	//     if err := json.Unmarshal(envelope.Data, &record); err != nil {
	//         return nil, fmt.Errorf("failed to unmarshal v2 metadata: %w", err)
	//     }
	//     // Migrate v2 to current version if needed
	//     return migrateV2ToV1(&record), nil

	default:
		return nil, fmt.Errorf("unsupported metadata version: %d", envelope.Version)
	}
}

// IsValid checks if the metadata fingerprint matches the current file state
func (r *MetadataRecord_v1) IsValid(size int64, modTime time.Time) bool {
	return r.Size == size && r.ModTime.Equal(modTime)
}

// Database operation implementations

// getMetadataOp implements kv.Op for reading metadata
type getMetadataOp struct {
	key    string
	result *MetadataRecord
	err    error
}

func (op *getMetadataOp) Do(ctx context.Context, b kv.Bucket) error {
	data := b.Get([]byte(op.key))
	if data == nil {
		op.err = fmt.Errorf("metadata not found")
		return nil
	}

	record, err := DecodeMetadata(data)
	if err != nil {
		op.err = err
		return nil
	}

	op.result = record
	return nil
}

// putMetadataOp implements kv.Op for writing metadata
type putMetadataOp struct {
	key    string
	record *MetadataRecord
}

func (op *putMetadataOp) Do(ctx context.Context, b kv.Bucket) error {
	data, err := op.record.Encode()
	if err != nil {
		return fmt.Errorf("failed to encode metadata: %w", err)
	}

	return b.Put([]byte(op.key), data)
}

// deleteMetadataOp implements kv.Op for deleting metadata
type deleteMetadataOp struct {
	key string
}

func (op *deleteMetadataOp) Do(ctx context.Context, b kv.Bucket) error {
	return b.Delete([]byte(op.key))
}

// Public API functions

// getMetadata retrieves metadata from the database
func getMetadata(ctx context.Context, db *kv.DB, remote string) (*MetadataRecord, error) {
	if db == nil {
		return nil, fmt.Errorf("database not initialized")
	}

	op := &getMetadataOp{key: remote}
	err := db.Do(false, op) // false = read operation
	if err != nil {
		return nil, err
	}

	if op.err != nil {
		return nil, op.err
	}

	return op.result, nil
}

// putMetadata stores metadata in the database
func putMetadata(ctx context.Context, db *kv.DB, remote string, record *MetadataRecord) error {
	if db == nil {
		return fmt.Errorf("database not initialized")
	}

	op := &putMetadataOp{
		key:    remote,
		record: record,
	}

	return db.Do(true, op) // true = write operation
}

// deleteMetadata removes metadata from the database
func deleteMetadata(ctx context.Context, db *kv.DB, remote string) error {
	if db == nil {
		return nil // Silently succeed if DB not enabled
	}

	op := &deleteMetadataOp{key: remote}
	return db.Do(true, op) // true = write operation
}
