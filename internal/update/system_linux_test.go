package update

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestManagedArgumentUsesGoFlagSemantics(t *testing.T) {
	for _, args := range [][]string{{"--db=/fixed"}, {"-db", "/fixed"}, {"--listen", "127.0.0.1:8080", "--db", "/fixed"}} {
		if !fixedArgument(args, "db", "/fixed") {
			t.Fatalf("rejected %v", args)
		}
	}
	for _, args := range [][]string{{"--db=/fixed", "-db=/other"}, {"--db=/fixed", "--db=/fixed"}, {"--", "--db=/fixed"}, {"positional", "--db=/fixed"}, {"--db"}, {"--db=/other"}} {
		if fixedArgument(args, "db", "/fixed") {
			t.Fatalf("accepted %v", args)
		}
	}
}

// safeFile 打开不存在的目录或文件时返回 unix.ENOENT；chooseSource 依赖它满足 errors.Is(err, fs.ErrNotExist)
// 才能把“来源配置没写”当作 github。这条用例只在 linux 构建里运行，钉住该前提。
func TestReadSafeMissingIsNotExist(t *testing.T) {
	_, err := readSafe(filepath.Join(t.TempDir(), "absent", "config.json"), os.Getuid(), 16)
	if !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("err = %v, want fs.ErrNotExist", err)
	}
}

func TestManagedFileRejectsLinksAndWritableFiles(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "binary")
	if err := os.WriteFile(path, []byte("binary"), 0600); err != nil {
		t.Fatal(err)
	}
	f, err := safeFile(path, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	link := filepath.Join(d, "link")
	if err = os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err = safeFile(link, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("accepted symbolic link")
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Link(path, link); err != nil {
		t.Fatal(err)
	}
	if f, err = safeFile(path, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("accepted hard link")
	}
	if err = os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err = os.Chmod(path, 0666); err != nil {
		t.Fatal(err)
	}
	if f, err = safeFile(path, os.Getuid()); err == nil {
		f.Close()
		t.Fatal("accepted writable file")
	}
	if err = os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err = safeFile(path, os.Getuid()+1); err == nil {
		f.Close()
		t.Fatal("accepted wrong owner")
	}
}
