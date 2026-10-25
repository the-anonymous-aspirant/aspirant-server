package storage

import (
	"bytes"
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// system_3 #8727 (F1 of the #8653 security review): bytes the card editor
// stores under cardgen/<sha256> must not be reachable through the MD5 index
// that the Member-tier GET /fetch-object/:etag resolves, and the legacy
// delete must not remove them from under a live card_assets row.

func TestCardgenPutIsNotIndexedByETag(t *testing.T) {
	ls := setupTestStorage(t)

	content := "admin-only artwork bytes"
	etag := fmt.Sprintf("%x", md5.Sum([]byte(content)))
	for _, spelling := range cardgenSpellings(fmt.Sprintf("%064x", 1)) {
		if err := ls.Put(spelling, bytes.NewReader([]byte(content))); err != nil {
			t.Fatalf("Put %q: %v", spelling, err)
		}
		data, info, err := ls.GetByETag(etag)
		if err != nil {
			t.Fatalf("GetByETag after Put %q: %v", spelling, err)
		}
		if data != nil || info != nil {
			t.Fatalf("cardgen bytes resolved by MD5 after Put %q: got %d bytes, info %+v", spelling, len(data), info)
		}
	}
	// The bytes are still stored and readable by key (the gated route's path).
	stored, err := ls.Get("cardgen/" + fmt.Sprintf("%064x", 1))
	if err != nil || string(stored) != content {
		t.Fatalf("Get by key: %v, %q", err, string(stored))
	}

	// Positive control: the same bytes under a legacy key still resolve.
	if err := ls.Put("images/control.bin", bytes.NewReader([]byte(content))); err != nil {
		t.Fatalf("Put images: %v", err)
	}
	data, info, err := ls.GetByETag(etag)
	if err != nil || data == nil || info == nil {
		t.Fatalf("control: legacy key did not resolve by MD5: %v, %v", err, info)
	}
	if info.Key != "images/control.bin" {
		t.Fatalf("control resolved to %q, want images/control.bin", info.Key)
	}
}

func TestBuildIndexSkipsCardgenFiles(t *testing.T) {
	dir := t.TempDir()
	content := []byte("pre-existing cardgen file")
	cardgenPath := filepath.Join(dir, "cardgen", fmt.Sprintf("%064x", 2))
	if err := os.MkdirAll(filepath.Dir(cardgenPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cardgenPath, content, 0644); err != nil {
		t.Fatal(err)
	}
	legacyPath := filepath.Join(dir, "images", "legacy.png")
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte("legacy bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	ls, err := NewLocalStorage(dir)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	if n := ls.IndexSize(); n != 1 {
		t.Fatalf("index size %d, want 1 (the legacy file only)", n)
	}
	etag := fmt.Sprintf("%x", md5.Sum(content))
	data, _, err := ls.GetByETag(etag)
	if err != nil {
		t.Fatalf("GetByETag: %v", err)
	}
	if data != nil {
		t.Fatal("pre-existing cardgen file resolved by MD5 after index rebuild")
	}
	legacyETag := fmt.Sprintf("%x", md5.Sum([]byte("legacy bytes")))
	if data, _, _ := ls.GetByETag(legacyETag); data == nil {
		t.Fatal("control: legacy file did not resolve by MD5 after index rebuild")
	}
}

// cardgenSpellings are the key forms that resolve inside cardgen/: the
// canonical key and two spellings filepath.Clean folds onto it (R1 of the PR
// 145 review).
func cardgenSpellings(sha string) []string {
	return []string{"cardgen/" + sha, "./cardgen/" + sha, "images/../cardgen/" + sha}
}

func TestDeleteRefusesCardgenKeys(t *testing.T) {
	ls := setupTestStorage(t)
	key := "cardgen/" + fmt.Sprintf("%064x", 3)
	if err := ls.Put(key, bytes.NewReader([]byte("keep me"))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	for _, spelling := range cardgenSpellings(fmt.Sprintf("%064x", 3)) {
		err := ls.Delete(spelling)
		if !errors.Is(err, ErrKeyReserved) {
			t.Fatalf("Delete %q: err %v, want ErrKeyReserved", spelling, err)
		}
		if data, err := ls.Get(key); err != nil || string(data) != "keep me" {
			t.Fatalf("bytes gone after refused delete of %q: %v, %q", spelling, err, string(data))
		}
	}
	// Positive control: a legacy key still deletes.
	if err := ls.Put("images/gone.png", bytes.NewReader([]byte("x"))); err != nil {
		t.Fatal(err)
	}
	if err := ls.Delete("images/gone.png"); err != nil {
		t.Fatalf("control delete: %v", err)
	}
}
