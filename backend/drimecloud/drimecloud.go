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
		rootID: nil, // Always start with null root_id
	}

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

		// Copy:       f.copy,       // Drime has duplicate endpoint
		// Move:       f.move,       // Drime has move endpoint
		// DirMove:    f.dirMove,    // Can move folders too
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

	// Read the content
	content, err := io.ReadAll(in)
	if err != nil {
		return nil, fmt.Errorf("failed to read content: %w", err)
	}

	// Create folder structure if needed
	dir := path.Dir(remote)
	var parentID *int64 = nil // Start from root
	if dir != "." && dir != "" {
		parentID, err = f.ensureFolderPath(ctx, dir)
		if err != nil {
			return nil, fmt.Errorf("failed to create folder structure: %w", err)
		}
	}

	// Create multipart form body
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add file content
	fileWriter, err := writer.CreateFormFile("file", path.Base(remote))
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}
	_, err = fileWriter.Write(content)
	if err != nil {
		return nil, fmt.Errorf("failed to write file content: %w", err)
	}

	// Add parent ID if specified
	if parentID != nil {
		err = writer.WriteField("parentId", strconv.FormatInt(*parentID, 10))
		if err != nil {
			return nil, fmt.Errorf("failed to add parentId field: %w", err)
		}
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

	return &Object{
		fs:    f,
		entry: *response.FileEntry,
	}, nil
}

// ensureFolderPath creates folder structure and returns the final folder ID
func (f *Fs) ensureFolderPath(ctx context.Context, folderPath string) (*int64, error) {
	if folderPath == "" || folderPath == "." {
		return nil, nil // Root is always null
	}

	parts := strings.Split(strings.Trim(folderPath, "/"), "/")
	var currentParentID *int64 = nil // Always start from root (null)

	for _, part := range parts {
		// Check if folder already exists
		entries, err := f.listEntries(ctx, currentParentID, "folder")
		if err != nil {
			return nil, err
		}

		found := false
		for _, entry := range entries {
			if entry.Name == part && entry.Type == "folder" {
				currentParentID = &entry.ID
				found = true
				break
			}
		}

		if !found {
			// Create folder
			folderID, err := f.createFolder(ctx, part, currentParentID)
			if err != nil {
				return nil, err
			}
			currentParentID = folderID
		}
	}

	return currentParentID, nil
}

// createFolder creates a new folder
func (f *Fs) createFolder(ctx context.Context, name string, parentID *int64) (*int64, error) {
	payload := map[string]interface{}{
		"name": name,
	}
	if parentID != nil {
		payload["parentId"] = *parentID
	}

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

	return &response.Folder.ID, nil
}

// Mkdir creates a directory
func (f *Fs) Mkdir(ctx context.Context, dir string) error {
	_, err := f.ensureFolderPath(ctx, dir)
	return err
}

// List lists the objects and directories in dir
func (f *Fs) List(ctx context.Context, dir string) (entries fs.DirEntries, err error) {
	var parentID *int64 = nil // Start from root

	// If we have a directory path, traverse to find its ID
	if dir != "" {
		parentID, err = f.ensureFolderPath(ctx, dir)
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
			entries = append(entries, fs.NewDir(entry.Name, time.Time{}))
		} else {
			entries = append(entries, &Object{fs: f, entry: entry})
		}
	}

	return entries, nil
}

// NewObject finds the Object at remote
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	dir := path.Dir(remote)
	name := path.Base(remote)

	var parentID *int64 = nil // Start from root
	var err error

	if dir != "." && dir != "" {
		parentID, err = f.ensureFolderPath(ctx, dir)
		if err != nil {
			return nil, err
		}
	}

	entries, err := f.listEntries(ctx, parentID, "")
	if err != nil {
		return nil, err
	}

	for _, entry := range entries {
		if entry.Name == name && entry.Type != "folder" {
			return &Object{fs: f, entry: entry}, nil
		}
	}

	return nil, fs.ErrorObjectNotFound
}

// Remove removes a file
func (f *Fs) Remove(ctx context.Context, remote string) error {
	obj, err := f.NewObject(ctx, remote)
	if err != nil {
		return err
	}
	return obj.Remove(ctx)
}

// Rmdir removes a directory
func (f *Fs) Rmdir(ctx context.Context, dir string) error {
	if dir == "" {
		return fmt.Errorf("cannot remove root directory")
	}

	parentID, err := f.ensureFolderPath(ctx, dir)
	if err != nil {
		return err
	}

	if parentID == nil {
		return fmt.Errorf("directory not found")
	}

	payload := map[string]interface{}{
		"entryIds":      []string{strconv.FormatInt(*parentID, 10)},
		"deleteForever": true,
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
	fs    *Fs
	entry FileEntry
}

// Fs returns the parent Fs
func (o *Object) Fs() fs.Info {
	return o.fs
}

// Remote returns the remote path
func (o *Object) Remote() string {
	return o.entry.Name
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
