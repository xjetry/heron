package client

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/update"
)

type updaterFake struct {
	status    update.Status
	reads     chan struct{}
	ready     chan string
	submitted chan update.Request
}

func (f *updaterFake) Status(context.Context) update.Status {
	select {
	case f.reads <- struct{}{}:
	default:
	}
	return f.status
}
func (f *updaterFake) Ready(_ context.Context, v string) error { f.ready <- v; return nil }
func (f *updaterFake) Submit(_ context.Context, r update.Request) (update.Job, error) {
	f.submitted <- r
	return update.Job{Request: r, State: "downloading"}, nil
}

func TestAgentUpdateRequiresSuccessfulReportForReady(t *testing.T) {
	for _, state := range []string{"verifying", "downloading"} {
		t.Run(state, func(t *testing.T) {
			f := &updaterFake{status: update.Status{Supported: true, Job: &update.Job{Request: update.Request{Version: "v0.3.0"}, State: state}}, reads: make(chan struct{}, 8), ready: make(chan string, 8), submitted: make(chan update.Request, 8)}
			c := NewUpdateCoordinator("v0.3.0", slog.New(slog.NewTextHandler(io.Discard, nil)))
			c.client = f
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			done := make(chan struct{})
			go func() { c.Run(ctx); close(done) }()
			defer func() { cancel(); <-done }()
			select {
			case <-f.reads:
			case <-ctx.Done():
				t.Fatal("no initial status")
			}
			select {
			case <-f.ready:
				t.Fatal("ready sent before successful report")
			default:
			}
			c.Reported(nil)
			if state == "verifying" {
				select {
				case v := <-f.ready:
					if v != "v0.3.0" {
						t.Fatalf("ready version=%s", v)
					}
				case <-ctx.Done():
					t.Fatal("successful report did not confirm candidate")
				}
			} else {
				select {
				case <-f.reads:
				case <-ctx.Done():
					t.Fatal("successful report not handled")
				}
				cancel()
				<-done
				done = closedChannel()
				select {
				case <-f.ready:
					t.Fatal("confirmed non-verifying task")
				default:
				}
			}
		})
	}
}

func closedChannel() chan struct{} { c := make(chan struct{}); close(c); return c }

func TestAgentUpdateDeliveryIsNonblockingAndRestricted(t *testing.T) {
	c := NewUpdateCoordinator("v0.2.0", slog.New(slog.NewTextHandler(io.Discard, nil)))
	task := &heronv1.UpdateTask{Id: "0123456789abcdef", Version: "v0.3.0", ExpiresAt: time.Now().Add(time.Hour).Unix(), State: "dispatched"}
	c.Reported(task)
	task.Version = "v9.0.0"
	c.Reported(nil)
	f := &updaterFake{status: update.Status{Supported: true}, reads: make(chan struct{}, 8), ready: make(chan string, 8), submitted: make(chan update.Request, 8)}
	c.client = f
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	defer func() { cancel(); <-done }()
	select {
	case r := <-f.submitted:
		if r.Version != "v0.3.0" || r.ID != "0123456789abcdef" {
			t.Fatalf("submitted changed task: %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("no submitted update")
	}
}
