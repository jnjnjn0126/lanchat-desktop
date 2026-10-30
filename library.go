// 文件库：预览、归档、打包（v3.2）
//
// 定位变化：共享文件夹不再只是「聊天附件落地的地方」，它本身就是一台共享盘。
// 这个文件负责让用户能把它当盘用 —— 看得见、打得开、收拾得动。
package main

import (
	"archive/zip"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/sys/windows"
)

/* ===================== 文件库列表 ===================== */

type fileEntry struct {
	Name  string `json:"name"`
	Dir   bool   `json:"dir"`
	Size  int64  `json:"size"`
	Mtime int64  `json:"mtime"`
}

// /api/files?path=  文件库用：一次拿到名字、类型、大小、修改时间。
// 和 /api/list 分开是故意的：list 是给「列目录找文件句柄」用的（每次都要 stat），
// 文件库要的是能直接显示的元数据，一次 ReadDir 全带回来。
func handleFiles(w http.ResponseWriter, r *http.Request) {
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
	out := make([]fileEntry, 0, len(des))
	var total int64
	for _, de := range des {
		n := de.Name()
		if strings.EqualFold(n, "_lanchat") {
			continue
		}
		// .part 是「还没传完」的临时文件，列出来只会让人困惑
		if strings.HasSuffix(n, ".part") {
			continue
		}
		e := fileEntry{Name: n, Dir: de.IsDir()}
		if info, err := de.Info(); err == nil {
			e.Mtime = info.ModTime().UnixMilli()
			if !e.Dir {
				e.Size = info.Size()
				total += e.Size
			}
		}
		out = append(out, e)
	}
	writeJSON(w, map[string]interface{}{
		"entries": out,
		"count":   len(out),
		"total":   total,
		"path":    abs,
	})
}

/* ===================== 预览 ===================== */

var textExt = map[string]bool{
	".txt": true, ".md": true, ".markdown": true, ".csv": true, ".tsv": true, ".log": true,
	".json": true, ".xml": true, ".yml": true, ".yaml": true, ".ini": true, ".conf": true,
	".go": true, ".js": true, ".mjs": true, ".cjs": true, ".ts": true, ".jsx": true, ".tsx": true,
	".py": true, ".java": true, ".kt": true, ".c": true, ".h": true, ".cpp": true, ".hpp": true,
	".cs": true, ".rs": true, ".rb": true, ".php": true, ".sql": true, ".sh": true, ".bash": true,
	".bat": true, ".cmd": true, ".ps1": true, ".html": true, ".htm": true, ".css": true,
	".scss": true, ".less": true, ".vue": true, ".srt": true, ".lrc": true, ".diff": true,
}

var imageExt = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif",
	".webp": "image/webp", ".bmp": "image/bmp", ".svg": "image/svg+xml", ".ico": "image/x-icon",
	".avif": "image/avif",
}

var mediaExt = map[string]string{
	".mp4": "video/mp4", ".webm": "video/webm", ".m4v": "video/mp4",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".m4a": "audio/mp4", ".ogg": "audio/ogg",
}

const previewTextLimit = 2 << 20 // 2 MB，再大就不在窗口里显示了

// decodeToUTF8 把文件内容变成能直接显示的字符串。
// 中文用户的 txt 有相当一部分是 ANSI（GBK），不是 UTF-8，直接当 UTF-8 会全是乱码。
//
// 判断编码只看前 4KB：整文件校验不行 —— 预览会按 2MB 截断，
// 截断点可能正好切在一个汉字中间，那样全文都会被误判成 GBK。
func decodeToUTF8(b []byte) string {
	if len(b) >= 3 && b[0] == 0xEF && b[1] == 0xBB && b[2] == 0xBF {
		b = b[3:]
	}
	head := b
	if len(head) > 4096 {
		head = head[:4096]
	}
	if utf8.Valid(head) {
		/* 2MB 的截断点可能正好切开一个汉字。末尾只回退「不足一个字符」的那几个
		   字节就行，不用对整个 2MB 再跑一遍 utf8.Valid。 */
		for len(b) > 0 {
			r, size := utf8.DecodeLastRune(b)
			if r != utf8.RuneError || size > 1 {
				break
			}
			b = b[:len(b)-1]
		}
		return string(b)
	}
	n, err := windows.MultiByteToWideChar(0, 0, &b[0], int32(len(b)), nil, 0)
	if err != nil || n <= 0 {
		return string(b)
	}
	buf := make([]uint16, n)
	if _, err := windows.MultiByteToWideChar(0, 0, &b[0], int32(len(b)), &buf[0], n); err != nil {
		return string(b)
	}
	return windows.UTF16ToString(buf)
}

