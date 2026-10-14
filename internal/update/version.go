package update

import (
	"strconv"
	"strings"
)

// ValidVersion 只接受无前导零且各段不超过 uint32 的正式版本。
func ValidVersion(version string) bool {
	_, ok := parseVersion(version)
	return ok
}

// Newer 同时验证当前版本与目标版本；开发版本不参与在线更新排序。
func Newer(target, current string) bool {
	a, aOK := parseVersion(target)
	b, bOK := parseVersion(current)
	if !aOK || !bOK {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return a[i] > b[i]
		}
	}
	return false
}

// ReleaseTag 解析 release 的 tag：vMAJOR.MINOR.PATCH，或其后带 semver 预发布后缀 -<点分标识符>。返回去掉预发布
// 后缀的正式版本与是否为预发布。构建元数据（+）不接受：check_version 拒绝它，release 的 tag 里不会出现。标识符
// 只含 [0-9A-Za-z-] 且不为空，纯数字的不带前导 0（semver 2.0 第 9 条）。正式版部分与 ValidVersion 同一个解析。
func ReleaseTag(tag string) (core string, prerelease bool, ok bool) {
	core, pre, hasPre := strings.Cut(tag, "-")
	if !ValidVersion(core) {
		return "", false, false
	}
	if !hasPre {
		return core, false, true
	}
	for _, id := range strings.Split(pre, ".") {
		if id == "" {
			return "", false, false
		}
		numeric := true
		for _, ch := range id {
			switch {
			case ch >= '0' && ch <= '9':
			case ch >= 'a' && ch <= 'z', ch >= 'A' && ch <= 'Z', ch == '-':
				numeric = false
			default:
				return "", false, false
			}
		}
		if numeric && len(id) > 1 && id[0] == '0' {
			return "", false, false
		}
	}
	return core, true, true
}

func parseVersion(version string) ([3]uint32, bool) {
	var result [3]uint32
	if !strings.HasPrefix(version, "v") {
		return result, false
	}
	parts := strings.Split(version[1:], ".")
	if len(parts) != len(result) {
		return result, false
	}
	for i, part := range parts {
		if part == "" || len(part) > 10 || len(part) > 1 && part[0] == '0' {
			return result, false
		}
		for _, ch := range part {
			if ch < '0' || ch > '9' {
				return result, false
			}
		}
		n, err := strconv.ParseUint(part, 10, 32)
		if err != nil {
			return result, false
		}
		result[i] = uint32(n)
	}
	return result, true
}
