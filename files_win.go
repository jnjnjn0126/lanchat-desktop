// 路径地址、复制、剪贴板这些杂活（v3.0）
//
// 为什么单独一个文件：
//   · 「共享文件夹的地址」要能从各种路径形态推出来（本机目录 / 映射网络盘 Z: / 直接 UNC）
//   · 「文件夹内打开」要能在共享文件夹不在本机时也打得开
//   · 文件气泡的「复制」= 把文件真放进 Windows 剪贴板（CF_HDROP）
//
// 这些都跟 HTTP 接口无关，main.go 里只留薄薄一层 handler。
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

/* ===================== 共享文件夹的「地址」 =====================
   同一个文件夹，在两台电脑上的写法可能完全不同：
     台式机上：D:\共享
     笔记本上：Z:\            （映射网络盘）
     或者直接：\\Xin-desktop\共享
   聊天里要显示给对方看的地址，必须统一成 \\主机\共享名\子目录 这种，
   这样对面、以及没做映射的电脑都能直接粘到资源管理器里打开。 */

func isUNCPath(p string) bool {
	p = strings.TrimSpace(p)
	return strings.HasPrefix(p, `\\`) || strings.HasPrefix(p, `//`)
}

var (
	mprDLL                   = windows.NewLazySystemDLL("mpr.dll")
	procWNetGetUniversalName = mprDLL.NewProc("WNetGetUniversalNameW")
)

// UNIVERSAL_NAME_INFOW { LPWSTR lpUniversalName; } 后面紧跟字符串本体
type universalNameInfoW struct {
	lpUniversalName *uint16
	buf             [1024]uint16
}

