package api

import (
	"testing"

	"github.com/xjetry/heron/internal/testdeps"
)

func TestConstructorsRequireEveryDep(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	testdeps.RequireEveryField(t, "api.Deps", h.deps(), func(d Deps) { New(h.svc.cfg, d) })
	testdeps.RequireEveryField(t, "api.PublicDeps", h.publicDeps(), func(d PublicDeps) { NewPublic(h.pub.cfg, d) })
}
