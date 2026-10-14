package geo

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/hub/store"
)

// 计数器包住真实读取器，区分没有查询与查询但没有写入；不伪造后端结果。
type countedBackend struct {
	Backend
	calls []string
}

func (b *countedBackend) Lookup(ctx context.Context, s store.GeoSettings, addr netip.Addr) (string, error) {
	b.calls = append(b.calls, addr.String())
	return b.Backend.Lookup(ctx, s, addr)
}

func withMMDB(t *testing.T, f *fixture) *countedBackend {
	t.Helper()
	db, err := OpenMMDB("testdata/country.mmdb")
	if err != nil {
		t.Fatal(err)
	}
	b := &countedBackend{Backend: db}
	f.r.backend = b
	return b
}

func TestMMDBPublicOnceAndPrivateNever(t *testing.T) {
	f := newFixture(t)
	b := withMMDB(t, f)
	f.enable(false)
	v4 := f.report("v4", "8.8.8.8")
	v6 := f.report("v6", "2606:4700::1111")
	private := f.report("private", "10.0.0.1")
	f.sweep()
	if len(b.calls) != 0 {
		t.Errorf("disabled mmdb calls = %v, want none", b.calls)
	}
	f.enable(true)
	f.sweep()
	f.wantCountry(v4, "US", "8.8.8.8")
	f.wantCountry(v6, "AU", "2606:4700::1111")
	f.wantCountry(private, "", "")
	if len(b.calls) != 2 {
		t.Errorf("mmdb calls = %v, want the two public addresses only", b.calls)
	}
	f.sweep()
	f.clk.Advance(2 * time.Hour)
	f.sweep()
	if len(b.calls) != 2 {
		t.Errorf("successful mmdb address queried again: %v", b.calls)
	}
	f.wantRequests()
}

// 本地查不到（1.1.1.1 无记录、11.0.0.1 的记录缺 country）与查得的码无效（9.9.9.9 是小写 us）都按失败退避一小时。
// 日志写明是哪一种：查不到记成"无记录"，不借用校验答案的文案。
func TestMMDBFailuresBackOff(t *testing.T) {
	for _, c := range []struct {
		addr string
		err  error
	}{{"1.1.1.1", errNoCountryRecord}, {"9.9.9.9", errNotCountry}, {"11.0.0.1", errNoCountryRecord}} {
		t.Run(c.addr, func(t *testing.T) {
			f := newFixture(t)
			b := withMMDB(t, f)
			f.enable(true)
			id := f.report("node", c.addr)
			f.sweep()
			f.wantCountry(id, "", "")
			if len(b.calls) != 1 {
				t.Fatalf("initial mmdb calls = %v, want one", b.calls)
			}
			if logs, want := f.logs.String(), fmt.Sprintf("err=%q", c.err.Error()); !strings.Contains(logs, want) {
				t.Errorf("failure logged as %q, want %s", logs, want)
			}
			f.clk.Advance(time.Hour - time.Second)
			f.sweep()
			if len(b.calls) != 1 {
				t.Errorf("mmdb calls before one hour = %v, want one", b.calls)
			}
			f.clk.Advance(time.Second)
			f.sweep()
			if len(b.calls) != 2 {
				t.Errorf("mmdb calls at one hour = %v, want two", b.calls)
			}
			f.wantRequests()
		})
	}
}

// mmdb 下 geo.url 不生效，退避键的服务一项是库路径：查不到的地址进了退避之后改 geo.url，不到一小时仍不重查。
func TestMMDBBackoffSurvivesAGeoURLChange(t *testing.T) {
	f := newFixture(t)
	b := withMMDB(t, f)
	f.enable(true)
	id := f.report("node", "11.0.0.1")
	f.sweep()
	if len(b.calls) != 1 {
		t.Fatalf("initial mmdb calls = %v, want one", b.calls)
	}
	other := f.svc.srv.URL + "/other/{ip}"
	if err := f.saveGeo(t.Context(), store.GeoUpdate{URL: &other}); err != nil {
		t.Fatal(err)
	}
	f.clk.Advance(time.Minute)
	f.sweep()
	if len(b.calls) != 1 {
		t.Errorf("mmdb calls after changing geo.url = %v, want the backoff to hold", b.calls)
	}
	f.wantCountry(id, "", "")
	f.wantRequests()
}
