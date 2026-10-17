package api

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"
	"github.com/xjetry/heron/internal/hub/store"
)

func TestOperationErrorMapsNameValidation(t *testing.T) {
	t.Parallel()
	svc := &Service{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, raw := range []string{" \t", strings.Repeat("名", 65)} {
		_, nameErr := store.CleanName(raw)
		if nameErr == nil {
			t.Fatalf("CleanName(%q) did not reject invalid name", raw)
		}
		for _, err := range []error{nameErr, fmt.Errorf("save node: %w", nameErr)} {
			got := svc.operationError(err, "node", "save")
			var connectErr *connect.Error
			if !errors.As(got, &connectErr) || connectErr.Code() != connect.CodeInvalidArgument || connectErr.Message() != nameErr.Error() {
				t.Errorf("mapped name error = %v, want InvalidArgument with %q", got, nameErr.Error())
			}
		}
	}
	if got := svc.operationError(errors.New("storage failed"), "node", "save"); connect.CodeOf(got) != connect.CodeInternal {
		t.Fatalf("non-validation error = %v, want Internal", got)
	}
}
