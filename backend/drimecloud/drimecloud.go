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
	"mime/multipart"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/fs/config/configstruct"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/lib/rest"
)

const (
	apiBaseURL = "https://app.drime.cloud/api/v1"
)

// Options defines the configuration for the backend.
type Options struct {
	AuthToken string `config:"auth_token"`
}

// Register the backend with rclone
func init() {
	fs.Register(&fs.RegInfo{
		Name:        "drime",
		Description: "Drime Cloud Storage",
		NewFs:       NewFs,
		Options: []fs.Option{
			{Name: "auth_token", Help: "Your Drime API auth token"},
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

// Fs represents a connection to Drime storage
type Fs struct {
	name   string
	root   string
	opt    Options
	client *rest.Client
	rootID *int64 // ID of the root folder for this mount
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
		root:   root, // TODO: maybe do better latter?
		opt:    *opt,
		client: client,
		rootID: nil, // Always start with null root_id
	}

	entry, err := f.getOrCreateFileEntry(ctx, root, true)

	if err == nil {
		if entry != nil && entry.Type != "folder" {
			f.root = path.Dir(f.root)
			return f, fs.ErrorIsFile
		}
	}

	// if err != nil {
	// 	return nil, fmt.Errorf("couldn't identify the type of root: %w", err)
	// }

	// if entry != nil && entry.Type != "folder" {
	// 	rootDir := path.Dir(root)
	// 	fs.Debugf(f, "Setting root to %q", rootDir)
	// 	f.root = path.Dir(root)
	// }

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
		WriteMimeType:            false, // Upload endpoint doesn't allow setting mime type
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

		// TODO:
		// Copy: f.Copy
		Move:    f.Move, // Drime has move endpoint
		DirMove: f.DirMove,
		// PublicLink: f.publicLink, // Has shareable links API
		// PutStream:  f.putStream,  // Upload endpoint can handle streams
		// CleanUp:    f.cleanUp,    // Has restore from trash functionality
	}
}

// listEntries lists entries in a folder
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

// listEntries lists entries in a folder
func (f *Fs) listEntries(ctx context.Context, parentID *int64, entryType string) ([]FileEntry, error) {
	params := url.Values{}
	params.Set("perPage", "10000") // Increased to handle more entries

	if parentID != nil {
		params.Set("parentIds", strconv.FormatInt(*parentID, 10))
	}

	if entryType != "" {
		params.Set("type", entryType)
	}

	var response PaginatedResponse
	_, err := f.client.CallJSON(ctx, &rest.Opts{
		Method:     "GET",
		Path:       "/drive/file-entries",
		Parameters: params,
	}, nil, &response)

	if err != nil {
		return nil, err
	}

	// Return just the data array
	return response.Data, nil
}

// Put uploads a new file
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	return f.putUnchecked(ctx, in, src)
}

func (f *Fs) putUnchecked(ctx context.Context, in io.Reader, src fs.ObjectInfo) (fs.Object, error) {
	remote := src.Remote()
	fullPath := path.Join(f.root, remote)

	// Read the content
	content, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}

	// Create folder structure if needed
	dir := path.Dir(fullPath)

	var parentEntry *FileEntry = nil // Start from root
	if dir != "." && dir != "" {
		parentEntry, err = f.getOrCreateFileEntry(ctx, dir, true)
		if err != nil {
			return nil, fmt.Errorf("failed to create folder structure: %w", err)
		}
	}

	// Create multipart form body
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add file content
	fileWriter, err := writer.CreateFormFile("file", path.Base(fullPath))
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	_, err = fileWriter.Write(content)
	if err != nil {
		return nil, fmt.Errorf("failed to write file content: %w", err)
	}

	parentIdStr := "<nil>"
	// Add parent ID if specified
	if parentEntry != nil {
		parentIdStr = strconv.FormatInt(parentEntry.ID, 10)
		err = writer.WriteField("parentId", parentIdStr)
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
	fs.Debugf(f, "Upload request: parentID=%q, filename=%q, fullPath=%q",
		parentIdStr, path.Base(fullPath), fullPath)
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

	return &Object{
		fs:       f,
		entry:    *response.FileEntry,
		fullPath: remote, // Keep the original remote path for the object
	}, nil
}

func (f *Fs) getOrCreateFileEntry(ctx context.Context, entryPath string, create bool) (*FileEntry, error) {
	entryPath = strings.Trim(entryPath, "/")
	if entryPath == "" || entryPath == "." {
		return nil, nil // Root is always nil
	}

	parts := strings.Split(strings.Trim(entryPath, "/"), "/")
	var currentEntry *FileEntry = nil

	for _, part := range parts {
		var parentID *int64 = nil
		if currentEntry != nil {
			parentID = &currentEntry.ID
		}

		fs.Debugf(f, "Processing entry part: %q, parentID: %v", part, parentID)

		// Check if entry already exists
		entries, err := f.listEntries(ctx, parentID, "")
		if err != nil {
			return nil, err
		}

		fs.Debugf(f, "Found %d entries in parent %v", len(entries), parentID)

		// Use slices.IndexFunc to find the entry
		entryIndex := slices.IndexFunc(entries, func(entry FileEntry) bool {
			return entry.Name == part
		})

		if entryIndex != -1 {
			// Entry found
			entry := entries[entryIndex]
			fs.Debugf(f, "Found existing entry %q with ID %d, type %q", part, entry.ID, entry.Type)
			currentEntry = &entry
		} else {
			// Entry not found
			if !create {
				return nil, fs.ErrorDirNotFound
			}

			fs.Debugf(f, "Entry %q not found, creating new folder with parentID %v", part, parentID)

			// Create folder - createFolder returns the complete FileEntry
			currentEntry, err = f.createFolder(ctx, part, parentID)
			if err != nil {
				return nil, err
			}

			fs.Debugf(f, "Created folder %q with ID %d", part, currentEntry.ID)
		}
	}

	return currentEntry, nil
}

// getRemoteFolderId creates folder structure and returns the final folder ID
// func (f *Fs) getRemoteFolderId(ctx context.Context, folderPath string, create bool) (*int64, error) {
// 	folderPath = strings.Trim(folderPath, "/")
// 	if folderPath == "" || folderPath == "." {
// 		return nil, nil // Root is always null
// 	}

// 	parts := strings.Split(strings.Trim(folderPath, "/"), "/")
// 	var currentParentID *int64 = nil // Always start from root (null)
// 	for _, part := range parts {
// 		fs.Debugf(f, "Processing folder part: %q, currentParentID: %v", part, currentParentID)

// 		// Check if folder already exists
// 		entries, err := f.listEntries(ctx, currentParentID, "folder")
// 		if err != nil {
// 			return nil, err
// 		}

// 		fs.Debugf(f, "Found %d folder entries in parent %v", len(entries), currentParentID)

// 		found := false
// 		for _, entry := range entries {
// 			fs.Debugf(f, "Checking entry: name=%q, type=%q, id=%d", entry.Name, entry.Type, entry.ID)
// 			if entry.Name == part && entry.Type == "folder" {
// 				currentParentID = &entry.ID
// 				found = true
// 				fs.Debugf(f, "Found existing folder %q with ID %d", part, entry.ID)
// 				break
// 			}
// 		}

// 		if !found {
// 			if !create {
// 				// Don't create - return error that folder doesn't exist
// 				return nil, fmt.Errorf("folder %q not found in path %q", part, folderPath)
// 			}

// 			fs.Debugf(f, "Folder %q not found, creating new folder with parentID %v", part, currentParentID)
// 			// Create folder
// 			folderID, err := f.createFolder(ctx, part, currentParentID)
// 			if err != nil {
// 				return nil, err
// 			}
// 			currentParentID = folderID
// 			fs.Debugf(f, "Created folder %q with ID %d", part, *folderID)
// 		} else {
// 			fs.Debugf(f, "Using existing folder %q with ID %d", part, *currentParentID)
// 		}
// 	}

// 	return currentParentID, nil
// }

// createFolder creates a new folder
// func (f *Fs) createFolder(ctx context.Context, name string, parentID *int64) (*int64, error) {
// 	payload := map[string]interface{}{
// 		"name": name,
// 	}
// 	if parentID != nil {
// 		payload["parentId"] = *parentID
// 	}

// 	fs.Debugf(f, "Request to /folders with the following payload: name=%q, parentId=%v", name, parentID)

// 	var response APIResponse
// 	_, err := f.client.CallJSON(ctx, &rest.Opts{
// 		Method: "POST",
// 		Path:   "/folders",
// 	}, &payload, &response)

// 	if err != nil {
// 		return nil, fmt.Errorf("failed to create folder: %w", err)
// 	}

// 	if response.Status != "success" || response.Folder == nil {
// 		return nil, fmt.Errorf("failed to create folder: %s", response.Message)
// 	}

// 	return &response.Folder.ID, nil
// }

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
	fullPath := path.Join(f.root, dir)
	fs.Debugf(f, "Mkdir folder part: %q, fullPath: %q", dir, fullPath)
	_, err := f.getOrCreateFileEntry(ctx, fullPath, true)
	return err
}

