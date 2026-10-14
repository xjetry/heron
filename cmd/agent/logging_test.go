package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"

	heronv1 "github.com/xjetry/heron/gen/heron/v1"
	"github.com/xjetry/heron/internal/agent/agentlog"
	"github.com/xjetry/heron/internal/agentwire"
	"github.com/xjetry/heron/internal/hubclient"
	"github.com/xjetry/heron/internal/testwait"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}
func (s *syncBuffer) String() string { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

// hub 在一个合法响应之后往 idle 连接上多送字节时，net/http 用标准库 log 写出这些字节。run 的日志装配必须把它接进
// agentlog 的出口（限长、限速），而不是任它直接写 stderr。
func TestStandardLogGoesThroughTheBoundedExit(t *testing.T) {
	prevDefault, prevFlags, prevOut := slog.Default(), log.Flags(), log.Writer()
	t.Cleanup(func() { slog.SetDefault(prevDefault); log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	var out syncBuffer
	agentlog.Install(newHandler(&out))

	body, err := proto.Marshal(&heronv1.ReportResponse{ReportIntervalMs: 10000})
	if err != nil {
		t.Fatal(err)
	}
	junk := "HUB_CONTROLLED_" + strings.Repeat("x", 4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: application/proto\r\nContent-Length: %d\r\n\r\n", len(body))
		rw.Write(body)
		rw.Flush()
		// 等客户端把连接放回 idle 再送多余字节，Transport 的 readLoop 才会走到"idle 连接上的意外响应"那条日志。
		time.Sleep(50 * time.Millisecond)
		rw.WriteString(junk)
		rw.Flush()
		bufio.NewReader(conn).ReadByte()
		conn.Close()
	}))
	defer srv.Close()

	req := connect.NewRequest(&heronv1.ReportRequest{})
	if _, err := hubclient.New(srv.URL, 5*time.Second, agentwire.MaxResponseBytes).Report(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	testwait.Until(t, 5*time.Millisecond, func() bool { return strings.Contains(out.String(), "HUB_CONTROLLED_") },
		"the standard-library log line never reached the bounded exit; output: %s", testwait.When(out.String))
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if len(line) > 4*agentlog.MaxValueLen {
			t.Fatalf("line of %d bytes escaped the value bound: %.200s", len(line), line)
		}
	}
}

// 生产入口本身接管标准库 log，而且在加载配置之前：上面的测试自己调用 Install，删掉 runRun 里的那一行它照样通过。
// 这里给一个读不到的配置让 runRun 在第一步就返回，接管必须已经发生。
func TestRunInstallsTheBoundedExitFirst(t *testing.T) {
	prevDefault, prevFlags, prevOut := slog.Default(), log.Flags(), log.Writer()
	t.Cleanup(func() { slog.SetDefault(prevDefault); log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := runRun([]string{"--config", filepath.Join(t.TempDir(), "missing.json")}); err == nil {
		t.Fatal("runRun accepted a missing config")
	}
	if _, ok := slog.Default().Handler().(*agentlog.Handler); !ok {
		t.Fatalf("after runRun the default handler is %T, want *agentlog.Handler: the standard-library log would bypass the bound", slog.Default().Handler())
	}
}
