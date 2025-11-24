package drime

// 5. Minimum implementation checklist

// Required for Fs:

// ✅ Name(), Root(), String(), Precision(), Hashes(), Features()
// ✅ List(), NewObject(), Put(), Mkdir(), Rmdir()

// Required for Object:

// ✅ Fs(), Remote(), ModTime(), Size(), Storable(), Hash()
// ✅ SetModTime(), Open(), Update(), Remove()

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/dircache"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiBaseURL      = "https://app.drime.cloud/api/v1"
	defaultChunkSize = 25 * 1024 * 1024 // 25 MB chunks for multipart upload
)

// Options defines the configuration for the backend.
type Options struct {
	AuthToken    string               `config:"auth_token"`
	UploadCutoff fs.SizeSuffix        `config:"upload_cutoff"`
}

// Register the backend with rclone
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "drime",
		Description: "Drime Cloud Storage",
		NewFs:       NewFs,
		Options: []fs.Option{
			{Name: "auth_token", Help: "Your Drime API auth token"},
			{
				Name:     "upload_cutoff",
				Help:     "Cutoff for switching to multipart upload (minimum 5 MB)",
				Default:  fs.SizeSuffix(25 * 1024 * 1024), // 25 MB
				Advanced: true,
			},
		},
	})
}

type FileEntry struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	FileSize  int64  `json:"file_size"`
	ParentID  *int64 `json:"parent_id"`
	Type      string `json:"type"`
	URL       string `json:"url"`
	Hash      string `json:"hash"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// APIResponse represents standard API response
type APIResponse struct {
	Status    string      `json:"status"`
	Message   string      `json:"message,omitempty"`
	FileEntry *FileEntry  `json:"fileEntry,omitempty"`
	Folder    *FileEntry  `json:"folder,omitempty"`
	Entries   []FileEntry `json:"entries,omitempty"`
}

// MultipartCreateResponse represents response from /s3/multipart/create
type MultipartCreateResponse struct {
	UploadID string `json:"uploadId"`
	Key      string `json:"key"`
}

// SignedURL represents a signed URL for a part upload
type SignedURL struct {
	PartNumber int    `json:"partNumber"`
	URL        string `json:"url"`
}

// MultipartSignURLsResponse represents response from /s3/multipart/batch-sign-part-urls
type MultipartSignURLsResponse struct {
	URLs []SignedURL `json:"urls"`
}

// MultipartPart represents a completed part for the complete request
type MultipartPart struct {
	PartNumber int    `json:"PartNumber"`
	ETag       string `json:"ETag"`
}

// S3EntryResponse represents response from /s3/entries
type S3EntryResponse struct {
	FileEntry *FileEntry `json:"fileEntry"`
}

// Fs represents a connection to Drime storage
type Fs struct {
	name     string
	root     string
	opt      Options
	client   *rest.Client
	dirCache *dircache.DirCache
}

// NewFs constructs a new filesystem instance
func NewFs(ctx context.Context, name, root string, m configmap.Mapper) (fs.Fs, error) {
	opt := new(Options)
	err := configstruct.Set(m, opt)
	if err != nil {
		return nil, err
	}

	root = strings.Trim(root, "/")

	if opt.AuthToken == "" {
		return nil, fmt.Errorf("auth_token is required")
	}

	client := rest.NewClient(http.DefaultClient).SetRoot(apiBaseURL)
	client.SetHeader("Authorization", "Bearer "+opt.AuthToken)

	f := &Fs{
		name:   name,
		root:   root,
		opt:    *opt,
		client: client,
	}

	// Initialize directory cache
	// Empty string ("") is the true root ID (represents parentID = nil in the API)
	f.dirCache = dircache.New(root, "", f)

	// If root is empty, we're at the API root - no need to validate
	if root == "" {
		return f, nil
	}

	// Try to find the root directory without creating it
	err = f.dirCache.FindRoot(ctx, false)
	if err != nil {
		// Root doesn't exist as a directory
		// Maybe it's a file? Try splitting into parent directory and filename
		newRoot, remote := dircache.SplitPath(root)
		tempF := *f
		tempF.dirCache = dircache.New(newRoot, "", &tempF)
		tempF.root = newRoot

		// Try to find the parent directory
		err = tempF.dirCache.FindRoot(ctx, false)
		if err != nil {
			// Parent doesn't exist either
			// This is OK - root will be created on demand when needed
			// Reset dirCache to avoid cached failure state
			f.dirCache = dircache.New(root, "", f)
			return f, nil
		}

		// Parent exists - check if remote is a file in it
		_, err := tempF.NewObject(ctx, remote)
		if err != nil {
			// Not a file either, return original f
			// Reset dirCache to avoid cached failure state
			f.dirCache = dircache.New(root, "", f)
			return f, nil
		}

		// It IS a file! Adjust root to be the parent directory
		// Note: We must update f (not return tempF) because features are bound to f
		f.dirCache = tempF.dirCache
		f.root = tempF.root
		return f, fs.ErrorIsFile
	}

	// Root exists and is a folder
	return f, nil
}

// Name returns the name of the remote
func (f *Fs) Name() string {
	return f.name
}

// Root returns the root of the remote
func (f *Fs) Root() string {
	return f.root
}

// String returns a description of the FS
func (f *Fs) String() string {
	return fmt.Sprintf("Drime root '%s'", f.root)
}

// Precision returns the precision of this Fs
func (f *Fs) Precision() time.Duration {
	return time.Second
}

// Hashes returns the supported hash sets
func (f *Fs) Hashes() hash.Set {
	return hash.Set(hash.None)
}

// Features returns the optional features of this Fs
func (f *Fs) Features() *fs.Features {
	return &fs.Features{
		// Feature flags, whether Fs
		CaseInsensitive:          false, // No indication of case insensitivity in API
		DuplicateFiles:           true,  // API allows duplicate endpoint, so likely allows duplicate names
		ReadMimeType:             true,  // FileEntry has "mime" field (e.g., "image/png")
		WriteMimeType:            true,  // Upload endpoints accept mime/clientMime parameter
		CanHaveEmptyDirectories:  true,  // Has folder creation endpoint separate from file upload
		BucketBased:              false, // Not bucket-based, uses hierarchical folders
		BucketBasedRootOK:        false, // Not applicable
		SetTier:                  false, // No storage tier functionality in API
		GetTier:                  false, // No storage tier functionality in API
		ServerSideAcrossConfigs:  false, // No server-side copy between different configs
		IsLocal:                  false, // This is a cloud storage service
		SlowModTime:              false, // ModTime is directly available in FileEntry
		SlowHash:                 false, // Hash is directly available in FileEntry.hash field
		ReadMetadata:             true,  // Can read description, created_at, updated_at, etc.
		WriteMetadata:            true,  // Can update entry via PUT /file-entries/{entryId}
		UserMetadata:             true,  // FileEntry has "description" field that can be set
		ReadDirMetadata:          true,  // Folders are FileEntries too, so same metadata available
		WriteDirMetadata:         true,  // Can update folder entries via PUT endpoint
		WriteDirSetModTime:       false, // No indication that folder modtime can be set explicitly
		UserDirMetadata:          true,  // Folders can have descriptions too
		DirModTimeUpdatesOnWrite: true,  // Typical behavior for most cloud storage
		FilterAware:              true,  // API supports query, type, and other filters
		PartialUploads:           false, // No indication of partial upload visibility
		NoMultiThreading:         false, // No indication of threading restrictions
		Overlay:                  false, // This is a direct backend, not a wrapper
		ChunkWriterDoesntSeek:    false, // Not applicable for this implementation
		DoubleSlash:              false, // No indication of special double slash handling

		Copy:    f.Copy,    // Drime has duplicate endpoint for server-side copy
		Move:    f.Move,    // Drime has move endpoint
		DirMove: f.DirMove, // Drime can move directories
		// TODO:
		// PublicLink: f.publicLink, // Has shareable links API
		// PutStream:  f.putStream,  // Upload endpoint can handle streams
		// CleanUp:    f.cleanUp,    // Has restore from trash functionality
	}
}

// FindLeaf finds a directory entry (leaf) in a parent directory (pathID)
// This is one of the two required methods for the dircache.DirCacher interface
func (f *Fs) FindLeaf(ctx context.Context, pathID, leaf string) (pathIDOut string, found bool, err error) {
	// Convert pathID string to *int64 for Drime API
	// Empty string means root (parentID = nil)
	var parentID *int64
	if pathID != "" {
		id, _ := strconv.ParseInt(pathID, 10, 64)
		parentID = &id
	}

	// List all entries in the parent directory
	entries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return "", false, err
	}

	// Search for a FOLDER with the name "leaf"
	// Note: FindLeaf only finds folders, not files
	for _, entry := range entries {
		if entry.Name == leaf && entry.Type == "folder" {
			// Found it! Return the ID as a string
			return strconv.FormatInt(entry.ID, 10), true, nil
		}
	}

	// Not found - this is not an error, just means folder doesn't exist
	return "", false, nil
}

// CreateDir creates a new directory
// This is the second required method for the dircache.DirCacher interface
func (f *Fs) CreateDir(ctx context.Context, pathID, leaf string) (newID string, err error) {
	fs.Debugf(f, "CreateDir called: pathID=%q, leaf=%q", pathID, leaf)

	// Convert pathID string to *int64 for Drime API
	var parentID *int64
	if pathID != "" {
		id, _ := strconv.ParseInt(pathID, 10, 64)
		parentID = &id
	}

	// Create the folder using existing helper function
	folder, err := f.createFolder(ctx, leaf, parentID)
	if err != nil {
		fs.Debugf(f, "CreateDir failed: %v", err)
		return "", err
	}

	fs.Debugf(f, "CreateDir succeeded: newID=%d", folder.ID)
	// Return the new folder's ID as a string
	return strconv.FormatInt(folder.ID, 10), nil
}

// findDirID is a helper that finds a directory ID and converts it to *int64 for the Drime API
// Returns nil for root directory (empty dir)
func (f *Fs) findDirID(ctx context.Context, dir string, create bool) (*int64, error) {
	fs.Debugf(f, "findDirID called: dir=%q, create=%v", dir, create)

	// Let dirCache resolve the directory (it calls FindRoot automatically)
	dirID, err := f.dirCache.FindDir(ctx, dir, create)
	if err != nil {
		fs.Debugf(f, "findDirID: FindDir returned error: %v", err)
		return nil, err
	}

	fs.Debugf(f, "findDirID: FindDir returned dirID=%q", dirID)

	// Empty dirID means true API root (parentID = nil in Drime API)
	if dirID == "" {
		return nil, nil
	}

	// Parse and return the directory ID
	id, err := strconv.ParseInt(dirID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid directory ID %q: %w", dirID, err)
	}
	return &id, nil
}

// PaginatedResponse represents API paginated response
type PaginatedResponse struct {
	CurrentPage int         `json:"current_page"`
	Data        []FileEntry `json:"data"`
	From        int         `json:"from"`
	LastPage    int         `json:"last_page"`
	NextPage    *int        `json:"next_page"`
	PerPage     int         `json:"per_page"`
	PrevPage    *int        `json:"prev_page"`
	To          int         `json:"to"`
	Total       int         `json:"total"`
}

// listEntries lists entries in a folder with pagination support
func (f *Fs) listEntries(ctx context.Context, parentID *int64, entryType string) ([]FileEntry, error) {
	var allEntries []FileEntry

	// First, make an initial request to get the last_page value
	params := url.Values{}
	params.Set("perPage", "200") // Small page size for testing pagination
	params.Set("page", "1")

	if parentID != nil {
		params.Set("parentIds", strconv.FormatInt(*parentID, 10))
	}

	if entryType != "" {
		params.Set("type", entryType)
	}

	var initialResponse PaginatedResponse
	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method:     "GET",
		Path:       "/drive/file-entries",
		Parameters: params,
	}, nil, &initialResponse)

	if err != nil {
		return nil, err
	}

	fs.Debugf(f, "Pagination: page 1/%d, got %d entries, total=%d",
		initialResponse.LastPage, len(initialResponse.Data), initialResponse.Total)

	// Add first page data
	allEntries = append(allEntries, initialResponse.Data...)

	// Now loop through all remaining pages
	for i := 2; i <= initialResponse.LastPage; i++ {
		params.Set("page", strconv.Itoa(i))

		var response PaginatedResponse
		_, err := f.client.CallJSON(ctx, &rest.Opts{
			Method:     "GET",
			Path:       "/drive/file-entries",
			Parameters: params,
		}, nil, &response)

		if err != nil {
			return nil, err
		}

		fs.Debugf(f, "Pagination: page %d/%d, got %d entries",
			i, initialResponse.LastPage, len(response.Data))

		// Append this page's data to our collection
		allEntries = append(allEntries, response.Data...)
	}

	fs.Debugf(f, "Pagination complete: fetched %d total entries", len(allEntries))

	return allEntries, nil
}

// Put uploads a new file
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.put(ctx, in, src, false)
}

// put handles both new uploads and updates with size-based routing
func (f *Fs) put(ctx context.Context, in io.Reader, src fs.ObjectInfo, isUpdate bool) (fs.Object, error) {
	remote := src.Remote()
	size := src.Size()

	// Get parent directory ID
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	parentID, err := f.findDirID(ctx, dir, true)
	if err != nil {
		return nil, fmt.Errorf("failed to find parent directory: %w", err)
	}

	// Choose upload method based on size
	// Use multipart upload if size is known and >= UploadCutoff
	// If size is unknown (< 0), use simple upload for safety
	if size >= 0 && size >= int64(f.opt.UploadCutoff) {
		fs.Debugf(f, "Using multipart upload for %s (size: %d bytes >= cutoff: %d bytes)",
			remote, size, f.opt.UploadCutoff)
		return f.uploadMultipart(ctx, in, src, parentID)
	}

	fs.Debugf(f, "Using simple upload for %s (size: %d bytes)", remote, size)
	return f.putUnchecked(ctx, in, src)
}

func (f *Fs) putUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo) (fs.Object, error) {
	remote := src.Remote()

	// Read the content
	content, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}

	// Get parent directory, creating if necessary
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}

	// Use dircache to find/create parent directory and convert to *int64
	parentID, err := f.findDirID(ctx, dir, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create folder structure: %w", err)
	}

	// Create multipart form body
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Detect MIME type from filename
	mimeType := fs.MimeTypeFromName(remote)

	// Add file content with MIME type
	partWriter, err := writer.CreatePart(map[string][]string{
		"Content-Disposition": {fmt.Sprintf(`form-data; name="file"; filename="%s"`, path.Base(remote))},
		"Content-Type":        {mimeType},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create form part: %w", err)
	}
	_, err = partWriter.Write(content)
	if err != nil {
		return nil, fmt.Errorf("failed to write file content: %w", err)
	}

	// Add parent ID field
	if parentID != nil {
		err = writer.WriteField("parentId", strconv.FormatInt(*parentID, 10))
		if err != nil {
			return nil, fmt.Errorf("failed to add parentId field: %w", err)
		}
	} else {
		writer.WriteField("parentId", "null")
	}

	// Close the multipart writer
	err = writer.Close()
	if err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Make the API call
	resp, err := f.client.Call(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/uploads",
		Body:   &body,
		ExtraHeaders: map[string]string{
			"Content-Type": writer.FormDataContentType(),
		},
	})
	if err != nil {
		return nil, fmt.Errorf("upload failed: %w", err)
	}
	defer resp.Body.Close()

	// Check HTTP status
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		bodyBytes, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("upload failed with status %d: %s", resp.StatusCode, string(bodyBytes))
	}

	// Parse JSON response
	var response APIResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	if response.Status != "success" || response.FileEntry == nil {
		return nil, fmt.Errorf("upload failed: %s", response.Message)
	}

	obj := &Object{
		fs:       f,
		entry:    *response.FileEntry,
		fullPath: remote, // Keep the original remote path for the object
	}

	// Drime doesn't allow updating any metadata
	/*
	// Preserve the source file's modification time
	err = obj.SetModTime(ctx, src.ModTime(ctx))
	if err != nil {
		fs.Debugf(f, "Failed to set modification time: %v", err)
		// Don't fail the upload if we can't set modtime
	}
	*/

	return obj, nil
}

// uploadMultipart uploads a file using S3 multipart upload for large files
func (f *Fs) uploadMultipart(ctx context.Context, in io.Reader, src fs.ObjectInfo, parentID *int64) (*Object, error) {
	remote := src.Remote()
	size := src.Size()
	fileName := path.Base(remote)

	fs.Debugf(f, "Starting multipart upload for %s (size: %d)", fileName, size)

	// Detect MIME type from filename
	mimeType := fs.MimeTypeFromName(remote)

	// Step 1: Initialize multipart upload
	initPayload := map[string]interface{}{
		"filename":     fileName,
		"mime":         mimeType,
		"size":         size,
		"extension":    strings.TrimPrefix(path.Ext(fileName), "."),
		"relativePath": "",
		"workspaceId":  0,
	}

	var createResp MultipartCreateResponse
	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/s3/multipart/create",
	}, &initPayload, &createResp)
	if err != nil {
		return nil, fmt.Errorf("failed to initialize multipart upload: %w", err)
	}

	uploadID := createResp.UploadID
	key := createResp.Key
	fs.Debugf(f, "Multipart upload initialized: uploadId=%s, key=%s", uploadID, key)

	// Calculate number of parts
	numParts := int(math.Ceil(float64(size) / float64(defaultChunkSize)))
	fs.Debugf(f, "Will upload %d parts", numParts)

	// Collect all uploaded parts
	var uploadedParts []MultipartPart

	// Step 2 & 3: Get signed URLs and upload parts
	// For simplicity, we'll get all signed URLs at once (Python CLI does batching, but let's keep it simple)
	partNumbers := make([]int, numParts)
	for i := 0; i < numParts; i++ {
		partNumbers[i] = i + 1
	}

	signPayload := map[string]interface{}{
		"key":         key,
		"uploadId":    uploadID,
		"partNumbers": partNumbers,
	}

	var signResp MultipartSignURLsResponse
	_, err = f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/s3/multipart/batch-sign-part-urls",
	}, &signPayload, &signResp)
	if err != nil {
		// Abort the upload
		f.abortMultipartUpload(ctx, key, uploadID)
		return nil, fmt.Errorf("failed to get signed URLs: %w", err)
	}

	fs.Debugf(f, "Got %d signed URLs", len(signResp.URLs))

	// Upload each part
	for i, signedURL := range signResp.URLs {
		partNum := signedURL.PartNumber

		// Calculate part size
		partSize := defaultChunkSize
		if i == numParts-1 {
			// Last part might be smaller
			partSize = int(size - int64(i)*defaultChunkSize)
		}

		// Read the chunk
		chunk := make([]byte, partSize)
		n, err := io.ReadFull(in, chunk)
		if err != nil && err != io.ErrUnexpectedEOF {
			f.abortMultipartUpload(ctx, key, uploadID)
			return nil, fmt.Errorf("failed to read chunk %d: %w", partNum, err)
		}
		chunk = chunk[:n]

		fs.Debugf(f, "Uploading part %d/%d (%d bytes)", partNum, numParts, len(chunk))

		// Upload part to R2
		req, err := http.NewRequestWithContext(ctx, "PUT", signedURL.URL, bytes.NewReader(chunk))
		if err != nil {
			f.abortMultipartUpload(ctx, key, uploadID)
			return nil, fmt.Errorf("failed to create PUT request for part %d: %w", partNum, err)
		}
		req.Header.Set("Content-Type", "application/octet-stream")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			f.abortMultipartUpload(ctx, key, uploadID)
			return nil, fmt.Errorf("failed to upload part %d: %w", partNum, err)
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			f.abortMultipartUpload(ctx, key, uploadID)
			return nil, fmt.Errorf("failed to upload part %d: status %d: %s", partNum, resp.StatusCode, string(body))
		}

		// Get ETag from response
		etag := resp.Header.Get("ETag")
		etag = strings.Trim(etag, "\"")

		uploadedParts = append(uploadedParts, MultipartPart{
			PartNumber: partNum,
			ETag:       etag,
		})

		fs.Debugf(f, "Part %d uploaded successfully (ETag: %s)", partNum, etag)
	}

	// Step 4: Complete multipart upload
	completePayload := map[string]interface{}{
		"key":      key,
		"uploadId": uploadID,
		"parts":    uploadedParts,
	}

	var completeResp map[string]interface{}
	_, err = f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/s3/multipart/complete",
	}, &completePayload, &completeResp)
	if err != nil {
		return nil, fmt.Errorf("failed to complete multipart upload: %w", err)
	}

	fs.Debugf(f, "Multipart upload completed")

	// Step 5: Create file entry in Drime (use mimeType detected earlier)
	entryPayload := map[string]interface{}{
		"clientMime":      mimeType,
		"clientName":      fileName,
		"filename":        path.Base(key),
		"size":            size,
		"clientExtension": strings.TrimPrefix(path.Ext(fileName), "."),
		"relativePath":    "",
		"workspaceId":     0,
	}

	// Add parentId if not root
	if parentID != nil {
		entryPayload["parentId"] = *parentID
	}

	var entryResp S3EntryResponse
	_, err = f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/s3/entries",
	}, &entryPayload, &entryResp)
	if err != nil {
		return nil, fmt.Errorf("failed to create file entry: %w", err)
	}

	if entryResp.FileEntry == nil {
		return nil, fmt.Errorf("file entry creation returned no data")
	}

	fs.Debugf(f, "File entry created: ID=%d", entryResp.FileEntry.ID)

	obj := &Object{
		fs:       f,
		entry:    *entryResp.FileEntry,
		fullPath: remote,
	}

	// Drime doesn't allow updating any metadata
	/*
	// Preserve the source file's modification time
	err = obj.SetModTime(ctx, src.ModTime(ctx))
	if err != nil {
		fs.Debugf(f, "Failed to set modification time: %v", err)
		// Don't fail the upload if we can't set modtime
	}
	*/

	return obj, nil
}

// abortMultipartUpload aborts a multipart upload in case of error
func (f *Fs) abortMultipartUpload(ctx context.Context, key, uploadID string) {
	payload := map[string]interface{}{
		"key":      key,
		"uploadId": uploadID,
	}

	var resp map[string]interface{}
	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/s3/multipart/abort",
	}, &payload, &resp)
	if err != nil {
		fs.Errorf(f, "Failed to abort multipart upload: %v", err)
	} else {
		fs.Debugf(f, "Multipart upload aborted: uploadId=%s", uploadID)
	}
}


// createFolder creates a new folder and returns FileEntry
func (f *Fs) createFolder(ctx context.Context, name string, parentID *int64) (*FileEntry, error) {
	payload := map[string]interface{}{
		"name": name,
	}
	if parentID != nil {
		payload["parentId"] = *parentID
	}

	fs.Debugf(f, "Request to /folders with the following payload: name=%q, parentId=%v", name, parentID)

	var response APIResponse
	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/folders",
	}, &payload, &response)

	if err != nil {
		return nil, fmt.Errorf("failed to create folder: %w", err)
	}

	if response.Status != "success" || response.Folder == nil {
		return nil, fmt.Errorf("failed to create folder: %s", response.Message)
	}

	return response.Folder, nil
}

// Mkdir creates a directory
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	// Use dircache to find/create directory (create=true)
	// dircache automatically creates all intermediate directories
	_, err := f.dirCache.FindDir(ctx, dir, true)
	return err
}

// List lists the objects and directories in dir
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	// Use dircache to find the directory ID and convert to *int64
	parentID, err := f.findDirID(ctx, dir, false)
	if err != nil {
		// If directory doesn't exist, return empty list instead of error
		// This allows copy operations to create the directory
		fs.Debugf(f, "List: directory not found, returning empty list: %v", err)
		return nil, nil
	}

	// List all entries in the directory
	fileEntries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return nil, err
	}

	// Convert API entries to fs.DirEntries
	for _, entry := range fileEntries {
		if entry.Type == "folder" {
			entries = append(entries, fs.NewDir(path.Join(dir, entry.Name), time.Time{}))
		} else {
			// Construct full path relative to the filesystem root
			var relativePath string
			if dir == "" {
				relativePath = entry.Name
			} else {
				relativePath = path.Join(dir, entry.Name)
			}

			entries = append(entries, &Object{
				fs:       f,
				entry:    entry,
				fullPath: relativePath, // Relative to f.root
			})
		}
	}

	return entries, nil
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	// Split remote into directory and filename
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	name := path.Base(remote)

	// Use dircache to find parent directory ID and convert to *int64
	parentID, err := f.findDirID(ctx, dir, false)
	if err != nil {
		// If parent directory doesn't exist, the object definitely doesn't exist
		fs.Debugf(f, "NewObject: parent directory not found, returning ErrorObjectNotFound: %v", err)
		return nil, fs.ErrorObjectNotFound
	}

	// List entries in parent directory
	entries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return nil, err
	}

	// Search for the file
	for _, entry := range entries {
		if entry.Name == name {
			if entry.Type == "folder" {
				return nil, fs.ErrorIsDir
			}

			return &Object{
				fs:       f,
				entry:    entry,
				fullPath: remote, // Use the requested remote path
			}, nil
		}
	}

	return nil, fs.ErrorObjectNotFound
}

// Remove removes a file
func (f *Fs) Remove(ctx context.Context, remote string) error {

	fs.Debugf(f, "FS Remove remote: %q", remote)
	obj, err := f.NewObject(ctx, remote)
	if err != nil {
		return err
	}
	return obj.Remove(ctx)
}

// Rmdir removes a directory
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	// Check if trying to remove API root (both f.root and dir are empty)
	if dir == "" && f.root == "" {
		return fmt.Errorf("cannot remove root directory")
	}

	// Use dircache to find the directory ID
	dirID, err := f.dirCache.FindDir(ctx, dir, false)
	if err != nil {
		return err
	}

	// Convert dirID to int64
	id, _ := strconv.ParseInt(dirID, 10, 64)

	// Delete the directory via API
	payload := map[string]interface{}{
		"entryIds":      []string{strconv.FormatInt(id, 10)},
		"deleteForever": false,
	}

	var response APIResponse
	_, err = f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/file-entries/delete",
	}, &payload, &response)

	if err != nil {
		return fmt.Errorf("failed to remove directory: %w", err)
	}

	if response.Status != "success" {
		return fmt.Errorf("failed to remove directory: %s", response.Message)
	}

	// Flush the directory from cache since it's been deleted
	f.dirCache.FlushDir(dir)

	return nil
}

// Object represents a file in Drime
type Object struct {
	fs       *Fs
	entry    FileEntry
	fullPath string
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.fullPath //o.entry.Name
}

// Hash returns the hash of an object
func (o *Object) Hash(ctx context.Context, ht hash.Type) (string, error) {
	return "", hash.ErrUnsupported
}

// Size returns the size of an object in bytes
func (o *Object) Size() int64 {
	return o.entry.FileSize
}

func (o *Object) String() string {
	return fmt.Sprintf("Drime file '%s' (%s)", o.entry.Name, o.entry.Type)
}

// ModTime returns the modification time of the object
func (o *Object) ModTime(ctx context.Context) time.Time {
	if o.entry.UpdatedAt != "" {
		if t, err := time.Parse(time.RFC3339, o.entry.UpdatedAt); err == nil {
			return t
		}
	}
	return time.Time{} // Return zero time if parsing fails
}

func (o *Object) ID() string {
	return strconv.FormatInt(o.entry.ID, 10)
}

// SetModTime sets the modification time of the file
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	// Drime API does not support updating file metadata via PUT /file-entries/{id}
	// Attempts to set created_at, updated_at, or other fields return 500 Internal Server Error
	// These fields are auto-managed by the database on the server side
	return fs.ErrorCantSetModTime

	// Code below is commented out - does not work with Drime API:
	/*
	// Format time as RFC3339 to match API format
	timestamp := modTime.Format(time.RFC3339)

	// Try to set created_at instead of updated_at
	// (updated_at is likely auto-managed by the database)
	payload := map[string]interface{}{
		"created_at": timestamp,
	}

	var response APIResponse
	_, err := o.fs.client.CallJSON(ctx, &rest.Opts{
		Method: "PUT",
		Path:   fmt.Sprintf("/file-entries/%d", o.entry.ID),
	}, &payload, &response)

	if err != nil {
		return fmt.Errorf("failed to set modtime: %w", err)
	}

	if response.Status != "success" {
		return fmt.Errorf("failed to set modtime: %s", response.Message)
	}

	// Update our cached entries
	o.entry.CreatedAt = timestamp
	o.entry.UpdatedAt = timestamp

	return nil
	*/
}

// Storable returns whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open opens the file for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Download file using the hash-based download endpoint
	// GET /file-entries/download/{hash} redirects to R2 signed URL
	downloadPath := fmt.Sprintf("/file-entries/download/%s", o.entry.Hash)

	resp, err := o.fs.client.Call(ctx, &rest.Opts{
		Method: "GET",
		Path:   downloadPath,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download file: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("download failed with status %d: %s", resp.StatusCode, string(body))
	}

	return resp.Body, nil
}

// Update updates the object with the new content
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	// Upload the new version
	newObj, err := o.fs.put(ctx, in, src, true)
	if err != nil {
		return err
	}

	// Remove old object
	err = o.Remove(ctx)
	if err != nil {
		return err
	}

	// Update this object with new data
	*o = *(newObj.(*Object))
	return nil
}

// Remove removes this object
func (o *Object) Remove(ctx context.Context) error {

	fs.Debugf(o, "Removing file: ID=%d, Path=%s", o.entry.ID, o.fullPath)

	payload := map[string]interface{}{
		"entryIds":      []string{strconv.FormatInt(o.entry.ID, 10)},
		"deleteForever": true,
	}

	var response APIResponse
	_, err := o.fs.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/file-entries/delete",
	}, &payload, &response)

	if err != nil {
		return fmt.Errorf("failed to remove file: %w", err)
	}

	if response.Status != "success" {
		return fmt.Errorf("failed to remove file: %s", response.Message)
	}

	return nil
}

func (f *Fs) moveEntries(ctx context.Context, entryIDs []int64, destParentID *int64) ([]FileEntry, error) {
	// Prepare move request
	payload := map[string]interface{}{
		"entryIds": entryIDs,
	}

	// Set destination parent ID (null for root)
	if destParentID != nil {
		payload["destinationId"] = *destParentID
	} else {
		payload["destinationId"] = nil
	}

	// Make the move API call
	var response struct {
		Status  string      `json:"status"`
		Message string      `json:"message,omitempty"`
		Entries []FileEntry `json:"entries,omitempty"`
	}

	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/file-entries/move",
	}, &payload, &response)

	if err != nil {
		return nil, fmt.Errorf("failed to move entries: %w", err)
	}

	if response.Status != "success" {
		return nil, fmt.Errorf("move failed: %s", response.Message)
	}

	return response.Entries, nil
}

// duplicateEntries duplicates file entries to a destination folder
func (f *Fs) duplicateEntries(ctx context.Context, entryIDs []int64, destParentID *int64) ([]FileEntry, error) {
	// Prepare duplicate request
	payload := map[string]interface{}{
		"entryIds": entryIDs,
	}

	// Set destination parent ID (null for root)
	if destParentID != nil {
		payload["destinationId"] = *destParentID
	} else {
		payload["destinationId"] = nil
	}

	// Make the duplicate API call
	var response struct {
		Status  string      `json:"status"`
		Message string      `json:"message,omitempty"`
		Entries []FileEntry `json:"entries,omitempty"`
	}

	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method: "POST",
		Path:   "/file-entries/duplicate",
	}, &payload, &response)

	if err != nil {
		return nil, fmt.Errorf("failed to duplicate entries: %w", err)
	}

	if response.Status != "success" {
		return nil, fmt.Errorf("duplicate failed: %s", response.Message)
	}

	return response.Entries, nil
}

// Move moves a file from src to dst
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	// Extract directory and filename
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	dstName := path.Base(remote)

	// Check if filename is changing (Move can't rename, only change parent)
	if srcObj.entry.Name != dstName {
		return nil, fs.ErrorCantMove
	}

	// Find destination directory ID and convert to *int64
	destFolderID, err := f.findDirID(ctx, dir, false)
	if err != nil {
		return nil, fmt.Errorf("destination directory doesn't exist: %w", err)
	}

	// Move the entry via API
	_, err = f.moveEntries(ctx, []int64{srcObj.entry.ID}, destFolderID)
	if err != nil {
		return nil, err
	}

	// Fetch and return the moved object
	return f.NewObject(ctx, remote)
}

// DirMove moves a directory from src to dst
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}

	// Check if trying to move root
	if srcRemote == "" && srcFs.root == "" {
		return fmt.Errorf("can't move the root folder")
	}

	// Find source directory ID
	srcDirID, err := srcFs.dirCache.FindDir(ctx, srcRemote, false)
	if err != nil {
		return err
	}

	srcID, _ := strconv.ParseInt(srcDirID, 10, 64)

	// Find/create destination directory ID and convert to *int64
	dstParentID, err := f.findDirID(ctx, dstRemote, true)
	if err != nil {
		return err
	}

	// Move the directory via API
	_, err = f.moveEntries(ctx, []int64{srcID}, dstParentID)
	if err != nil {
		return err
	}

	// Flush the source path since it no longer exists at the old location
	srcFs.dirCache.FlushDir(srcRemote)

	return nil
}

// Copy copies a file from src to dst using server-side duplicate API
func (f *Fs) Copy(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantCopy
	}

	// Extract directory and filename
	dir := path.Dir(remote)
	if dir == "." {
		dir = ""
	}
	dstName := path.Base(remote)

	// Find/create destination directory ID and convert to *int64
	destFolderID, err := f.findDirID(ctx, dir, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create destination directory: %w", err)
	}

	// Duplicate the entry
	duplicatedEntries, err := f.duplicateEntries(ctx, []int64{srcObj.entry.ID}, destFolderID)
	if err != nil {
		return nil, err
	}

	if len(duplicatedEntries) == 0 {
		return nil, fmt.Errorf("duplicate API returned empty list")
	}

	// Check if the returned entry has the expected name
	duplicatedEntry := duplicatedEntries[0]

	// If the name doesn't match, we may need to rename it
	if duplicatedEntry.Name != dstName {
		fs.Debugf(f, "Duplicated entry name %q doesn't match expected name %q, may need to rename", duplicatedEntry.Name, dstName)
		// For now, we'll just use what the API returned
		// TODO: Implement rename if needed
	}

	return &Object{
		fs:       f,
		entry:    duplicatedEntry,
		fullPath: remote,
	}, nil
}
