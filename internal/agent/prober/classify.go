package prober

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"net"
	"syscall"
)

// classify 统一连接与发送失败口径：这一次没有联通是可达性事实，计入丢包；本地无法发起才是 error。
func classify(err error) Outcome {
	var timeout net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &timeout) && timeout.Timeout()) {
		return Outcome{Timeout: true}
	}
	for _, remote := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.ETIMEDOUT, syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EHOSTDOWN} {
		if errors.Is(err, remote) {
			return Outcome{Timeout: true}
		}
	}
	// TLS 握手失败（含证书错误：过期、名字不符、自签）是"这个端点此刻给不出一次有效握手"，计入丢包。
	// 对端只回 alert 的握手失败（协议版本不兼容、无共同密码套件、SNI 不被接受）没有可判的类型：
	// crypto/tls 的 alert 类型未导出，Go 把它包成 *net.OpError{Op: "remote error"}，Op 是唯一稳定的
	// 判据；同属"服务在、握手给不出"的可达性事实，与证书错误同归丢包。OpError 的其他 Op（dial、
	// read 等本机与链路失败）不匹配这条，仍落各自的归类。
	var unknownAuthority x509.UnknownAuthorityError
	var hostnameMismatch x509.HostnameError
	var certInvalid x509.CertificateInvalidError
	var certVerify *tls.CertificateVerificationError
	// pinError 同属这一类：钉住校验失败（指纹不符、不在有效期）也是握手给不出，计入丢包。
	var pin *pinError
	var notTLS tls.RecordHeaderError
	if errors.As(err, &unknownAuthority) || errors.As(err, &hostnameMismatch) || errors.As(err, &certInvalid) || errors.As(err, &certVerify) || errors.As(err, &pin) || errors.As(err, &notTLS) {
		return Outcome{Timeout: true}
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "remote error" {
		return Outcome{Timeout: true}
	}
	return Outcome{Err: err.Error()}
}
