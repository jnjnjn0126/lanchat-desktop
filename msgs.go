// 批量读消息（v3.2）
//
// 以前前端是「列一次目录 + 每条消息一个 /api/read」。聊天记录攒到几千条以后，
// 启动就是几千次 SMB 往返，开窗口要等几十秒；每 400ms 的轮询也要把整个目录列一遍。
//
// 现在换成一次请求拿一页（默认 300 条）：
//   · 启动：一次 /api/msgs?limit=300
//   · 增量：一次 /api/msgs?after=<最新 ts>
//   · 往上翻：一次 /api/msgs?before=<最早 ts>
//
// 文件名是「13 位时间戳-6 位随机.json」，字典序就是时间序，所以排序和筛选
// 全在文件名上做，不需要读文件、也不需要 stat。
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const msgsPageDefault = 300
const msgsPageMax = 800

type msgFile struct {
	Fn string          `json:"fn"`
	Mt int64           `json:"mt"` // 文件 mtime（毫秒）：共享文件夹所在机器统一赋值，比消息里的 ts 可靠
	M  json.RawMessage `json:"m"`
}

func padTs(ts int64) string { return fmt.Sprintf("%013d", ts) }

func qInt(r *http.Request, key string) int64 {
	n, _ := strconv.ParseInt(r.URL.Query().Get(key), 10, 64)
	return n
}

// /api/msgs?path=<msgs 目录>&before=&after=&limit=
func handleMsgs(w http.ResponseWriter, r *http.Request) {
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

	names := make([]string, 0, len(des))
	ents := make(map[string]os.DirEntry, len(des))
	for _, de := range des {
		n := de.Name()
		if de.IsDir() || !strings.HasSuffix(n, ".json") {
			continue
		}
		names = append(names, n)
		ents[n] = de
	}
	sort.Strings(names)

	before := qInt(r, "before")
	after := qInt(r, "after")
	limit := int(qInt(r, "limit"))
	if limit <= 0 {
		limit = msgsPageDefault
	}
	if limit > msgsPageMax {
		limit = msgsPageMax
	}

	var pick []string
	hasMore := false
	switch {
	case after > 0:
		key := padTs(after)
		for _, n := range names {
			// 同毫秒的多条消息用 >= 会重复读，用文件名严格大于水位线
			if n > key {
				pick = append(pick, n)
			}
		}
		if len(pick) > limit {
			pick, hasMore = pick[:limit], true
		}
	case before > 0:
		key := padTs(before)
		for _, n := range names {
			if n < key {
				pick = append(pick, n)
			}
		}
		if len(pick) > limit {
			pick, hasMore = pick[len(pick)-limit:], true
		}
	default:
		if len(names) > limit {
			pick, hasMore = names[len(names)-limit:], true
		} else {
			pick = names
		}
	}

	out := make([]msgFile, 0, len(pick))
	for _, n := range pick {
		b, err := os.ReadFile(filepath.Join(abs, n))
		if err != nil || !json.Valid(b) {
			continue // 正在被写、或者被删掉了，跳过就行，下一轮还会再来
		}
		var mt int64
		if de, ok := ents[n]; ok {
			if fi, err := de.Info(); err == nil {
				mt = fi.ModTime().UnixMilli()
			}
		}
		out = append(out, msgFile{Fn: n, Mt: mt, M: b})
	}
	writeJSON(w, map[string]interface{}{
		"items":   out,
		"hasMore": hasMore,
		"total":   len(names),
	})
}

// /api/stat?path=  单个路径的大小与时间（断点续传靠它拿 .part 已写多少）
func handleStat(w http.ResponseWriter, r *http.Request) {
	abs, err := cleanPath(r.URL.Query().Get("path"), true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	st, err := os.Stat(abs)
	if err != nil {
		// 不存在不是错误：前端拿它判断「有没有残留的 .part」
		writeJSON(w, map[string]interface{}{"exists": false})
		return
	}
	writeJSON(w, map[string]interface{}{
		"exists": !st.IsDir(),
		"dir":    st.IsDir(),
		"size":   st.Size(),
		"mtime":  st.ModTime().UnixMilli(),
	})
}
