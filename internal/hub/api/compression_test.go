package api

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/hub/theme/themetest"
)

// 直接读取线上字节，不经 Connect 客户端解压；gRPC 的压缩位在消息帧里，只有检查 HTTP 头会漏掉它。
// 管理端只有 GetSnapshot 压缩（理由与它的字符串字段清单见 Service.Handler），其余过程一律不压缩。
func TestAdminResponseCompressionPolicy(t *testing.T) {
	t.Parallel()
	h := newHarness(t, "")
	h.login(t)
	h.createNode(t, strings.Repeat("node-", 10))
	srv := httptest.NewUnstartedServer(h.srv.Config.Handler)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	client := srv.Client()
	client.Transport.(*http.Transport).DisableCompression = true
	loginURL, err := http.NewRequest(http.MethodGet, h.srv.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	cookies := h.http.Jar.Cookies(loginURL.URL)
	for _, protocol := range []string{"connect", "grpc", "grpc-web"} {
		for _, compressedRequest := range []bool{false, true} {
			requestEncoding := "identity-request"
			if compressedRequest {
				requestEncoding = "gzip-request"
			}
			for _, tc := range []struct {
				name, method string
				request      proto.Message
				response     proto.Message
				anonymous    bool
				compressed   bool
			}{
				{"list", "ListNodes", &heronv1.ListNodesRequest{}, &heronv1.ListNodesResponse{}, false, false},
				{"upload", "UploadTheme", &heronv1.UploadThemeRequest{Package: themetest.Minimal(t, "compression")}, &heronv1.UploadThemeResponse{}, false, false},
				{"unauthenticated", "ListNodes", &heronv1.ListNodesRequest{}, nil, true, false},
				{"snapshot", "GetSnapshot", &heronv1.GetSnapshotRequest{}, &heronv1.GetSnapshotResponse{}, false, true},
			} {
				t.Run(protocol+"/"+requestEncoding+"/"+tc.name, func(t *testing.T) {
					var body []byte
					var err error
					if protocol == "connect" {
						body, err = protojson.Marshal(tc.request)
					} else {
						body, err = proto.Marshal(tc.request)
					}
					if err != nil {
						t.Fatal(err)
					}
					if compressedRequest {
						var buf bytes.Buffer
						zw := gzip.NewWriter(&buf)
						if _, err := zw.Write(body); err != nil {
							t.Fatal(err)
						}
						if err := zw.Close(); err != nil {
							t.Fatal(err)
						}
						body = buf.Bytes()
					}
					if protocol != "connect" {
						frame := make([]byte, 5+len(body))
						if compressedRequest {
							frame[0] = 1
						}
						binary.BigEndian.PutUint32(frame[1:5], uint32(len(body)))
						copy(frame[5:], body)
						body = frame
					}
					req, err := http.NewRequest(http.MethodPost, srv.URL+"/heron.v1.AdminService/"+tc.method, bytes.NewReader(body))
					if err != nil {
						t.Fatal(err)
					}
					req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
					if protocol == "connect" {
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set("Connect-Protocol-Version", "1")
						if compressedRequest {
							req.Header.Set("Content-Encoding", "gzip")
						}
					} else {
						req.Header.Set("Content-Type", "application/"+protocol+"+proto")
						req.Header.Set("Grpc-Accept-Encoding", "gzip")
						req.Header.Set("TE", "trailers")
						if compressedRequest {
							req.Header.Set("Grpc-Encoding", "gzip")
						}
					}
					if !tc.anonymous {
						for _, cookie := range cookies {
							req.AddCookie(cookie)
						}
					}
					resp, err := client.Do(req)
					if err != nil {
						t.Fatal(err)
					}
					defer resp.Body.Close()
					wire, err := io.ReadAll(resp.Body)
					if err != nil {
						t.Fatal(err)
					}
					if got := resp.Header.Get("Cache-Control"); got != "no-store, no-transform" {
						t.Errorf("Cache-Control = %q, want no-store, no-transform", got)
					}
					wantEncoding := ""
					if protocol == "connect" && tc.compressed {
						wantEncoding = "gzip"
					}
					if got := resp.Header.Get("Content-Encoding"); got != wantEncoding {
						t.Errorf("Content-Encoding = %q, want %q", got, wantEncoding)
					}
					if protocol == "connect" {
						want := http.StatusOK
						if tc.anonymous {
							want = http.StatusUnauthorized
						}
						if tc.compressed {
							wire = gunzip(t, wire)
						}
						if resp.StatusCode != want {
							t.Fatalf("status = %d, want %d: %s", resp.StatusCode, want, wire)
						}
						if tc.response != nil {
							if err := protojson.Unmarshal(wire, tc.response); err != nil {
								t.Errorf("JSON response: %v", err)
							}
						}
						return
					}
					if resp.ProtoMajor != 2 || resp.StatusCode != http.StatusOK {
						t.Fatalf("gRPC transport = %s %d", resp.Proto, resp.StatusCode)
					}
					status := resp.Trailer.Get("Grpc-Status")
					if status == "" {
						status = resp.Header.Get("Grpc-Status")
					}
					messages := 0
					for len(wire) > 0 {
						if len(wire) < 5 || uint64(binary.BigEndian.Uint32(wire[1:5])) > uint64(len(wire)-5) {
							t.Fatalf("invalid gRPC frame: %x", wire)
						}
						flags, n := wire[0], int(binary.BigEndian.Uint32(wire[1:5]))
						payload := wire[5 : 5+n]
						wire = wire[5+n:]
						if flags&0x80 == 0 && (flags&1 != 0) != tc.compressed {
							t.Errorf("gRPC response frame flags=%#x, want compressed=%v", flags, tc.compressed)
						}
						if flags&1 != 0 {
							payload = gunzip(t, payload)
						}
						if flags&0x80 != 0 {
							for _, line := range strings.Split(string(payload), "\r\n") {
								if key, value, ok := strings.Cut(line, ":"); ok && strings.EqualFold(key, "grpc-status") {
									status = strings.TrimSpace(value)
								}
							}
						} else {
							messages++
							if tc.response != nil {
								if err := proto.Unmarshal(payload, tc.response); err != nil {
									t.Errorf("protobuf response: %v", err)
								}
							}
						}
					}
					wantStatus, wantMessages := "0", 1
					if tc.anonymous {
						wantStatus, wantMessages = "16", 0
					}
					if status != wantStatus || messages != wantMessages {
						t.Errorf("gRPC status/messages = %q/%d, want %q/%d", status, messages, wantStatus, wantMessages)
					}
				})
			}
		}
	}
	for _, tc := range []struct {
		name, body, origin string
		want               int
	}{
		{"origin-rejected", "{}", "https://other.invalid", http.StatusForbidden},
		{"decode-rejected", "{", "", http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/heron.v1.AdminService/ListNodes", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			resp, err := client.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.want {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.want)
			}
			if got := resp.Header.Get("Cache-Control"); got != "no-store, no-transform" {
				t.Errorf("Cache-Control = %q, want no-store, no-transform", got)
			}
			if got := resp.Header.Get("Content-Encoding"); got != "" {
				t.Errorf("Content-Encoding = %q, want uncompressed response", got)
			}
		})
	}
	t.Run("public-control", func(t *testing.T) {
		got := pubGet(t, h, "GetSnapshot", jsonQuery("{}"), map[string]string{"Accept-Encoding": "gzip, deflate, br, zstd"})
		if got.status != http.StatusOK || got.header.Get("Content-Encoding") != "gzip" || got.header.Get("Cache-Control") != "max-age=1" {
			t.Fatalf("public status/encoding/cache = %d/%q/%q", got.status, got.header.Get("Content-Encoding"), got.header.Get("Cache-Control"))
		}
	})
}

