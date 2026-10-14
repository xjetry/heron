package updates

import (
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
)

func TestSupersededOnlyWhenRunningVersionMeetsTargetOfAnUnfinishedRecord(t *testing.T) {
	task := func(state, version string) *heronv1.UpdateTask {
		return &heronv1.UpdateTask{Id: "0123456789abcdef", Version: version, State: state}
	}
	for _, c := range []struct {
		name      string
		task      *heronv1.UpdateTask
		running   string
		executing bool
		want      bool
	}{
		{"no task", nil, "v0.3.0", false, false},
		{"failed, target reached", task("failed", "v0.3.0"), "v0.3.0", false, true},
		{"failed, running newer than target", task("failed", "v0.3.0"), "v0.4.0", false, true},
		{"failed, target not reached", task("failed", "v0.3.0"), "v0.2.0", false, false},
		{"rolled back, target reached", task("rolled_back", "v0.3.0"), "v0.3.0", false, true},
		{"expired, target reached", task("expired", "v0.3.0"), "v0.3.0", false, true},
		{"cancelled, target reached", task("cancelled", "v0.3.0"), "v0.3.0", false, true},
		{"unconfirmed, target reached", task("unconfirmed", "v0.3.0"), "v0.3.0", false, true},
		{"succeeded stays as the record of how the target was reached", task("succeeded", "v0.3.0"), "v0.3.0", false, false},
		{"queued, target reached by another path", task("queued", "v0.3.0"), "v0.3.0", false, true},
		{"dispatched, local updater has no record of it", task("dispatched", "v0.3.0"), "v0.3.0", false, true},
		{"verifying while the local updater still executes it", task("verifying", "v0.3.0"), "v0.3.0", true, false},
		{"development build is not comparable", task("failed", "v0.3.0"), "dev", false, false},
		{"empty running version", task("failed", "v0.3.0"), "", false, false},
		{"invalid task version", task("failed", "latest"), "v0.3.0", false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := Superseded(c.task, c.running, c.executing); got != c.want {
				t.Fatalf("Superseded = %v, want %v", got, c.want)
			}
		})
	}
}
