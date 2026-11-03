package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
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
// 并发事务各自基于旧盘提交，后写者会覆盖前者的键（丢更新），共享旧式
// path+".tmp" 还会互相搬走内容。
//
// 判别力（codex 三审要求）：
//   - 所有 worker 用 barrier 同时起跑，最大化交错窗口（变异验证：删掉锁后本测试失败）；
//   - 一个并发读线程在写入期间反复读取文件，任何一次读到非法 JSON 都判失败
//     （原子替换必须保证任何时刻文件都可解析）；
//   - 结束后检查无残留临时文件。
//
// 未覆盖（如实记录）：PersistIdentityVersions 与面板保存的竞争由同一把
// configWriteMu 结构性保证（它持锁后只调 saveConfigLocked），但本测试未直接
// 驱动该闭包；热应用顺序一致性也不在本测试范围。
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
	start := make(chan struct{})
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // barrier：同时起跑，放大读写交错窗口
			raw := []byte(fmt.Sprintf(`{"test_key_%d": %d}`, i, i))
			if _, err := saveConfig(raw, path, live, p, up, sch); err != nil {
				errs <- err
			}
		}(i)
	}

	// 并发读线程：写入期间文件必须始终是合法 JSON（rename 原子替换）。
	// 仅 POSIX：Windows 上并发读句柄会让 rename 报 Access denied（测试环境语义
	// 差异，生产 Linux 不受影响），在 Windows 下跳过该维度。
	stop := make(chan struct{})
	readerDone := make(chan struct{})
	var readBad atomic.Value // 记录首个读到非法 JSON 的快照
	if runtime.GOOS != "windows" {
		go func() {
			defer close(readerDone)
			for {
				select {
				case <-stop:
					return
				default:
				}
				if b, err := os.ReadFile(path); err == nil && !json.Valid(b) {
					readBad.Store(string(b))
					return
				}
				time.Sleep(time.Millisecond)
			}
		}()
	} else {
		close(readerDone) // Windows 跳过并发读维度（见上方说明）
	}

	close(start)
	wg.Wait()
	close(stop)
	<-readerDone
	close(errs)
	for err := range errs {
		t.Fatalf("saveConfig 失败: %v", err)
	}
	if bad, ok := readBad.Load().(string); ok {
		t.Fatalf("并发写入期间读到非法 JSON（非原子替换）: %.120q", bad)
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
