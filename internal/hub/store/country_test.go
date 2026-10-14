package store

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/xjetry/heron/internal/clock"
	"github.com/xjetry/heron/internal/hub/metric"
)

// v13 的完整 DDL：v12 加上节点的来源地址。
var schemaV13 = append(slices.Clone(schemaV12), "ALTER TABLE node ADD COLUMN last_source TEXT NOT NULL DEFAULT ''")

// 旧库的节点升级后三列都是空串：没有国家，已有的来源地址原样保留，查询开启后按它补查。
func TestMigrationFromV13AddsEmptyCountry(t *testing.T) {
	t.Parallel()
	migrated := migrateFrom(t, 13, func(t *testing.T, db *sql.DB) {
		if _, err := db.Exec("INSERT INTO node (id, name, token_hash, created_at, last_source) VALUES (7, 'kept', x'00', 1, '8.8.8.8')"); err != nil {
			t.Fatal(err)
		}
	})
	if v := userVersion(t, migrated.r); v != schemaVersion {
		t.Fatalf("user_version = %d, want %d", v, schemaVersion)
	}
	n, err := migrated.GetNode(t.Context(), 7)
	if err != nil || n.LastSource != "8.8.8.8" || n.Country != "" || n.CountryIP != "" || n.CountryPin != "" {
		t.Fatalf("node after migration: %+v %v", n, err)
	}
}

// SetLookupCountry 只在 last_source 仍是所查地址时写入：地址已变时不写、返回 false；节点不存在时返回 ErrNotFound。
func TestSetLookupCountryIsConditionalOnTheAddress(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	id, _, _ := s.CreateNode(ctx, NewNode{Name: "n"}, hash(1))
	row := metric.Row{NodeID: id, TS: 600, CoverageStart: 600, Bucket: metric.NewBucket(), LastSeen: clk.Now(), Source: "1.1.1.1"}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{row}}); err != nil {
		t.Fatal(err)
	}
	if set, err := s.SetLookupCountry(ctx, id, "8.8.8.8", "US"); err != nil || set {
		t.Fatalf("answer for another address: set = %v %v", set, err)
	}
	if n, err := s.GetNode(ctx, id); err != nil || n.Country != "" || n.CountryIP != "" {
		t.Fatalf("answer for another address was written: %+v %v", n, err)
	}
	if set, err := s.SetLookupCountry(ctx, id, "1.1.1.1", "AU"); err != nil || !set {
		t.Fatalf("answer for the current address: set = %v %v", set, err)
	}
	if n, err := s.GetNode(ctx, id); err != nil || n.Country != "AU" || n.CountryIP != "1.1.1.1" {
		t.Fatalf("after a matching answer: %+v %v", n, err)
	}
	if set, err := s.SetLookupCountry(ctx, id+1, "1.1.1.1", "AU"); !errors.Is(err, ErrNotFound) || set {
		t.Fatalf("missing node: set = %v %v, want ErrNotFound", set, err)
	}
}

// 写者自己维持"country 与 country_ip 同空同非空"：空地址（会匹配从未上报的节点）与不是国家码的值都返回错误、
// 什么都不写，不靠调用方先过滤。
func TestSetLookupCountryRejectsEmptyAddressAndNonCountry(t *testing.T) {
	t.Parallel()
	s, clk := open(t)
	ctx := t.Context()
	silent, _, _ := s.CreateNode(ctx, NewNode{Name: "never reported"}, hash(1))
	reported, _, _ := s.CreateNode(ctx, NewNode{Name: "reported"}, hash(2))
	row := metric.Row{NodeID: reported, TS: 600, CoverageStart: 600, Bucket: metric.NewBucket(), LastSeen: clk.Now(), Source: "8.8.8.8"}
	if _, err := s.WriteMinuteBatch(ctx, metric.Batch{Rows: []metric.Row{row}}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		id            int64
		addr, country string
		want          string
	}{
		{silent, "", "US", "lookup country of node 1: empty address"},
		{reported, "8.8.8.8", "", `lookup country of node 2 for 8.8.8.8: "" is not two uppercase letters`},
		{reported, "8.8.8.8", "us", `"us" is not two uppercase letters`},
		{reported, "8.8.8.8", "USA", `"USA" is not two uppercase letters`},
	} {
		if set, err := s.SetLookupCountry(ctx, c.id, c.addr, c.country); set || err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("SetLookupCountry(%d, %q, %q) = %v %v, want an error containing %q", c.id, c.addr, c.country, set, err, c.want)
		}
	}
	for _, id := range []int64{silent, reported} {
		if n, err := s.GetNode(ctx, id); err != nil || n.Country != "" || n.CountryIP != "" {
			t.Fatalf("rejected write left %+v %v", n, err)
		}
	}
}

