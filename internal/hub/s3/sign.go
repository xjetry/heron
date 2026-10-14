package s3

import (
	"cmp"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
)

func hashHex(body []byte) string {
	h := sha256.Sum256(body)
	return hex.EncodeToString(h[:])
}

func mac(key []byte, value string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(value))
	return h.Sum(nil)
}

// S3 的路径就是对象键，不能清理重复斜杠、点段或二次转义百分号。
func escape(value string, slash bool) string {
	const digits = "0123456789ABCDEF"
	var out strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("-_.~", rune(c)) || slash && c == '/' {
			out.WriteByte(c)
		} else {
			out.WriteByte('%')
			out.WriteByte(digits[c>>4])
			out.WriteByte(digits[c&15])
		}
	}
	return out.String()
}

// canonicalQuery 先按编码后的键、键相同再按编码后的值排序。对整串 k=v 排序在一个键是另一个键的前缀时会排错：
// a-b=1 排在 a=2 之前（'-' 小于 '='），规范顺序是 a=2&a-b=1。请求线上发的也是这个串，线上与签名同一顺序。
func canonicalQuery(query url.Values) string {
	type pair struct{ k, v string }
	var pairs []pair
	for k, values := range query {
		ek := escape(k, false)
		for _, v := range values {
			pairs = append(pairs, pair{ek, escape(v, false)})
		}
	}
	slices.SortFunc(pairs, func(a, b pair) int { return cmp.Or(strings.Compare(a.k, b.k), strings.Compare(a.v, b.v)) })
	out := make([]string, len(pairs))
	for i, p := range pairs {
		out[i] = p.k + "=" + p.v
	}
	return strings.Join(out, "&")
}

func canonicalRequest(req *http.Request, payloadHash string) (string, string) {
	host := req.Host
	if host == "" {
		host = req.URL.Host
	}
	values := map[string][]string{"host": {host}}
	for key, vs := range req.Header {
		key = strings.ToLower(key)
		if key == "authorization" || key == "host" || key == "user-agent" || key == "transfer-encoding" {
			continue
		}
		for _, v := range vs {
			values[key] = append(values[key], strings.Join(strings.Fields(v), " "))
		}
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var headers strings.Builder
	for _, k := range keys {
		headers.WriteString(k + ":" + strings.Join(values[k], ",") + "\n")
	}
	signed := strings.Join(keys, ";")
	path := req.URL.Path
	if path == "" {
		path = "/"
	}
	return strings.Join([]string{req.Method, escape(path, true), canonicalQuery(req.URL.Query()), headers.String(), signed, payloadHash}, "\n"), signed
}

func authorization(req *http.Request, canonical, signed, region, service, accessKey, secret string) (string, string) {
	stamp := req.Header.Get("X-Amz-Date")
	day := stamp[:8]
	scope := day + "/" + region + "/" + service + "/aws4_request"
	sts := "AWS4-HMAC-SHA256\n" + stamp + "\n" + scope + "\n" + hashHex([]byte(canonical))
	key := mac(mac(mac(mac([]byte("AWS4"+secret), day), region), service), "aws4_request")
	return sts, "AWS4-HMAC-SHA256 Credential=" + accessKey + "/" + scope + ", SignedHeaders=" + signed + ", Signature=" + hex.EncodeToString(mac(key, sts))
}
