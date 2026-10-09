package advisor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"fx-bot/backend/internal/domain/market"
)

// ClaudeCLIAdvisor.Generate() の責務を切り出したもの。
//   - PromptAssembler: prompt.md と MarketSummary JSON を連結して subprocess の
//     stdin payload を作る
//   - ResponseParser: stdout のフェンス剥がし + YAML 開始判定 + 崩れた YAML の救済
//
// それぞれを独立した struct にすることで:
//   - bytes ベースで単体テストできる
//   - Generate() 本体は subprocess 実行と失敗分類だけに集中できる

// ---------- PromptAssembler ----------

// PromptAssembler は Claude CLI の stdin payload を組み立てる。
//
// Payload 形式: "<prompt body>\n\nInput JSON:\n<summary json>\n"
//
// Build は (payload, summaryJSON, err) を返す。summaryJSON は呼出側が
// AdvisorRun.InputJSON に保存するために必要 (payload に埋めたものと同一)。
type PromptAssembler struct {
	PromptPath string
}

// Build は prompt.md を読み込み、Summary JSON と連結した stdin payload を返す。
func (a *PromptAssembler) Build(summary *market.MarketSummary) ([]byte, []byte, error) {
	if a.PromptPath == "" {
		return nil, nil, errors.New("advisor: prompt path empty")
	}
	prompt, err := os.ReadFile(a.PromptPath)
	if err != nil {
		return nil, nil, fmt.Errorf("advisor: read prompt: %w", err)
	}
	summaryJSON, err := json.MarshalIndent(summary, "", "  ")
	if err != nil {
		return nil, nil, fmt.Errorf("advisor: marshal summary: %w", err)
	}
	var buf bytes.Buffer
	buf.Write(prompt)
	buf.WriteString("\n\nInput JSON:\n")
	buf.Write(summaryJSON)
	buf.WriteString("\n")
	return buf.Bytes(), summaryJSON, nil
}

// ---------- ResponseParser ----------

// ResponseParser は Claude CLI の stdout を ParsedYAML に変換する。
//
// Parse は次の状態を errors で返す:
//   - fence/whitespace のみで実体がない (Status=ParseError)
//   - config_id を持たない (Status=ParseError)
//   - 正常な YAML 本文
type ResponseParser struct{}

var (
	openFenceRE  = regexp.MustCompile(`^\s*` + "```" + `(?:yaml|yml)?\s*\n`)
	closeFenceRE = regexp.MustCompile(`\n\s*` + "```" + `\s*\n*\s*$`)

	// 本文中のどこかにあるフェンスブロックの中身を抽出するための正規表現。
	// プロローグが "config_id:" を含むケースで
	// yamlStartRE がプロローグ行に誤マッチするのを防ぐ。
	fenceBlockRE = regexp.MustCompile("(?s)" + "```" + `(?:yaml|yml)?\s*\n(.*?)\n` + "```")

	// 設計上 config_id は YAML 最初のキー。Claude が冒頭 prose を入れた場合の
	// 防御用に、最初の "config_id:" 行頭まで前置文字列を捨てる。
	yamlStartRE = regexp.MustCompile(`(?m)^[ \t]*config_id\s*:`)

	// bodyKeyREs は本物の strategy_config 本文だけが持つトップレベルキー。Claude が
	// YAML 手前に吐く「人間向けプレビュー/要約ブロック」は config_id + generated_at は
	// 持つが、これらネスト構造のキーは持たない。各 config_id ブロックでこの数を数え、
	// 最多のブロックを本文として選ぶことでプレビューを確実に捨てる
	// (プレビュー先頭も config_id+generated_at なので、generated_at 隣接を本文の目印に
	// するとプレビューを誤選択し本物を切り捨ててしまう)。
	bodyKeyREs = []*regexp.Regexp{
		regexp.MustCompile(`(?m)^[ \t]*market_regime\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*strategy\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*entry\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*exit\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*risk\s*:`),
		regexp.MustCompile(`(?m)^[ \t]*no_trade\s*:`),
	}

	// spaceEntityRE は半角スペースの HTML 数値実体参照 (decimal &#32; / hex &#x20;) と
	// &nbsp;。Claude が稀に全空白をこれらに置換した完全な config を吐く
	// ("config_id:&#32;..." でコロン後スペース不在 → go-yaml が
	// "did not find expected key")。parse 前に実スペースへ戻す。
	spaceEntityRE = regexp.MustCompile(`&#0*32;|&#[xX]0*20;|&nbsp;`)

	// colonNoSpaceRE は行頭キーのコロン直後にスペースが無い箇所 (`config_id:"..."`,
	// `  type:range`) を捕まえる。Claude が prompt ルール #1 (コロン後に半角スペース)
	// を破ると go-yaml が "mapping values are not allowed in this context" で死ぬ。
	// 行頭 (任意インデント) の英字始まりキー + ":" + 非空白の
	// ときだけ後挿入するので、値中の時刻 "15:30:00" 等 (行頭キーでない) は触らない。
	colonNoSpaceRE = regexp.MustCompile(`(?m)^([ \t]*[A-Za-z_][A-Za-z0-9_]*):([^ \t\r\n])`)

	// keyNoColonQuoteRE は行頭キーの直後にコロンが無く二重引用符が続く箇所
	// (`config_id"..."`) を捕まえる。Claude が config_id 直前に prose を
	// 入れた回にコロンごと脱落することがある。colonNoSpaceRE はコロンが
	// 存在する前提なのでこのケースを救えない。行頭の英字キー + `"` のときだけ `: ` を
	// 挿入する。正常行 (`reason: "..."`) はコロンがあるので当たらない。
	keyNoColonQuoteRE = regexp.MustCompile(`(?m)^([ \t]*[A-Za-z_][A-Za-z0-9_]*)"`)

	// tzLabelRE は RFC3339 タイムスタンプ直後に付いた括弧つきタイムゾーンラベル
	// (` (JST)` / ` (UTC)` 等) を捕まえる。Claude が prompt の「JST, ISO8601」を
	// 誤解し generated_at/valid_from に `"2026-06-02T21:09:51+09:00 (JST)"` と
	// ラベルを付けると go-yaml の time.Time parse が `extra text: " (JST)"` で死に
	// config 全体が却下される。秒 + offset(Z|±hh:mm)
	// で終わる時刻の直後の `(...)` だけを剥がすので、reason 中の `(recheck: 10 min)`
	// 等 (時刻形でない) には当たらない。捕捉した時刻本体 ($1) だけ残す。
	tzLabelRE = regexp.MustCompile(`(\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2}))\s*\([^)\n]*\)`)
)

