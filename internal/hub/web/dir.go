package web

import (
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"syscall"
)

// DirHandler 服务运维用 --public-dir 放置的替换公开页。
//
// 文件经 os.Root 打开：路径与符号链接都越不出 dir，越界即打开失败，按不存在处理。符号链接只在是相对链接、
// 且解析过程中一步都不走出 dir 时才跟随；绝对链接一律拒绝，即使指向 dir 之内（os.Root："Symbolic links must not
// be absolute"），先绕出再绕回的相对链接同样拒绝。每个请求重新打开 dir：运维原子替换目录（rename）之后，
// 下一个请求就读到新内容。打开带 O_NONBLOCK，这是 opener 的契约：目录里的 FIFO 在没有写端时不会让 open 挂住，
// 随后的普通文件检查把它当作不存在；对普通文件的读取它不起作用。
// index.html 在构造时核对，必须是目录内的普通文件：它回答每一个不是文件的路径，缺了它 hub 不启动。
//
// 这个目录与面板同源，是信任边界之内的内容：目录里的脚本能读 /admin/，也能带着来访管理员的会话 cookie 调
// AdminService 的任意方法——同源请求照常带 cookie，SameSite=Strict 不拦同源。只放与 hub 二进制同等可信的内容；
// 不受信任的主题必须经主题安装与沙箱托管，使用 SDK 取公开数据。
func DirHandler(dir string) (http.Handler, error) {
	open := func(rel string) (fs.File, error) {
		root, err := os.OpenRoot(dir)
		if err != nil {
			return nil, err
		}
		defer root.Close()
		return root.OpenFile(filepath.FromSlash(rel), os.O_RDONLY|syscall.O_NONBLOCK, 0)
	}
	// 目录不存在、缺 index.html、index.html 是越界或绝对的链接、不是普通文件，都违反同一条约束，报错写约束本身再附底层原因。
	refuse := func(cause error) error {
		return fmt.Errorf("--public-dir %s: index.html must be a regular file inside the directory (it answers every path that is not a file): %w", dir, cause)
	}
	f, err := open("index.html")
	if err != nil {
		return nil, refuse(err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, refuse(err)
	}
	if !info.Mode().IsRegular() {
		return nil, refuse(fmt.Errorf("it has mode %v", info.Mode()))
	}
	return serveFiles("/", customHeaders, func(string) string { return "no-cache" }, open), nil
}

// customHeaders 仅用于部署者提供的可信目录，不是主题包策略；目录脚本具有面板同源权限。
// 文件名不保证带内容哈希，所以一律 no-cache，404 也一样。
func customHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	h.Set("Cache-Control", "no-cache")
}
