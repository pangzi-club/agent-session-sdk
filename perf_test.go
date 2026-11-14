package agentsession

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheReturnsCopiesNotSharedSlices(t *testing.T) {
	home := t.TempDir()
	file := writeClaudeIndexFixture(t, home, "sess-cache", "tool call roundtrip", "2026-11-06T02:25:32.506Z")
	// Give the fixture an assistant tool call so entries carry a Tools slice.
	appendFile(t, file, "\n"+`{"type":"assistant","timestamp":"2026-11-06T02:25:40.506Z","message":{"role":"assistant","content":[{"type":"tool_use","name":"Bash","id":"t1","input":{"command":"ls"}}]}}`)

	cache := NewCache(4)
	info := SessionInfo{ID: "sess-cache", FilePath: &file}
	first, err := cache.Load("claude", info)
	if err != nil {
		t.Fatal(err)
	}
	var toolEntry int = -1
	for i, entry := range first {
		if len(entry.Tools) == 1 {
			toolEntry = i
		}
	}
	if toolEntry < 0 {
		t.Fatalf("fixture produced no tool entry: %+v", first)
	}
	// Corrupt the returned copy; the cache must not see it.
	first[toolEntry].Tools[0].Name = "TAMPERED"
	first[toolEntry].Text = "TAMPERED"

	second, err := cache.Load("claude", info)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range second {
		if len(entry.Tools) == 1 && entry.Tools[0].Name == "TAMPERED" {
			t.Fatal("caller mutation of a cached transcript leaked into the cache")
		}
		if entry.Text == "TAMPERED" {
			t.Fatal("caller mutation of entry text leaked into the cache")
		}
	}
}

func TestCacheInvalidatesOnSameMillisecondRewrite(t *testing.T) {
	home := t.TempDir()
	file := filepath.Join(home, "sess.jsonl")
	writeFile(t, file, claudeUserLine("first prompt", "2026-11-06T02:25:32.506Z")+"\n")
	base := time.Now()
	if err := os.Chtimes(file, base, base); err != nil {
		t.Fatal(err)
	}

	cache := NewCache(4)
	info := SessionInfo{ID: "sess", FilePath: &file}
	before, err := cache.Load("claude", info)
	if err != nil {
		t.Fatal(err)
	}
	if !containsText(before, "first prompt") {
		t.Fatalf("initial entries = %+v", before)
	}

	// Same size, same millisecond, different nanoseconds: a millisecond
	// signature would alias to the stale transcript.
	writeFile(t, file, claudeUserLine("second promp", "2026-11-06T02:25:32.506Z")+"\n")
	rewrite := base.Add(200 * time.Microsecond)
	if err := os.Chtimes(file, rewrite, rewrite); err != nil {
		t.Fatal(err)
	}
	if rewrite.UnixMilli() != base.UnixMilli() {
		t.Fatalf("test setup escaped the same millisecond: %v vs %v", base, rewrite)
	}

	after, err := cache.Load("claude", info)
	if err != nil {
		t.Fatal(err)
	}
	if !containsText(after, "second promp") || containsText(after, "first prompt") {
		t.Fatalf("same-millisecond rewrite did not invalidate the cache: %+v", after)
	}
}

func TestCacheEvictsLeastRecentlyUsed(t *testing.T) {
	home := t.TempDir()
	firstFile := writeClaudeIndexFixture(t, home, "sess-a", "session a", "2026-11-06T02:25:32.506Z")
	secondFile := writeClaudeIndexFixture(t, home, "sess-b", "session b", "2026-11-06T02:25:32.506Z")

	cache := NewCache(1)
	first := SessionInfo{ID: "sess-a", FilePath: &firstFile}
	second := SessionInfo{ID: "sess-b", FilePath: &secondFile}
	if _, err := cache.Load("claude", first); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Load("claude", second); err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	_, hasFirst := cache.items["claude:sess-a"]
	_, hasSecond := cache.items["claude:sess-b"]
	cache.mu.Unlock()
	if hasFirst || !hasSecond {
		t.Fatalf("eviction left items first=%v second=%v, want only the latest", hasFirst, hasSecond)
	}
}

