// Command releasesign 为 release 产物签名与验签。release 流水线在单独的 job 里运行它：那个 job 只编译本命令与
// internal/releasesig（只依赖标准库），私钥经环境变量只交给签名那一步。
//
//	releasesign sign   -version vX.Y.Z -dir dist   读 HERON_RELEASE_SIGNING_KEY，写 dist/SHA256SUMS.sig
//	releasesign verify -version vX.Y.Z -dir DIR    用 internal/releasesig 的受信公钥验 DIR 下的签名
package main

import (
	"crypto/ed25519"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/xjetry/heron/internal/releasesig"
)

func main() {
	if err := run(os.Args[1:], os.Getenv, releasesig.Trusted()); err != nil {
		fmt.Fprintln(os.Stderr, "releasesign:", err)
		os.Exit(1)
	}
}

const usage = "usage: releasesign sign|verify -version vX.Y.Z -dir DIR"

func run(args []string, getenv func(string) string, trusted []ed25519.PublicKey) error {
	if len(args) == 0 {
		return errors.New(usage)
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	version := fs.String("version", "", "release tag")
	dir := fs.String("dir", "", "directory holding SHA256SUMS")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *version == "" || *dir == "" || fs.NArg() != 0 {
		return errors.New(usage)
	}
	sums, err := readLimited(filepath.Join(*dir, "SHA256SUMS"), releasesig.MaxSums)
	if err != nil {
		return err
	}
	switch args[0] {
	case "sign":
		pemKey := getenv("HERON_RELEASE_SIGNING_KEY")
		if pemKey == "" {
			return errors.New("HERON_RELEASE_SIGNING_KEY is empty; refusing to publish an unsigned release")
		}
		key, err := releasesig.ParsePrivateKey([]byte(pemKey))
		if err != nil {
			return err
		}
		sig, err := releasesig.Sign(key, *version, sums)
		if err != nil {
			return err
		}
		// 私钥与仓库里的公钥不匹配时签出的文件没有任何更新器会接受：在写文件与发布之前失败。
		if err := releasesig.Verify(trusted, *version, sums, sig); err != nil {
			return fmt.Errorf("signing key does not match a trusted public key: %w", err)
		}
		return os.WriteFile(filepath.Join(*dir, "SHA256SUMS.sig"), sig, 0o644)
	case "verify":
		sig, err := readLimited(filepath.Join(*dir, "SHA256SUMS.sig"), releasesig.MaxFile)
		if err != nil {
			return err
		}
		return releasesig.Verify(trusted, *version, sums, sig)
	}
	return errors.New(usage)
}

func readLimited(path string, limit int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return b, nil
}