// 把映射网络盘（Z:\a\b）换成共享地址（\\主机\共享名\a\b）。
// 不是映射盘、或者这个盘根本没映射到共享时返回空串。
func driveUNC(p string) string {
	p = strings.TrimSpace(p)
	if len(p) < 2 || p[1] != ':' {
		return ""
	}
	drive, err := windows.UTF16PtrFromString(p[:2] + `\`)
	if err != nil {
		return ""
	}
	var uni universalNameInfoW
	n := uint32(unsafe.Sizeof(uni))
	r, _, _ := procWNetGetUniversalName.Call(
		uintptr(unsafe.Pointer(drive)),
		1, // UNIVERSAL_NAME_INFO_LEVEL
		uintptr(unsafe.Pointer(&uni)),
		uintptr(unsafe.Pointer(&n)))
	if r != 0 || uni.lpUniversalName == nil {
		return ""
	}
	root := utf16zPtr(uni.lpUniversalName)
	if root == "" {
		return ""
	}
	// Z:\a\b → <共享地址>\a\b；Z:\ 本身不要留尾巴
	rest := strings.TrimRight(p[2:], `\`)
	return strings.TrimRight(root, `\`) + rest
}

// dirAddress 返回 dir 的共享地址和类型：
//
//	unc   dir 本身就是 \\主机\共享名...
//	map   dir 在本机是一块映射网络盘（Z:），地址来自这块盘的映射目标
//	local dir 在本机磁盘上（可能被本机某个共享覆盖，也可能没共享）
//
// 地址推不出来时返回空串，调用方退回本机路径显示。
func dirAddress(dir string) (string, string) {
	dir = normSharePath(dir)
	if dir == "" {
		return "", ""
	}
	if isUNCPath(dir) {
		return dir, "unc"
	}
	if u := driveUNC(dir); u != "" {
		return u, "map"
	}
	if s, rel, ok := coverShare(dir); ok {
		u := s.UNC
		if rel != "" {
			u += `\` + rel
		}
		return u, "local"
	}
	return "", "local"
}

/* ===================== 打开资源管理器 =====================
   远程共享最坑的一点：映射盘（Z:）只在「当前登录会话」里存在。
   程序万一以管理员身份跑起来，或者在别人的电脑上打开，Z: 可能根本不存在，
   这时要把路径换成它的 UNC 地址再打开，而不是直接报错。 */

func resolveOpenPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	if !filepath.IsAbs(p) {
		abs, err := filepath.Abs(p)
		if err != nil {
			return "", err
		}
		p = abs
	}
	p = filepath.Clean(p)
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	// 映射盘 → 共享地址
	if u := driveUNC(p); u != "" && !strings.EqualFold(u, p) {
		if _, err := os.Stat(u); err == nil {
			return u, nil
		}
	}
	// 共享地址 → 本机映射盘（有些电脑只认盘符，反着也试一把）
	if isUNCPath(p) {
		if local := uncToDrive(p); local != "" {
			if _, err := os.Stat(local); err == nil {
				return local, nil
			}
		}
	}
	return "", fmt.Errorf("打不开这个位置：%s 现在访问不了（网络盘可能没连上）", p)
}

// 共享地址 → 本机映射盘符（\\主机\共享名\a → Z:\a）。
// 只查本机已有的映射，不去连网。
func uncToDrive(p string) string {
	parts := strings.SplitN(strings.TrimPrefix(p, `\\`), `\`, 3)
	if len(parts) < 2 {
		return ""
	}
	share := `\\` + parts[0] + `\` + parts[1]
	rest := ""
	if len(parts) == 3 {
		rest = `\` + parts[2]
	}
	for _, d := range driveList() {
		if strings.EqualFold(strings.TrimRight(driveUNC(d), `\`), strings.TrimRight(share, `\`)) {
			return d + rest
		}
	}
	return ""
}

// 本机现有的盘符（C: D: Z: …）
func driveList() []string {
	out := []string{}
	for c := 'A'; c <= 'Z'; c++ {
		d := string(c) + `:\`
		if _, err := os.Stat(d); err == nil {
			out = append(out, d)
		}
	}
	return out
}

/* ===================== 重名处理 =====================
   往共享文件夹里放东西时不能覆盖别人的同名文件：重名就自动加 (2)、(3)… */

func uniquePath(parent, name string, isDir bool) string {
	name = strings.TrimSpace(name)
	if name == "" {
		name = "未命名"
	}
	base, ext := name, ""
	if !isDir {
		ext = filepath.Ext(name)
		base = strings.TrimSuffix(name, ext)
		if base == "" {
			base, ext = name, ""
		}
	}
	p := filepath.Join(parent, name)
	for i := 2; i <= 999; i++ {
		if _, err := os.Stat(p); err != nil {
			return p
		}
		p = filepath.Join(parent, fmt.Sprintf("%s (%d)%s", base, i, ext))
	}
	return filepath.Join(parent, fmt.Sprintf("%s (%d)%s", base, time.Now().Unix()%1000, ext))
}

/* ===================== 复制（另存为用） ===================== */

// copyAny 把 src（文件或整个文件夹）复制进 dstDir，重名自动改名。
// 返回最终路径、文件数、字节数。
func copyAny(src, dstDir string) (string, int, int64, error) {
	return copyAnyAs(src, dstDir, "")
}

// copyAnyAs 同上，另外可以指定存成什么名字（wantName 为空就用原来的名字）
func copyAnyAs(src, dstDir, wantName string) (string, int, int64, error) {
	src = filepath.Clean(strings.TrimSpace(src))
	dstDir = filepath.Clean(strings.TrimSpace(dstDir))
	if src == "" || dstDir == "" {
		return "", 0, 0, fmt.Errorf("路径为空")
	}
	st, err := os.Stat(src)
	if err != nil {
		return "", 0, 0, fmt.Errorf("源文件找不到了")
	}
	if st.IsDir() && strings.HasPrefix(strings.ToLower(dstDir+`\`), strings.ToLower(src+`\`)) {
		return "", 0, 0, fmt.Errorf("不能复制到它自己里面")
	}
	name := filepath.Base(src)
	if n := strings.TrimSpace(wantName); n != "" {
		name = filepath.Base(n) // 只取文件名，别把路径带进来
	}
	dst := uniquePath(dstDir, name, st.IsDir())
	if st.IsDir() {
		n, b, err := copyTree(src, dst)
		if err != nil {
			return "", n, b, err
		}
		return dst, n, b, nil
	}
	if err := copyFileTo(src, dst); err != nil {
		return "", 0, 0, err
	}
	return dst, 1, st.Size(), nil
}

func copyTree(src, dst string) (int, int64, error) {
	files, bytes := 0, int64(0)
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, rerr := filepath.Rel(src, p)
		if rerr != nil {
			return rerr
		}
		target := filepath.Join(dst, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if !info.Mode().IsRegular() {
			return nil // 跳过符号链接之类的怪东西
		}
		if err := copyFileTo(p, target); err != nil {
			return err
		}
		files++
		bytes += info.Size()
		return nil
	})
	return files, bytes, err
}

func copyFileTo(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}

/* ===================== 剪贴板：放「文件」而不是文字 =====================
   文件气泡上的「复制」要能直接粘到资源管理器 / 微信 / Word，
   需要往剪贴板写 CF_HDROP（就是拖拽文件时用的那种格式）。 */

var (
	user32DLL             = windows.NewLazySystemDLL("user32.dll")
	kernel32DLL           = windows.NewLazySystemDLL("kernel32.dll")
	procOpenClipboard     = user32DLL.NewProc("OpenClipboard")
	procCloseClipboard    = user32DLL.NewProc("CloseClipboard")
	procEmptyClipboard    = user32DLL.NewProc("EmptyClipboard")
	procSetClipboardData  = user32DLL.NewProc("SetClipboardData")
	procGlobalAlloc       = kernel32DLL.NewProc("GlobalAlloc")
	procGlobalLock        = kernel32DLL.NewProc("GlobalLock")
	procGlobalUnlock      = kernel32DLL.NewProc("GlobalUnlock")
	procGlobalFree        = kernel32DLL.NewProc("GlobalFree")
	procShellExecute      = shell32.NewProc("ShellExecuteW")
)

const (
	cfHDrop       = 15
	gmemMoveable  = 0x0002
	dropFilesSize = 20 // DROPFILES: DWORD pFiles + POINT(8) + BOOL fNC + BOOL fWide
)

// 把若干文件/文件夹放进剪贴板（相当于在资源管理器里按了 Ctrl+C）
func setClipboardFiles(paths []string) error {
	if len(paths) == 0 {
		return fmt.Errorf("没有文件")
	}
	var body []uint16
	for _, p := range paths {
		body = append(body, windows.StringToUTF16(p)...)
	}
	body = append(body, 0) // 双 \0 结尾
	total := dropFilesSize + len(body)*2

	h, _, e := procGlobalAlloc.Call(gmemMoveable, uintptr(total))
	if h == 0 {
		return fmt.Errorf("内存分配失败：%v", e)
	}
	ptr, _, _ := procGlobalLock.Call(h)
	if ptr == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("内存锁定失败")
	}
	hdr := unsafe.Slice((*byte)(unsafe.Pointer(ptr)), total)
	for i := 0; i < dropFilesSize; i++ {
		hdr[i] = 0
	}
	*(*uint32)(unsafe.Pointer(&hdr[0])) = dropFilesSize // pFiles：文件列表的偏移
	*(*uint32)(unsafe.Pointer(&hdr[16])) = 1           // fWide = TRUE（UTF-16）
	dst := unsafe.Slice((*uint16)(unsafe.Pointer(ptr+uintptr(dropFilesSize))), len(body))
	copy(dst, body)
	procGlobalUnlock.Call(h)

	// 剪贴板可能正被别的程序占着，重试几次
	opened := false
	for i := 0; i < 10; i++ {
		if r, _, _ := procOpenClipboard.Call(getOwner()); r != 0 {
			opened = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !opened {
		procGlobalFree.Call(h)
		return fmt.Errorf("剪贴板正被其他程序占用，稍后再试")
	}
	defer procCloseClipboard.Call()

	procEmptyClipboard.Call()
	if r, _, e2 := procSetClipboardData.Call(cfHDrop, h); r == 0 {
		procGlobalFree.Call(h)
		return fmt.Errorf("写入剪贴板失败：%v", e2)
	}
	// 成功后这块内存归系统管，不能自己释放
	return nil
}

/* ===================== 用 ShellExecute 打开（兜底） ===================== */

func shellOpen(path string) error {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	verb, _ := windows.UTF16PtrFromString("open")
	r, _, _ := procShellExecute.Call(0, uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(p)), 0, 0, 1 /* SW_SHOWNORMAL */)
	if r <= 32 {
		return fmt.Errorf("打开失败（code %d）", r)
	}
	return nil
}