// /api/preview?path=  按类型内联返回，前端用 iframe / img / pre 显示
func handlePreview(w http.ResponseWriter, r *http.Request) {
	abs, err := cleanPath(r.URL.Query().Get("path"), true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	if st.IsDir() {
		writeErr(w, 400, "文件夹不能预览")
		return
	}
	ext := strings.ToLower(filepath.Ext(abs))

	switch {
	case textExt[ext]:
		f, err := os.Open(abs)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer f.Close()
		b, _ := io.ReadAll(io.LimitReader(f, previewTextLimit+1))
		truncated := false
		if len(b) > previewTextLimit {
			b = b[:previewTextLimit]
			truncated = true
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Truncated", fmt.Sprint(truncated))
		_, _ = io.WriteString(w, decodeToUTF8(b))
	case imageExt[ext] != "":
		w.Header().Set("Content-Type", imageExt[ext])
		serveInline(w, r, abs)
	case mediaExt[ext] != "":
		w.Header().Set("Content-Type", mediaExt[ext])
		serveInline(w, r, abs)
	case ext == ".pdf":
		w.Header().Set("Content-Type", "application/pdf")
		serveInline(w, r, abs)
	default:
		writeErr(w, 415, "这个格式不方便在窗口里看，用「用本机程序打开」")
	}
}

// serveInline 打开文件并让浏览器内联渲染（PDF 靠这个才不会被当成下载）
func serveInline(w http.ResponseWriter, r *http.Request, abs string) {
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
	w.Header().Set("Content-Disposition", "inline")
	w.Header().Set("Cache-Control", "no-store")
	http.ServeContent(w, r, st.Name(), st.ModTime(), f)
}

/* ===================== 归档 ===================== */

type archiveGroup struct {
	Month string `json:"month"`
	Count int    `json:"count"`
}

// /api/archive
//
//	GET  ?root=<共享根>          只看不改：根目录散着多少文件、能按哪几个月归档
//	POST {root, month}           真的把那个月的文件移进 <month>\ 子目录
//
// 特意做成「先建议、用户点头才动」：自动搬用户的文件是不可接受的。
func handleArchive(w http.ResponseWriter, r *http.Request) {
	if r.Method == "GET" {
		root, err := cleanPath(r.URL.Query().Get("root"), true)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		files, _ := looseFiles(root)
		counts := map[string]int{}
		for _, f := range files {
			counts[f.month]++
		}
		groups := make([]archiveGroup, 0, len(counts))
		for m, c := range counts {
			groups = append(groups, archiveGroup{Month: m, Count: c})
		}
		sort.Slice(groups, func(i, j int) bool { return groups[i].Month < groups[j].Month })
		writeJSON(w, map[string]interface{}{
			"total":  len(files),
			"groups": groups,
		})
		return
	}

	var req struct {
		Root  string `json:"root"`
		Month string `json:"month"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	root, err := cleanPath(req.Root, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if req.Month == "" {
		writeErr(w, 400, "要指定归档到哪个月")
		return
	}
	files, err := looseFiles(root)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	target := filepath.Join(root, req.Month)
	if err := os.MkdirAll(target, 0o755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	moved := 0
	for _, f := range files {
		if f.month != req.Month {
			continue
		}
		dst := uniquePath(target, f.name, false)
		if err := os.Rename(f.path, dst); err != nil {
			continue // 被占用就跳过，不中断整批
		}
		watcher.noteSelf(f.name)
		watcher.noteSelf(filepath.Base(dst))
		moved++
	}
	writeJSON(w, map[string]interface{}{"ok": true, "moved": moved, "dir": req.Month})
}

type looseFile struct {
	name  string
	path  string
	month string
}

// looseFiles 根目录里「散着」的文件：不是文件夹、不是程序自己的 _lanchat、
// 不是正在传的 .part。归档只碰这些。
func looseFiles(root string) ([]looseFile, error) {
	des, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	out := make([]looseFile, 0, len(des))
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || strings.EqualFold(n, "_lanchat") || strings.HasSuffix(n, ".part") {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		out = append(out, looseFile{
			name:  n,
			path:  filepath.Join(root, n),
			month: info.ModTime().Format("2006-01"),
		})
	}
	return out, nil
}

/* ===================== 打包 ===================== */

// /api/zip  把一个文件夹打成 zip 放进共享根目录。
// 几千个小文件在 SMB 上一个个创建特别慢，压成一个再发能快一个数量级。
func handleZip(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Src  string `json:"src"`
		Name string `json:"name"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	src, err := cleanPath(req.Src, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := safeFileName(req.Name)
	if name == "" {
		name = filepath.Base(src)
	}
	if !strings.HasSuffix(strings.ToLower(name), ".zip") {
		name += ".zip"
	}
	root := currentRoot()
	if root == "" {
		writeErr(w, 400, "还没打开共享文件夹")
		return
	}
	dst := uniquePath(root, name, false)

	files, bytes, err := zipDir(src, dst)
	if err != nil {
		_ = os.Remove(dst)
		writeErr(w, 500, "打包失败："+err.Error())
		return
	}
	watcher.noteSelf(filepath.Base(dst))
	writeJSON(w, map[string]interface{}{
		"ok": true, "name": filepath.Base(dst), "path": dst,
		"files": files, "size": bytes,
	})
}

// /api/open  用本机默认程序打开。预览不了的格式（docx / xlsx / exe…）走这里。
func handleOpen(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p, err := resolveOpenPath(req.Path)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := shellOpen(p); err != nil {
		writeErr(w, 500, "打不开："+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true})
}

// safeFileName 去掉 Windows 文件名里不能用的字符（前端已经过滤过一次，
// 这里是后端自己的兜底：接口可能被脚本直接调）
func safeFileName(n string) string {
	s := strings.Map(func(r rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`+"\r\n\t", r) {
			return '_'
		}
		return r
	}, n)
	s = strings.TrimSpace(s)
	if s == "." || s == ".." {
		return ""
	}
	if len([]rune(s)) > 120 {
		s = string([]rune(s)[:120])
	}
	return s
}

func zipDir(src, dst string) (int, int64, error) {
	out, err := os.Create(dst)
	if err != nil {
		return 0, 0, err
	}
	defer out.Close()
	zw := zip.NewWriter(out)
	var count int
	var total int64
	err = filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		hdr.Method = zip.Deflate
		fw, err := zw.CreateHeader(hdr)
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		n, cerr := io.Copy(fw, in)
		in.Close()
		total += n
		count++
		return cerr
	})
	if cerr := zw.Close(); err == nil {
		err = cerr
	}
	return count, total, err
}
