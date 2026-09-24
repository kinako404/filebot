package state

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestMarkAndSent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if store.Sent("/a.jpg", 1, 2) {
		t.Fatal("新记录不应命中")
	}
	store.Mark("/a.jpg", 1, 2)
	if !store.Sent("/a.jpg", 1, 2) {
		t.Fatal("应当命中")
	}
	if store.Sent("/a.jpg", 1, 3) || store.Sent("/a.jpg", 9, 2) {
		t.Fatal("大小或时间不同不算命中")
	}
}

func TestSaveAndReloadUsesPythonCompatibleFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "state.json")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	store.Mark("/data/clip.mp4", 1700000000000000000, 4096)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatal(err)
	}
	files, ok := parsed["files"].(map[string]any)
	if !ok {
		t.Fatalf("缺少 files 字段：%s", raw)
	}
	entry, ok := files["/data/clip.mp4"].([]any)
	if !ok || len(entry) != 2 {
		t.Fatalf("记录格式不对：%v", files["/data/clip.mp4"])
	}
	if parsed["version"].(float64) != 1 {
		t.Fatalf("version 应为 1：%v", parsed["version"])
	}

	reloaded, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reloaded.Sent("/data/clip.mp4", 1700000000000000000, 4096) {
		t.Fatal("重新载入后应当命中")
	}
	if reloaded.Sent("/data/clip.mp4", 1700000000000000001, 4096) {
		t.Fatal("mtime 不同不应命中")
	}
}

func TestOpenMissingFileIsEmpty(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "nope.json"))
	if err != nil {
		t.Fatal(err)
	}
	if store.Len() != 0 {
		t.Fatalf("len = %d", store.Len())
	}
}

func TestOpenBrokenFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("应当报错")
	}
}

func TestEmptyPathDoesNotPersist(t *testing.T) {
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	store.Mark("/a", 1, 1)
	if err := store.Save(); err != nil {
		t.Fatal(err)
	}
	if !store.Sent("/a", 1, 1) {
		t.Fatal("内存记录应当保留")
	}
}

func TestPruneKeepsStoreBounded(t *testing.T) {
	store, err := Open("")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < maxEntries+100; i++ {
		store.Mark(string(rune('a'+i%26))+string(rune(i)), 1, 1)
	}
	if store.Len() > maxEntries {
		t.Fatalf("超出上限：%d", store.Len())
	}
}