// 显示值：pin 非空取 pin（manual），否则查得值（lookup），都空为 none。
func TestDisplayCountry(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		n       Node
		country string
		source  CountrySource
	}{
		{Node{}, "", CountryNone},
		{Node{Country: "US", CountryIP: "8.8.8.8"}, "US", CountryLookup},
		{Node{CountryPin: "JP"}, "JP", CountryManual},
		{Node{Country: "US", CountryIP: "8.8.8.8", CountryPin: "JP"}, "JP", CountryManual},
	} {
		if country, source := c.n.DisplayCountry(); country != c.country || source != c.source {
			t.Errorf("DisplayCountry(%+v) = %q %v, want %q %v", c.n, country, source, c.country, c.source)
		}
	}
}

// 国家查询设置：从未保存过为开、默认服务地址；GeoUpdate 里缺席的项不改；只给外观的保存不碰它们；保存为关即关。
func TestGeoSettingsDefaultsAndPartialUpdate(t *testing.T) {
	t.Parallel()
	s, _ := open(t)
	ctx := t.Context()
	if g, err := s.GeoSettings(ctx); err != nil || g != (GeoSettings{Enabled: true, URL: "https://ipinfo.io/{ip}/country"}) {
		t.Fatalf("never saved: %+v %v", g, err)
	}
	on, url := true, "https://geo.example/{ip}"
	saved, err := s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "dark"}, Geo: GeoUpdate{Enabled: &on, URL: &url}})
	if want := (GeoSettings{Enabled: true, URL: url}); err != nil || saved.Geo != want {
		t.Fatalf("save returned %+v %v, want %+v", saved.Geo, err, want)
	}
	saved, err = s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "light", Title: "t"}})
	if want := (GeoSettings{Enabled: true, URL: url}); err != nil || saved.Geo != want {
		t.Fatalf("appearance-only save returned %+v %v, want %+v", saved.Geo, err, want)
	}
	off := false
	if _, err := s.SaveSettings(ctx, SettingsUpdate{Appearance: &SiteAppearance{Theme: "light"}, Geo: GeoUpdate{Enabled: &off}}); err != nil {
		t.Fatal(err)
	}
	st, err := s.Settings(ctx)
	if want := (GeoSettings{URL: url}); err != nil || st.Geo != want || st.Site != (SiteSettings{Theme: "light", PublicEnabled: true}) {
		t.Fatalf("settings = %+v %+v %v, want %+v", st.Site, st.Geo, err, want)
	}
}

// 开关存 0 / 1：保存写出的就是这两个值。库里出现别的文本（手工改库、别的写法）时读设置报错，既不按关也不按开；
// 这样的库打开时就被拒绝。运行中的库读到它时，查询器因读设置失败而不出网。
func TestGeoEnabledIsZeroOrOneAndOtherValuesRefuseToOpen(t *testing.T) {
	t.Parallel()
	s, _ := openAt(t)
	ctx := t.Context()
	for on, want := range map[bool]string{true: "1", false: "0"} {
		if _, err := s.SaveSettings(ctx, SettingsUpdate{Geo: GeoUpdate{Enabled: &on}}); err != nil {
			t.Fatal(err)
		}
		var got string
		if err := s.r.QueryRowContext(ctx, "SELECT value FROM setting WHERE key = 'geo.enabled'").Scan(&got); err != nil || got != want {
			t.Fatalf("saved enabled=%v as %q %v, want %q", on, got, err, want)
		}
		if g, err := s.GeoSettings(ctx); err != nil || g.Enabled != on {
			t.Fatalf("stored %q read as %+v %v", want, g, err)
		}
	}
	for _, v := range []string{"true", "false", "TRUE", "yes", "", "2", " 1"} {
		t.Run(v, func(t *testing.T) {
			s, path := openAt(t)
			if err := s.write(t.Context(), func(tx *sql.Tx) error {
				_, err := tx.Exec("INSERT INTO setting (key, value) VALUES ('geo.enabled', ?)", v)
				return err
			}); err != nil {
				t.Fatal(err)
			}
			want := fmt.Sprintf("setting geo.enabled must be 0 or 1; got %q", v)
			if g, err := s.GeoSettings(t.Context()); err == nil || err.Error() != want {
				t.Fatalf("running store read %q as %+v %v, want error %q", v, g, err, want)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(path, clock.NewFake(time.Now()), slog.Default(), RequireCurrentSchema)
			if reopened != nil {
				reopened.Close()
			}
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("reopen with geo.enabled = %q: %v, want an error containing %q", v, err, want)
			}
		})
	}
}
