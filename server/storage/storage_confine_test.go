package storage

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// system_3 #8657: a storage key is a relative path inside basePath and
// nothing else. Before this, filepath.Clean kept a leading ".." on a relative
// key and Join resolved it, so "../files/x" from the Member upload route wrote
// a sibling of the store (into /data/files on the deployed container).

func confinedStore(t *testing.T) (*LocalStorage, string) {
	t.Helper()
	parent := t.TempDir()
	base := filepath.Join(parent, "assets")
	ls, err := NewLocalStorage(base)
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	return ls, parent
}

// outsideFiles lists every file under parent that is not inside parent/assets.
func outsideFiles(t *testing.T, parent string) []string {
	t.Helper()
	var out []string
	filepath.WalkDir(parent, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !strings.HasPrefix(p, filepath.Join(parent, "assets")+string(filepath.Separator)) {
			out = append(out, p)
		}
		return nil
	})
	return out
}

func TestResolve_RefusesKeysThatLeaveTheStore(t *testing.T) {
	ls, parent := confinedStore(t)
	escaping := []string{
		"../x",
		"a/../../x",
		"../files/users/7/x.pdf",
		"../../root/x",
		"..",
		"../",
		"/etc/x",
		"",
		"a\x00b",
		".",
		"./",
		"a/..", // resolves to the store root itself
	}
	for _, key := range escaping {
		t.Run(strings.ReplaceAll(key, "\x00", "<NUL>"), func(t *testing.T) {
			if err := ls.Put(key, strings.NewReader("payload")); !errors.Is(err, ErrKeyOutsideStore) {
				t.Errorf("Put(%q) = %v, want ErrKeyOutsideStore", key, err)
			}
			if _, err := ls.Get(key); !errors.Is(err, ErrKeyOutsideStore) {
				t.Errorf("Get(%q) = %v, want ErrKeyOutsideStore", key, err)
			}
			if _, err := ls.Stat(key); !errors.Is(err, ErrKeyOutsideStore) {
				t.Errorf("Stat(%q) = %v, want ErrKeyOutsideStore", key, err)
			}
			if err := ls.Delete(key); !errors.Is(err, ErrKeyOutsideStore) {
				t.Errorf("Delete(%q) = %v, want ErrKeyOutsideStore", key, err)
			}
			if key != "" {
				if _, err := ls.List(key); !errors.Is(err, ErrKeyOutsideStore) {
					t.Errorf("List(%q) = %v, want ErrKeyOutsideStore", key, err)
				}
			}
		})
	}
	if files := outsideFiles(t, parent); len(files) != 0 {
		t.Fatalf("refused keys still wrote outside the store: %v", files)
	}
}

func TestResolve_InsideKeysRoundTrip(t *testing.T) {
	ls, parent := confinedStore(t)
	// Positive control: an ordinary key, a nested key, and a key with a
	// redundant segment that still lands inside.
	for _, key := range []string{"images/a.png", "deep/er/path/file.bin", "images/./b.png", "images/sub/../c.png"} {
		if err := ls.Put(key, strings.NewReader("data-"+key)); err != nil {
			t.Fatalf("Put(%q): %v", key, err)
		}
		got, err := ls.Get(key)
		if err != nil || string(got) != "data-"+key {
			t.Fatalf("Get(%q) = %q, %v", key, got, err)
		}
		if _, err := ls.Stat(key); err != nil {
			t.Fatalf("Stat(%q): %v", key, err)
		}
	}
	objs, err := ls.List("images")
	if err != nil {
		t.Fatalf("List(images): %v", err)
	}
	if len(objs) != 3 {
		t.Errorf("List(images) = %d objects, want 3", len(objs))
	}
	all, err := ls.List("")
	if err != nil || len(all) != 4 {
		t.Errorf("List(\"\") = %d objects, %v; want 4", len(all), err)
	}
	if err := ls.Delete("images/a.png"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files := outsideFiles(t, parent); len(files) != 0 {
		t.Fatalf("inside keys wrote outside the store: %v", files)
	}
}

func TestNewLocalStorage_BasePathIsAbsolute(t *testing.T) {
	dir := t.TempDir()
	wd, _ := os.Getwd()
	defer os.Chdir(wd)
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	ls, err := NewLocalStorage("relative-base")
	if err != nil {
		t.Fatalf("NewLocalStorage: %v", err)
	}
	if !filepath.IsAbs(ls.basePath) {
		t.Errorf("basePath = %q, want absolute", ls.basePath)
	}
	if err := ls.Put("../escape.txt", strings.NewReader("x")); !errors.Is(err, ErrKeyOutsideStore) {
		t.Errorf("Put(../escape.txt) on a relative base = %v, want ErrKeyOutsideStore", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); !os.IsNotExist(err) {
		t.Errorf("escape.txt was written next to a relative base")
	}
}
