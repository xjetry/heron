package main

import (
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/xjetry/heron/internal/agentwire"
)

const (
	defaultTTL = 30 * time.Second
	minTTL     = agentwire.MinTTL
)

// parseTTL 解析 --offline-after（亦可经 HERON_OFFLINE_AFTER 给出）。TTL 是离线发现延迟的上界，也是这条链上
// 唯一被直接配置的量：上报间隔、退避上限、告警宽限期下限都由它反推。
//
// 空串取默认值，与缺席同义：这是该 flag 自己的空值语义，沿用它作为环境变量时的既有行为（HERON_OFFLINE_AFTER=""
// 一直是取默认）。其它 flag 的空值语义各由它们自己定，通用回填 applyFlagEnv 不替任何 flag 解释空串。
func parseTTL(s string) (time.Duration, error) {
	if s == "" {
		return defaultTTL, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%s: %w", ttlSource, err)
	}
	if d < minTTL {
		return 0, fmt.Errorf("%s: %v is below the minimum %v", ttlSource, d, minTTL)
	}
	if d > agentwire.MaxTTL {
		return 0, fmt.Errorf("%s: %v is above the maximum %v", ttlSource, d, agentwire.MaxTTL)
	}
	return d, nil
}

const (
	offlineAfterFlag = "offline-after"
	adminOriginFlag  = "admin-origin"
)

var ttlSource = flagSource(offlineAfterFlag)

// flagSource 是 flag 值的错误前缀，同时点名 flag 与环境变量（--offline-after (HERON_OFFLINE_AFTER)）：applyFlagEnv
// 回填之后，值从哪一处来解析方看不到，运维两处都可能去改。
func flagSource(name string) string { return "--" + name + " (" + flagEnvName(name) + ")" }

// flagEnvName 是 flag 对应的环境变量名：横线换成下划线、转大写、加 HERON_ 前缀（offline-after → HERON_OFFLINE_AFTER）。
func flagEnvName(name string) string {
	return "HERON_" + strings.ToUpper(strings.ReplaceAll(name, "-", "_"))
}

// applyFlagEnv 在 fs.Parse 之后执行 serve 配置面唯一的一条规则：显式 flag > HERON_<FLAG> > flag 默认值。
// 环境变量是否给出看 lookupEnv 的存在位，设置为空串也算给出，与显式写出空值的 flag 同义；空值意味着什么由各 flag
// 自己定义，这里不特判。回填经 fs.Set，所以之后的 fs.Visit 把回填的 flag 也算作已给出，"是否给出"的判定
// （如 --geo-mmdb）对两种来源一致。值不合法时错误点名环境变量与 flag。
func applyFlagEnv(fs *flag.FlagSet, lookupEnv func(string) (string, bool)) error {
	explicit := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { explicit[f.Name] = true })
	var err error
	fs.VisitAll(func(f *flag.Flag) {
		if err != nil || explicit[f.Name] {
			return
		}
		name := flagEnvName(f.Name)
		value, ok := lookupEnv(name)
		if !ok {
			return
		}
		if setErr := fs.Set(f.Name, value); setErr != nil {
			err = fmt.Errorf("%s (--%s): invalid value %q: %w", name, f.Name, value, setErr)
		}
	})
	return err
}

func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

var localtimePath = "/etc/localtime"

// loadZone 的三个来源都经 namedZone 规范化后加载，Location 的名字会随流量响应交给面板。
// fallback 只表示本机时区不可判定，serve 据此告警；显式指定的无效时区仍拒绝启动。
func loadZone(name string) (*time.Location, bool, error) {
	if name != "" {
		loc, err := namedZone(name)
		return loc, false, err
	}
	if name := os.Getenv("TZ"); name != "" {
		if loc, err := namedZone(name); err == nil {
			return loc, false, nil
		}
	}
	if target, err := filepath.EvalSymlinks(localtimePath); err == nil {
		if _, name, ok := strings.Cut(target, "zoneinfo/"); ok && name != "" {
			if loc, err := namedZone(name); err == nil {
				return loc, false, nil
			}
		}
	}
	return time.UTC, true, nil
}

// canonicalZone 去掉 tzdata 的编码目录前缀；同一区域的周期判定与面板 Intl 格式化必须共用规范名。
// Go 能加载的名字不一定能被 Intl 接受，Local 和空串也没有可下发的区域含义。
func canonicalZone(name string) (string, bool) {
	name = strings.TrimPrefix(name, "posix/")
	name = strings.TrimPrefix(name, "right/")
	return name, name != "" && name != "Local"
}

func namedZone(name string) (*time.Location, error) {
	canonical, ok := canonicalZone(name)
	if !ok {
		return nil, fmt.Errorf("--timezone: %q is not an IANA time zone", name)
	}
	loc, err := time.LoadLocation(canonical)
	if err != nil {
		return nil, fmt.Errorf("--timezone: %w", err)
	}
	return loc, nil
}
