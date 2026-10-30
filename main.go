// 局域网聊天 · 原生版（Go + WebView2）
//
// 与 v1.6 网页版的区别：
//
//	· 文件读写由本程序完成（真实磁盘路径），不再依赖浏览器的 File System Access API
//	· 文件夹选择用 Windows 原生对话框，不需要「记住路径」「自定义协议」那一套
//	· 「打开文件夹」直接调 explorer.exe /select，不需要注册表
//
// 前端通过 /api/* 访问本机 127.0.0.1 端口，页面代码与网页版基本一致。
package main

import (
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

//go:embed web/index.html
var webFS embed.FS

const appVersion = "3.3"
const shareFolderName = "局域网聊天共享"

/* ===================== 配置 ===================== */

// 用过的共享文件夹（「打开已有」菜单里按最近使用排序）
type recentEntry struct {
	Path string `json:"path"`
	Name string `json:"name"`
	TS   int64  `json:"ts"`
}

type config struct {
	Root string `json:"root"` // 上次使用的共享文件夹（绝对路径）
	// 开机自启：以注册表 HKCU\...\Run 的实际值为准，这里只做记录
	Autostart bool `json:"autostart"`
	// 点 ✕ 的行为：tray = 收进托盘（默认），exit = 直接退出
	CloseAction string `json:"closeAction"`
	// 最近用过的共享文件夹（最多 8 条，最近的在前面）
	Recents []recentEntry `json:"recents"`
	// 侧边栏「本机的网络共享」那一组里被手动删掉的共享路径（小写、反斜杠归一化）。
	// 只是不再列出来，Windows 那边的共享和磁盘上的文件一概不动；
	// 以后再打开这个文件夹（进了「最近打开」）会自动把这条隐藏记录去掉。
	MutedShares []string `json:"mutedShares,omitempty"`
	// 上次用的文件夹被从「文件夹列表」里删掉了：启动时别再自动进去，
	// 但 Root 还要留着当路径校验的根，不然文件操作的沙箱就没了
	NoResume bool `json:"noResume"`
	// 以管理员身份重启前记下的待办：启动时自动把共享补上
	Pending *pendingShare `json:"pending,omitempty"`
	// 上次补做共享的结果（给页面提示用）
	PendingMsg string `json:"pendingMsg,omitempty"`
	// 本机身份（v3.1 起）。以前昵称和 peerId 存在页面的 localStorage 里，
	// 但每次启动监听的端口都是随机的，页面 origin 一变 localStorage 就空了，
	// 于是昵称被重置、peerId 也换了一个 —— 老消息因此被认成「别人发的」。
	// 现在存在配置里：重启不丢，换共享文件夹也跟着走。
	Profile profileEntry `json:"profile"`
}

// 本机身份：昵称 + 稳定的 peerId。
// peerId 只是这台机器在聊天里的标识，不参与任何加密，随机生成一次就固定下来。
type profileEntry struct {
	Name   string `json:"name,omitempty"`
	PeerID string `json:"peerId,omitempty"`
}

type pendingShare struct {
	Path string `json:"path"`
	Name string `json:"name"`
	Perm string `json:"perm"` // read / full
}

var (
	cfgMu   sync.RWMutex
	cfg     config
	cfgFile string
)

func initConfig() {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	base := filepath.Join(dir, "LanChat")
	_ = os.MkdirAll(base, 0o755)
	cfgFile = filepath.Join(base, "config.json")
	if b, err := os.ReadFile(cfgFile); err == nil {
		_ = json.Unmarshal(b, &cfg)
	}
	ensureProfile()
}

// ensureProfile 保证本机有一个稳定的 peerId。
// v3.1 之前这个值只存在页面 localStorage 里，端口一变就丢，所以老用户的
// peerId 已经找不回来了 —— 这里生成一个新的，之后就一直用它。
func ensureProfile() {
	cfgMu.Lock()
	changed := false
	if strings.TrimSpace(cfg.Profile.PeerID) == "" {
		cfg.Profile.PeerID = newPeerID()
		changed = true
	}
	if n := strings.TrimSpace(cfg.Profile.Name); n != "" && n != cfg.Profile.Name {
		cfg.Profile.Name = n
		changed = true
	}
	cfgMu.Unlock()
	if changed {
		_ = saveConfig()
	}
}

// newPeerID 32 位十六进制，跟页面以前生成的 uuid 用法一致（只要求唯一）
func newPeerID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%016x%016x", time.Now().UnixNano(), os.Getpid())
	}
	return hex.EncodeToString(b)
}

// myNick 返回页面该显示的昵称：用户设过就用它，否则用「电脑-XXXX」
func myNick() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	if n := strings.TrimSpace(cfg.Profile.Name); n != "" {
		return n
	}
	id := cfg.Profile.PeerID
	if len(id) >= 4 {
		return "电脑-" + strings.ToUpper(id[:4])
	}
	return "电脑"
}

