package storage

import (
	"bytes"
	"crypto/md5"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func setupTestStorage(t *testing.T) *LocalStorage {
	t.Helper()
	dir := t.TempDir()
	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return ls
}

func writeTestFile(t *testing.T, ls *LocalStorage, key, content string) string {
	t.Helper()
	err := ls.Put(key, bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatalf("Put(%s): %v", key, err)
	}
	h := md5.Sum([]byte(content))
	return fmt.Sprintf("%x", h)
}

func TestPutAndGet(t *testing.T) {
	ls := setupTestStorage(t)

	content := "hello world"
	err := ls.Put("test/hello.txt", bytes.NewReader([]byte(content)))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	data, err := ls.Get("test/hello.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != content {
		t.Errorf("Get returned %q, want %q", string(data), content)
	}
}

func TestGetByETag(t *testing.T) {
	ls := setupTestStorage(t)

	content := "asset data"
	etag := writeTestFile(t, ls, "images/icon.png", content)

	data, info, err := ls.GetByETag(etag)
	if err != nil {
		t.Fatalf("GetByETag: %v", err)
	}
	if data == nil {
		t.Fatal("GetByETag returned nil data")
	}
	if string(data) != content {
		t.Errorf("GetByETag returned %q, want %q", string(data), content)
	}
	if info.Key != "images/icon.png" {
		t.Errorf("info.Key = %q, want %q", info.Key, "images/icon.png")
	}
	if info.ContentType != "image/png" {
		t.Errorf("info.ContentType = %q, want %q", info.ContentType, "image/png")
	}
}

func TestGetByETagWithQuotes(t *testing.T) {
	ls := setupTestStorage(t)

	content := "quoted etag test"
	etag := writeTestFile(t, ls, "test.txt", content)

	// S3-style quoted ETag
	quoted := fmt.Sprintf("\"%s\"", etag)
	data, _, err := ls.GetByETag(quoted)
	if err != nil {
		t.Fatalf("GetByETag: %v", err)
	}
	if data == nil {
		t.Fatal("GetByETag with quotes returned nil")
	}
}

func TestGetByETagNotFound(t *testing.T) {
	ls := setupTestStorage(t)

	data, info, err := ls.GetByETag("nonexistent")
	if err != nil {
		t.Fatalf("GetByETag: %v", err)
	}
	if data != nil || info != nil {
		t.Error("expected nil for nonexistent ETag")
	}
}

func TestList(t *testing.T) {
	ls := setupTestStorage(t)

	writeTestFile(t, ls, "images/a.png", "aaa")
	writeTestFile(t, ls, "images/b.png", "bbb")
	writeTestFile(t, ls, "audio/c.mp3", "ccc")

	// List all
	all, err := ls.List("")
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(all) != 3 {
		t.Errorf("List(\"\") returned %d items, want 3", len(all))
	}

	// List prefix
	images, err := ls.List("images")
	if err != nil {
		t.Fatalf("List(images): %v", err)
	}
	if len(images) != 2 {
		t.Errorf("List(images) returned %d items, want 2", len(images))
	}
}

func TestDelete(t *testing.T) {
	ls := setupTestStorage(t)

	content := "to be deleted"
	etag := writeTestFile(t, ls, "temp.txt", content)

	// Verify it exists
	data, _, _ := ls.GetByETag(etag)
	if data == nil {
		t.Fatal("file should exist before delete")
	}

	err := ls.Delete("temp.txt")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// Verify ETag index is cleaned up
	data, _, _ = ls.GetByETag(etag)
	if data != nil {
		t.Error("file should not be found by ETag after delete")
	}

	// Verify file is gone
	_, err = ls.Get("temp.txt")
	if err == nil {
		t.Error("Get should fail after delete")
	}
}

func TestStat(t *testing.T) {
	ls := setupTestStorage(t)

	content := "stat me"
	etag := writeTestFile(t, ls, "audio/track.mp3", content)

	info, err := ls.Stat("audio/track.mp3")
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if info.Size != int64(len(content)) {
		t.Errorf("Size = %d, want %d", info.Size, len(content))
	}
	if info.ETag != etag {
		t.Errorf("ETag = %q, want %q", info.ETag, etag)
	}
	if info.ContentType != "audio/mpeg" {
		t.Errorf("ContentType = %q, want %q", info.ContentType, "audio/mpeg")
	}
}

func TestContentTypeDetection(t *testing.T) {
	cases := []struct {
		path     string
		expected []string
	}{
		{"file.png", []string{"image/png"}},
		{"file.mp3", []string{"audio/mpeg"}},
		{"file.wav", []string{"audio/x-wav", "audio/vnd.wave"}},
		{"file.jpg", []string{"image/jpeg"}},
		{"file.json", []string{"application/json"}},
		{"file.unknown", []string{"application/octet-stream"}},
	}
	for _, tc := range cases {
		ct := detectContentType(tc.path)
		found := false
		for _, exp := range tc.expected {
			if ct == exp {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("detectContentType(%q) = %q, want one of %v", tc.path, ct, tc.expected)
		}
	}
}

func TestIndexRebuildsOnStartup(t *testing.T) {
	dir := t.TempDir()

	// Pre-populate directory with files
	subdir := filepath.Join(dir, "images")
	os.MkdirAll(subdir, 0755)
	os.WriteFile(filepath.Join(subdir, "test.png"), []byte("pre-existing"), 0644)

	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}

	if ls.IndexSize() != 1 {
		t.Errorf("IndexSize = %d, want 1", ls.IndexSize())
	}

	// Verify the pre-existing file is findable by ETag
	h := md5.Sum([]byte("pre-existing"))
	etag := fmt.Sprintf("%x", h)
	data, _, err := ls.GetByETag(etag)
	if err != nil {
		t.Fatalf("GetByETag: %v", err)
	}
	if data == nil {
		t.Error("pre-existing file should be in index after startup")
	}
}

func TestPutCreatesParentDirs(t *testing.T) {
	ls := setupTestStorage(t)

	err := ls.Put("deeply/nested/dir/file.txt", bytes.NewReader([]byte("nested")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}

	data, err := ls.Get("deeply/nested/dir/file.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if string(data) != "nested" {
		t.Errorf("got %q, want %q", string(data), "nested")
	}
}
