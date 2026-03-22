# S3 to Local Storage Migration

## Purpose

Replace AWS S3 with local filesystem storage for all asset serving on the aspirant platform. This eliminates the AWS dependency, reduces latency (assets served from the same machine), and removes recurring S3 costs.

## Scope

### In Scope

- Introduce a `StorageBackend` interface in aspirant-server
- Implement `LocalStorage` backend using the local filesystem
- Migrate all S3 handler code (`FetchObjectHandler`, `UploadImageHandler`, `ListS3AssetsHandler`) to use the new interface
- Migrate the Word Weaver dictionary loader to use the new interface
- Add asset volume mount to aspirant-deploy compose files
- One-time data migration: download S3 bucket contents to the cell
- Remove `aws-sdk-go` dependency from `go.mod`

### Out of Scope

- Refactoring the existing user file system (`files.go`) to use `StorageBackend` — this is a follow-up opportunity, not a requirement for this migration
- Changes to aspirant-client — the client talks to `/api/fetch-object/:etag`, not S3 directly
- Changes to `asset_mappings.go` — the server-side mapping file stays as-is
- CDN or caching layers

## Current State

### How S3 is used today

```
Client (asset_manager.js)
  │  semantic name → MD5 hash lookup (client-side map)
  │
  ▼
GET /api/fetch-object/{md5-hash}
  │
  ▼
Server (misc.go: FetchObjectHandler)
  │  1. InitS3Session()
  │  2. FindKeyByETag() — paginate entire bucket to match ETag
  │  3. FetchFileFromS3() — download file by key
  │  4. Detect content-type (3-case switch)
  │  5. Return bytes
  ▼
AWS S3 (eu-north-1)
```

### Files in S3

| Category | Count | Examples |
|----------|-------|---------|
| UI images | ~25 | Icons, avatars, backgrounds |
| Game assets | ~15 | Game icons, gift tiles |
| Audio files | ~6 | Sound effects, background music |
| Dictionary | 1 | Word Weaver JSON (~large) |
| User uploads | Variable | Pet photos, misc |

### Pain points in current design

1. **ETag pagination** — every asset fetch paginates the entire bucket to find the matching ETag. O(n) per request.
2. **No storage abstraction** — S3 code is directly in handlers and `data_functions`, not behind an interface.
3. **Duplicate file I/O patterns** — `files.go` (user files) and `misc.go` (S3 assets) both do file serving with different approaches.
4. **Weak content-type detection** — only 3 extensions handled (`.mp3`, `.wav`, `.png`), everything else is `application/octet-stream`.

## Target State

```
Client (asset_manager.js)          [NO CHANGES]
  │  semantic name → MD5 hash
  │
  ▼
GET /api/fetch-object/{md5-hash}   [NO ROUTE CHANGES]
  │
  ▼
Server (misc.go: FetchObjectHandler)
  │  1. Build filename from hash    [CHANGED]
  │  2. storage.Get(key)            [CHANGED — uses interface]
  │  3. Detect content-type         [IMPROVED — mime.TypeByExtension]
  │  4. Return bytes
  ▼
Local filesystem: /data/assets/    [NEW — replaces S3]
```

### Storage interface

```go
// StorageBackend abstracts file storage operations.
type StorageBackend interface {
    Get(key string) ([]byte, error)
    Put(key string, data io.Reader) error
    Delete(key string) error
    List(prefix string) ([]ObjectInfo, error)
    Stat(key string) (*ObjectInfo, error)
}

type ObjectInfo struct {
    Key          string
    Size         int64
    LastModified time.Time
    ContentType  string
    ETag         string    // MD5 hash of file contents
}
```

### Local directory layout

```
/data/assets/
├── images/
│   ├── ludde.png
│   ├── home_icon.png
│   └── ...
├── audio/
│   ├── ludde-sound.mp3
│   ├── game-bg-music.mp3
│   └── ...
├── games/
│   └── dictionary.json
└── uploads/
    └── ... (user-uploaded images)
```

### ETag lookup strategy

Current: paginate entire S3 bucket matching ETag strings.

New: build an in-memory index on startup. The `LocalStorage` implementation walks the asset directory once, computes MD5 hashes, and builds a `map[string]string` (hash → filepath). Lookups become O(1). The index refreshes on `Put` and `Delete` operations.

## Requirements

### Functional

1. `GET /fetch-object/:etag` returns identical responses (same bytes, same content-types) as before
2. `POST /upload` writes files to the local asset directory instead of S3
3. `GET /s3-assets` (renamed to `/assets`) lists local files with the same metadata shape
4. Word Weaver dictionary loads from local filesystem at startup
5. All existing client asset hashes continue to resolve correctly

### Non-Functional

1. Asset serving latency must be lower than S3 (trivially true for local filesystem)
2. No AWS credentials required in `.env` after migration
3. Asset directory must be on the RAID1 array (`/data/aspirant/assets`) for redundancy
4. Server startup must not fail if the asset directory is empty (graceful degradation)

## Acceptance Criteria

- [ ] All 75+ client asset hashes resolve and serve correctly
- [ ] Word Weaver game works (dictionary loads)
- [ ] Upload via `/upload` endpoint writes to local filesystem
- [ ] Admin asset listing works
- [ ] `aws-sdk-go` removed from `go.mod`
- [ ] No S3 environment variables required (`AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `S3_BUCKET_NAME`)
- [ ] Docker compose updated with asset volume mount
- [ ] Integration tests pass

## Standards

Follows conventions defined in [aspirant-meta](https://github.com/the-anonymous-aspirant/aspirant-meta).
