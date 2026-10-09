package main

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// 各 up.sql に対応する down.sql が存在し空ではない
// ことを保証する。0001 の down.sql は DROP TABLE CASCADE のみで再生可能だが、
// 後続の ALTER 系 (0002+) は down が空だと checkout 後の rollback が壊れるので
// このテストで予防線を張る。
//
// 本格的な round-trip (up → down → up が冪等) テストには独立した DB が
// 必要で本テストでは扱わない。少なくとも「down が空でない」「正しい
// SQL キーワードを含む」だけ確認する。

func TestAllUpHaveNonEmptyDown(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	migDir := filepath.Join(filepath.Dir(thisFile), "..", "..", "migrations")

	entries, err := os.ReadDir(migDir)
	if err != nil {
		t.Fatalf("read migrations dir: %v", err)
	}

	upFiles := map[string]bool{}
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".up.sql") {
			upFiles[strings.TrimSuffix(name, ".up.sql")] = true
		}
	}
	if len(upFiles) == 0 {
		t.Fatal("no .up.sql files found")
	}

	for base := range upFiles {
		downName := base + ".down.sql"
		downPath := filepath.Join(migDir, downName)
		body, err := os.ReadFile(downPath)
		if err != nil {
			t.Errorf("%s: missing or unreadable: %v", downName, err)
			continue
		}
		trimmed := strings.TrimSpace(string(body))
		// 空 / コメントのみは却下。最低 1 行の SQL を含むこと。
		hasSQL := false
		for _, line := range strings.Split(trimmed, "\n") {
			line = strings.TrimSpace(line)
			if line == "" || strings.HasPrefix(line, "--") {
				continue
			}
			hasSQL = true
			break
		}
		if !hasSQL {
			t.Errorf("%s: down file has no SQL statements (only comments/blank)", downName)
		}
		// 典型的に必要なキーワードのどれかを含むこと (DROP / ALTER)。
		// up が ALTER ADD なら down は ALTER DROP のはず、up が CREATE なら DROP のはず。
		bodyUpper := strings.ToUpper(trimmed)
		if !strings.Contains(bodyUpper, "DROP") &&
			!strings.Contains(bodyUpper, "ALTER") {
			t.Errorf("%s: down file does not contain DROP or ALTER (reversal SQL missing)", downName)
		}
	}
}