// TestMapLimitSharesGlobalBudget proves the process-wide file-work budget
// holds even when several pools run at once.
func TestMapLimitSharesGlobalBudget(t *testing.T) {
	var running, peak int64
	var wg sync.WaitGroup
	for pool := 0; pool < 3; pool++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := MapLimit(make([]int, 32), FileConcurrency, func(int) (struct{}, error) {
				current := atomic.AddInt64(&running, 1)
				for {
					old := atomic.LoadInt64(&peak)
					if current <= old || atomic.CompareAndSwapInt64(&peak, old, current) {
						break
					}
				}
				time.Sleep(time.Millisecond)
				atomic.AddInt64(&running, -1)
				return struct{}{}, nil
			})
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if got := atomic.LoadInt64(&peak); got > FileConcurrency {
		t.Fatalf("peak concurrent workers = %d, want <= %d", got, FileConcurrency)
	}
}

func TestSessionIndexRefreshIsSerialized(t *testing.T) {
	home := t.TempDir()
	writeClaudeIndexFixture(t, home, "sess-1", "Serialize", "2026-11-06T02:25:32.506Z")
	index, err := OpenSessionIndex(home)
	if err != nil {
		t.Fatal(err)
	}
	defer index.Close()

	index.refresh.Lock() // held by the "caller": Refresh must block, not deadlock or race
	done := make(chan error, 1)
	go func() {
		_, err := index.Refresh("claude")
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("Refresh completed while the refresh lock was held: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	index.refresh.Unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Refresh did not finish after the lock was released")
	}
}

func TestReadHeadLinesReadsAtMostMaxBytes(t *testing.T) {
	dir := t.TempDir()
	big := filepath.Join(dir, "big.jsonl")
	body := strings.Repeat("a", 64*1024) // exactly 64 KiB, no trailing newline
	writeFile(t, big, body)
	lines, err := ReadHeadLines(big, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if total := len(strings.Join(lines, "\n")); total != 4096 {
		t.Fatalf("read %d bytes, want exactly the 4096-byte cap", total)
	}

	small := filepath.Join(dir, "small.jsonl")
	writeFile(t, small, "one\ntwo\n")
	lines, err = ReadHeadLines(small, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(lines, "\n") != "one\ntwo\n" {
		t.Fatalf("short file lines = %q", lines)
	}
}

func containsText(entries []Entry, text string) bool {
	for _, entry := range entries {
		if strings.Contains(entry.Text, text) {
			return true
		}
	}
	return false
}

func claudeUserLine(text, timestamp string) string {
	return fmt.Sprintf(`{"type":"user","cwd":"/Users/demo/app","timestamp":%q,"message":{"role":"user","content":[{"type":"text","text":%q}]}}`, timestamp, text)
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

// ---- benchmarks over synthetic large stores ----

func benchmarkClaudeStore(b *testing.B, sessions int) {
	b.Helper()
	home := b.TempDir()
	project := filepath.Join(home, ".claude", "projects", "-Users-demo-bench")
	if err := os.MkdirAll(project, 0o755); err != nil {
		b.Fatal(err)
	}
	for i := 0; i < sessions; i++ {
		file := filepath.Join(project, fmt.Sprintf("bench-%d.jsonl", i))
		writeBenchFile(b, file, fmt.Sprintf(`{"type":"user","cwd":"/Users/demo/bench","timestamp":"2026-11-06T02:25:32.506Z","message":{"role":"user","content":[{"type":"text","text":"bench prompt %d"}]}}`+"\n", i))
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ScanClaude(filepath.Join(home, ".claude")); err != nil {
			b.Fatal(err)
		}
	}
}

func writeBenchFile(b *testing.B, path, content string) {
	b.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		b.Fatal(err)
	}
}

func BenchmarkScanClaude100Sessions(b *testing.B)  { benchmarkClaudeStore(b, 100) }
func BenchmarkScanClaude1000Sessions(b *testing.B) { benchmarkClaudeStore(b, 1000) }