func saveConfig() error {
	b, _ := json.MarshalIndent(cfg, "", "  ")
	return os.WriteFile(cfgFile, b, 0o644)
}

func rootDir() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.Root
}

// 记一笔「用过这个文件夹」：去重、置顶、最多留 8 条
func pushRecent(path string) {
	path = filepath.Clean(strings.TrimSpace(path))
	if path == "" || path == "." {
		return
	}
	name := pathName(path)
	cfgMu.Lock()
	list := []recentEntry{{Path: path, Name: name, TS: time.Now().Unix()}}
	for _, it := range cfg.Recents {
		if !strings.EqualFold(filepath.Clean(it.Path), path) {
			list = append(list, it)
		}
	}
	if len(list) > 8 {
		list = list[:8]
	}
	cfg.Recents = list
	// 这个路径以前被从「本机的网络共享」里删过 → 现在主动打开它，就把隐藏记录去掉
	cfg.MutedShares = dropPath(cfg.MutedShares, path)
	cfgMu.Unlock()
	_ = saveConfig()
}

// 取路径的最后一段。filepath.Base 对 UNC 路径（\\主机\共享名）会返回奇怪的值，
// 侧边栏就会显示成一个「\\」，所以这里自己按两种分隔符切。
func pathName(p string) string {
	p = strings.TrimRight(strings.TrimSpace(p), `\/`)
	if p == "" {
		return ""
	}
	if i := strings.LastIndexAny(p, `\/`); i >= 0 {
		return p[i+1:]
	}
	return p
}

// 共享路径的比较键：反斜杠归一化 + 小写
func shareKey(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	return strings.ToLower(normSharePath(p))
}

func dropPath(list []string, path string) []string {
	k := shareKey(path)
	if k == "" {
		return list
	}
	out := make([]string, 0, len(list))
	for _, it := range list {
		if it != k {
			out = append(out, it)
		}
	}
	return out
}

func shareMuted(path string) bool {
	k := shareKey(path)
	if k == "" {
		return false
	}
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	for _, it := range cfg.MutedShares {
		if it == k {
			return true
		}
	}
	return false
}

/* ===================== 路径校验 ===================== */

func cleanPath(p string, mustExist bool) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", fmt.Errorf("路径为空")
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	root := rootDir()
	if root != "" {
		rel, err := filepath.Rel(root, abs)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
			return "", fmt.Errorf("路径不在共享文件夹内")
		}
	}
	if mustExist {
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("路径不存在")
		}
	}
	return abs, nil
}

/* ===================== Windows 文件夹选择框 ===================== */

var (
	shell32               = windows.NewLazySystemDLL("shell32.dll")
	ole32                 = windows.NewLazySystemDLL("ole32.dll")
	procSHBrowseForFolder = shell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromIDL  = shell32.NewProc("SHGetPathFromIDListW")
	procCoTaskMemFree     = ole32.NewProc("CoTaskMemFree")
	procCoInitializeEx    = ole32.NewProc("CoInitializeEx")
	procCoUninitialize    = ole32.NewProc("CoUninitialize")
)

// BROWSEINFOW
type browseInfo struct {
	hwndOwner      uintptr
	pidlRoot       uintptr
	pszDisplayName *uint16
	lpszTitle      *uint16
	ulFlags        uint32
	lpfn           uintptr
	lParam         uintptr
	iImage         int32
}

// 弹出系统文件夹选择框；用户取消时返回 ok=false
func pickFolder(owner uintptr, title string) (path string, ok bool) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	// 该 API 需要 COM（STA），HTTP 处理 goroutine 默认没有初始化
	procCoInitializeEx.Call(0, 2 /* COINIT_APARTMENTTHREADED */)
	defer procCoUninitialize.Call()

	buf := make([]uint16, windows.MAX_PATH)
	t, err := windows.UTF16PtrFromString(title)
	if err != nil {
		return "", false
	}
	bi := browseInfo{
		hwndOwner:      owner,
		pszDisplayName: &buf[0],
		lpszTitle:      t,
		// BIF_RETURNONLYFSDIRS | BIF_EDITBOX | BIF_NEWDIALOGSTYLE
		ulFlags: 0x0001 | 0x0010 | 0x0040,
	}
	pidl, _, _ := procSHBrowseForFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return "", false // 用户取消
	}
	defer procCoTaskMemFree.Call(pidl)

	out := make([]uint16, windows.MAX_PATH)
	r, _, _ := procSHGetPathFromIDL.Call(pidl, uintptr(unsafe.Pointer(&out[0])))
	if r == 0 {
		return "", false
	}
	return windows.UTF16ToString(out), true
}

/* ===================== HTTP API ===================== */

var (
	ownerHWND  uintptr
	ownerHWNDL sync.RWMutex
)

func setOwner(h uintptr) {
	ownerHWNDL.Lock()
	ownerHWND = h
	ownerHWNDL.Unlock()
}

func getOwner() uintptr {
	ownerHWNDL.RLock()
	defer ownerHWNDL.RUnlock()
	return ownerHWND
}

