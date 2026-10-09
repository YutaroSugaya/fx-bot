package advisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"fx-bot/backend/internal/domain/market"
	"gopkg.in/yaml.v3"
)

// Generate() の内部は 3 つの責務に分離されている。
//   - PromptAssembler: prompt.md を読んで Summary JSON と連結 (stdin payload)
//   - subprocess 実行 (Generate 本体に残す)
//   - ResponseParser: stdout → ParsedYAML (fence 剥がし + YAML 開始判定)
//
// 旧 stripFences 関数はパッケージプライベートでテスト不能 (内部ヘルパ扱い)
// だったので、ResponseParser 構造体に昇格して単独テスト可能にする。

// --- PromptAssembler ---

func TestPromptAssembler_BuildsStdinPayload(t *testing.T) {
	dir := t.TempDir()
	promptPath := filepath.Join(dir, "prompt.md")
	if err := os.WriteFile(promptPath, []byte("PROMPT BODY"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &PromptAssembler{PromptPath: promptPath}
	summary := &market.MarketSummary{Symbol: "USD_JPY"}
	got, summaryJSON, err := a.Build(summary)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	s := string(got)
	if !strings.HasPrefix(s, "PROMPT BODY") {
		t.Errorf("payload should start with prompt body; got %q...", s[:min(40, len(s))])
	}
	if !strings.Contains(s, "Input JSON:") {
		t.Error("payload should contain 'Input JSON:' separator")
	}
	if !strings.Contains(s, "\"symbol\": \"USD_JPY\"") {
		t.Error("payload should contain marshalled summary JSON")
	}
	if !strings.Contains(string(summaryJSON), "\"symbol\": \"USD_JPY\"") {
		t.Error("returned summaryJSON must be the same as embedded in payload")
	}
}

func TestPromptAssembler_MissingPromptPath(t *testing.T) {
	a := &PromptAssembler{PromptPath: ""}
	_, _, err := a.Build(&market.MarketSummary{})
	if err == nil {
		t.Fatal("expected error for empty prompt path")
	}
}

func TestPromptAssembler_PromptFileNotFound(t *testing.T) {
	a := &PromptAssembler{PromptPath: "/no/such/prompt.md"}
	_, _, err := a.Build(&market.MarketSummary{})
	if err == nil {
		t.Fatal("expected error for missing prompt file")
	}
}

// --- ResponseParser ---

func TestResponseParser_StripFencesAndDetectsYAMLStart(t *testing.T) {
	p := &ResponseParser{}
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "yaml fence + content",
			in:   "```yaml\nconfig_id: abc\nstrategy: x\n```\n",
			want: "config_id: abc\nstrategy: x",
		},
		{
			name: "plain fence",
			in:   "```\nconfig_id: y\n```",
			want: "config_id: y",
		},
		{
			name: "prose preceded yaml (no fence)",
			in:   "Note: data sparse\nconfig_id: z\nstrategy: x\n",
			want: "config_id: z\nstrategy: x",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, perr := p.Parse([]byte(tc.in))
			if perr != nil {
				t.Fatalf("Parse: %v", perr)
			}
			if got := strings.TrimSpace(string(got)); got != tc.want {
				t.Errorf("got %q want %q", got, tc.want)
			}
		})
	}
}

func TestResponseParser_RejectsEmptyOrFenceOnly(t *testing.T) {
	p := &ResponseParser{}
	tests := []struct {
		name       string
		in         string
		wantSubstr string // err.Error() should contain this
	}{
		{"empty", "", "empty"},
		{"whitespace only", "   \n   ", "empty"},
		// fence-only は中身ゼロ。YAML 本文不在として "config_id" 欠落で reject。
		{"fence only", "```yaml\n```\n", "config_id"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := p.Parse([]byte(tc.in))
			if err == nil {
				t.Fatalf("expected error for %q", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q; got %v", tc.wantSubstr, err)
			}
		})
	}
}

func TestResponseParser_RejectsWithoutConfigID(t *testing.T) {
	p := &ResponseParser{}
	_, err := p.Parse([]byte("strategy: scalp\nrisk: low\n"))
	if err == nil {
		t.Fatal("expected error when no config_id key present")
	}
	if !strings.Contains(err.Error(), "config_id") {
		t.Errorf("error should mention config_id; got %v", err)
	}
}

// Claude が「コードフェンス禁止 / プロローグ禁止」を破り、
// フェンスの前に "config_id:" を含む説明文をつけることがある → 素朴な parser はプロローグ
// 行の "config_id:" を YAML 開始位置と誤認し、go-yaml が backtick で死亡する。
// フェンスブロックがある場合はその中身を最優先で抽出する。
func TestResponseParser_PreambleWithFencedYAML(t *testing.T) {
	p := &ResponseParser{}
	in := "config_id: `time` (2026-05-28T05:23:45Z) → `20260528-052345-usdjpy`。next_advisor_run: 14:23 JST → 30 分。\n" +
		"\n" +
		"```yaml\n" +
		"config_id: \"20260528-052345-usdjpy\"\n" +
		"symbol: USD_JPY\n" +
		"```\n"
	got, err := p.Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := "config_id: \"20260528-052345-usdjpy\"\nsymbol: USD_JPY"
	if g := strings.TrimSpace(string(got)); g != want {
		t.Errorf("got %q\nwant %q", g, want)
	}
}

