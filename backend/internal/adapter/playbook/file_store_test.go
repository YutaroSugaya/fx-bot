package playbook

import (
	"testing"
	"time"
)

func TestFileStore_AppendLatest(t *testing.T) {
	s := &FileStore{Dir: t.TempDir()}

	// no history yet -> ok=false, empty
	if _, ok, err := s.Latest("USD_JPY"); err != nil || ok {
		t.Fatalf("empty: ok=%v err=%v", ok, err)
	}

	now := time.Date(2026, 6, 19, 3, 0, 0, 0, time.UTC)
	if err := s.Append(Version{Symbol: "USD_JPY", Rules: "v1 rules", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(Version{Symbol: "USD_JPY", Rules: "v2 rules\n- 押し目だけ買う", CreatedAt: now.Add(time.Hour)}); err != nil {
		t.Fatal(err)
	}

	v, ok, err := s.Latest("USD_JPY")
	if err != nil || !ok {
		t.Fatalf("latest: ok=%v err=%v", ok, err)
	}
	if v.Rules != "v2 rules\n- 押し目だけ買う" {
		t.Errorf("latest rules = %q, want v2 (newest wins)", v.Rules)
	}

	// a different symbol stays independent
	if _, ok, _ := s.Latest("EUR_JPY"); ok {
		t.Errorf("EUR_JPY should have no history")
	}
}
