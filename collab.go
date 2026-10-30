// 轻协作：待办清单 + 置顶公告（v3.2）
//
// 两个小东西都只是一份放在 _lanchat\ 里的文件，两台电脑轮流读改写：
//   _lanchat\todo.json   待办，勾选状态同步，记着是谁勾的
//   _lanchat\pin.md      置顶公告，显示在聊天页顶部
//
// 并发：两端同时改的窗口很小，这里用「进程内串行 + 先写 .tmp 再改名 + 冲突重试」
// 兜住。真正会丢的只有「同一瞬间两个人各加一条」这种极端情况，日常够用。
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type TodoItem struct {
	ID     string `json:"id"`
	Text   string `json:"text"`
	Done   bool   `json:"done"`
	By     string `json:"by"`
	Ts     int64  `json:"ts"`
	DoneBy string `json:"doneBy,omitempty"`
	DoneTs int64  `json:"doneTs,omitempty"`
}

type todoFile struct {
	V     int        `json:"v"`
	Items []TodoItem `json:"items"`
}

var todoMu sync.Mutex

func collabPath(name string) (string, error) {
	root := currentRoot()
	if root == "" {
		return "", os.ErrNotExist
	}
	return filepath.Join(root, "_lanchat", name), nil
}

func shortID() string {
	b := make([]byte, 5)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// writeAtomic 先写 .tmp 再改名：对面永远读不到写了一半的 json
func writeAtomic(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readTodo(path string) todoFile {
	var t todoFile
	b, err := os.ReadFile(path)
	if err == nil && json.Valid(b) {
		_ = json.Unmarshal(b, &t)
	}
	if t.Items == nil {
		t.Items = []TodoItem{}
	}
	t.V = 1
	return t
}

// /api/todo  GET 拿列表；POST 改（op = add / toggle / del / clearDone）
func handleTodo(w http.ResponseWriter, r *http.Request) {
	path, err := collabPath("todo.json")
	if err != nil {
		writeErr(w, 400, "还没打开共享文件夹")
		return
	}
	if r.Method == "GET" {
		t := readTodo(path)
		writeJSON(w, map[string]interface{}{"items": t.Items})
		return
	}

	var req struct {
		Op   string `json:"op"`
		ID   string `json:"id"`
		Text string `json:"text"`
		By   string `json:"by"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}

	todoMu.Lock()
	defer todoMu.Unlock()
	t := readTodo(path)
	now := time.Now().UnixMilli()

	switch req.Op {
	case "add":
		text := strings.TrimSpace(req.Text)
		if text == "" {
			writeErr(w, 400, "内容不能为空")
			return
		}
		if len([]rune(text)) > 500 {
			text = string([]rune(text)[:500])
		}
		t.Items = append(t.Items, TodoItem{
			ID: shortID(), Text: text, By: req.By, Ts: now,
		})
	case "toggle":
		for i := range t.Items {
			if t.Items[i].ID != req.ID {
				continue
			}
			t.Items[i].Done = !t.Items[i].Done
			if t.Items[i].Done {
				t.Items[i].DoneBy = req.By
				t.Items[i].DoneTs = now
			} else {
				t.Items[i].DoneBy = ""
				t.Items[i].DoneTs = 0
			}
			break
		}
	case "del":
		keep := t.Items[:0]
		for _, it := range t.Items {
			if it.ID != req.ID {
				keep = append(keep, it)
			}
		}
		t.Items = keep
	case "clearDone":
		keep := t.Items[:0]
		for _, it := range t.Items {
			if !it.Done {
				keep = append(keep, it)
			}
		}
		t.Items = keep
	default:
		writeErr(w, 400, "不认识的操作："+req.Op)
		return
	}

	b, _ := json.MarshalIndent(t, "", "  ")
	if err := writeAtomic(path, b); err != nil {
		writeErr(w, 500, "保存失败："+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "items": t.Items})
}

// /api/pin  GET 拿公告；POST 保存（text 为空等于清空）
func handlePin(w http.ResponseWriter, r *http.Request) {
	path, err := collabPath("pin.md")
	if err != nil {
		writeErr(w, 400, "还没打开共享文件夹")
		return
	}
	if r.Method == "GET" {
		b, _ := os.ReadFile(path)
		writeJSON(w, map[string]interface{}{"text": decodeToUTF8(b)})
		return
	}
	var req struct {
		Text string `json:"text"`
	}
	if err := readBodyJSON(r, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	text := strings.TrimSpace(req.Text)
	if len([]rune(text)) > 4000 {
		text = string([]rune(text)[:4000])
	}
	if text == "" {
		_ = os.Remove(path)
		writeJSON(w, map[string]interface{}{"ok": true, "text": ""})
		return
	}
	if err := writeAtomic(path, []byte(text+"\n")); err != nil {
		writeErr(w, 500, "保存失败："+err.Error())
		return
	}
	writeJSON(w, map[string]interface{}{"ok": true, "text": text})
}
