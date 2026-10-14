package main

import (
	"crypto/tls"
	"encoding/binary"
	"errors"
	"sync"
)

// TLS 记录层的类型（RFC 8446 第 5.1 节）。记录头在 TLS 1.3 里仍是明文：类型、版本、长度三项可数，
// 记录内容除最初的 ClientHello / ServerHello 外都已加密，TLS 1.3 的加密记录一律标为 application_data。
const (
	recChangeCipherSpec = 20
	recAlert            = 21
	recHandshake        = 22
	recApplicationData  = 23
	recHeaderLen        = 5
)

// HelloInfo 是一条明文 hello 的摘要。决定握手字节数的是密钥交换组（X25519MLKEM768 的密钥份额比 X25519 大
// 一千多字节）与证书链，二者前者在这里可见，后者在加密之后，只能从字节数看出。
type HelloInfo struct {
	Msg string `json:"msg"`
	// ClientHello：SNI、提供的 ALPN、key_share 里的组与各自份额字节数、supported_versions 的首选项。
	// ServerHello：选定的版本、密码套件、key_share 的组与份额字节数。ALPN 的选择在 TLS 1.3 里写在加密的
	// EncryptedExtensions 中，记录层看不到，协商结果以反代日志为准。
	SNI        string   `json:"sni,omitempty"`
	ALPN       []string `json:"alpn,omitempty"`
	Version    string   `json:"version,omitempty"`
	Cipher     string   `json:"cipher,omitempty"`
	KeyShares  []string `json:"key_shares,omitempty"`
	ShareBytes []int    `json:"share_bytes,omitempty"`
	Bytes      int      `json:"bytes"`
}

// helloRetryRandom 是 HelloRetryRequest 的固定 random（RFC 8446 第 4.1.3 节）：它与 ServerHello 同一消息类型，只能按 random 区分。
var helloRetryRandom = [32]byte{
	0xcf, 0x21, 0xad, 0x74, 0xe5, 0x9a, 0x61, 0x11, 0xbe, 0x1d, 0x8c, 0x02, 0x1e, 0x65, 0xb8, 0x91,
	0xc2, 0xa2, 0x11, 0x16, 0x7a, 0xbb, 0x8c, 0x5e, 0x07, 0x9e, 0x09, 0xe2, 0xc8, 0xa8, 0x33, 0x9c,
}

// recordParser 从一个方向的字节流里切出 TLS 记录。它只读记录头决定边界，不缓存加密内容；
// 只有该方向最初的明文握手记录会被收集，用来解析 hello。
type recordParser struct {
	hdr   [recHeaderLen]byte
	hn    int
	left  int
	typ   byte
	size  int
	hello []byte
	done  bool // hello 已解析或已放弃
	bad   bool // 流不是 TLS 记录：之后不再切分，避免把随机字节当长度
}

// feed 处理一段字节，对每条完整的记录调用 rec，对解析出的 hello 调用 hello。
func (p *recordParser) feed(b []byte, rec func(typ byte, n int), hello func(HelloInfo)) {
	for len(b) > 0 && !p.bad {
		if p.hn < recHeaderLen {
			k := copy(p.hdr[p.hn:], b)
			p.hn += k
			b = b[k:]
			if p.hn < recHeaderLen {
				return
			}
			p.typ = p.hdr[0]
			p.left = int(binary.BigEndian.Uint16(p.hdr[3:5]))
			p.size = recHeaderLen + p.left
			// 记录头的版本字段只取 0x0301–0x0304，类型只取 20–23；不符说明这一方向不是 TLS（或已错位）。
			if p.typ < recChangeCipherSpec || p.typ > recApplicationData || p.hdr[1] != 3 || p.hdr[2] > 4 {
				p.bad = true
				return
			}
		}
		k := min(p.left, len(b))
		if p.typ == recHandshake && !p.done {
			p.hello = append(p.hello, b[:k]...)
		}
		p.left -= k
		b = b[k:]
		if p.left == 0 {
			rec(p.typ, p.size)
			if p.typ == recHandshake && !p.done {
				if info, ok, err := parseHello(p.hello); err != nil {
					p.done = true
				} else if ok {
					info.Bytes = len(p.hello)
					hello(info)
					p.done = true
				}
			}
			p.hn = 0
		}
	}
}

var errShort = errors.New("short")

// parseHello 解析握手消息里的第一条 hello。ok 为假且无错误表示消息还没收全（跨记录），继续收集。
func parseHello(b []byte) (HelloInfo, bool, error) {
	if len(b) < 4 {
		return HelloInfo{}, false, nil
	}
	typ := b[0]
	n := int(b[1])<<16 | int(b[2])<<8 | int(b[3])
	if len(b) < 4+n {
		return HelloInfo{}, false, nil
	}
	body := reader(b[4 : 4+n])
	switch typ {
	case 1:
		info, err := parseClientHello(&body)
		return info, err == nil, err
	case 2:
		info, err := parseServerHello(&body)
		return info, err == nil, err
	}
	return HelloInfo{}, false, errors.New("not a hello")
}

type reader []byte

func (r *reader) bytes(n int) ([]byte, error) {
	if len(*r) < n {
		return nil, errShort
	}
	out := (*r)[:n]
	*r = (*r)[n:]
	return out, nil
}

func (r *reader) u8() (int, error) {
	b, err := r.bytes(1)
	if err != nil {
		return 0, err
	}
	return int(b[0]), nil
}