// realBody is a well-formed strategy_config YAML body (proper `key: value`
// spacing + 2-space nesting), identical in shape to configs that parse fine
// on their own. The failures below are purely a preamble PREFIXED above it.
const realBody = `config_id: "20260529-062142-usdjpy"
generated_at: "2026-05-29T15:21:42+09:00"
valid_from: "2026-05-29T06:22:00Z"
valid_until: "2026-05-29T07:22:00Z"
symbol: USD_JPY
enabled: false
market_regime:
  type: range
  confidence: 0.72
  reason: range-bound, no edge
strategy:
  name: no_trade
`

// Claude can prepend an UNFENCED reasoning preamble whose first line also starts
// with "config_id:" (sometimes plus a 2nd prose line / backticks / a typo),
// then emit the real YAML below. A parser that jumps to the FIRST
// "config_id:" (the preamble) hands preamble+body to go-yaml, which dies
// on duplicate config_id / backtick tokens. Fix: anchor on the config_id line
// immediately followed by "generated_at:" (the schema's mandatory 2nd field).
func TestResponseParser_UnfencedPreambleStartingWithConfigID(t *testing.T) {
	cases := []struct{ name, in string }{
		{
			name: "preamble + prose line (would dup config_id)",
			in: "config_id: 20260529-062142-usdjpy | valid 06:22→07:22Z | no_trade → cadence 30.\n" +
				"\nBuild the final config:\n\n" + realBody,
		},
		{
			name: "backtick preamble + typo line",
			in:   "config_id: `20260529-062142-usdjpy`\n\ncongig_id:\n\n" + realBody,
		},
		{
			name: "prose preamble with backticks and arrows",
			in: "config_id: time `2026-05-29T06:25:52Z` → `20260529-062142-usdjpy`\n" +
				"\nvalid_from = `2026-05-29T06:26:00Z`, valid_until = +60min\n\n" + realBody,
		},
		{
			// Claude emitted the whole config twice (draft + final / verbatim
			// repeat) → old go-yaml died on duplicate keys. Keep first block.
			name: "duplicate full config (preamble + body + body)",
			in:   "config_id: 20260529-062142-usdjpy draft\n\n" + realBody + realBody,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ResponseParser{}).Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(out, &m); err != nil {
				t.Fatalf("parsed output is not valid YAML (preamble not stripped): %v\n---\n%s", err, out)
			}
			if m["config_id"] != "20260529-062142-usdjpy" {
				t.Errorf("config_id = %v, want real quoted id (preamble leaked into body)", m["config_id"])
			}
		})
	}
}

// previewBlock reproduces the human-readable "summary" block Claude sometimes
// emits BEFORE the real YAML. It leads
// with config_id + generated_at (so the generated_at-adjacency
// anchor mistakes it for the real body), but its later lines are prose-
// corrupted: a quoted scalar followed by "(60 min)" and a "→" arrow line that
// go-yaml rejects. The real, well-formed body (realBody) follows it.
const previewBlock = `config_id: "20260601-053910-usdjpy"
generated_at: "2026-06-01T14:39:10+09:00"
valid_from: "2026-06-01T05:40:00Z"
valid_until: "2026-06-01T06:40:00Z" (60 min)
next_advisor_run: 14:39 JST, Tokyo afternoon (not a US-event window), no_trade → 30
`

// Claude can prepend a multi-line PREVIEW block whose first two lines are
// config_id + generated_at — exactly the signature the generated_at-adjacency
// anchor uses to identify the real body. stripPreamble would anchor on the
// preview and oneConfigBlock would then discard the real config that follows,
// leaving the prose-corrupted preview that dies at "yaml: line 3: did not find
// expected key". Fix: among all config_id blocks, select the one with the most
// schema body keys (the preview has none of market_regime/strategy/…).
func TestResponseParser_PreviewBlockBeforeRealBody(t *testing.T) {
	in := previewBlock + "\n" + realBody
	out, err := (&ResponseParser{}).Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsed output is not valid YAML (preview not discarded): %v\n---\n%s", err, out)
	}
	if m["config_id"] != "20260529-062142-usdjpy" {
		t.Errorf("config_id = %v, want the real body's id (preview block was selected instead)", m["config_id"])
	}
}

// The preview+real pair can also arrive INSIDE a ```yaml fence. A fence branch
// that takes the fence content verbatim never runs preview/block selection, so
// go-yaml sees a duplicate config_id + prose and rejects. Fix: route fenced
// content through the same block selection.
func TestResponseParser_FencedPreviewBlockBeforeRealBody(t *testing.T) {
	in := "```yaml\n" + previewBlock + "\n" + realBody + "```\n"
	out, err := (&ResponseParser{}).Parse([]byte(in))
	if err != nil {
		t.Fatalf("Parse returned error: %v", err)
	}
	var m map[string]any
	if err := yaml.Unmarshal(out, &m); err != nil {
		t.Fatalf("parsed output is not valid YAML (fence preview not discarded): %v\n---\n%s", err, out)
	}
	if m["config_id"] != "20260529-062142-usdjpy" {
		t.Errorf("config_id = %v, want the real body's id", m["config_id"])
	}
}

