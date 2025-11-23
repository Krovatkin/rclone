# Drime Cloud Backend Tests

## Setup

```bash
# Build the backend
go build ./backend/drimecloud

# Create test data directory
mkdir -p test_data
```

## Test 1: Put (Upload)

Upload a file to a remote directory.

```bash
echo "test upload content" > test_data/upload_test.txt
./rclone copy test_data/upload_test.txt drimecloud:test_suite/
```

## Test 2: List

List files in a remote directory.

```bash
./rclone ls drimecloud:test_suite/
```

Expected: Should show `upload_test.txt` with size 20 bytes.

## Test 3: NewObject + Open (Download)

Download and display file content.

```bash
./rclone cat drimecloud:test_suite/upload_test.txt
```

Expected: Should output "test upload content".

## Test 4: Mkdir

Create nested directories.

```bash
./rclone mkdir drimecloud:test_suite/subdir1/subdir2
./rclone lsd drimecloud:test_suite/
```

Expected: Should show `subdir1` directory.

## Test 5: Copy

Copy a file using server-side copy.

```bash
./rclone copy drimecloud:test_suite/upload_test.txt drimecloud:test_suite/subdir1/
./rclone ls drimecloud:test_suite/subdir1/
```

Expected: Should show `upload_test.txt` in subdir1.

## Test 6: Move (File)

Move a file between directories.

```bash
echo "move test" > test_data/move_test.txt
./rclone copy test_data/move_test.txt drimecloud:test_suite/
./rclone move drimecloud:test_suite/move_test.txt drimecloud:test_suite/subdir1/subdir2/
./rclone ls drimecloud:test_suite/subdir1/subdir2/
```

Expected: Should show `move_test.txt` in subdir2, and file should be removed from test_suite root.

## Test 7: DirMove

Move an entire directory.

```bash
./rclone mkdir drimecloud:test_suite/dir_to_move
echo "content" > test_data/file_in_dir.txt
./rclone copy test_data/file_in_dir.txt drimecloud:test_suite/dir_to_move/
./rclone moveto drimecloud:test_suite/dir_to_move drimecloud:test_suite/subdir1/moved_dir
./rclone ls drimecloud:test_suite/subdir1/moved_dir/
```

Expected: Directory should be moved with all contents intact.

## Test 8: Update

Update an existing file.

```bash
echo "updated content" > test_data/upload_test.txt
./rclone copy test_data/upload_test.txt drimecloud:test_suite/
./rclone cat drimecloud:test_suite/upload_test.txt
```

Expected: Should output "updated content".

## Test 9: Remove (Delete File)

Delete a file.

```bash
echo "to delete" > test_data/delete_test.txt
./rclone copy test_data/delete_test.txt drimecloud:test_suite/
./rclone delete drimecloud:test_suite/delete_test.txt
./rclone ls drimecloud:test_suite/ | grep -q delete_test.txt && echo "✗ File still exists" || echo "✓ Remove successful"
```

Expected: File should be deleted and not appear in listing.

## Test 10: Rmdir

Remove an empty directory.

```bash
./rclone rmdir drimecloud:test_suite/subdir1/subdir2
./rclone lsd drimecloud:test_suite/subdir1/ | grep -q subdir2 && echo "✗ Dir still exists" || echo "✓ Rmdir successful"
```

Expected: Directory should be removed.

## Test 11: Pagination (25 Files)

Test pagination with multiple files.

```bash
mkdir -p test_data/pagination
seq -f "test_data/pagination/file_%02g.txt" 1 25 | xargs -I {} sh -c 'echo "test content" > {}'
./rclone copy test_data/pagination/ drimecloud:test_suite/pagination_test/
./rclone ls drimecloud:test_suite/pagination_test/ | wc -l
```

Expected: Should list all 25 files (tests pagination with perPage=5).

## Test 12: Root Operations

Upload and download from root directory.

```bash
echo "root file" > test_data/root_test.txt
./rclone copy test_data/root_test.txt drimecloud:
./rclone cat drimecloud:root_test.txt
```

Expected: Should output "root file".

## Test 13: Nested Directory Creation

Create deeply nested directories automatically during upload.

```bash
mkdir -p test_data/nested/deep/path
echo "deep file" > test_data/nested/deep/path/file.txt
./rclone copy test_data/nested/deep/path/file.txt drimecloud:test_suite/nested/deep/path/
./rclone cat drimecloud:test_suite/nested/deep/path/file.txt
```

Expected: Should create all intermediate directories and output "deep file".

## Cleanup

```bash
# Remote cleanup
./rclone purge drimecloud:test_suite
./rclone delete drimecloud:root_test.txt

# Local cleanup
rm -rf test_data
```

## Test Results (Latest Run)

All 13 tests passed successfully after rebase to latest upstream.

- ✓ Put (Upload)
- ✓ List
- ✓ NewObject + Open (Download)
- ✓ Mkdir (nested directories)
- ✓ Copy (server-side)
- ✓ Move (file)
- ✓ DirMove (directory)
- ✓ Update (existing file)
- ✓ Remove (delete file)
- ✓ Rmdir (remove directory)
- ✓ Pagination (25 files)
- ✓ Root operations
- ✓ Nested directory creation