func writeJSON(w http.ResponseWriter, v interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, msg)
}

func readBodyJSON(r *http.Request, v interface{}) error {
	defer r.Body.Close()
	b, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return err
	}
	if len(b) == 0 {
		return nil
	}
	return json.Unmarshal(b, v)
}

// 1x1 透明 PNG：必须给浏览器一个「合法图片」，否则某些 WebView2 版本在
// 处理 favicon 404 时会直接把浏览器进程搞崩（表现为窗口白屏、页面卡住）。
var faviconPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D,
	0x49, 0x48, 0x44, 0x52, 0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4, 0x89, 0x00, 0x00, 0x00,
	0x0A, 0x49, 0x44, 0x41, 0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00, 0x00, 0x00, 0x00, 0x49,
	0x45, 0x4E, 0x44, 0xAE, 0x42, 0x60, 0x82,
}

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(faviconPNG)
}

func handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	b, err := webFS.ReadFile("web/index.html")
	if err != nil {
		writeErr(w, 500, "页面缺失")
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(b)
}

func handleInfo(w http.ResponseWriter, r *http.Request) {
	touchPage()
	cfgMu.RLock()
	noResume := cfg.NoResume
	peerID := cfg.Profile.PeerID
	cfgMu.RUnlock()
	root := rootDir()
	name := ""
	if root != "" {
		name = pathName(root)
		if _, err := os.Stat(root); err != nil {
			root, name = "", ""
		}
		// 用户把这个文件夹从列表里删掉了：不再自动进、也不显示「继续上次」
		if noResume {
			root, name = "", ""
		}
	}
	writeJSON(w, map[string]interface{}{
		"native":  true,
		"version": appVersion,
		"root":    root,
		"name":    name,
		"admin":   isElevated(),
		"host":    machineName(),
		"peerId":  peerID, // v3.1：本机身份，页面不再自己生成
		"nick":    myNick(),
		"notice":  takePendingMsg(), // 管理员重启后补做共享的结果
	})
}

// /api/profile  改昵称。存进配置里，重启和换文件夹都不会丢。
func handleProfile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, 400, "参数不对")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeErr(w, 400, "昵称不能为空")
		return
	}
	if len([]rune(name)) > 16 {
		name = string([]rune(name)[:16])
	}
	cfgMu.Lock()
	cfg.Profile.Name = name
	cfgMu.Unlock()
	if err := saveConfig(); err != nil {
		writeErr(w, 500, "保存失败："+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok":     true,
		"name":   name,
		"peerId": myPeerID(),
	})
}

func myPeerID() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.Profile.PeerID
}

func handlePickFolder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Title string `json:"title"`
	}
	_ = readBodyJSON(r, &req)
	title := req.Title
	if title == "" {
		title = "选择共享文件夹"
	}
	p, ok := pickFolder(getOwner(), title)
	if !ok {
		writeJSON(w, map[string]interface{}{"canceled": true})
		return
	}
	writeJSON(w, map[string]interface{}{"path": p, "name": pathName(p)})
}

func handleSetRoot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, err := filepath.Abs(strings.TrimSpace(req.Path))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		writeErr(w, 400, "不是有效的文件夹")
		return
	}
	cfgMu.Lock()
	cfg.Root = abs
	cfg.NoResume = false // 用户主动进来了，下次启动照常自动打开
	cfgMu.Unlock()
	if err := saveConfig(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	pushRecent(abs)
	writeJSON(w, map[string]interface{}{"root": abs, "name": pathName(abs)})
}

func handleMkdir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, err := cleanPath(req.Path, false)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	watcher.noteSelf(filepath.Base(abs))
	writeJSON(w, map[string]interface{}{"ok": true})
}

/* ===================== 共享文件夹：最近列表 / 一键新建 ===================== */

