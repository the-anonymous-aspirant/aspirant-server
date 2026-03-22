package storage

import (
	"crypto/md5"
	"fmt"
	"io"
	"mime"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

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

	ls := &LocalStorage{
		basePath: basePath,
		index:    make(map[string]string),
	}

	if err := ls.buildIndex(); err != nil {
		return nil, fmt.Errorf("build index: %w", err)
	}

	return ls, nil
}

func (ls *LocalStorage) Get(key string) ([]byte, error) {
	path := filepath.Join(ls.basePath, filepath.Clean(key))
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
	path := filepath.Join(ls.basePath, filepath.Clean(key))

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
	path := filepath.Join(ls.basePath, filepath.Clean(key))

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
	searchPath := filepath.Join(ls.basePath, filepath.Clean(prefix))
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
	path := filepath.Join(ls.basePath, filepath.Clean(key))
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
