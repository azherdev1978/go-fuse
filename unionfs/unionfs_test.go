package unionfs

import (
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/hanwen/go-fuse/fuse"
	"github.com/hanwen/go-fuse/raw"
)

var _ = fmt.Print
var _ = log.Print

func TestFilePathHash(t *testing.T) {
	// Simple test coverage.
	t.Log(filePathHash("xyz/abc"))
}

var testOpts = UnionFsOptions{
	DeletionCacheTTL: entryTtl,
	DeletionDirName:  "DELETIONS",
	BranchCacheTTL:   entryTtl,
	HiddenFiles:      []string{"hidden"},
}

func setRecursiveWritable(t *testing.T, dir string, writable bool) {
	err := filepath.Walk(
		dir,
		func(path string, fi os.FileInfo, err error) error {
			var newMode uint32
			if writable {
				newMode = uint32(fi.Mode().Perm()) | 0200
			} else {
				newMode = uint32(fi.Mode().Perm()) &^ 0222
			}
			if fi.Mode() | os.ModeSymlink != 0 { return nil }
			return os.Chmod(path, os.FileMode(newMode))
		})
	if err != nil {
		t.Fatalf("Walk failed: %v", err)
	}
}

// Creates 3 directories on a temporary dir: /mnt with the overlayed
// (unionfs) mount, rw with modifiable data, and ro on the bottom.
func setupUfs(t *testing.T) (workdir string, cleanup func()) {
	// Make sure system setting does not affect test.
	syscall.Umask(0)

	wd, _ := ioutil.TempDir("", "unionfs")
	err := os.Mkdir(wd+"/mnt", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	err = os.Mkdir(wd+"/rw", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	os.Mkdir(wd+"/ro", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	var fses []fuse.FileSystem
	fses = append(fses, fuse.NewLoopbackFileSystem(wd+"/rw"))
	fses = append(fses,
		NewCachingFileSystem(fuse.NewLoopbackFileSystem(wd+"/ro"), 0))
	ufs := NewUnionFs(fses, testOpts)

	// We configure timeouts are smaller, so we can check for
	// UnionFs's cache consistency.
	opts := &fuse.FileSystemOptions{
		EntryTimeout:    entryTtl / 2,
		AttrTimeout:     entryTtl / 2,
		NegativeTimeout: entryTtl / 2,
	}

	pathfs := fuse.NewPathNodeFs(ufs,
		&fuse.PathNodeFsOptions{ClientInodes: true})
	state, conn, err := fuse.MountNodeFileSystem(wd+"/mnt", pathfs, opts)
	if err != nil {
		t.Fatalf("MountNodeFileSystem failed: %v", err)
	}
	conn.Debug = fuse.VerboseTest()
	state.Debug = fuse.VerboseTest()
	go state.Loop()

	return wd, func() {
		err := state.Unmount()
		if err != nil {
			return 
		}
		setRecursiveWritable(t, wd, true)
		os.RemoveAll(wd)
	}
}

func readFromFile(t *testing.T, path string) string {
	b, err := ioutil.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	return string(b)
}

func dirNames(t *testing.T, path string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}

	result := make(map[string]bool)
	names, err := f.Readdirnames(-1)
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	err = f.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	for _, nm := range names {
		result[nm] = true
	}
	return result
}

func checkMapEq(t *testing.T, m1, m2 map[string]bool) {
	if !mapEq(m1, m2) {
		msg := fmt.Sprintf("mismatch: got %v != expect %v", m1, m2)
		panic(msg)
	}
}

func mapEq(m1, m2 map[string]bool) bool {
	if len(m1) != len(m2) {
		return false
	}

	for k, v := range m1 {
		val, ok := m2[k]
		if !ok || val != v {
			return false
		}
	}
	return true
}

func fileExists(path string) bool {
	f, err := os.Lstat(path)
	return err == nil && f != nil
}

func TestUnionFsAutocreateDeletionDir(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Remove(wd + "/rw/DELETIONS")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	err = os.Mkdir(wd+"/mnt/dir", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	_, err = ioutil.ReadDir(wd + "/mnt/dir")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}
}

func TestUnionFsSymlink(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Symlink("/foobar", wd+"/mnt/link")
	if err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	val, err := os.Readlink(wd + "/mnt/link")
	if err != nil {
		t.Fatalf("Readlink failed: %v", err)
	}

	if val != "/foobar" {
		t.Errorf("symlink mismatch: %v", val)
	}
}

func TestUnionFsSymlinkPromote(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Mkdir(wd+"/ro/subdir", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	err = os.Symlink("/foobar", wd+"/mnt/subdir/link")
	if err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}
}

func TestUnionFsChtimes(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	WriteFile(t, wd+"/ro/file", "a")
	err := os.Chtimes(wd+"/ro/file", time.Unix(42, 0), time.Unix(43, 0))
	if err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	err = os.Chtimes(wd+"/mnt/file", time.Unix(82, 0), time.Unix(83, 0))
	if err != nil {
		t.Fatalf("Chtimes failed: %v", err)
	}

	fi, err := os.Lstat(wd + "/mnt/file")
	stat := fuse.ToStatT(fi)
	if stat.Atim.Sec != 82 || stat.Mtim.Sec != 83 {
		t.Error("Incorrect timestamp", fi)
	}
}

func TestUnionFsChmod(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	ro_fn := wd + "/ro/file"
	m_fn := wd + "/mnt/file"
	WriteFile(t, ro_fn, "a")
	err := os.Chmod(m_fn, 00070)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	fi, err := os.Lstat(m_fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Mode()&07777 != 00270 {
		t.Errorf("Unexpected mode found: %o", uint32(fi.Mode().Perm()))
	}
	_, err = os.Lstat(wd + "/rw/file")
	if err != nil {
		t.Errorf("File not promoted")
	}
}

func TestUnionFsChown(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	ro_fn := wd + "/ro/file"
	m_fn := wd + "/mnt/file"
	WriteFile(t, ro_fn, "a")

	err := os.Chown(m_fn, 0, 0)
	code := fuse.ToStatus(err)
	if code != fuse.EPERM {
		t.Error("Unexpected error code", code, err)
	}
}

func TestUnionFsDelete(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	WriteFile(t, wd+"/ro/file", "a")
	_, err := os.Lstat(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	err = os.Remove(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	_, err = os.Lstat(wd + "/mnt/file")
	if err == nil {
		t.Fatal("should have disappeared.")
	}
	delPath := wd + "/rw/" + testOpts.DeletionDirName
	names := dirNames(t, delPath)
	if len(names) != 1 {
		t.Fatal("Should have 1 deletion", names)
	}

	for k := range names {
		c, err := ioutil.ReadFile(delPath + "/" + k)
		if err != nil {
			t.Fatalf("ReadFile failed: %v", err)
		}
		if string(c) != "file" {
			t.Fatal("content mismatch", string(c))
		}
	}
}

func TestUnionFsBasic(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	WriteFile(t, wd+"/rw/rw", "a")
	WriteFile(t, wd+"/ro/ro1", "a")
	WriteFile(t, wd+"/ro/ro2", "b")

	names := dirNames(t, wd+"/mnt")
	expected := map[string]bool{
		"rw": true, "ro1": true, "ro2": true,
	}
	checkMapEq(t, names, expected)

	WriteFile(t, wd+"/mnt/new", "new contents")
	if !fileExists(wd + "/rw/new") {
		t.Errorf("missing file in rw layer", names)
	}

	contents := readFromFile(t, wd+"/mnt/new")
	if contents != "new contents" {
		t.Errorf("read mismatch: '%v'", contents)
	}
	WriteFile(t, wd+"/mnt/ro1", "promote me")
	if !fileExists(wd + "/rw/ro1") {
		t.Errorf("missing file in rw layer", names)
	}

	err := os.Remove(wd + "/mnt/new")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	names = dirNames(t, wd+"/mnt")
	checkMapEq(t, names, map[string]bool{
		"rw": true, "ro1": true, "ro2": true,
	})

	names = dirNames(t, wd+"/rw")
	checkMapEq(t, names, map[string]bool{
		testOpts.DeletionDirName: true,
		"rw": true, "ro1": true,
	})
	names = dirNames(t, wd+"/rw/"+testOpts.DeletionDirName)
	if len(names) != 0 {
		t.Errorf("Expected 0 entry in %v", names)
	}

	err = os.Remove(wd + "/mnt/ro1")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	names = dirNames(t, wd+"/mnt")
	checkMapEq(t, names, map[string]bool{
		"rw": true, "ro2": true,
	})

	names = dirNames(t, wd+"/rw")
	checkMapEq(t, names, map[string]bool{
		"rw": true, testOpts.DeletionDirName: true,
	})

	names = dirNames(t, wd+"/rw/"+testOpts.DeletionDirName)
	if len(names) != 1 {
		t.Errorf("Expected 1 entry in %v", names)
	}
}

func TestUnionFsPromote(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Mkdir(wd+"/ro/subdir", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	WriteFile(t, wd+"/ro/subdir/file", "content")
	WriteFile(t, wd+"/mnt/subdir/file", "other-content")
}

func TestUnionFsCreate(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/subdir/sub2", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}
	WriteFile(t, wd+"/mnt/subdir/sub2/file", "other-content")
	_, err = os.Lstat(wd + "/mnt/subdir/sub2/file")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
}

func TestUnionFsOpenUndeletes(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	WriteFile(t, wd+"/ro/file", "X")
	err := os.Remove(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	WriteFile(t, wd+"/mnt/file", "X")
	_, err = os.Lstat(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
}

func TestUnionFsMkdir(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	dirname := wd + "/mnt/subdir"
	err := os.Mkdir(dirname, 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	err = os.Remove(dirname)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
}

func TestUnionFsMkdirPromote(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	dirname := wd + "/ro/subdir/subdir2"
	err := os.MkdirAll(dirname, 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	err = os.Mkdir(wd+"/mnt/subdir/subdir2/dir3", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	fi, _ := os.Lstat(wd + "/rw/subdir/subdir2/dir3")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi == nil || !fi.IsDir() {
		t.Error("is not a directory: ", fi)
	}
}

func TestUnionFsRmdirMkdir(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Mkdir(wd+"/ro/subdir", 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	dirname := wd + "/mnt/subdir"
	err = os.Remove(dirname)
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	err = os.Mkdir(dirname, 0755)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
}

func TestUnionFsRename(t *testing.T) {
	type Config struct {
		f1_ro bool
		f1_rw bool
		f2_ro bool
		f2_rw bool
	}

	configs := make([]Config, 0)
	for i := 0; i < 16; i++ {
		c := Config{i&0x1 != 0, i&0x2 != 0, i&0x4 != 0, i&0x8 != 0}
		if !(c.f1_ro || c.f1_rw) {
			continue
		}

		configs = append(configs, c)
	}

	for i, c := range configs {
		t.Log("Config", i, c)
		wd, clean := setupUfs(t)
		if c.f1_ro {
			WriteFile(t, wd+"/ro/file1", "c1")
		}
		if c.f1_rw {
			WriteFile(t, wd+"/rw/file1", "c2")
		}
		if c.f2_ro {
			WriteFile(t, wd+"/ro/file2", "c3")
		}
		if c.f2_rw {
			WriteFile(t, wd+"/rw/file2", "c4")
		}

		err := os.Rename(wd+"/mnt/file1", wd+"/mnt/file2")
		if err != nil {
			t.Fatalf("Rename failed: %v", err)
		}

		_, err = os.Lstat(wd + "/mnt/file1")
		if err == nil {
			t.Errorf("Should have lost file1")
		}
		_, err = os.Lstat(wd + "/mnt/file2")
		if err != nil {
			t.Errorf("Should have gotten file2: %v", err)
		}
		err = os.Rename(wd+"/mnt/file2", wd+"/mnt/file1")
		if err != nil {
			t.Fatalf("Rename failed: %v", err)
		}

		_, err = os.Lstat(wd + "/mnt/file2")
		if err == nil {
			t.Errorf("Should have lost file2")
		}
		_, err = os.Lstat(wd + "/mnt/file1")
		if err != nil {
			t.Errorf("Should have gotten file1: %v", err)
		}
		clean()
	}
}

func TestUnionFsRenameDirBasic(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/dir/subdir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	err = os.Rename(wd+"/mnt/dir", wd+"/mnt/renamed")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	if fi, _ := os.Lstat(wd + "/mnt/dir"); fi != nil {
		t.Fatalf("%s/mnt/dir should have disappeared: %v", wd, fi)
	}

	if fi, _ := os.Lstat(wd + "/mnt/renamed"); fi == nil || !fi.IsDir() {
		t.Fatalf("%s/mnt/renamed should be directory: %v", wd, fi)
	}

	entries, err := ioutil.ReadDir(wd + "/mnt/renamed")
	if err != nil || len(entries) != 1 || entries[0].Name() != "subdir" {
		t.Errorf("readdir(%s/mnt/renamed) should have one entry: %v, err %v", wd, entries, err)
	}

	if err = os.Mkdir(wd+"/mnt/dir", 0755); err != nil {
		t.Errorf("mkdir should succeed %v", err)
	}
}

func TestUnionFsRenameDirAllSourcesGone(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/dir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	err = ioutil.WriteFile(wd+"/ro/dir/file.txt", []byte{42}, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	setRecursiveWritable(t, wd+"/ro", false)
	err = os.Rename(wd+"/mnt/dir", wd+"/mnt/renamed")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	names := dirNames(t, wd+"/rw/"+testOpts.DeletionDirName)
	if len(names) != 2 {
		t.Errorf("Expected 2 entries in %v", names)
	}
}

func TestUnionFsRenameDirWithDeletions(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/dir/subdir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	err = ioutil.WriteFile(wd+"/ro/dir/file.txt", []byte{42}, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = ioutil.WriteFile(wd+"/ro/dir/subdir/file.txt", []byte{42}, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	if fi, _ := os.Lstat(wd + "/mnt/dir/subdir/file.txt"); fi == nil || fi.Mode()&os.ModeType != 0 {
		t.Fatalf("%s/mnt/dir/subdir/file.txt should be file: %v", wd, fi)
	}

	err = os.Remove(wd + "/mnt/dir/file.txt")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	err = os.Rename(wd+"/mnt/dir", wd+"/mnt/renamed")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	if fi, _ := os.Lstat(wd + "/mnt/dir/subdir/file.txt"); fi != nil {
		t.Fatalf("%s/mnt/dir/subdir/file.txt should have disappeared: %v", wd, fi)
	}

	if fi, _ := os.Lstat(wd + "/mnt/dir"); fi != nil {
		t.Fatalf("%s/mnt/dir should have disappeared: %v", wd, fi)
	}

	if fi, _ := os.Lstat(wd + "/mnt/renamed"); fi == nil || !fi.IsDir() {
		t.Fatalf("%s/mnt/renamed should be directory: %v", wd, fi)
	}

	if fi, _ := os.Lstat(wd + "/mnt/renamed/file.txt"); fi != nil {
		t.Fatalf("%s/mnt/renamed/file.txt should have disappeared %#v", wd, fi)
	}

	if err = os.Mkdir(wd+"/mnt/dir", 0755); err != nil {
		t.Errorf("mkdir should succeed %v", err)
	}

	if fi, _ := os.Lstat(wd + "/mnt/dir/subdir"); fi != nil {
		t.Fatalf("%s/mnt/dir/subdir should have disappeared %#v", wd, fi)
	}
}

func TestUnionFsRenameSymlink(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Symlink("linktarget", wd+"/ro/link")
	if err != nil {
		t.Fatalf("Symlink failed: %v", err)
	}

	err = os.Rename(wd+"/mnt/link", wd+"/mnt/renamed")
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	if fi, _ := os.Lstat(wd + "/mnt/link"); fi != nil {
		t.Fatalf("%s/mnt/link should have disappeared: %v", wd, fi)
	}

	if fi, _ := os.Lstat(wd + "/mnt/renamed"); fi == nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s/mnt/renamed should be link: %v", wd, fi)
	}

	if link, err := os.Readlink(wd + "/mnt/renamed"); err != nil || link != "linktarget" {
		t.Fatalf("readlink(%s/mnt/renamed) should point to 'linktarget': %v, err %v", wd, link, err)
	}
}

func TestUnionFsWritableDir(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	dirname := wd + "/ro/subdir"
	err := os.Mkdir(dirname, 0555)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	fi, err := os.Lstat(wd + "/mnt/subdir")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Mode().Perm()&0222 == 0 {
		t.Errorf("unexpected permission %o", fi.Mode().Perm())
	}
}

func TestUnionFsWriteAccess(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	fn := wd + "/ro/file"
	// No write perms.
	err := ioutil.WriteFile(fn, []byte("foo"), 0444)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	err = syscall.Access(wd+"/mnt/file", raw.W_OK)
	if err != nil {
		if err != nil {
			t.Fatalf("Access failed: %v", err)
		}
	}
}

func TestUnionFsLink(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	content := "blabla"
	fn := wd + "/ro/file"
	err := ioutil.WriteFile(fn, []byte(content), 0666)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	err = os.Link(wd+"/mnt/file", wd+"/mnt/linked")
	if err != nil {
		t.Fatalf("Link failed: %v", err)
	}

	fi2, err := os.Lstat(wd + "/mnt/linked")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	fi1, err := os.Lstat(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	s1 := fuse.ToStatT(fi1)
	s2 := fuse.ToStatT(fi2)
	if s1.Ino != s2.Ino {
		t.Errorf("inode numbers should be equal for linked files %v, %v", s1.Ino, s2.Ino)
	}
	c, err := ioutil.ReadFile(wd + "/mnt/linked")
	if string(c) != content {
		t.Errorf("content mismatch got %q want %q", string(c), content)
	}
}

func TestUnionFsTruncate(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	WriteFile(t, wd+"/ro/file", "hello")
	setRecursiveWritable(t, wd+"/ro", false)

	os.Truncate(wd+"/mnt/file", 2)
	content := readFromFile(t, wd+"/mnt/file")
	if content != "he" {
		t.Errorf("unexpected content %v", content)
	}
	content2 := readFromFile(t, wd+"/rw/file")
	if content2 != content {
		t.Errorf("unexpected rw content %v", content2)
	}
}

func TestUnionFsCopyChmod(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	contents := "hello"
	fn := wd + "/mnt/y"
	err := ioutil.WriteFile(fn, []byte(contents), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	err = os.Chmod(fn, 0755)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	fi, err := os.Lstat(fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Mode()&0111 == 0 {
		t.Errorf("1st attr error %o", fi.Mode())
	}
	time.Sleep(entryTtl)
	fi, err = os.Lstat(fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Mode()&0111 == 0 {
		t.Errorf("uncached attr error %o", fi.Mode())
	}
}

func abs(dt int64) int64 {
	if dt >= 0 {
		return dt
	}
	return -dt
}

func TestUnionFsTruncateTimestamp(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	contents := "hello"
	fn := wd + "/mnt/y"
	err := ioutil.WriteFile(fn, []byte(contents), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	truncTs := time.Now()
	err = os.Truncate(fn, 3)
	if err != nil {
		t.Fatalf("Truncate failed: %v", err)
	}

	fi, err := os.Lstat(fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	if truncTs.Sub(fi.ModTime()) > 100*time.Millisecond {
		t.Error("timestamp drift", truncTs, fi.ModTime())
	}
}

func TestUnionFsRemoveAll(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/dir/subdir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	contents := "hello"
	fn := wd + "/ro/dir/subdir/y"
	err = ioutil.WriteFile(fn, []byte(contents), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	err = os.RemoveAll(wd + "/mnt/dir")
	if err != nil {
		t.Error("Should delete all")
	}

	for _, f := range []string{"dir/subdir/y", "dir/subdir", "dir"} {
		if fi, _ := os.Lstat(filepath.Join(wd, "mount", f)); fi != nil {
			t.Errorf("file %s should have disappeared: %v", f, fi)
		}
	}

	names, err := Readdirnames(wd + "/rw/DELETIONS")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names) != 3 {
		t.Fatal("unexpected names", names)
	}
}

// Warning: test fails under coreutils < 8.0 because of non-posix behaviour
// of the "rm" tool -- which relies on behaviour that doesn't work in fuse.
func TestUnionFsRmRf(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.MkdirAll(wd+"/ro/dir/subdir", 0755)
	if err != nil {
		t.Fatalf("MkdirAll failed: %v", err)
	}

	contents := "hello"
	fn := wd + "/ro/dir/subdir/y"
	err = ioutil.WriteFile(fn, []byte(contents), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	bin, err := exec.LookPath("rm")
	if err != nil {
		t.Fatalf("LookPath failed: %v", err)
	}
	command := fmt.Sprintf("%s -f %s/mnt/dir", bin, wd)
	log.Printf("Command: %s", command)
	names, _ := Readdirnames(wd + "/mnt/dir")
	log.Printf("Contents of %s/mnt/dir: %s", wd, strings.Join(names, ", "))
	cmd := exec.Command(bin, "-rf", wd+"/mnt/dir")
	err = cmd.Run()
	if err != nil {
		t.Fatal("rm -rf returned error:", err)
	}

	for _, f := range []string{"dir/subdir/y", "dir/subdir", "dir"} {
		if fi, _ := os.Lstat(filepath.Join(wd, "mount", f)); fi != nil {
			t.Errorf("file %s should have disappeared: %v", f, fi)
		}
	}

	names, err = Readdirnames(wd + "/rw/DELETIONS")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names) != 3 {
		t.Fatal("unexpected names", names)
	}
}

func Readdirnames(dir string) ([]string, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}

	defer f.Close()
	return f.Readdirnames(-1)
}

func TestUnionFsDropDeletionCache(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := ioutil.WriteFile(wd+"/ro/file", []byte("bla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	_, err = os.Lstat(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	err = os.Remove(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}
	fi, _ := os.Lstat(wd + "/mnt/file")
	if fi != nil {
		t.Fatal("Lstat() should have failed", fi)
	}

	names, err := Readdirnames(wd + "/rw/DELETIONS")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names) != 1 {
		t.Fatal("unexpected names", names)
	}
	os.Remove(wd + "/rw/DELETIONS/" + names[0])
	fi, _ = os.Lstat(wd + "/mnt/file")
	if fi != nil {
		t.Fatal("Lstat() should have failed", fi)
	}

	// Expire kernel entry.
	time.Sleep((6 * entryTtl) / 10)
	err = ioutil.WriteFile(wd+"/mnt/.drop_cache", []byte(""), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	_, err = os.Lstat(wd + "/mnt/file")
	if err != nil {
		t.Fatal("Lstat() should have succeeded", err)
	}
}

func TestUnionFsDropCache(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := ioutil.WriteFile(wd+"/ro/file", []byte("bla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	_, err = os.Lstat(wd + "/mnt/.drop_cache")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	names, err := Readdirnames(wd + "/mnt")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names) != 1 || names[0] != "file" {
		t.Fatal("unexpected names", names)
	}

	err = ioutil.WriteFile(wd+"/ro/file2", []byte("blabla"), 0644)
	names2, err := Readdirnames(wd + "/mnt")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names2) != len(names) {
		t.Fatal("mismatch", names2)
	}

	err = ioutil.WriteFile(wd+"/mnt/.drop_cache", []byte("does not matter"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	names2, err = Readdirnames(wd + "/mnt")
	if len(names2) != 2 {
		t.Fatal("mismatch 2", names2)
	}
}

func TestUnionFsDisappearing(t *testing.T) {
	// This init is like setupUfs, but we want access to the
	// writable Fs.
	wd, _ := ioutil.TempDir("", "")
	defer os.RemoveAll(wd)
	err := os.Mkdir(wd+"/mnt", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	err = os.Mkdir(wd+"/rw", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	os.Mkdir(wd+"/ro", 0700)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}

	wrFs := fuse.NewLoopbackFileSystem(wd + "/rw")
	var fses []fuse.FileSystem
	fses = append(fses, wrFs)
	fses = append(fses, fuse.NewLoopbackFileSystem(wd+"/ro"))
	ufs := NewUnionFs(fses, testOpts)

	opts := &fuse.FileSystemOptions{
		EntryTimeout:    entryTtl,
		AttrTimeout:     entryTtl,
		NegativeTimeout: entryTtl,
	}

	nfs := fuse.NewPathNodeFs(ufs, nil)
	state, _, err := fuse.MountNodeFileSystem(wd+"/mnt", nfs, opts)
	if err != nil {
		t.Fatalf("MountNodeFileSystem failed: %v", err)
	}
	defer state.Unmount()
	state.Debug = fuse.VerboseTest()
	go state.Loop()

	log.Println("TestUnionFsDisappearing2")

	err = ioutil.WriteFile(wd+"/ro/file", []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	err = os.Remove(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	state.ThreadSanitizerSync()
	oldRoot := wrFs.Root
	wrFs.Root = "/dev/null"
	state.ThreadSanitizerSync()
	time.Sleep((3 * entryTtl) / 2)

	_, err = ioutil.ReadDir(wd + "/mnt")
	if err == nil {
		t.Fatal("Readdir should have failed")
	}
	log.Println("expected readdir failure:", err)

	err = ioutil.WriteFile(wd+"/mnt/file2", []byte("blabla"), 0644)
	if err == nil {
		t.Fatal("write should have failed")
	}
	log.Println("expected write failure:", err)

	// Restore, and wait for caches to catch up.
	wrFs.Root = oldRoot
	state.ThreadSanitizerSync()
	time.Sleep((3 * entryTtl) / 2)

	_, err = ioutil.ReadDir(wd + "/mnt")
	if err != nil {
		t.Fatal("Readdir should succeed", err)
	}
	err = ioutil.WriteFile(wd+"/mnt/file2", []byte("blabla"), 0644)
	if err != nil {
		t.Fatal("write should succeed", err)
	}
}

func TestUnionFsDeletedGetAttr(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := ioutil.WriteFile(wd+"/ro/file", []byte("blabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	f, err := os.Open(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer f.Close()

	err = os.Remove(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Remove failed: %v", err)
	}

	if fi, err := f.Stat(); err != nil || fi.Mode()&os.ModeType != 0 {
		t.Fatalf("stat returned error or non-file: %v %v", err, fi)
	}
}

func TestUnionFsDoubleOpen(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()
	err := ioutil.WriteFile(wd+"/ro/file", []byte("blablabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	roFile, err := os.Open(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("Open failed: %v", err)
	}
	defer roFile.Close()
	rwFile, err := os.OpenFile(wd+"/mnt/file", os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	defer rwFile.Close()

	output, err := ioutil.ReadAll(roFile)
	if err != nil {
		t.Fatalf("ReadAll failed: %v", err)
	}
	if len(output) != 0 {
		t.Errorf("After r/w truncation, r/o file should be empty too: %q", string(output))
	}

	want := "hello"
	_, err = rwFile.Write([]byte(want))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	b := make([]byte, 100)

	roFile.Seek(0, 0)
	n, err := roFile.Read(b)
	if err != nil {
		t.Fatalf("Read failed: %v", err)
	}
	b = b[:n]

	if string(b) != "hello" {
		t.Errorf("r/w and r/o file are not synchronized: got %q want %q", string(b), want)
	}
}

func TestUnionFsFdLeak(t *testing.T) {
	beforeEntries, err := ioutil.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	wd, clean := setupUfs(t)
	err = ioutil.WriteFile(wd+"/ro/file", []byte("blablabla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	contents, err := ioutil.ReadFile(wd + "/mnt/file")
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}

	err = ioutil.WriteFile(wd+"/mnt/file", contents, 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}

	clean()

	afterEntries, err := ioutil.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("ReadDir failed: %v", err)
	}

	if len(afterEntries) != len(beforeEntries) {
		t.Errorf("/proc/self/fd changed size: after %v before %v", len(beforeEntries), len(afterEntries))
	}
}

func TestUnionFsStatFs(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	s1 := syscall.Statfs_t{}
	err := syscall.Statfs(wd+"/mnt", &s1)
	if err != nil {
		t.Fatal("statfs mnt", err)
	}
	if s1.Bsize == 0 {
		t.Fatal("Expect blocksize > 0")
	}
}

func TestUnionFsFlushSize(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	fn := wd + "/mnt/file"
	f, err := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	n, err := f.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}

	f.Close()
	fi, err = os.Lstat(fn)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Size() != int64(n) {
		t.Errorf("got %d from Stat().Size, want %d", fi.Size(), n)
	}
}

func TestUnionFsFlushRename(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := ioutil.WriteFile(wd+"/mnt/file", []byte("x"), 0644)

	fn := wd + "/mnt/tmp"
	f, err := os.OpenFile(fn, os.O_WRONLY|os.O_CREATE, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	fi, err := f.Stat()
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}

	n, err := f.Write([]byte("hello"))
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	f.Close()

	dst := wd + "/mnt/file"
	err = os.Rename(fn, dst)
	if err != nil {
		t.Fatalf("Rename failed: %v", err)
	}

	fi, err = os.Lstat(dst)
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	if fi.Size() != int64(n) {
		t.Errorf("got %d from Stat().Size, want %d", fi.Size(), n)
	}
}

func TestUnionFsTruncGetAttr(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	c := []byte("hello")
	f, err := os.OpenFile(wd+"/mnt/file", os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("OpenFile failed: %v", err)
	}
	_, err = f.Write(c)
	if err != nil {
		t.Fatalf("Write failed: %v", err)
	}
	err = f.Close()
	if err != nil {
		t.Fatalf("Close failed: %v", err)
	}

	fi, err := os.Lstat(wd + "/mnt/file")
	if fi.Size() != int64(len(c)) {
		t.Fatalf("Length mismatch got %d want %d", fi.Size(), len(c))
	}
}

func TestUnionFsPromoteDirTimeStamp(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := os.Mkdir(wd+"/ro/subdir", 0750)
	if err != nil {
		t.Fatalf("Mkdir failed: %v", err)
	}
	err = ioutil.WriteFile(wd+"/ro/subdir/file", []byte("hello"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	err = os.Chmod(wd+"/mnt/subdir/file", 0060)
	if err != nil {
		t.Fatalf("Chmod failed: %v", err)
	}

	fRo, err := os.Lstat(wd + "/ro/subdir")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}
	fRw, err := os.Lstat(wd + "/rw/subdir")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	// TODO - need to update timestamps after promoteDirsTo calls,
	// not during.
	if false && fRo.ModTime().Equal(fRw.ModTime()) {
		t.Errorf("Changed timestamps on promoted subdir: ro %d rw %d", fRo.ModTime(), fRw.ModTime())
	}

	if fRo.Mode().Perm()|0200 != fRw.Mode().Perm() {
		t.Errorf("Changed mode ro: %v, rw: %v", fRo.Mode(), fRw.Mode())
	}
}

func TestUnionFsCheckHiddenFiles(t *testing.T) {
	wd, clean := setupUfs(t)
	defer clean()

	err := ioutil.WriteFile(wd+"/ro/hidden", []byte("bla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	err = ioutil.WriteFile(wd+"/ro/not_hidden", []byte("bla"), 0644)
	if err != nil {
		t.Fatalf("WriteFile failed: %v", err)
	}
	setRecursiveWritable(t, wd+"/ro", false)

	fi, _ := os.Lstat(wd + "/mnt/hidden")
	if fi != nil {
		t.Fatal("Lstat() should have failed", fi)
	}
	_, err = os.Lstat(wd + "/mnt/not_hidden")
	if err != nil {
		t.Fatalf("Lstat failed: %v", err)
	}

	names, err := Readdirnames(wd + "/mnt")
	if err != nil {
		t.Fatalf("Readdirnames failed: %v", err)
	}
	if len(names) != 1 || names[0] != "not_hidden" {
		t.Fatal("unexpected names", names)
	}
}