// Every space in an otherwise well-formed config can be emitted as the HTML
// numeric entity "&#32;", so "config_id:&#32;..." has no real space after the
// colon and go-yaml dies with "did not find expected key". A partial variant
// entity-encodes only the first line. Fix: normalize
// space entities back to real spaces before parsing.
func TestResponseParser_HTMLEntityEncodedSpaces(t *testing.T) {
	cases := []struct{ name, in string }{
		{"all spaces entity-encoded", strings.ReplaceAll(realBody, " ", "&#32;")},
		{"first line only entity-encoded", strings.Replace(realBody, "config_id: ", "config_id:&#32;", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ResponseParser{}).Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(out, &m); err != nil {
				t.Fatalf("parsed output is not valid YAML (entities not normalized): %v\n---\n%s", err, out)
			}
			if m["config_id"] != "20260529-062142-usdjpy" {
				t.Errorf("config_id = %v, want real id", m["config_id"])
			}
		})
	}
}

// An otherwise complete, well-formed config whose FIRST line drops the space
// after the colon — `config_id:"..."`. go-yaml reads line 1 as a plain scalar
// and dies at line 2 with "mapping values are not allowed in this context".
// This violates the prompt's first rule (space after every colon). Fix: normalize a
// missing space after the colon on key lines before parsing.
func TestResponseParser_MissingSpaceAfterColon(t *testing.T) {
	cases := []struct{ name, in string }{
		{"first line only", strings.Replace(realBody, "config_id: ", "config_id:", 1)},
		{"every key", strings.ReplaceAll(realBody, ": ", ":")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ResponseParser{}).Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(out, &m); err != nil {
				t.Fatalf("parsed output is not valid YAML (colon spacing not normalized): %v\n---\n%s", err, out)
			}
			if m["config_id"] != "20260529-062142-usdjpy" {
				t.Errorf("config_id = %v, want real id", m["config_id"])
			}
		})
	}
}

// Reject shape: `parsing time "2026-05-29T15:21:42+09:00 (JST)": extra text: " (JST)"`.
// Claude can append a parenthesized timezone label INSIDE the quoted timestamp
// value. go-yaml parses generated_at / valid_from / valid_until into time.Time
// (RFC3339); the trailing " (JST)" makes time parse fail and the whole config
// reject. Fix: strip a parenthesized TZ label that directly follows an RFC3339
// timestamp before parsing.
func TestResponseParser_StripsTimezoneLabelAfterTimestamp(t *testing.T) {
	cases := []struct{ name, in string }{
		{"jst label after +09:00 offset", strings.Replace(realBody,
			`generated_at: "2026-05-29T15:21:42+09:00"`,
			`generated_at: "2026-05-29T15:21:42+09:00 (JST)"`, 1)},
		{"utc label after Z", strings.Replace(realBody,
			`valid_from: "2026-05-29T06:22:00Z"`,
			`valid_from: "2026-05-29T06:22:00Z (UTC)"`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ResponseParser{}).Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			var cfg struct {
				GeneratedAt time.Time `yaml:"generated_at"`
				ValidFrom   time.Time `yaml:"valid_from"`
			}
			if err := yaml.Unmarshal(out, &cfg); err != nil {
				t.Fatalf("timezone label not stripped, time.Time parse failed: %v\n---\n%s", err, out)
			}
		})
	}
}

// Claude can prepend a prose preamble then emit `config_id"<id>"` —
// the colon after config_id dropped ENTIRELY (not just the space). The rest
// of the body is well-formed. colonNoSpaceRE only fixes `key:value` (colon
// present); it cannot rescue `key"value"`. selectBody never finds the block
// because yamlStartRE needs a colon → "missing config_id". Fix: insert ": "
// when a line-start key is immediately followed by a double-quote.
func TestResponseParser_MissingColonBeforeQuotedConfigID(t *testing.T) {
	cases := []struct{ name, in string }{
		{
			"config_id colon dropped, no preamble",
			strings.Replace(realBody, `config_id: "`, `config_id"`, 1),
		},
		{
			"prose preamble + config_id colon dropped",
			"All subagents have reported; integration is unambiguous.\n\n" +
				strings.Replace(realBody, `config_id: "`, `config_id"`, 1),
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := (&ResponseParser{}).Parse([]byte(tc.in))
			if err != nil {
				t.Fatalf("Parse returned error: %v", err)
			}
			var m map[string]any
			if err := yaml.Unmarshal(out, &m); err != nil {
				t.Fatalf("parsed output is not valid YAML (colon not restored): %v\n---\n%s", err, out)
			}
			if m["config_id"] != "20260529-062142-usdjpy" {
				t.Errorf("config_id = %v, want real id", m["config_id"])
			}
		})
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
