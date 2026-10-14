package testwait

import (
	"bytes"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestPauseAtLogBlocksOnlyTheFirstMatchingRecord(t *testing.T) {
	var out bytes.Buffer
	h, entered, release := PauseAtLog(slog.NewTextHandler(&out, &slog.HandlerOptions{Level: slog.LevelWarn}), "stop here")
	defer release()
	log := slog.New(h).With("k", "v")
	log.Warn("before")
	log.Info("filtered by next")
	done := make(chan struct{})
	go func() {
		log.Warn("stop here")
		close(done)
	}()
	select {
	case <-entered:
	case <-time.After(Bound):
		t.Fatal("matching record did not pause")
	}
	select {
	case <-done:
		t.Fatal("matching record returned before release")
	case <-time.After(10 * time.Millisecond):
	}
	later := make(chan struct{})
	go func() {
		log.Warn("stop here")
		close(later)
	}()
	select {
	case <-later:
	case <-time.After(Bound):
		t.Fatal("a later matching record paused too")
	}
	release()
	<-done
	release()
	got := out.String()
	for _, want := range []string{"msg=before k=v", "msg=\"stop here\" k=v"} {
		if !strings.Contains(got, want) {
			t.Errorf("forwarded records lack %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "filtered by next") {
		t.Errorf("forwarded a record below next's level:\n%s", got)
	}
	if n := strings.Count(got, "stop here"); n != 2 {
		t.Errorf("forwarded %d of the 2 matching records:\n%s", n, got)
	}
}
