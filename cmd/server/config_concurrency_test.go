package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/linguo2625469/workbuddy2api-panel/internal/livecfg"
	"github.com/linguo2625469/workbuddy2api-panel/internal/pool"
	"github.com/linguo2625469/workbuddy2api-panel/internal/scheduler"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// TestSaveConfigConcurrentNoLostUpdates 并发保存不得丢更新（codex 二轮 P1-1 完整版）。
//
// 每个 goroutine 提交自己特有的键；saveConfig 的语义是「读盘 → 深合并 → 整档
// 提交」，正确持锁时最终文件应包含全部 N 个键；若无 configWriteMu 串行化，
// 并发事务各自基于旧盘提交，后写者会覆盖前者的键（丢更新），且共享
// path+".tmp" 还可能搬走他人的内容。同时断言无残留临时文件、文件始终是合法 JSON。
func TestSaveConfigConcurrentNoLostUpdates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	initial := `{"listen":":7863","api_key":"k","auth_dir":"./auths","state_file":"./state.json"}`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	live := livecfg.New(livecfg.Snapshot{})
	p := pool.New(filepath.Join(dir, "state.json"))
	up := upstream.New()
	sch := scheduler.New(scheduler.Config{Pool: p, Upstream: up})

	const n = 20
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			// 错开启动时刻，放大「读旧盘 → 写回」的交错窗口。
			time.Sleep(time.Duration(i%5) * time.Millisecond)
			raw := []byte(fmt.Sprintf(`{"test_key_%d": %d}`, i, i))
			if _, err := saveConfig(raw, path, live, p, up, sch); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("saveConfig 失败: %v", err)
	}

	final, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(final, &m); err != nil {
		t.Fatalf("并发写后配置不是合法 JSON（交错写入）: %v", err)
	}
	for i := 0; i < n; i++ {
		key := fmt.Sprintf("test_key_%d", i)
		if _, ok := m[key]; !ok {
			t.Fatalf("并发保存丢更新：缺少 %s（最终键数=%d/%d）", key, len(m), n+4)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("残留临时文件: %s", e.Name())
		}
	}
}
