package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"io"
	"os"
	"slices"
	"sync"
)

// Event 是中继与观察器写出的一行 JSON。各字段按 Ev 取用，其余留空：
//   - open：Peer 是接入方地址；
//   - data：Dir 方向写出 N 字节；
//   - rec：Dir 方向一条完整的 TLS 记录，Type 是记录类型，N 是含 5 字节记录头的长度；
//   - hello：Hello 是该方向第一条明文握手消息的摘要（ClientHello 或 ServerHello）；
//   - http：HTTP 是一次 HTTP/1.1 请求与应答（只在明文一侧观察）；
//   - close：连接两个方向都已结束，Up、Down 是合计；Err 非空表示连目标失败。
type Event struct {
	T     int64       `json:"t"`
	Ev    string      `json:"ev"`
	Conn  uint64      `json:"conn"`
	Peer  string      `json:"peer,omitempty"`
	Dir   string      `json:"dir,omitempty"`
	N     int64       `json:"n,omitempty"`
	Type  uint8       `json:"type,omitempty"`
	Up    int64       `json:"up,omitempty"`
	Down  int64       `json:"down,omitempty"`
	Err   string      `json:"err,omitempty"`
	Hello *HelloInfo  `json:"hello,omitempty"`
	HTTP  *HTTPRecord `json:"http,omitempty"`
}

// EventLog 串行写事件：中继每条连接两个 goroutine 加观察器同时写，一行必须完整落在一次写里。
// data、rec、hello 事件的时间戳是中继读到那段字节的时刻（调用方给出），其余事件在写入时取时刻。前者早于写日志，
// 负载下两个方向的日志写入先后与读到的先后可以不同，所以文件的行序不是时间序：读者按 T 稳定排序后再用
// （ReadEvents 已排好）。
type EventLog struct {
	mu sync.Mutex
	w  *bufio.Writer
	f  io.Closer
}

func NewEventLog(w io.Writer) *EventLog {
	l := &EventLog{w: bufio.NewWriter(w)}
	if c, ok := w.(io.Closer); ok && w != os.Stdout && w != os.Stderr {
		l.f = c
	}
	return l
}

func (l *EventLog) Write(e Event) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if e.T == 0 {
		e.T = clock().UnixNano()
	}
	b, err := json.Marshal(e)
	if err != nil {
		panic(err) // Event 只含可编码的字段
	}
	_, _ = l.w.Write(append(b, '\n'))
	// 每行立即落盘：中继被信号终止时日志里已有的行都是完整的，分析不必处理半行。
	_ = l.w.Flush()
}

func (l *EventLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	err := l.w.Flush()
	if l.f != nil {
		if cerr := l.f.Close(); err == nil {
			err = cerr
		}
	}
	return err
}

// ReadEvents 读回事件日志并按时间戳稳定排序；时间戳相同的事件保持写入顺序（同一段字节的 data 在它切出的
// rec 之前）。
func ReadEvents(r io.Reader) ([]Event, error) {
	var out []Event
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		var e Event
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	slices.SortStableFunc(out, func(a, b Event) int { return cmp.Compare(a.T, b.T) })
	return out, sc.Err()
}