// /api/recents  主页「打开已有」二级菜单的数据：
// 最近用过的文件夹 + 本机现成的 SMB 共享（点一下就能进去）
func handleRecents(w http.ResponseWriter, r *http.Request) {
	host := machineName()
	cfgMu.RLock()
	src := append([]recentEntry{}, cfg.Recents...)
	cfgMu.RUnlock()

	type recentOut struct {
		Path   string `json:"path"`
		Name   string `json:"name"`
		TS     int64  `json:"ts"`
		Exists bool   `json:"exists"`
		Share  string `json:"share"` // 已被哪个共享覆盖（空 = 没共享）
		UNC    string `json:"unc"`
	}
	recents := []recentOut{}
	seen := map[string]bool{}
	for _, it := range src {
		/* 老配置里可能存的是正斜杠（比如 C:/Users/xxx），这里统一成 Windows 的反斜杠，
		   不然同一个文件夹会在侧边栏里出现两遍，删一条还剩一条 */
		p := filepath.Clean(it.Path)
		if p == "" || p == "." || seen[strings.ToLower(p)] {
			continue
		}
		seen[strings.ToLower(p)] = true
		name := it.Name
		if strings.Trim(name, `\/ `) == "" { // 老配置里可能存着一个「\\」，用路径兜底
			name = pathName(p)
		}
		o := recentOut{Path: p, Name: name, TS: it.TS}
		if st, err := os.Stat(p); err == nil && st.IsDir() {
			o.Exists = true
		}
		if s, rel, ok := coverShare(p); ok && o.Exists {
			o.Share = s.Name
			o.UNC = s.UNC
			if rel != "" {
				o.UNC += `\` + rel
			}
		}
		recents = append(recents, o)
	}

	// 本机共享里挑看起来像聊天文件夹的放前面，其余按名字排
	shares := []smbShare{}
	for _, s := range listShares() {
		/* 用户右键删过的共享不再列出来（只影响这份列表，不动 Windows 的共享） */
		if shareMuted(s.Path) {
			continue
		}
		shares = append(shares, s)
	}
	writeJSON(w, map[string]interface{}{
		"host":    host,
		"recents": recents,
		"shares":  shares,
		"admin":   isElevated(),
		"default": defaultParent(),
	})
}

// 新建时默认落在「文档」下（没有就用用户目录）
func defaultParent() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		doc := filepath.Join(home, "Documents")
		if st, err := os.Stat(doc); err == nil && st.IsDir() {
			return doc
		}
		return home
	}
	return ""
}

// /api/create-share  一步到位：建文件夹 + 建 _lanchat + 开共享 + 授权
func handleCreateShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent    string `json:"parent"`
		Name      string `json:"name"`
		Share     *bool  `json:"share"`
		Perm      string `json:"perm"` // read / full
		ShareName string `json:"shareName"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	parent := strings.TrimSpace(req.Parent)
	name := strings.TrimSpace(req.Name)
	if parent == "" {
		writeErr(w, 400, "请先选择位置")
		return
	}
	if name == "" {
		name = shareFolderName
	}
	if strings.ContainsAny(name, `\/:*?"<>|`) {
		writeErr(w, 400, `文件夹名不能包含 \ / : * ? " < > |`)
		return
	}
	pabs, err := filepath.Abs(parent)
	if err != nil {
		writeErr(w, 400, "位置不合法")
		return
	}
	if st, err := os.Stat(pabs); err != nil || !st.IsDir() {
		writeErr(w, 400, "选的位置不存在（网络盘要先连上）")
		return
	}
	target := filepath.Join(pabs, name)
	existBefore := false
	if _, err := os.Stat(target); err == nil {
		existBefore = true
	}
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeErr(w, 500, "创建文件夹失败："+err.Error())
		return
	}
	// 提前把 _lanchat 的三层目录建好，另一台打开时不会因为父目录不存在而卡住
	for _, sub := range []string{"_lanchat", "_lanchat\\msgs", "_lanchat\\blobs", "_lanchat\\peers"} {
		_ = os.MkdirAll(filepath.Join(target, sub), 0o755)
	}

	resp := map[string]interface{}{
		"ok":          true,
		"path":        target,
		"name":        name,
		"existBefore": existBefore,
		"shared":      false,
		"admin":       isElevated(),
		"notes":       []string{},
	}

	wantShare := true
	if req.Share != nil {
		wantShare = *req.Share
	}
	if wantShare {
		readOnly := strings.EqualFold(strings.TrimSpace(req.Perm), "read")
		perm := "full"
		if readOnly {
			perm = "read"
		}
		shareName := strings.TrimSpace(req.ShareName)
		if shareName == "" {
			shareName = name
		}
		setup, err := ensureShareFor(target, shareName, readOnly)
		if err != nil {
			// 文件夹已经建好了，共享没成：把原因带回去，页面提示「以管理员身份重启」
			resp["shareError"] = err.Error()
			resp["admin"] = isElevated()
			writeJSON(w, resp)
			return
		}
		resp["shared"] = true
		resp["shareName"] = setup.ShareName
		resp["unc"] = setup.UNC
		resp["perm"] = perm
		resp["reused"] = setup.Reused
		resp["shareCreated"] = setup.Created
		if len(setup.Notes) > 0 {
			resp["notes"] = setup.Notes
		}
	}
	pushRecent(target)
	writeJSON(w, resp)
}

// /api/relaunch-admin  以管理员身份重新打开自己（并把这次没做完的共享记成待办）
func handleRelaunchAdmin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Pending *pendingShare `json:"pending"`
	}
	_ = readBodyJSON(r, &req)
	if req.Pending != nil && strings.TrimSpace(req.Pending.Path) != "" {
		cfgMu.Lock()
		cfg.Pending = req.Pending
		cfg.PendingMsg = ""
		cfgMu.Unlock()
		_ = saveConfig()
	}
	if err := relaunchAsAdmin(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
	go func() {
		time.Sleep(250 * time.Millisecond)
		quitApp()
	}()
}

// 启动时补做上次没成功的共享（管理员重启后走这里）
func runPendingShare() {
	cfgMu.Lock()
	p := cfg.Pending
	cfgMu.Unlock()
	if p == nil || p.Path == "" {
		return
	}
	if !isElevated() {
		return // 还是没权限：留着，下次以管理员启动时再补
	}
	cfgMu.Lock()
	cfg.Pending = nil
	cfgMu.Unlock()
	readOnly := strings.EqualFold(p.Perm, "read")
	setup, err := ensureShareFor(p.Path, p.Name, readOnly)
	msg := ""
	if err != nil {
		msg = "上次没做成的共享仍未成功：" + err.Error()
		debugf("pending share: %v", err)
	} else {
		msg = "已补好共享：" + setup.UNC
		if len(setup.Notes) > 0 {
			msg += "（" + strings.Join(setup.Notes, "；") + "）"
		}
		debugf("pending share ok: %s", setup.UNC)
	}
	cfgMu.Lock()
	cfg.PendingMsg = msg
	cfgMu.Unlock()
	_ = saveConfig()
}

func takePendingMsg() string {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	msg := cfg.PendingMsg
	cfg.PendingMsg = ""
	return msg
}

func handleList(w http.ResponseWriter, r *http.Request) {
	abs, err := cleanPath(r.URL.Query().Get("path"), true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	des, err := os.ReadDir(abs)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	entries := make([][2]string, 0, len(des))
	for _, de := range des {
		kind := "file"
		if de.IsDir() {
			kind = "dir"
		}
		entries = append(entries, [2]string{de.Name(), kind})
	}
	writeJSON(w, map[string]interface{}{"entries": entries})
}

func handleRead(w http.ResponseWriter, r *http.Request) {
	abs, err := cleanPath(r.URL.Query().Get("path"), true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

// 追加写：mode=trunc 首次写入（截断），mode=append 后续分片
func handleWrite(w http.ResponseWriter, r *http.Request) {
	abs, err := cleanPath(r.URL.Query().Get("path"), false)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	flag := os.O_CREATE | os.O_WRONLY
	if strings.EqualFold(r.URL.Query().Get("mode"), "append") {
		flag |= os.O_APPEND
	} else {
		flag |= os.O_TRUNC
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	f, err := os.OpenFile(abs, flag, 0o644)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n, err := io.Copy(f, r.Body)
	cerr := f.Close()
	_ = r.Body.Close()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if cerr != nil {
		writeErr(w, 500, cerr.Error())
		return
	}
	// 自己写的文件不算「别人放进来的」
	watcher.noteSelf(filepath.Base(abs))
	writeJSON(w, map[string]interface{}{"ok": true, "bytes": n})
}

func handleRemove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, err := cleanPath(req.Path, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.RemoveAll(abs); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// 打开资源管理器：select=true 时定位并选中该文件。
//
// v3.0 起不再要求「路径必须在共享根里」：共享文件夹在对方电脑上时（映射盘 Z: 或 UNC），
// 路径校验用不上，这里只要求「这个位置真的能访问」。映射盘在这台机器上看不见时，
// resolveOpenPath 会换成它的共享地址（\\主机\共享名）再打开 —— 以前就是卡在这一步。
func handleReveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path   string `json:"path"`
		Select bool   `json:"select"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, err := resolveOpenPath(req.Path)
	if err != nil {
		debugf("reveal 失败 %q: %v", req.Path, err)
		writeErr(w, 400, err.Error())
		return
	}
	debugf("reveal %q -> %q select=%v", req.Path, abs, req.Select)
	isDir := false
	if st, e := os.Stat(abs); e == nil && st.IsDir() {
		isDir = true
	}
	if isDir || !req.Select {
		if err := startDetached("explorer.exe", abs); err != nil {
			if e2 := shellOpen(abs); e2 != nil {
				writeErr(w, 500, "打不开资源管理器："+err.Error())
				return
			}
		}
	} else {
		// 文件：定位并选中（explorer 的 /select 只对文件有意义）
		if err := startDetached("explorer.exe", "/select,"+abs); err != nil {
			if e2 := shellOpen(abs); e2 != nil {
				writeErr(w, 500, "打不开资源管理器："+err.Error())
				return
			}
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": abs, "dir": isDir})
}

// /api/reveal-dir  在资源管理器里打开任意一个存在的文件夹。
// 侧边栏里的文件夹可能不在当前共享根下面（比如别的盘上的旧文件夹），
// 所以这里不套 cleanPath，只要求「真实存在 + 是文件夹」。
func handleRevealDir(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" {
		writeErr(w, 400, "路径为空")
		return
	}
	abs, err := resolveOpenPath(p)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		writeErr(w, 400, "这个文件夹找不到了")
		return
	}
	if err := startDetached("explorer.exe", abs); err != nil {
		if e2 := shellOpen(abs); e2 != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": abs})
}

/* ===================== v3.0：文件 / 文件夹消息 =====================
   聊天里发的文件、文件夹现在都直接落在共享文件夹根目录里（原名），
   两台电脑的资源管理器里都能看见；消息里显示它在共享文件夹里的地址。 */

// /api/where  当前共享文件夹的地址（\\主机\共享名 形式）。
// 气泡里的地址、以及「文件夹内打开」都按它来 —— 远程共享时用它就不会再受盘符影响。
func handleWhere(w http.ResponseWriter, r *http.Request) {
	root := rootDir()
	unc, kind := dirAddress(root)
	writeJSON(w, map[string]interface{}{
		"root": root,
		"unc":  unc,
		"kind": kind, // unc / map / local
		"host": machineName(),
	})
}

// /api/unique  在共享文件夹里给 name 找一个不重名的名字（重名加 (2)、(3)…）
func handleUnique(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Parent string `json:"parent"`
		Name   string `json:"name"`
		Dir    bool   `json:"dir"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	parent, err := cleanPath(req.Parent, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	full := uniquePath(parent, req.Name, req.Dir)
	writeJSON(w, map[string]interface{}{
		"path": full,
		"name": filepath.Base(full),
	})
}

// /api/rename  改个名（发文件时先写 xxx.part，传完再改成正式名字）
func handleRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	from, err := cleanPath(req.From, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	to, err := cleanPath(req.To, false)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := os.Rename(from, to); err != nil {
		writeErr(w, 500, "改名失败："+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": to})
}

// /api/copy  「另存为」：把共享文件夹里的一个文件/整个文件夹复制到用户选的目录。
// src 可能是映射盘或共享地址，dst 是用户自己选的任意文件夹（不限于共享根）。
func handleCopy(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src    string `json:"src"`
		DstDir string `json:"dstDir"`
		Name   string `json:"name"` // 可选：另存成什么名字（老消息的 blob 文件名很难看）
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	src, err := resolveOpenPath(req.Src)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dstDir := filepath.Clean(strings.TrimSpace(req.DstDir))
	if st, serr := os.Stat(dstDir); serr != nil || !st.IsDir() {
		writeErr(w, 400, "保存位置不是一个文件夹")
		return
	}
	dst, files, bytes, err := copyAnyAs(src, dstDir, req.Name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{
		"ok": true, "path": dst, "name": filepath.Base(dst),
		"files": files, "bytes": bytes,
	})
}

// /api/save-text  把一条文字消息存成 .txt（另存为文字消息用）
func handleSaveText(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Dir  string `json:"dir"`
		Name string `json:"name"`
		Text string `json:"text"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	dir := filepath.Clean(strings.TrimSpace(req.Dir))
	if st, serr := os.Stat(dir); serr != nil || !st.IsDir() {
		writeErr(w, 400, "保存位置不是一个文件夹")
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = "聊天记录.txt"
	}
	full := uniquePath(dir, name, false)
	// 带 BOM，双击用记事本打开不会乱码
	if err := os.WriteFile(full, []byte("\ufeff"+req.Text), 0o644); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": full, "name": filepath.Base(full)})
}

// /api/clipboard-file  把文件/文件夹放进剪贴板（相当于在资源管理器里 Ctrl+C）
func handleClipboardFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	abs, err := resolveOpenPath(req.Path)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := setClipboardFiles([]string{abs}); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "path": abs})
}

// /api/forget-recent  把一条记录从「文件夹列表」里删掉。
// 只动列表：磁盘上的文件夹、里面的聊天记录一律不碰（删文件不可撤销，不替用户做）。
func handleForgetRecent(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p := strings.TrimSpace(req.Path)
	if p == "" {
		writeErr(w, 400, "路径为空")
		return
	}
	p = filepath.Clean(p)

	removed := 0
	cfgMu.Lock()
	keep := make([]recentEntry, 0, len(cfg.Recents))
	for _, it := range cfg.Recents {
		if strings.EqualFold(filepath.Clean(it.Path), p) {
			removed++
			continue
		}
		keep = append(keep, it)
	}
	cfg.Recents = keep
	if cfg.Root != "" && strings.EqualFold(filepath.Clean(cfg.Root), p) {
		// 别再自动打开它，也别在主页显示「继续上次」；
		// Root 本身留着，路径校验还要靠它兜底
		cfg.NoResume = true
	}
	cfgMu.Unlock()
	_ = saveConfig()
	writeJSON(w, map[string]interface{}{"ok": true, "removed": removed})
}

// /api/mute-share  把侧边栏「本机的网络共享」那一组里的一条从列表里删掉（mute=true），
// 或者恢复显示（mute=false）。只写配置文件：Windows 的共享、磁盘上的文件夹都不动。
func handleMuteShare(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
		Mute *bool  `json:"mute"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	k := shareKey(req.Path)
	if k == "" {
		writeErr(w, 400, "路径为空")
		return
	}
	mute := true
	if req.Mute != nil {
		mute = *req.Mute
	}

	cfgMu.Lock()
	if mute {
		list := append(dropPath(cfg.MutedShares, req.Path), k)
		if len(list) > 64 { // 别无限长，最多记 64 条
			list = list[len(list)-64:]
		}
		cfg.MutedShares = list
	} else {
		cfg.MutedShares = dropPath(cfg.MutedShares, req.Path)
	}
	n := len(cfg.MutedShares)
	cfgMu.Unlock()
	_ = saveConfig()
	writeJSON(w, map[string]interface{}{"ok": true, "muted": mute, "count": n})
}

// 页面报错回传（只在本机回环地址监听，方便排查「页面为什么没反应」）
func handleLog(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Msg   string `json:"msg"`
		Where string `json:"where"`
	}
	_ = readBodyJSON(r, &req)
	debugf("[page] %s | %s", req.Where, req.Msg)
	writeJSON(w, map[string]interface{}{"ok": true})
}

func handleShutdown(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]interface{}{"ok": true})
	go func() { os.Exit(0) }()
}

/* ===================== 调试日志（LANCHAT_DEBUG=1 时启用） ===================== */

var (
	debugOn  = os.Getenv("LANCHAT_DEBUG") == "1"
	debugMu  sync.Mutex
	debugLog *os.File
)

func initDebugLog() {
	if !debugOn {
		return
	}
	base := filepath.Join(filepath.Dir(cfgFile), "logs")
	_ = os.MkdirAll(base, 0o755)
	f, err := os.OpenFile(filepath.Join(base, "api.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err == nil {
		debugLog = f
	}
}

func debugf(format string, a ...interface{}) {
	if debugLog == nil {
		return
	}
	debugMu.Lock()
	defer debugMu.Unlock()
	_, _ = fmt.Fprintf(debugLog, "%s %s\n", time.Now().Format("15:04:05.000"), fmt.Sprintf(format, a...))
}

// 记录每个请求，排查「页面到底有没有调用接口」这类问题
func withLog(h http.HandlerFunc) http.HandlerFunc {
	if !debugOn {
		return h
	}
	return func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, code: 200}
		start := time.Now()
		h(rec, r)
		debugf("%s %s -> %d (%s)", r.Method, r.URL.RequestURI(), rec.code, time.Since(start))
	}
}

type statusRecorder struct {
	http.ResponseWriter
	code int
}

func (s *statusRecorder) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

/* ===================== 页面存活检测 =====================
   页面每 3 秒 /api/ping 一次；WebView2 万一崩了（窗口白屏、点不动），
   这里就会长时间收不到请求，程序随即改用系统浏览器打开，至少保证能用。 */

var lastSeen int64 // 页面最后一次请求的时间（UnixNano）

func touchPage() { atomic.StoreInt64(&lastSeen, time.Now().UnixNano()) }

func handlePing(w http.ResponseWriter, r *http.Request) {
	touchPage()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

func openInBrowser(url string) {
	_ = exec.Command("cmd", "/c", "start", "", url).Start()
}

func watchWebView(url string) {
	start := time.Now()
	const idle = 6 * time.Second
	for time.Since(start) < 25*time.Second {
		time.Sleep(400 * time.Millisecond)
		last := atomic.LoadInt64(&lastSeen)
		if last == 0 {
			if time.Since(start) > 10*time.Second {
				debugf("watchdog: 10 秒没收到页面请求，改用系统浏览器")
				openInBrowser(url)
				return
			}
			continue
		}
		if time.Since(time.Unix(0, last)) > idle {
			debugf("watchdog: 页面停止响应（可能 WebView2 崩溃），改用系统浏览器")
			openInBrowser(url)
			return
		}
	}
}

func newMux() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/", withLog(handleIndex))
	mux.HandleFunc("/favicon.ico", handleFavicon)
	mux.HandleFunc("/api/info", withLog(handleInfo))
	mux.HandleFunc("/api/profile", withLog(handleProfile))
	mux.HandleFunc("/api/ping", withLog(handlePing))
	mux.HandleFunc("/api/pick-folder", withLog(handlePickFolder))
	mux.HandleFunc("/api/set-root", withLog(handleSetRoot))
	mux.HandleFunc("/api/recents", withLog(handleRecents))
	mux.HandleFunc("/api/create-share", withLog(handleCreateShare))
	mux.HandleFunc("/api/relaunch-admin", withLog(handleRelaunchAdmin))
	mux.HandleFunc("/api/mkdir", withLog(handleMkdir))
	mux.HandleFunc("/api/list", withLog(handleList))
	mux.HandleFunc("/api/read", withLog(handleRead))
	mux.HandleFunc("/api/write", withLog(handleWrite))
	mux.HandleFunc("/api/remove", withLog(handleRemove))
	mux.HandleFunc("/api/reveal", withLog(handleReveal))
	mux.HandleFunc("/api/reveal-dir", withLog(handleRevealDir))
	mux.HandleFunc("/api/where", withLog(handleWhere))
	mux.HandleFunc("/api/unique", withLog(handleUnique))
	mux.HandleFunc("/api/rename", withLog(handleRename))
	mux.HandleFunc("/api/copy", withLog(handleCopy))
	mux.HandleFunc("/api/save-text", withLog(handleSaveText))
	mux.HandleFunc("/api/clipboard-file", withLog(handleClipboardFile))
	mux.HandleFunc("/api/forget-recent", withLog(handleForgetRecent))
	mux.HandleFunc("/api/mute-share", withLog(handleMuteShare))
	mux.HandleFunc("/api/log", withLog(handleLog))
	mux.HandleFunc("/api/shutdown", withLog(handleShutdown))
	mux.HandleFunc("/api/settings", withLog(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			handleSetSettings(w, r)
			return
		}
		handleGetSettings(w, r)
	}))
	mux.HandleFunc("/api/window", withLog(handleWindow))
	mux.HandleFunc("/api/open-cfg", withLog(handleOpenCfgDir))

	// v3.2：文件库 / 批量消息 / 变动通知 / 轻协作
	mux.HandleFunc("/api/files", withLog(handleFiles))
	mux.HandleFunc("/api/open", withLog(handleOpen))
	mux.HandleFunc("/api/preview", withLog(handlePreview))
	mux.HandleFunc("/api/archive", withLog(handleArchive))
	mux.HandleFunc("/api/zip", withLog(handleZip))
	mux.HandleFunc("/api/msgs", withLog(handleMsgs))
	mux.HandleFunc("/api/stat", withLog(handleStat))
	mux.HandleFunc("/api/watch", withLog(handleWatch))
	mux.HandleFunc("/api/todo", withLog(handleTodo))
	mux.HandleFunc("/api/pin", withLog(handlePin))
	return mux
}

/* ===================== 启动 ===================== */

func netListen() (net.Listener, error) {
	if p := strings.TrimSpace(os.Getenv("LANCHAT_PORT")); p != "" {
		return net.Listen("tcp", "127.0.0.1:"+p)
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

func httpServe(ln net.Listener) error { return http.Serve(ln, newMux()) }

// 起一个外部程序（explorer 之类），不关心它的退出
func startDetached(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if err := cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}

func hasTrayFlag() bool {
	for _, a := range os.Args[1:] {
		if a == "--tray" || a == "-tray" || a == "/tray" {
			return true
		}
	}
	return false
}

func main() {
	// 窗口消息必须在创建窗口的同一个线程上分发
	runtime.LockOSThread()

	initConfig()
	initDebugLog()
	debugf("start v%s root=%q closeAction=%q", appVersion, rootDir(), closeActionName())

	// 外壳自检：只建一个纯 Win32 窗口，用来单独验证无边框/圆角/托盘
	if os.Getenv("LANCHAT_SHELLTEST") == "1" {
		shellTestMain()
		return
	}

	ln, err := netListen()
	if err != nil {
		panic(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		_ = httpServe(ln)
	}()
	url := fmt.Sprintf("http://127.0.0.1:%d/", port)

	// 上次以管理员身份重启时记下的待办（没做成的共享），趁现在补上
	go runPendingShare()

	// 自检模式：只跑 HTTP 服务，不开窗口（供自动化测试）
	if os.Getenv("LANCHAT_HEADLESS") == "1" {
		_ = os.WriteFile(filepath.Join(os.TempDir(), "lanchat_url.txt"), []byte(url), 0o644)
		select {}
	}

	w := webview2.NewWithOptions(webview2.WebViewOptions{
		Debug:     false,
		AutoFocus: true,
		DataPath:  filepath.Join(os.Getenv("LOCALAPPDATA"), "LanChat", "webview"),
		WindowOptions: webview2.WindowOptions{
			Title:  "Lanchat v" + appVersion,
			Width:  chatWinW,
			Height: chatWinH,
			Center: true,
		},
	})
	if w == nil {
		// 没有 WebView2 运行时：退回系统默认浏览器（功能不受影响）
		debugf("WebView2 不可用，改用系统浏览器")
		openInBrowser(url)
		select {}
	}

	hwnd := uintptr(w.Window())
	setOwner(hwnd)

	// 无边框 + 圆角，并接管 ✕ / Alt+F4 的行为。启动时是主页的小卡片
	roundedOK = applyFrameless(hwnd, "chat")
	installMainHook(hwnd)
	if trayIcon == 0 {
		trayIcon = makeAppIcon(32)
	}
	setWindowIcon(hwnd, trayIcon)
	createTray(hwnd)
	debugf("window: hwnd=%#x dwm圆角=%v tray=%v", hwnd, roundedOK, trayOK)

	w.Navigate(url)
	go watchWebView(url)

	// 开机自启带 --tray 参数：启动后直接收进托盘，不打扰用户
	if hasTrayFlag() {
		go func() {
			time.Sleep(900 * time.Millisecond)
			hideQuiet()
		}()
	}

	w.Run()
	removeTray()
}
