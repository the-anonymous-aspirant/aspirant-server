package storage

import (
	"crypto/md5"
	"errors"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ErrKeyOutsideStore is returned by every LocalStorage method that turns a key
// into a path when the key is empty, carries a NUL byte, is absolute, or
// resolves to the store root or outside it. Handlers map it to 400.
//
// Before this (system_3 #8657) a key was joined to basePath after
// filepath.Clean, and Clean keeps a leading ".." on a relative key, so
// "../files/x" resolved to a sibling of the store. The Member upload route
// passed its key verbatim from the client.
var ErrKeyOutsideStore = errors.New("storage: key resolves outside the store")

// ObjectInfo holds metadata about a stored object.
type ObjectInfo struct {
	Key          string    `json:"key"`
	Size         int64     `json:"size"`
	LastModified time.Time `json:"last_modified"`
	ContentType  string    `json:"content_type"`
	ETag         string    `json:"etag"`
}

// StorageBackend abstracts file storage operations.
type StorageBackend interface {
	Get(key string) ([]byte, error)
	GetByETag(etag string) ([]byte, *ObjectInfo, error)
	Put(key string, data io.Reader) error
	Delete(key string) error
	List(prefix string) ([]ObjectInfo, error)
	Stat(key string) (*ObjectInfo, error)
}

// LocalStorage implements StorageBackend using the local filesystem.
type LocalStorage struct {
	basePath string

	mu    sync.RWMutex
	index map[string]string // etag → relative key
}

// NewLocalStorage creates a LocalStorage rooted at basePath and builds the ETag index.
func NewLocalStorage(basePath string) (*LocalStorage, error) {
	if err := os.MkdirAll(basePath, 0755); err != nil {
		return nil, fmt.Errorf("create base path: %w", err)
	}

	// Absolute so the confinement check in resolve compares like with like.
	absBase, err := filepath.Abs(basePath)
	if err != nil {
		return nil, fmt.Errorf("resolve base path: %w", err)
	}

	ls := &LocalStorage{
		basePath: absBase,
		index:    make(map[string]string),
	}

	if err := ls.buildIndex(); err != nil {
		return nil, fmt.Errorf("build index: %w", err)
	}

	return ls, nil
}

// resolve turns a storage key into an absolute path inside basePath, or
// returns ErrKeyOutsideStore. The check is lexical: the cleaned key must be
// relative and the joined path must sit strictly below basePath.
func (ls *LocalStorage) resolve(key string) (string, error) {
	if key == "" || strings.IndexByte(key, 0) >= 0 {
		return "", ErrKeyOutsideStore
	}
	cleaned := filepath.Clean(key)
	if filepath.IsAbs(cleaned) {
		return "", ErrKeyOutsideStore
	}
	full := filepath.Join(ls.basePath, cleaned)
	rel, err := filepath.Rel(ls.basePath, full)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", ErrKeyOutsideStore
	}
	return full, nil
}

func (ls *LocalStorage) Get(key string) ([]byte, error) {
	path, err := ls.resolve(key)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func (ls *LocalStorage) GetByETag(etag string) ([]byte, *ObjectInfo, error) {
	etag = normalizeETag(etag)

	ls.mu.RLock()
	key, ok := ls.index[etag]
	ls.mu.RUnlock()

	if !ok {
		return nil, nil, nil
	}

	data, err := ls.Get(key)
	if err != nil {
		return nil, nil, err
	}

	info, err := ls.Stat(key)
	if err != nil {
		return nil, nil, err
	}

	return data, info, nil
}

func (ls *LocalStorage) Put(key string, data io.Reader) error {
	path, err := ls.resolve(key)
	if err != nil {
		return err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("create parent dirs: %w", err)
	}

	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	h := md5.New()
	w := io.MultiWriter(f, h)

	if _, err := io.Copy(w, data); err != nil {
		return err
	}

	etag := fmt.Sprintf("%x", h.Sum(nil))

	ls.mu.Lock()
	ls.index[etag] = key
	ls.mu.Unlock()

	return nil
}

func (ls *LocalStorage) Delete(key string) error {
	path, err := ls.resolve(key)
	if err != nil {
		return err
	}

	// Remove from index before deleting file
	info, err := ls.Stat(key)
	if err == nil && info != nil {
		ls.mu.Lock()
		delete(ls.index, info.ETag)
		ls.mu.Unlock()
	}

	return os.Remove(path)
}

func (ls *LocalStorage) List(prefix string) ([]ObjectInfo, error) {
	// An empty prefix lists the whole store; anything else must resolve
	// inside it like a key.
	searchPath := ls.basePath
	if prefix != "" {
		p, err := ls.resolve(prefix)
		if err != nil {
			return nil, err
		}
		searchPath = p
	}
	var objects []ObjectInfo

	err := filepath.WalkDir(searchPath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(ls.basePath, path)
		if err != nil {
			return nil
		}

		fi, err := d.Info()
		if err != nil {
			return nil
		}

		objects = append(objects, ObjectInfo{
			Key:          rel,
			Size:         fi.Size(),
			LastModified: fi.ModTime(),
			ContentType:  detectContentType(path),
			ETag:         ls.lookupETag(rel),
		})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return []ObjectInfo{}, nil
		}
		return nil, err
	}

	return objects, nil
}

func (ls *LocalStorage) Stat(key string) (*ObjectInfo, error) {
	path, err := ls.resolve(key)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}

	return &ObjectInfo{
		Key:          key,
		Size:         fi.Size(),
		LastModified: fi.ModTime(),
		ContentType:  detectContentType(path),
		ETag:         ls.lookupETag(key),
	}, nil
}

// buildIndex walks basePath and computes MD5 hashes for all files.
func (ls *LocalStorage) buildIndex() error {
	return filepath.WalkDir(ls.basePath, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}

		rel, err := filepath.Rel(ls.basePath, path)
		if err != nil {
			return nil
		}

		etag, err := computeMD5(path)
		if err != nil {
			return nil // skip files we can't hash
		}

		ls.index[etag] = rel
		return nil
	})
}

// lookupETag finds the ETag for a given key by reverse-searching the index.
func (ls *LocalStorage) lookupETag(key string) string {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	for etag, k := range ls.index {
		if k == key {
			return etag
		}
	}
	return ""
}

// IndexSize returns the number of entries in the ETag index.
func (ls *LocalStorage) IndexSize() int {
	ls.mu.RLock()
	defer ls.mu.RUnlock()
	return len(ls.index)
}

func computeMD5(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := md5.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}

func detectContentType(path string) string {
	ext := strings.ToLower(filepath.Ext(path))
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// normalizeETag strips surrounding double quotes from an ETag string.
func normalizeETag(etag string) string {
	return strings.Trim(etag, "\"")
}
