package s3

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

type untouchedBody struct{ reads, seeks int }

func (b *untouchedBody) Read([]byte) (int, error)       { b.reads++; return 0, io.EOF }
func (b *untouchedBody) Seek(int64, int) (int64, error) { b.seeks++; return 0, nil }

func TestDeadlineCheckedBeforeInputsAndHash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	defer srv.Close()
	c := clientFor(t, srv.URL)
	body := &untouchedBody{}
	ctx := context.Background()
	for name, op := range map[string]func() error{
		"request": func() error {
			r, err := c.request(ctx, http.MethodDelete, "key", nil, nil, 0, "")
			if r != nil {
				r.Body.Close()
			}
			return err
		},
		"put":     func() error { return c.PutObject(ctx, "key", body) },
		"put nil": func() error { return c.PutObject(ctx, "key", nil) },
		"get":     func() error { return c.GetObject(ctx, "key", nil, 0) },
		"delete":  func() error { return c.DeleteObject(ctx, "key") },
		"list":    func() error { _, err := c.ListObjectsV2(ctx, "", 0); return err },
	} {
		t.Run(name, func(t *testing.T) {
			var e *Error
			if err := op(); !errors.As(err, &e) || e.Kind != "request" || e.Detail != "context has no deadline" {
				t.Fatalf("deadline was not checked at entry: %v", err)
			}
		})
	}
	if body.reads != 0 || body.seeks != 0 {
		t.Fatalf("body touched before deadline validation: reads=%d seeks=%d", body.reads, body.seeks)
	}
}
