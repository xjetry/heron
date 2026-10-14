package s3

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xjetry/heron/internal/clock"
)

// 通用 SigV4 套件：前四组复制自上游，后三组的期望值由 botocore 算出（来源与生成方式见 testdata/README.md）。
func TestAWSVectors(t *testing.T) {
	for _, name := range []string{"get-vanilla", "post-x-www-form-urlencoded", "get-header-key-duplicate", "get-vanilla-query-order-key-case", "get-utf8", "get-space", "get-unreserved"} {
		t.Run(name, func(t *testing.T) {
			read := func(ext string) string {
				t.Helper()
				b, err := os.ReadFile(filepath.Join("testdata", name, name+"."+ext))
				if err != nil {
					t.Fatal(err)
				}
				return string(b)
			}
			head, body, _ := strings.Cut(read("req"), "\n\n")
			lines := strings.Split(head, "\n")
			// 请求行是"方法 目标 版本"，目标里可以有空格（get-space）：按第一个与最后一个空格切。
			method, rest, _ := strings.Cut(lines[0], " ")
			target := rest[:strings.LastIndex(rest, " ")]
			req, err := http.NewRequest(method, "https://example.amazonaws.com"+target, bytes.NewBufferString(body))
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range lines[1:] {
				k, v, ok := strings.Cut(line, ":")
				if !ok {
					t.Fatalf("invalid fixture header: %q", line)
				}
				if strings.EqualFold(k, "host") {
					req.Host = v
				} else {
					req.Header.Add(k, v)
				}
			}
			canonical, signed := canonicalRequest(req, hashHex([]byte(body)))
			if want := read("creq"); canonical != want {
				t.Errorf("canonical request mismatch\ngot: %q\nwant: %q", canonical, want)
			}
			sts, authorization := authorization(req, canonical, signed, "us-east-1", "service", "AKIDEXAMPLE", "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY")
			if want := read("sts"); sts != want {
				t.Errorf("string to sign mismatch\ngot: %q\nwant: %q", sts, want)
			}
			if want := read("authz"); authorization != want {
				t.Errorf("signature mismatch\ngot: %q\nwant: %q", authorization, want)
			}
		})
	}
}

// s3Vector 是 testdata/s3-signing.json 的一条：botocore 的 S3SigV4Auth 对同一请求给出的 Host、请求 URI 与 Authorization。
type s3Vector struct {
	VirtualHost      bool        `json:"virtual_host"`
	Endpoint         string      `json:"endpoint"`
	Region           string      `json:"region"`
	Method           string      `json:"method"`
	Key              string      `json:"key"`
	Query            [][2]string `json:"query"`
	Payload          string      `json:"payload"`
	Host             string      `json:"host"`
	RequestURI       string      `json:"request_uri"`
	CanonicalRequest string      `json:"canonical_request"`
	Authorization    string      `json:"authorization"`
}

// 每条向量经客户端的公开操作发出（带查询串的经 request 本身），服务端收到的 Host、请求 URI 与 Authorization 必须与
// botocore 逐字相同。向量覆盖 path-style 与 virtual-host、带与不带路径前缀和端口的 endpoint，键含空格、+、中文、%、~、
// 保留字符、重复斜杠与点段，查询串含重复键与互为前缀的键。
func TestS3SigningVectors(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "s3-signing.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors []s3Vector
	if err := json.Unmarshal(data, &vectors); err != nil || len(vectors) == 0 {
		t.Fatalf("vectors: %d, %v", len(vectors), err)
	}
	for i, v := range vectors {
		t.Run(fmt.Sprintf("%02d-%s-%s", i, v.Method, v.Key), func(t *testing.T) {
			var host, uri, auth string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				host, uri, auth = r.Host, r.RequestURI, r.Header.Get("Authorization")
				_, _ = io.Copy(io.Discard, r.Body)
			}))
			defer srv.Close()
			c, err := New(Config{Endpoint: v.Endpoint, Bucket: "backups", Region: v.Region, AccessKey: "access", Secret: "secret", VirtualHost: v.VirtualHost}, clock.NewFake(signingTime))
			if err != nil {
				t.Fatal(err)
			}
			defer dialTo(c, srv).CloseIdleConnections()
			ctx := deadline(t)
			switch {
			case len(v.Query) > 0:
				query := url.Values{}
				for _, kv := range v.Query {
					query.Add(kv[0], kv[1])
				}
				var resp *http.Response
				if resp, err = c.request(ctx, v.Method, v.Key, query, nil, 0, hashHex(nil)); err == nil {
					resp.Body.Close()
				}
			case v.Method == http.MethodPut:
				err = c.PutObject(ctx, v.Key, strings.NewReader(v.Payload))
			case v.Method == http.MethodGet:
				err = c.GetObject(ctx, v.Key, io.Discard, 1)
			case v.Method == http.MethodDelete:
				err = c.DeleteObject(ctx, v.Key)
			default:
				t.Fatalf("vector method %q", v.Method)
			}
			if err != nil {
				t.Fatal(err)
			}
			if host != v.Host || uri != v.RequestURI || auth != v.Authorization {
				t.Errorf("signed request differs from botocore\nhost %q\nwant %q\nuri  %q\nwant %q\nauth %q\nwant %q\nbotocore canonical request:\n%s", host, v.Host, uri, v.RequestURI, auth, v.Authorization, v.CanonicalRequest)
			}
		})
	}
}
