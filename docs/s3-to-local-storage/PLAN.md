# S3 to Local Storage — Development Plan

## Overview

6 steps. Each step is independently verifiable. Steps 1-4 are server code changes in `aspirant-server`. Step 5 is infrastructure in `aspirant-deploy`. Step 6 is the data migration and cleanup.

---

## Step 1: Create the StorageBackend interface and LocalStorage implementation

**New file:** `server/storage/storage.go`

Define the `StorageBackend` interface and `ObjectInfo` struct. Implement `LocalStorage` with:

- `basePath` field (configurable root directory)
- `index` field — `map[string]string` mapping MD5 ETag → relative file path
- `buildIndex()` — walks `basePath`, computes MD5 of each file, populates the index
- `Get(key)` — reads file from disk by key (filepath)
- `GetByETag(etag)` — looks up filepath via index, then reads file (replaces the S3 pagination pattern)
- `Put(key, data)` — writes file, creates parent dirs, updates index entry
- `Delete(key)` — removes file, removes index entry
- `List(prefix)` — walks directory, returns `[]ObjectInfo`
- `Stat(key)` — returns single `ObjectInfo`
- Content-type detection via `mime.TypeByExtension()` (stdlib) instead of the 3-case switch

**Verify:** Unit test `LocalStorage` with a temp directory — write a file, read it back, verify ETag index lookup works.

---

## Step 2: Migrate handlers to use StorageBackend

**Modify:** `server/handlers/misc.go`

- `FetchObjectHandler` — replace S3 session + `FindKeyByETag` + `FetchFileFromS3` with `storage.GetByETag(etag)`. Content-type comes from `ObjectInfo.ContentType`.
- `UploadImageHandler` — replace S3 upload with `storage.Put(path, fileContent)`.
- `ListS3AssetsHandler` — replace S3 list with `storage.List("")`. Rename route to `ListAssetsHandler`.

**Modify:** `server/routes.go`

- The storage backend must be initialized and injected. Options:
  - Pass via Gin context (like `db` is done today) — consistent with existing pattern
  - Or pass as a field on a handler struct — cleaner but bigger refactor
- **Decision:** use Gin context (`c.MustGet("storage")`) to match the existing `db` pattern. Minimal change.
- Rename `/s3-assets` route to `/assets` (update client `S3Assets.vue` endpoint reference)

**Verify:** Start server with `ASSET_BASE_PATH=/tmp/test-assets`, place a test file, confirm `GET /fetch-object/:hash` returns it.

---

## Step 3: Migrate Word Weaver dictionary loading

**Modify:** `server/data_functions/word_weaver_scripts.go`

- Replace `LoadDictionaryFromS3(bucket, key)` with `LoadDictionary(path string)` that reads from local filesystem.
- The dictionary file path comes from an env var or a well-known path (e.g. `/data/assets/games/dictionary.json`).

**Verify:** Start server, play Word Weaver — word validation works.

---

## Step 4: Remove AWS SDK dependency

**Delete:** `server/data_functions/s3_functions.go` (entire file — `ListObjects`, `UploadFileToS3`)

**Modify:** `server/data_functions/utils.go`
- Remove `InitS3Session()`, `FetchFileFromS3()`, `FindKeyByETag()`
- Remove AWS SDK imports
- Keep `GitCommit` / `GetGitCommit()` (unrelated, still needed)

**Modify:** `go.mod` / `go.sum`
- `go mod tidy` to remove `github.com/aws/aws-sdk-go` and transitive deps (`github.com/jmespath/go-jmespath`)

**Verify:** `go build ./...` succeeds with no AWS imports. `go mod tidy` removes the AWS dependency.

---

## Step 5: Update aspirant-deploy

**Modify:** `docker-compose.yml` (production)
- Add volume mount: `/data/aspirant/assets:/data/assets:ro` on server (read-only for serving)
- Actually needs read-write for uploads: `/data/aspirant/assets:/data/assets`
- Add `ASSET_BASE_PATH: /data/assets` environment variable to server
- Remove `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_BUCKET_NAME` from `.env` requirements

**Modify:** `docker-compose.dev.yml` (development)
- Add named volume: `assetdata-dev:/data/assets` on server
- Add `ASSET_BASE_PATH: /data/assets` environment variable

**Modify:** `aspirant-client` (minor)
- Update `S3Assets.vue` to call `/api/assets` instead of `/api/s3-assets`
- Rename component from `S3Assets` to `Assets` (optional, cosmetic)

**Verify:** `docker compose config` validates. `docker compose -f docker-compose.dev.yml config` validates.

---

## Step 6: Data migration and go-live

**One-time task** (on the aspirant-cell):

```bash
# Create the local asset directory
mkdir -p /data/aspirant/assets

# Sync S3 bucket contents to local
aws s3 sync s3://<bucket-name> /data/aspirant/assets/ --profile aspirant

# Verify file count matches
aws s3 ls s3://<bucket-name> --recursive | wc -l
find /data/aspirant/assets -type f | wc -l
```

**Deploy:**
1. Merge aspirant-server changes (Steps 1-4)
2. Build and push new server image
3. Merge aspirant-deploy changes (Step 5)
4. `docker compose pull && docker compose up -d` on the cell
5. Verify: hit every health endpoint, test asset loading in browser, play Word Weaver

**Cleanup (after verification):**
- Remove S3 env vars from `.env` on the cell
- Optionally empty the S3 bucket (keep for 30 days as backup, then delete)

---

## File Change Summary

| File | Action | Step |
|------|--------|------|
| `server/storage/storage.go` | **Create** | 1 |
| `server/storage/storage_test.go` | **Create** | 1 |
| `server/handlers/misc.go` | Modify | 2 |
| `server/routes.go` | Modify | 2 |
| `server/data_functions/word_weaver_scripts.go` | Modify | 3 |
| `server/data_functions/s3_functions.go` | **Delete** | 4 |
| `server/data_functions/utils.go` | Modify | 4 |
| `go.mod` / `go.sum` | Modify | 4 |
| `aspirant-deploy/docker-compose.yml` | Modify | 5 |
| `aspirant-deploy/docker-compose.dev.yml` | Modify | 5 |
| `aspirant-client/src/views/admin/S3Assets.vue` | Modify | 5 |

## Risks

| Risk | Mitigation |
|------|-----------|
| Missing files after S3 sync | Verify file count + spot-check hashes before cutting over |
| Hash mismatch (S3 ETag vs local MD5) | S3 ETags for single-part uploads are MD5 — should match. Multipart uploads have different ETags — check if any exist |
| Dictionary file too large for memory | Already loaded into memory today from S3 — no change in memory profile |
| Concurrent upload + index race | Single-server deployment — no concurrency risk. If needed later, add `sync.RWMutex` to the index |
