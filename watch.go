// 共享文件夹的变动通知（v3.2）
//
// 目的：别人从资源管理器往共享文件夹里拖东西，聊天里也得知道。
//
// 实现方式选了「定期快照 + 比对」，没用 ReadDirectoryChangesW：
//   · 共享文件夹在对方电脑上时是 SMB，变动通知在网络抖动后容易静默失效，
//     还得写一套重连 + 回退轮询的逻辑；
//   · 根目录一般只有几十个文件，2.5 秒 ReadDir 一次的代价可以忽略；
//   · 代码量只有前者的十分之一。
//
// 只盯根目录一层（不递归）。子目录里的变化不报，避免一台机器上几千个文件
// 的工程目录把轮询拖成负担。
package main

import (
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type fileSig struct {
	Size  int64
	Mtime int64
	IsDir bool
}

type fsEvent struct {
	Seq   int64  `json:"seq"`
	Kind  string `json:"kind"` // add / del
	Name  string `json:"name"`
	IsDir bool   `json:"dir"`
	Size  int64  `json:"size"`
	Ts    int64  `json:"ts"`
}

const watchInterval = 2500 * time.Millisecond
const watchKeep = 200 // 最多留多少条事件给前端追

type dirWatch struct {
	mu     sync.Mutex
	root   string
	snap   map[string]fileSig
	self   map[string]int64 // 自己刚写的名字 → 失效时间，别报成「别人放进来的」
	events []fsEvent
	seq    int64
	seeded bool
	last   time.Time
}

var watcher = &dirWatch{snap: map[string]fileSig{}, self: map[string]int64{}}

// noteSelf 记下「这个文件是我自己写的」。TTL 给 30 秒，够躲过一轮扫描。
func (dw *dirWatch) noteSelf(name string) {
	if name == "" {
		return
	}
	dw.mu.Lock()
	dw.self[name] = time.Now().Add(30 * time.Second).UnixMilli()
	dw.mu.Unlock()
}

func (dw *dirWatch) push(kind, name string, sig fileSig, now int64) {
	dw.seq++
	dw.events = append(dw.events, fsEvent{
		Seq: dw.seq, Kind: kind, Name: name, IsDir: sig.IsDir, Size: sig.Size, Ts: now,
	})
	if len(dw.events) > watchKeep {
		dw.events = dw.events[len(dw.events)-watchKeep:]
	}
}

// scan 比对一次。force=false 时按 watchInterval 节流。
func (dw *dirWatch) scan(root string, force bool) {
	if root == "" {
		return
	}
	/* 先在锁里决定「这一轮要不要扫」并把这个时间窗占住，ReadDir 放到锁外：
	   共享盘掉线时 ReadDir 可能卡十几秒，持锁扫会把 noteSelf（发消息时要调）
	   和 since（页面轮询要调）一起堵住，表现就是整个界面僵住。 */
	dw.mu.Lock()
	if dw.root != root {
		dw.root = root
		dw.snap = map[string]fileSig{}
		dw.seeded = false
	}
	if !force && time.Since(dw.last) < watchInterval {
		dw.mu.Unlock()
		return
	}
	dw.last = time.Now()
	dw.mu.Unlock()

	des, err := os.ReadDir(root)

	dw.mu.Lock()
	defer dw.mu.Unlock()
	if dw.root != root {
		return // 扫的过程中换了共享文件夹，这批结果作废
	}
	if err != nil {
		dw.snap = map[string]fileSig{}
		dw.seeded = false // 共享断了，恢复后重新建立基线，别把老文件全报一遍
		return
	}
	now := time.Now().UnixMilli()
	seen := make(map[string]fileSig, len(des))
	for _, de := range des {
		n := de.Name()
		if strings.EqualFold(n, "_lanchat") {
			continue
		}
		var sig fileSig
		sig.IsDir = de.IsDir()
		if info, err := de.Info(); err == nil {
			sig.Size = info.Size()
			sig.Mtime = info.ModTime().UnixMilli()
		}
		seen[n] = sig
	}

	if !dw.seeded {
		// 第一次只建基线：目录里已有的东西不算「刚放进来」
		dw.snap = seen
		dw.seeded = true
		dw.cleanSelf(now)
		return
	}

	for n, sig := range seen {
		if _, ok := dw.snap[n]; ok {
			continue
		}
		if dw.isSelf(n, now) {
			continue
		}
		dw.push("add", n, sig, now)
	}
	for n, sig := range dw.snap {
		if _, ok := seen[n]; !ok {
			if dw.isSelf(n, now) {
				continue
			}
			dw.push("del", n, sig, now)
		}
	}
	dw.snap = seen
	dw.cleanSelf(now)
}

func (dw *dirWatch) isSelf(name string, now int64) bool {
	exp, ok := dw.self[name]
	return ok && exp > now
}

func (dw *dirWatch) cleanSelf(now int64) {
	for n, exp := range dw.self {
		if exp <= now {
			delete(dw.self, n)
		}
	}
}

func (dw *dirWatch) since(seq int64) ([]fsEvent, int64) {
	dw.mu.Lock()
	defer dw.mu.Unlock()
	out := make([]fsEvent, 0, 8)
	for _, e := range dw.events {
		if e.Seq > seq {
			out = append(out, e)
		}
	}
	return out, dw.seq
}

func currentRoot() string {
	cfgMu.RLock()
	defer cfgMu.RUnlock()
	return cfg.Root
}

// /api/watch?after=<seq>  拉取共享文件夹根目录的变动
func handleWatch(w http.ResponseWriter, r *http.Request) {
	root := currentRoot()
	watcher.scan(root, false)
	events, seq := watcher.since(qInt(r, "after"))
	writeJSON(w, map[string]interface{}{"events": events, "seq": seq})
}