// selectBody は本物の strategy_config YAML 本文を取り出す。s を "config_id:" 行で
// 区切って複数ブロックに分割し、各ブロックが含む schema 本文キー (bodyKeyREs:
// market_regime/strategy/entry/exit/risk/no_trade) の数を数えて最多のブロックを返す。
// 同数なら最初のブロック (= 重複 config の draft+final / verbatim repeat は先頭を採用)。
// "config_id:" 行が無ければ s をそのまま返す (下流が reject)。
//
// なぜキー数か: Claude が YAML 手前に吐く「人間向けプレビュー/要約ブロック」は
// config_id + generated_at は持つがネスト構造の本文キーを持たない。generated_at 隣接
// だけを目印にするとこのプレビューを本物と誤認するが、本文キー数で比べればプレビュー
// (0 件) は必ず負ける。
// "config_id:" 行頭より前の prose は捨てられる (ブロックは config_id 行から始まる)。
func selectBody(s string) string {
	lines := strings.Split(s, "\n")
	var starts []int
	for i, ln := range lines {
		if yamlStartRE.MatchString(ln) {
			starts = append(starts, i)
		}
	}
	if len(starts) == 0 {
		return s
	}
	blockEnd := func(k int) int {
		if k+1 < len(starts) {
			return starts[k+1]
		}
		return len(lines)
	}
	best, bestScore := 0, -1
	for k, start := range starts {
		block := strings.Join(lines[start:blockEnd(k)], "\n")
		score := 0
		for _, re := range bodyKeyREs {
			if re.MatchString(block) {
				score++
			}
		}
		if score > bestScore { // strict >: ties keep the earliest block
			best, bestScore = k, score
		}
	}
	return strings.Join(lines[starts[best]:blockEnd(best)], "\n")
}

// Parse は stdout (CLI raw) を ParsedYAML に変換する。
func (p *ResponseParser) Parse(stdout []byte) ([]byte, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return nil, errors.New("empty stdout")
	}
	// 空白の HTML 実体参照を実スペースへ戻す。フェンス抽出や
	// 本文選択より前に行い、以降の正規表現が実スペース前提で動けるようにする。
	s := spaceEntityRE.ReplaceAllString(string(stdout), " ")
	// 行頭キーのコロン直後スペース欠落を補う。実体参照の復元後に
	// 行うことで "config_id:&#32;" → "config_id: " 経由の二重挿入を避ける。
	s = colonNoSpaceRE.ReplaceAllString(s, "$1: $2")
	// コロンごと脱落した行頭キー + 引用符 (`config_id"..."`) を補う。
	// colonNoSpaceRE (コロン前提) の後に行い、`key"` → `key: "` へ。
	s = keyNoColonQuoteRE.ReplaceAllString(s, "$1: \"")
	// タイムスタンプ直後の TZ ラベル ` (JST)` 等を剥がす。
	s = tzLabelRE.ReplaceAllString(s, "$1")
	// 本文中にフェンスブロックがあれば中身を優先抽出する。
	// Why: Claude がプロローグ + ```yaml ... ``` を返したとき、プロローグ側に
	// "config_id:" が含まれると yamlStartRE がそこを YAML 開始位置と誤認する
	// (backtick 始まりトークンで go-yaml が parse に失敗する)。
	// フェンス内を最初に取り出してから他の処理に進めれば、プロローグの形に
	// 依存せず安全に YAML を取り出せる。
	if m := fenceBlockRE.FindStringSubmatch(s); m != nil && strings.TrimSpace(m[1]) != "" {
		s = m[1]
	} else {
		s = openFenceRE.ReplaceAllString(s, "")
		s = closeFenceRE.ReplaceAllString(s, "")
	}
	// 冒頭の prose / プレビューブロックを捨てて本物の YAML 本文を選ぶ。フェンス内に
	// プレビュー+本物の重複ブロックが入るケースもあるため、
	// フェンス経由でも必ず本文選択を通す。
	s = selectBody(s)
	out := []byte(strings.TrimSpace(s))
	if len(out) == 0 {
		return nil, errors.New("all output was fence/whitespace")
	}
	if !yamlStartRE.Match(out) {
		return nil, errors.New("stdout did not contain strategy_config YAML (missing config_id)")
	}
	return out, nil
}
