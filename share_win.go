// Windows 网络共享：创建 / 枚举 SMB 共享、给文件夹授权、管理员检测
//
// 为什么不直接用 net.exe：它按控制台代码页输出（中文系统是 GBK），
// 解析和回显都要额外转码。NetShare* 这组 API 直接给宽字符，错误码也更干净。
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	netapi32          = windows.NewLazySystemDLL("netapi32.dll")
	pNetShareEnum     = netapi32.NewProc("NetShareEnum")
	pNetShareAdd      = netapi32.NewProc("NetShareAdd")
	pNetApiBufferFree = netapi32.NewProc("NetApiBufferFree")

	pIsUserAnAdmin = shell32.NewProc("IsUserAnAdmin")
	pShellExecute  = shell32.NewProc("ShellExecuteW")
)

const (
	maxPreferredLength = 0xFFFFFFFF // (DWORD)-1
	stypeSpecial       = 0x80000000 // ADMIN$ / C$ / IPC$ 这类隐藏共享

	// NetShareAdd/Enum 用的 SHARE_INFO_2
	shareInfoLevel = 2
)

// SHARE_INFO_2
type shareInfo2 struct {
	NetName     *uint16
	Type        uint32
	Remark      *uint16
	Permissions uint32
	MaxUses     uint32
	CurrentUses uint32
	Path        *uint16
	Passwd      *uint16
}

// 本机的一个 SMB 共享
type smbShare struct {
	Name   string `json:"name"`
	Path   string `json:"path"`
	Remark string `json:"remark"`
	UNC    string `json:"unc"`
	IsLan  bool   `json:"isLan"` // 名字或路径像「局域网聊天共享」
}

func utf16zPtr(p *uint16) string {
	if p == nil {
		return ""
	}
	return windows.UTF16ToString(unsafe.Slice(p, 4096))
}

// 当前进程有没有管理员权限（创建/删除共享需要）
func isElevated() bool {
	r, _, _ := pIsUserAnAdmin.Call()
	return r != 0
}

// SMB 那边给回来的路径是正斜杠，统一成反斜杠便于比较和展示
func normSharePath(p string) string {
	if p == "" {
		return ""
	}
	p = strings.ReplaceAll(strings.TrimSpace(p), "/", "\\")
	if len(p) > 2 && strings.HasSuffix(p, "\\") && p[1] != ':' {
		p = p[:len(p)-1]
	}
	return filepath.Clean(p)
}

func machineName() string {
	if h, err := os.Hostname(); err == nil && strings.TrimSpace(h) != "" {
		return h
	}
	return "本机"
}