func (r *reader) u16() (int, error) {
	b, err := r.bytes(2)
	if err != nil {
		return 0, err
	}
	return int(binary.BigEndian.Uint16(b)), nil
}

// vec 读一个以 width 字节长度为前缀的向量。
func (r *reader) vec(width int) (reader, error) {
	var n int
	var err error
	switch width {
	case 1:
		n, err = r.u8()
	case 2:
		n, err = r.u16()
	}
	if err != nil {
		return nil, err
	}
	b, err := r.bytes(n)
	return reader(b), err
}

// helloExtensions 逐个交出扩展的类型与内容。
func helloExtensions(r *reader, each func(typ int, body reader) error) error {
	if len(*r) == 0 {
		return nil
	}
	exts, err := r.vec(2)
	if err != nil {
		return err
	}
	for len(exts) > 0 {
		typ, err := exts.u16()
		if err != nil {
			return err
		}
		body, err := exts.vec(2)
		if err != nil {
			return err
		}
		if err := each(typ, body); err != nil {
			return err
		}
	}
	return nil
}

const (
	extServerName        = 0
	extALPN              = 16
	extSupportedVersions = 43
	extKeyShare          = 51
)

func parseClientHello(r *reader) (HelloInfo, error) {
	info := HelloInfo{Msg: "client_hello"}
	if _, err := r.bytes(2 + 32); err != nil { // legacy_version, random
		return info, err
	}
	for _, width := range []int{1, 2, 1} { // legacy_session_id, cipher_suites, legacy_compression_methods
		if _, err := r.vec(width); err != nil {
			return info, err
		}
	}
	err := helloExtensions(r, func(typ int, body reader) error {
		switch typ {
		case extServerName:
			list, err := body.vec(2)
			if err != nil {
				return err
			}
			for len(list) > 0 {
				kind, err := list.u8()
				if err != nil {
					return err
				}
				name, err := list.vec(2)
				if err != nil {
					return err
				}
				if kind == 0 {
					info.SNI = string(name)
				}
			}
		case extALPN:
			list, err := body.vec(2)
			if err != nil {
				return err
			}
			for len(list) > 0 {
				proto, err := list.vec(1)
				if err != nil {
					return err
				}
				info.ALPN = append(info.ALPN, string(proto))
			}
		case extSupportedVersions:
			list, err := body.vec(1)
			if err != nil {
				return err
			}
			if v, err := list.u16(); err == nil {
				info.Version = tls.VersionName(uint16(v))
			}
		case extKeyShare:
			list, err := body.vec(2)
			if err != nil {
				return err
			}
			for len(list) > 0 {
				group, err := list.u16()
				if err != nil {
					return err
				}
				share, err := list.vec(2)
				if err != nil {
					return err
				}
				info.KeyShares = append(info.KeyShares, tls.CurveID(group).String())
				info.ShareBytes = append(info.ShareBytes, len(share))
			}
		}
		return nil
	})
	return info, err
}

func parseServerHello(r *reader) (HelloInfo, error) {
	info := HelloInfo{Msg: "server_hello"}
	ver, err := r.u16()
	if err != nil {
		return info, err
	}
	info.Version = tls.VersionName(uint16(ver))
	random, err := r.bytes(32)
	if err != nil {
		return info, err
	}
	if [32]byte(random) == helloRetryRandom {
		info.Msg = "hello_retry_request"
	}
	if _, err := r.vec(1); err != nil {
		return info, err
	}
	suite, err := r.u16()
	if err != nil {
		return info, err
	}
	info.Cipher = tls.CipherSuiteName(uint16(suite))
	if _, err := r.u8(); err != nil {
		return info, err
	}
	err = helloExtensions(r, func(typ int, body reader) error {
		switch typ {
		case extSupportedVersions:
			v, err := body.u16()
			if err != nil {
				return err
			}
			info.Version = tls.VersionName(uint16(v))
		case extKeyShare:
			group, err := body.u16()
			if err != nil {
				return err
			}
			info.KeyShares = []string{tls.CurveID(group).String()}
			// HelloRetryRequest 的 key_share 只有选定的组，没有份额。
			if share, err := body.vec(2); err == nil {
				info.ShareBytes = []int{len(share)}
			}
		}
		return nil
	})
	return info, err
}

// TLSObserver 把每条连接两个方向的字节切成 TLS 记录写进事件日志。
type TLSObserver struct {
	Log *EventLog

	mu      sync.Mutex
	parsers map[uint64]*[2]recordParser
}

func (o *TLSObserver) parser(conn uint64, dir string) *recordParser {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.parsers == nil {
		o.parsers = map[uint64]*[2]recordParser{}
	}
	ps, ok := o.parsers[conn]
	if !ok {
		ps = new([2]recordParser)
		o.parsers[conn] = ps
	}
	if dir == dirUp {
		return &ps[0]
	}
	return &ps[1]
}

// Data 切出的记录与 hello 记为读到这段字节的时刻：handshakeSplit 按时间比较两个方向的记录先后。
func (o *TLSObserver) Data(conn uint64, dir string, b []byte, read int64) {
	o.parser(conn, dir).feed(b,
		func(typ byte, n int) {
			o.Log.Write(Event{T: read, Ev: "rec", Conn: conn, Dir: dir, Type: typ, N: int64(n)})
		},
		func(h HelloInfo) { o.Log.Write(Event{T: read, Ev: "hello", Conn: conn, Dir: dir, Hello: &h}) })
}

func (o *TLSObserver) Closed(conn uint64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	delete(o.parsers, conn)
}
