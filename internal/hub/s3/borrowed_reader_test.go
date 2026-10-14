package s3

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
)

func TestPutObjectLeavesBorrowedFileOpen(t *testing.T) {
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			t.Error(err)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer remote.Close()
	f, err := os.CreateTemp(t.TempDir(), "object-")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.WriteString("snapshot"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if err := clientFor(t, remote.URL).PutObject(deadline(t), "config/test.db", f); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("PutObject closed caller-owned file: %v", err)
	}
}