// 枚举本机的磁盘共享（跳过 IPC$/ADMIN$/C$ 这些系统隐藏共享）
func listShares() []smbShare {
	out := []smbShare{}
	var buf *byte
	var read, total, resume uint32
	r, _, _ := pNetShareEnum.Call(0, shareInfoLevel,
		uintptr(unsafe.Pointer(&buf)), uintptr(maxPreferredLength),
		uintptr(unsafe.Pointer(&read)), uintptr(unsafe.Pointer(&total)),
		uintptr(unsafe.Pointer(&resume)))
	if r != 0 || read == 0 || buf == nil {
		return out
	}
	defer pNetApiBufferFree.Call(uintptr(unsafe.Pointer(buf)))

	host := machineName()
	size := unsafe.Sizeof(shareInfo2{})
	for i := uint32(0); i < read; i++ {
		e := (*shareInfo2)(unsafe.Pointer(uintptr(unsafe.Pointer(buf)) + uintptr(i)*size))
		name := utf16zPtr(e.NetName)
		if name == "" || e.Type&stypeSpecial != 0 || strings.HasSuffix(name, "$") {
			continue
		}
		path := normSharePath(utf16zPtr(e.Path))
		out = append(out, smbShare{
			Name:   name,
			Path:   path,
			Remark: utf16zPtr(e.Remark),
			UNC:    `\\` + host + `\` + name,
			IsLan:  strings.Contains(name, "聊天") || strings.Contains(path, "聊天"),
		})
	}
	return out
}

func findShare(name string) (smbShare, bool) {
	for _, s := range listShares() {
		if strings.EqualFold(s.Name, name) {
			return s, true
		}
	}
	return smbShare{}, false
}

// 找出覆盖 dir 的共享（包括它的上级目录被共享的情况），取最长匹配
func coverShare(dir string) (smbShare, string, bool) {
	dir = normSharePath(dir)
	best := smbShare{}
	bestRoot := ""
	for _, s := range listShares() {
		root := normSharePath(s.Path)
		if root == "" {
			continue
		}
		if strings.EqualFold(dir, root) {
			if len(root) > len(bestRoot) {
				best, bestRoot = s, root
			}
			continue
		}
		if strings.HasPrefix(strings.ToLower(dir), strings.ToLower(root)+`\`) {
			if len(root) > len(bestRoot) {
				best, bestRoot = s, root
			}
		}
	}
	if bestRoot == "" {
		return smbShare{}, "", false
	}
	rel := ""
	if !strings.EqualFold(dir, bestRoot) {
		rel = strings.TrimPrefix(strings.TrimPrefix(dir, bestRoot), `\`)
	}
	return best, rel, true
}

func addShare(path, name string) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	remark, err := windows.UTF16PtrFromString("局域网聊天共享文件夹")
	if err != nil {
		return err
	}
	si := shareInfo2{NetName: n, Type: 0, Remark: remark, Path: p}
	var parm uint32
	r, _, _ := pNetShareAdd.Call(0, shareInfoLevel,
		uintptr(unsafe.Pointer(&si)), uintptr(unsafe.Pointer(&parm)))
	if r == 0 {
		return nil
	}
	switch r {
	case 5:
		return fmt.Errorf("需要管理员权限才能创建共享")
	case 2118:
		return fmt.Errorf("共享名「%s」已被占用", name)
	case 53, 67, 2310:
		return fmt.Errorf("路径所在的磁盘/目录不可用")
	case 87:
		return fmt.Errorf("共享名或路径不合法")
	default:
		return fmt.Errorf("创建共享失败（错误码 %d）", r)
	}
}

// 给文件夹授予 Everyone 权限：读写 = 修改(M)，只读 = 读取和执行(RX)。
// 走 icacls 而不是 API：它会把继承标记 (OI)(CI) 一并写好，省事。
func grantFolderACL(dir string, readOnly bool) error {
	perm := "*S-1-1-0:(OI)(CI)M"
	if readOnly {
		perm = "*S-1-1-0:(OI)(CI)RX"
	}
	cmd := exec.Command("icacls", dir, "/grant", perm, "/T", "/C", "/Q")
	cmd.SysProcAttr = &windows.SysProcAttr{HideWindow: true}
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		if msg == "" {
			return fmt.Errorf("设置文件夹权限失败（%v）", err)
		}
		return fmt.Errorf("设置文件夹权限失败：%s", firstLine(msg))
	}
	return nil
}

func firstLine(s string) string {
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		return s[:i]
	}
	return s
}

// 共享名清洗：Windows 不允许这几个字符，也不能以空格结尾
func cleanShareName(s string) string {
	bad := `\/:*?"<>|`
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(bad, r) || r < 32 {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.Trim(strings.TrimSpace(b.String()), ".")
	if len([]rune(out)) > 60 {
		out = string([]rune(out)[:60])
	}
	return out
}

type shareSetup struct {
	ShareName string   `json:"shareName"`
	UNC       string   `json:"unc"`
	Reused    bool     `json:"reused"`  // 复用了已有的共享
	Created   bool     `json:"created"` // 本次新建了共享
	Admin     bool     `json:"admin"`
	Notes     []string `json:"notes"`
}

// 让 dir 变成「能在局域网里访问」：已经共享的就复用，否则新建一个专用共享
//
// 注意这里不删别人已有的共享：名字被占用时改成「名字2」「名字3」，
// 绝不去动用户原来共享出去的目录。
func ensureShareFor(dir string, wantName string, readOnly bool) (shareSetup, error) {
	dir = normSharePath(dir)
	res := shareSetup{Admin: isElevated(), Notes: []string{}}

	// 1) 这个目录本身已经有共享 → 直接复用
	for _, s := range listShares() {
		if strings.EqualFold(normSharePath(s.Path), dir) {
			res.Reused = true
			res.ShareName = s.Name
			res.UNC = s.UNC
			res.Notes = append(res.Notes, fmt.Sprintf("这个目录已经有共享「%s」，直接复用", s.Name))
			if err := grantFolderACL(dir, readOnly); err != nil {
				res.Notes = append(res.Notes, err.Error())
			}
			return res, nil
		}
	}

	// 2) 共享名被别人占了 → 换一个名字（不动别人的共享）
	name := cleanShareName(wantName)
	if name == "" {
		name = cleanShareName(filepath.Base(dir))
	}
	if name == "" {
		name = "LanChat"
	}
	if _, ok := findShare(name); ok {
		base := name
		for i := 2; i <= 20; i++ {
			cand := fmt.Sprintf("%s%d", base, i)
			if _, busy := findShare(cand); !busy {
				name = cand
				break
			}
		}
		res.Notes = append(res.Notes, fmt.Sprintf("共享名「%s」已被别的目录占用，改用「%s」", base, name))
	}

	// 3) 新建共享 + 授权
	if err := addShare(dir, name); err != nil {
		return res, err
	}
	res.Created = true
	res.ShareName = name
	res.UNC = `\\` + machineName() + `\` + name
	if err := grantFolderACL(dir, readOnly); err != nil {
		res.Notes = append(res.Notes, err.Error())
	}
	// 上级目录本来就在共享里的话，顺带告诉用户还有一条现成的路径
	if s, rel, ok := coverShare(dir); ok && rel != "" {
		res.Notes = append(res.Notes, fmt.Sprintf("上级目录在共享「%s」里，也可以走 \\\\%s\\%s\\%s",
			s.Name, machineName(), s.Name, rel))
	}
	return res, nil
}

// 以管理员身份重新打开自己（创建共享失败时的补救）
func relaunchAsAdmin() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	verb, err := windows.UTF16PtrFromString("runas")
	if err != nil {
		return err
	}
	file, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return err
	}
	dir, _ := windows.UTF16PtrFromString(filepath.Dir(exe))
	// SW_SHOWNORMAL = 1；返回值 > 32 表示成功
	r, _, _ := pShellExecute.Call(getOwner(), uintptr(unsafe.Pointer(verb)),
		uintptr(unsafe.Pointer(file)), 0, uintptr(unsafe.Pointer(dir)), 1)
	if r <= 32 {
		return fmt.Errorf("提升权限失败（错误码 %d），可能是被系统策略拦下了", r)
	}
	return nil
}
