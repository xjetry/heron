package alert

import (
	"sync"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

func TestSweepConcurrentRuleWrites(t *testing.T) {
	f := newFixture(t)
	must(t, f.st.DeleteNode(t.Context(), f.ids[1]))
	f.clk.Advance(time.Minute)
	var wg sync.WaitGroup
	writesDone := make(chan struct{})
	readerReady := make(chan struct{})
	wg.Go(func() {
		f.e.Rules()
		close(readerReady)
		for {
			select {
			case <-writesDone:
				return
			default:
				f.e.Rules()
				f.e.Channels()
				f.e.States()
			}
		}
	})
	wg.Go(func() {
		for i := 0; i < 30; i++ {
			if err := f.e.SweepOffline(t.Context()); err != nil {
				t.Error(err)
				return
			}
		}
	})
	wg.Go(func() {
		defer close(writesDone)
		<-readerReady
		for i := 0; i < 30; i++ {
			r, err := f.e.SaveRule(t.Context(), offline())
			if err != nil {
				t.Error(err)
				return
			}
			if err := f.e.DeleteRule(t.Context(), r.ID); err != nil {
				t.Error(err)
				return
			}
			if _, err := f.e.SaveChannel(t.Context(), store.NotifyChannel{Name: "c", Kind: store.ChannelWebhook, Config: `{"url":"https://example.invalid"}`}); err != nil {
				t.Error(err)
				return
			}
			f.e.Forget(f.ids[1])
		}
	})
	wg.Wait()
}