// List lists the objects and directories in dir
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	fullPath := path.Join(f.root, dir)
	fs.Debugf(f, "In List: dir = %q , fullPath = %q", dir, fullPath)

	var parentID *int64 = nil

	if fullPath != "" {
		parentEntry, err := f.getOrCreateFileEntry(ctx, fullPath, false)

		if parentEntry.Type != "folder" {
			return nil, fs.ErrorIsFile
		}

		parentID = &parentEntry.ID
		if err != nil {
			return nil, err
		}
	}

	fileEntries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return nil, err
	}

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
	fullPath := path.Join(f.root, remote)
	fs.Debugf(f, "In NewObject: remote = %q, fullPath = %q", fullPath)
	dir := path.Dir(fullPath)
	name := path.Base(fullPath)

	var parentID *int64 = nil
	var err error

	if dir != "." && dir != "" {
		parentEntry, err := f.getOrCreateFileEntry(ctx, dir, false)
		parentID = &parentEntry.ID
		if err != nil {
			return nil, err
		}

		fs.Debugf(f, "Successfully found parent directory %q with ID: %d", dir, *parentID)
	}

	entries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {

		if entry.Name == name {

			if entry.Type == "folder" {
				return nil, fs.ErrorIsDir
			}

			fs.Debugf(f, "Successfully found object %q (%d)", name, entry.ID)
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
	fullPath := path.Join(f.root, dir)
	if fullPath == "" {
		return fmt.Errorf("cannot remove root directory")
	}

	parentEntry, err := f.getOrCreateFileEntry(ctx, fullPath, false)
	parentID := &parentEntry.ID

	if err != nil {
		return err
	}

	if parentID == nil {
		return fmt.Errorf("can't remove root")
	}

	if parentEntry.Type != "folder" {
		return fs.ErrorIsFile
	}

	payload := map[string]interface{}{
		"entryIds":      []string{strconv.FormatInt(*parentID, 10)},
		"deleteForever": false,
	}

	var response APIResponse
	_, err = f.client.CallJSON(ctx, &rest.Opts{
		Method: "DELETE",
		Path:   "/file-entries",
	}, &payload, &response)

	if err != nil {
		return fmt.Errorf("failed to remove directory: %w", err)
	}

	if response.Status != "success" {
		return fmt.Errorf("failed to remove directory: %s", response.Message)
	}

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

// SetModTime sets the modification time of the local fs object
func (o *Object) SetModTime(ctx context.Context, modTime time.Time) error {
	return fs.ErrorCantSetModTime
}

// Storable returns whether this object is storable
func (o *Object) Storable() bool {
	return true
}

// Open opens the file for read
func (o *Object) Open(ctx context.Context, options ...fs.OpenOption) (io.ReadCloser, error) {
	// Download file using the URL from the entry
	downloadURL := apiBaseURL + "/" + o.entry.URL

	resp, err := o.fs.client.Call(ctx, &rest.Opts{
		Method:  "GET",
		RootURL: downloadURL,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to download file: %w", err)
	}

	return resp.Body, nil
}

// Update updates the object with the new content
func (o *Object) Update(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) error {
	newObj, err := o.fs.putUnchecked(ctx, in, src)
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
		Method: "DELETE",
		Path:   "/file-entries",
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

// Move moves a file from src to dst
func (f *Fs) Move(ctx context.Context, src fs.Object, remote string) (fs.Object, error) {

	fs.Debugf(f, "In Move remote %q", remote)
	srcObj, ok := src.(*Object)
	if !ok {
		return nil, fs.ErrorCantMove
	}

	fs.Debugf(f, "In Move srcObj.fullPath = %q remote = %q", srcObj.fullPath, remote)

	fullPath := path.Join(f.root, remote)
	dir := path.Dir(fullPath)
	dstName := path.Base(fullPath)

	if srcObj.entry.Name != dstName {
		return nil, fs.ErrorCantMove
	}

	var destFolderID *int64 = nil
	if dir != "" && dir != "." {
		dirObj, err := f.getOrCreateFileEntry(ctx, dir, false)
		if err != nil {
			return nil, fmt.Errorf("destination directory doesn't exist: %s (%w)", dir, err)
		}
		destFolderID = &dirObj.ID
	}

	// Move the entry
	// DRIME API borked, moveEntries returns an empty list
	_, err := f.moveEntries(ctx, []int64{srcObj.entry.ID}, destFolderID)
	if err != nil {
		return nil, err
	}

	return f.NewObject(ctx, remote)

	// DRIME API borked because moveEntries
	// return &Object{
	// 	fs:       f,
	// 	entry:    movedEntries[0],
	// 	fullPath: remote,
	// }, nil
}

// DirMove moves a directory from src to dst
func (f *Fs) DirMove(ctx context.Context, src fs.Fs, srcRemote, dstRemote string) error {

	fs.Debugf(f, "In Move DirMove: %q", dstRemote)
	srcFs, ok := src.(*Fs)
	if !ok {
		return fs.ErrorCantDirMove
	}

	// Get source directory with full path
	srcFullPath := path.Join(srcFs.root, srcRemote)

	if srcFullPath == "" {
		return fmt.Errorf("can't move the root folder")
	}

	srcEntry, srcErr := srcFs.getOrCreateFileEntry(ctx, srcFullPath, false)

	if srcErr != nil {
		return srcErr
	}

	if srcEntry == nil {
		return fmt.Errorf("internal error: can't move the root folder")
	}

	if srcEntry.Type != "folder" {
		return fs.ErrorIsFile
	}

	// Get destination parent directory with full path
	dstFullPath := path.Join(f.root, dstRemote)
	dstEntry, dstErr := f.getOrCreateFileEntry(ctx, dstFullPath, true)

	if dstErr != nil {
		return dstErr
	}

	var dstParentID *int64 = nil

	if dstEntry != nil {
		if dstEntry.Type != "folder" {
			return fs.ErrorIsFile
		}
		dstParentID = &dstEntry.ID
	}

	// Move the directory
	_, moveErr := f.moveEntries(ctx, []int64{srcEntry.ID}, dstParentID)
	return moveErr
}
