package store

import (
	"errors"
	"fmt"
	"testing"
)

func TestTypedProbeErrorsKeepTextAndSentinels(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		err      error
		text     string
		sentinel error
		other    error
	}{
		{"node", NotFoundError{Kind: ObjectNode, ID: 42}, "node 42 does not exist", ErrNotFound, ErrNodeLimit},
		{"task", NotFoundError{Kind: ObjectProbeTask, ID: 999}, "probe task 999 does not exist", ErrNotFound, ErrNodeLimit},
		{"limit", NodeLimitError{NodeID: 1, Tasks: 65, Max: 64}, "node 1 would have 65 probe tasks (maximum 64)", ErrNodeLimit, ErrNotFound},
		{"inherited", InheritedLimitError{Tasks: 65, Max: 64}, "a new node would inherit 65 all-nodes probe tasks (maximum 64 per node); assign some of them to explicit nodes or delete them first", ErrNodeLimit, ErrNotFound},
		{"limit sentinel", ErrNodeLimit, "probe task limit per node exceeded", ErrNodeLimit, ErrNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.err.Error() != tc.text {
				t.Fatalf("text=%q want=%q", tc.err.Error(), tc.text)
			}
			wrapped := fmt.Errorf("context: %w", tc.err)
			if !errors.Is(wrapped, tc.sentinel) || errors.Is(wrapped, tc.other) {
				t.Fatalf("sentinel classification: %v", wrapped)
			}
		})
	}
}
