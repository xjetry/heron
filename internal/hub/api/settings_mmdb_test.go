package api

import (
	"path/filepath"
	"testing"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/geo"
)

// 回显的后端与路径取自装配给 api 的后端对象，UpdateSettings 对两项的缺席、给出、伪造与未知枚举一律忽略。
func TestSettingsGeoBackendIsReadOnly(t *testing.T) {
	t.Parallel()
	mmdbPath := filepath.Join("..", "geo", "testdata", "country.mmdb")
	local, err := geo.OpenMMDB(mmdbPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name    string
		backend geo.Backend // nil 即夹具装配的 HTTP 后端
		want    heronv1.GeoBackend
		path    string
	}{
		{"http", nil, heronv1.GeoBackend_GEO_BACKEND_HTTP, ""},
		{"mmdb", local, heronv1.GeoBackend_GEO_BACKEND_MMDB, mmdbPath},
	} {
		t.Run(c.name, func(t *testing.T) {
			var opts []harnessOption
			if c.backend != nil {
				opts = append(opts, withConfig(func(cfg *Config) { cfg.Geo = c.backend }))
			}
			h := newHarness(t, "", opts...)
			h.login(t)
			check := func(s *heronv1.Settings) {
				t.Helper()
				if s.GeoBackend != c.want || s.GeoMmdbPath != c.path {
					t.Errorf("backend = %v path = %q, want %v %q", s.GeoBackend, s.GeoMmdbPath, c.want, c.path)
				}
			}
			check(currentSettings(t, h))
			for _, value := range []struct {
				backend heronv1.GeoBackend
				path    string
			}{
				{}, {heronv1.GeoBackend_GEO_BACKEND_HTTP, ""}, {heronv1.GeoBackend_GEO_BACKEND_MMDB, "/other.mmdb"}, {999, "\x00invalid"},
			} {
				in := validSettings()
				in.GeoBackend, in.GeoMmdbPath = value.backend, value.path
				check(saveSettings(t, h, in))
				check(currentSettings(t, h))
			}
		})
	}
}

// 回显的后端取自 Config.Geo；缺了它 GetSettings 要到请求时才空指针，装配时就拒绝。
func TestNewRequiresTheGeoBackend(t *testing.T) {
	t.Parallel()
	defer func() {
		if r := recover(); r != "api.Config.Geo must be set" {
			t.Errorf("panic = %v, want api.Config.Geo must be set", r)
		}
	}()
	h := newHarness(t, "")
	cfg := h.svc.cfg
	cfg.Geo = nil
	New(cfg, h.deps())
}
