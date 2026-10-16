package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"

	"github.com/xjetry/heron/internal/hub/store"
)

func runPasswd(args []string) error { return runPasswdWith(args, os.Stdin, os.Stderr) }

// runPasswdWith 只在 hub 主机上跑：没有经网络的首次设置页，也就没有"谁先访问谁
// 占有"的窗口。运行中的 hub 不需要重启——登录路径每次都读库，旧会话则在同一
// 事务里被清空。
func runPasswdWith(args []string, in *os.File, prompt io.Writer) error {
	fs := flag.NewFlagSet("passwd", flag.ContinueOnError)
	db := fs.String("db", "heron.db", "SQLite database path")
	if err := fs.Parse(args); err != nil {
		return err
	}
	plain, err := readPassword(in, prompt)
	if err != nil {
		return err
	}
	st, a, err := openOffline(*db, true)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := a.SetPassword(context.Background(), plain); err != nil {
		return err
	}
	fmt.Fprintln(prompt, "admin password set; every existing session has been revoked")
	var ask func() (bool, error)
	if term.IsTerminal(int(in.Fd())) {
		ask = func() (bool, error) {
			fmt.Fprint(prompt, "Revoke all API tokens as well? [y/N] ")
			line, err := bufio.NewReader(in).ReadString('\n')
			if err != nil && !errors.Is(err, io.EOF) {
				return false, err
			}
			answer := strings.ToLower(strings.TrimSpace(line))
			return answer == "y" || answer == "yes", nil
		}
	}
	return reviewAPITokens(context.Background(), st, prompt, ask, *db)
}

// reviewAPITokens 在改密后列出现存 API token。改密不连带吊销：连带吊销会让每次轮换密码都
// 静默打断自动化；代价是密码泄漏期间被创建的 token 仍然有效，所以把清单摆到改密的人面前。
// ask 为 nil 表示没有终端可问（管道、容器初始化），此时只列出并给出吊销命令，默认不吊销。
func reviewAPITokens(ctx context.Context, st *store.Store, w io.Writer, ask func() (bool, error), db string) error {
	list, err := st.ListAPITokens(ctx)
	if err != nil || len(list) == 0 {
		return err
	}
	fmt.Fprintln(w, "API tokens are not revoked by a password change. Existing tokens:")
	if err := printAPITokens(w, list, time.Now()); err != nil {
		return err
	}
	if ask == nil {
		// 提示会被人粘贴进 shell。路径按 POSIX 单引号引用：空格与元字符按字面处理，单引号本身不能留在引号内。
		fmt.Fprintf(w, "to revoke them: heron-hub token revoke --all --db %s\n", shellSingle(db))
		return nil
	}
	yes, err := ask()
	if err != nil || !yes {
		return err
	}
	n, err := st.DeleteAllAPITokens(ctx)
	if err != nil {
		return err
	}
	fmt.Fprintf(w, "revoked %d API tokens\n", n)
	return nil
}

// shellSingle 把路径嵌进 POSIX shell 命令。单引号内除单引号外都按字面处理；
// 单引号本身要先结束引号、接一个转义的单引号、再重新打开。
func shellSingle(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// errNoPasswordInput：stdin 不是终端，且在读到任何字节之前就结束了。docker exec 不带 -i 时，
// 容器里的 stdin 是 /dev/null，宿主上的管道根本没有接进来；这时报"密码太短"会让人去查一个
// 从来没输入过的密码，所以单独报，并写明该加的参数。
var errNoPasswordInput = errors.New("no password on stdin: it is not a terminal and ended before any input (with docker exec, add -i to pipe the password in, or -it to type it)")

// readPassword 在终端上不回显地读两遍并比对；stdin 不是终端时读一行——供容器
// 初始化与脚本使用，仍不经网络。
func readPassword(in *os.File, prompt io.Writer) (string, error) {
	fd := int(in.Fd())
	if !term.IsTerminal(fd) {
		line, err := bufio.NewReader(in).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", err
		}
		// ReadString 只在 EOF 之前一个字节都没读到时返回空串；空行至少带着 '\n'，
		// 那是输入了一个空密码，留给 SetPassword 按长度拒绝。
		if line == "" {
			return "", errNoPasswordInput
		}
		return strings.TrimRight(line, "\r\n"), nil
	}
	fmt.Fprint(prompt, "New admin password: ")
	first, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	fmt.Fprint(prompt, "Repeat: ")
	second, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	if err != nil {
		return "", err
	}
	if string(first) != string(second) {
		return "", errors.New("passwords do not match")
	}
	return string(first), nil
}
