package web

import (
	"io/fs"
	"net/http"
	"strings"
)

const (
	encodingIdentity = ""
	encodingGzip     = "gzip"
	encodingBr       = "br"
)

type precompressedEncoding struct {
	name string
	ext  string
}

// precompressedEncodings 是服务端支持预压缩下发的编码，按权重相等时的偏好从高到低排列：
// br 压缩率高于 gzip，客户端以相同 q 值声明两者时优先下发 br。
var precompressedEncodings = []precompressedEncoding{
	{name: encodingBr, ext: ".br"},
	{name: encodingGzip, ext: ".gz"},
}

// precompressedFile 表示一个附带构建期预压缩副本的静态文件。
// 只要任一预压缩副本存在，响应就必须带 Vary: Accept-Encoding（即使本次选中的是原文），
// 否则共享缓存会把一种编码下的响应复用给不同 Accept-Encoding 的请求。
type precompressedFile interface {
	HasEncoding(enc string) bool
	OpenEncoding(enc string) (fs.File, error)
}

// selectEncoding 按 RFC 9110 的 Accept-Encoding 规则从可用编码中择优返回一种内容编码。
// 返回的 enc 为 "br"、"gzip" 或 ""（identity）；ok 为 false 表示客户端显式排除了全部可用表示
// （例如 identity;q=0 或 *;q=0 且无可用的压缩副本），调用方应回 406 Not Acceptable。
func selectEncoding(headerValues []string, hasBr, hasGzip bool) (enc string, ok bool) {
	qBr, qGzip, qIdentity := parseAcceptEncoding(headerValues)
	const rejected = -1
	bestQ := rejected
	if hasBr && qBr > bestQ {
		bestQ = qBr
		enc = encodingBr
	}
	if hasGzip && qGzip > bestQ {
		bestQ = qGzip
		enc = encodingGzip
	}
	if qIdentity > bestQ {
		bestQ = qIdentity
		enc = encodingIdentity
	}
	if bestQ == rejected {
		return "", false
	}
	return enc, true
}

// parseAcceptEncoding 解析请求的全部 Accept-Encoding 头行，返回 br、gzip、identity 的接受权重：
//   - -1：不可接受（未列出且无正权重 *，或显式给出 q=0）；
//   - 0：未显式提及 identity 与 * 时的默认可接受回退（低于任何显式正 q 值，但高于不可接受）；
//   - 1..1000：显式给出的正 q 值（按千分位整数表示，避免浮点舍入）。
//
// 未发送该头或值为空时，客户端可能不支持任何内容编码，只接受原文（identity=1000，其余 -1）。
func parseAcceptEncoding(headerValues []string) (qBr, qGzip, qIdentity int) {
	const (
		unset    = -2
		rejected = -1
		implicit = 0
		fullQ    = 1000
	)
	brQ, gzipQ, idQ, starQ := unset, unset, unset, unset
	parsedAny := false

	for _, line := range headerValues {
		for item := range strings.SplitSeq(line, ",") {
			item = strings.TrimSpace(item)
			if item == "" {
				continue
			}
			coding, q, valid := parseEncodingItem(item)
			if !valid {
				continue
			}
			parsedAny = true
			score := q
			if score == 0 {
				score = rejected
			}
			switch coding {
			case encodingBr:
				if brQ == unset || score > brQ {
					brQ = score
				}
			case encodingGzip, "x-gzip":
				if gzipQ == unset || score > gzipQ {
					gzipQ = score
				}
			case "identity":
				if idQ == unset || score > idQ {
					idQ = score
				}
			case "*":
				if starQ == unset || score > starQ {
					starQ = score
				}
			}
		}
	}

	if !parsedAny {
		return rejected, rejected, fullQ
	}
	resolve := func(explicit, fallback int) int {
		if explicit != unset {
			return explicit
		}
		if starQ != unset {
			return starQ
		}
		return fallback
	}
	return resolve(brQ, rejected), resolve(gzipQ, rejected), resolve(idQ, implicit)
}

func parseEncodingItem(item string) (coding string, q int, ok bool) {
	parts := strings.Split(item, ";")
	coding = strings.ToLower(strings.TrimSpace(parts[0]))
	if coding == "" {
		return "", 0, false
	}
	q = 1000
	seenQ := false
	for _, rawParam := range parts[1:] {
		param := strings.TrimSpace(rawParam)
		if param == "" {
			continue
		}
		k, v, hasEq := strings.Cut(param, "=")
		if !hasEq || !strings.EqualFold(strings.TrimSpace(k), "q") || seenQ {
			return "", 0, false
		}
		parsed, valid := parseQValue(strings.TrimSpace(v))
		if !valid {
			return "", 0, false
		}
		q = parsed
		seenQ = true
	}
	return coding, q, true
}

// parseQValue 按 RFC 9110 §12.4.2 解析 qvalue（0[.0-9{0,3}] 或 1[.0{0,3}]），返回 0..1000。
func parseQValue(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	switch s[0] {
	case '1':
		if len(s) == 1 {
			return 1000, true
		}
		if s[1] != '.' || len(s) > 5 {
			return 0, false
		}
		for i := 2; i < len(s); i++ {
			if s[i] != '0' {
				return 0, false
			}
		}
		return 1000, true
	case '0':
		if len(s) == 1 {
			return 0, true
		}
		if s[1] != '.' || len(s) > 5 {
			return 0, false
		}
		val := 0
		mult := 100
		for i := 2; i < len(s); i++ {
			c := s[i]
			if c < '0' || c > '9' {
				return 0, false
			}
			val += int(c-'0') * mult
			mult /= 10
		}
		return val, true
	default:
		return 0, false
	}
}

// appendVary 在 Vary 头中追加 token；若已存在同名字段或 * 则不重复写入，保留外层已设的其它字段。
func appendVary(h http.Header, token string) {
	for _, line := range h.Values("Vary") {
		for item := range strings.SplitSeq(line, ",") {
			item = strings.TrimSpace(item)
			if item == "*" || strings.EqualFold(item, token) {
				return
			}
		}
	}
	h.Add("Vary", token)
}