// GetSnapshot 的响应是压缩的，压缩安全的前提是 agent 能写进这条响应的文本只有受准入约束的写法（见 Service.Handler）。
// 这里枚举响应可达的全部字符串与字节字段：清单之外多出任何一个，都要先回答「它是谁写的、agent 能不能放任意文本进来」，
// 再决定加进清单还是从快照里拿掉。
func TestSnapshotStringFieldsAreAudited(t *testing.T) {
	t.Parallel()
	audited := map[string]string{
		"hub_version":                     "hub 构建时注入",
		"bound_agent_version":             "hub 构建时注入",
		"nodes.name":                      "管理员写入；非公开节点的名字正是要保护的内容",
		"nodes.metrics.boot_id":           "agent 写入，准入只收 UUID（ingest.validateBootID）",
		"nodes.metrics.net_counter_epoch": "agent 写入，准入只收小写十六进制摘要（agentwire.ValidateCounterEpoch）",
	}
	var found []string
	var walk func(prefix string, md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool)
	walk = func(prefix string, md protoreflect.MessageDescriptor, seen map[protoreflect.FullName]bool) {
		if seen[md.FullName()] {
			t.Fatalf("recursive message %s under %q: extend the audit before compressing it", md.FullName(), prefix)
		}
		seen[md.FullName()] = true
		defer delete(seen, md.FullName())
		fields := md.Fields()
		for i := 0; i < fields.Len(); i++ {
			f := fields.Get(i)
			path := prefix + string(f.Name())
			switch {
			case f.IsMap():
				t.Errorf("map field %s: audit its keys and values before compressing the snapshot", path)
			case f.Kind() == protoreflect.StringKind || f.Kind() == protoreflect.BytesKind:
				found = append(found, path)
			case f.Kind() == protoreflect.MessageKind:
				walk(path+".", f.Message(), seen)
			}
		}
	}
	walk("", (&heronv1.GetSnapshotResponse{}).ProtoReflect().Descriptor(), map[protoreflect.FullName]bool{})
	slices.Sort(found)
	want := slices.Sorted(maps.Keys(audited))
	if !slices.Equal(found, want) {
		t.Fatalf("GetSnapshot string fields = %v, audited = %v", found, want)
	}
}

func gunzip(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gzip body: %v", err)
	}
	return out
}
