package command

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"fx-bot/backend/internal/config"
	"fx-bot/backend/internal/port"
)

// PromotionResult is the outcome of one PromoteFromYAML run.
type PromotionResult struct {
	Promoted     bool
	ConfigID     string
	Parsed       *config.StrategyConfig
	RejectReason string
	Errors       *config.ValidationResult
}

// Promoter handles the Schema/HardLimit/Risk validation + active-config
// atomic promotion. It depends only on port interfaces — concrete repos and
// the active-YAML artifact backend come from the wiring layer.
type Promoter struct {
	Validator      *config.Validator
	StrategyRepo   port.StrategyConfigRepository
	ConfigPromoter port.StrategyConfigPromoter // 前 active の expire + 新 insert を 1 Tx で行う。nil なら fallback (2 文)
	ValidationRepo port.ConfigValidationEventRepository
	// ArtifactStore writes the human-readable active YAML "outbox" + cleans
	// up the next.yaml staging file, so usecase does no file I/O itself. Wiring passes
	// adapter/artifact.NewFileStrategyConfigStore(next, active).
	// nil-tolerant: YAML write is best-effort (DB is SoT) so missing store
	// is acceptable; promotion proceeds without writing the YAML artifact.
	ArtifactStore port.StrategyConfigArtifactStore
	Mode          config.Mode
	Source        string // strategy_configs.source CHECK: "auto" | "manual" | "event" | "fallback" (claude_cli は provider 列の方)
	// AllowedStrategies は strategy 名 whitelist。nil/empty なら whitelist チェック skip
	// (= 互換動作)。設定すれば ValidateSemantic で strategy.name が
	// このリスト外なら reject される。
	AllowedStrategies []string
	Clock             func() time.Time
	// Logger is optional. Used to warn on best-effort YAML writes that fail
	// after the DB has already committed (DB is the SoT, YAML is an artifact).
	Logger *slog.Logger
	// ExpectedSymbol pins this Promoter to one symbol. When non-empty,
	// PromoteFromYAML rejects any parsed YAML whose symbol differs. Empty
	// = skip the check.
	ExpectedSymbol string
}

// NewPromoter constructs a Promoter with sensible defaults. artifactStore
// is nil-tolerant — pass nil for tests that don't care about the YAML
// outbox; the wiring layer passes
// adapter/artifact.NewFileStrategyConfigStore(next, active).
func NewPromoter(
	validator *config.Validator,
	strategyRepo port.StrategyConfigRepository,
	validationRepo port.ConfigValidationEventRepository,
	artifactStore port.StrategyConfigArtifactStore,
	mode config.Mode,
	source string,
) *Promoter {
	return &Promoter{
		Validator:      validator,
		StrategyRepo:   strategyRepo,
		ValidationRepo: validationRepo,
		ArtifactStore:  artifactStore,
		Mode:           mode,
		Source:         source,
		Clock:          time.Now,
	}
}

// PromoteFromYAML attempts to validate and activate the given YAML.
//
// Workflow:
//  1. Parse YAML → on parse fail: insert validation event (schema fail) and a
//     "rejected" strategy_configs row with raw text. Return result.
//  2. Run ValidateAll (schema + hard limit + risk).
//  3. On any failure: insert per-type validation events, insert "rejected"
//     strategy_configs row with reason, return result. Active YAML untouched.
//  4. On all-pass: mark previous active row as expired in DB, insert new
//     "active" row, atomically rename next.yaml → active.yaml. Return Promoted.
//
// The raw YAML bytes are stored verbatim so we keep the original text Claude
// emitted (which may differ slightly from the parsed struct).
//
// triggerSource は発火源 (auto/manual/event)。strategy_configs.source に
// 入る値で、ダッシュボードでバッジ表示に使う。空文字なら p.Source (= "auto"
// 既定) にフォールバック。
func (p *Promoter) PromoteFromYAML(ctx context.Context, raw []byte, state config.AccountState, triggerSource string) (*PromotionResult, error) {
	res := &PromotionResult{}
	source := triggerSource
	if source == "" {
		source = p.Source
	}

	// Parse first to capture config_id even on validation failure.
	cfg, parseErr := config.ParseStrategyConfig(raw)
	if parseErr != nil {
		// Insert the parent strategy_configs row
		// FIRST so the config_validation_events FK resolves; in the reverse
		// order the event insert would FK-violate and the parse-error audit
		// trail would be lost. insertRejectedRow also populates
		// ParseFailureRaw so the strategy_config_parse_failures junction
		// captures the raw input.
		configID := p.Clock().UTC().Format("20060102-150405-parse-fail")
		if err := p.insertRejectedRow(ctx, configID, raw, parseErr.Error(), nil, source); err != nil && p.Logger != nil {
			p.Logger.Warn("promote_insert_rejected_failed_on_parse_error",
				"config_id", configID, "err", err)
		}
		if err := p.recordValidationEvent(ctx, configID, "schema", "fail", parseErr.Error()); err != nil && p.Logger != nil {
			p.Logger.Warn("promote_record_validation_event_failed_on_parse_error",
				"config_id", configID, "err", err)
		}
		res.RejectReason = "parse_error: " + parseErr.Error()
		res.Errors = &config.ValidationResult{Errors: []config.ValidationError{{
			Type: config.ValidationSchema, Message: parseErr.Error(),
		}}}
		return res, nil
	}

	// Salvage clear no_trade judgments before validation: when Claude marks a
	// config no_trade (enabled=false / strategy.name=no_trade / no_trade.enabled)
	// but leaves a vestigial entry.direction or stray exit/risk numbers, zero
	// the inert fields so the no_trade decision is not rejected over a cosmetic
	// inconsistency (repeated rejections would freeze the
	// active config past its valid_until). No-op for genuine enabled trades.
	cfg.CanonicalizeNoTrade()

	res.Parsed = cfg
	res.ConfigID = cfg.ConfigID
	if res.ConfigID == "" {
		res.ConfigID = p.Clock().UTC().Format("20060102-150405-noid")
	}

	// Symbol guard: reject YAML whose symbol disagrees with this Promoter's
	// pinned symbol. Insert a rejected row so the audit trail captures it.
	if p.ExpectedSymbol != "" && cfg.Symbol != p.ExpectedSymbol {
		reason := fmt.Sprintf("symbol_mismatch: expected %s, got %s",
			p.ExpectedSymbol, cfg.Symbol)
		if err := p.insertRejectedRow(ctx, res.ConfigID, raw, reason, cfg, source); err != nil && p.Logger != nil {
			p.Logger.Warn("promote_insert_rejected_failed_on_symbol_mismatch",
				"config_id", res.ConfigID, "err", err)
		}
		res.RejectReason = reason
		return res, nil
	}

	// Validate all four passes (schema / hard_limit / semantic / risk).
	all := p.Validator.ValidateAll(cfg, state, p.AllowedStrategies...)
	res.Errors = all

	if !all.OK() {
		// Validate-fail branch: insert the
		// rejected strategy_configs parent row BEFORE persisting
		// validation events; config_validation_events.config_id has a
		// NOT NULL FK to strategy_configs(config_id), so the reverse
		// order would drop every event (FK violation). This mirrors the
		// parse-failure branch above.
		reason := all.Summary()
		if err := p.insertRejectedRow(ctx, res.ConfigID, raw, reason, cfg, source); err != nil && p.Logger != nil {
			p.Logger.Warn("promote_insert_rejected_failed",
				"config_id", res.ConfigID, "err", err)
		}
		p.persistValidationEvents(ctx, res.ConfigID, all)
		res.RejectReason = reason
		return res, nil
	}
	// For accepted configs the pass events are written AFTER the suffix
	// retry loop locks in the final config_id (see below).

	// All-pass: expire previous active row(s) and insert new active row.
	// 1 Tx で原子的に行う (ConfigPromoter 経由)。
	//
	// Query the previous active for the
	// SAME (symbol, mode) — and when the re-promote uses the same
	// config_id, COMPARE raw YAML content. Same-id + same-content is
	// idempotent success; same-id + different-content is treated like a
	// duplicate-id collision so the suffix-retry path produces a fresh ID
	// rather than silently keeping the old DB row.
	prevActive, gaErr := p.StrategyRepo.GetActive(ctx, cfg.Symbol, string(p.Mode))
	if gaErr == nil && prevActive != nil && prevActive.ConfigID == res.ConfigID {
		if rawYAMLEqual(prevActive.RawYAML, string(raw)) {
			if p.Logger != nil {
				p.Logger.Info("promote_idempotent_same_config_already_active",
					"config_id", res.ConfigID)
			}
			res.Promoted = true
			return res, nil
		}
		// Same id, different content — force a fresh suffixed id so the
		// holder gets the same content the DB will store.
		if p.Logger != nil {
			p.Logger.Warn("promote_same_id_different_content_force_suffix",
				"config_id", res.ConfigID)
		}
		newID := suffixConfigID(res.ConfigID)
		raw = []byte(rewriteConfigIDInYAML(string(raw), newID))
		res.ConfigID = newID
		if cfg != nil {
			cfg.ConfigID = newID
		}
	}
	prevConfigID := ""
	if gaErr == nil && prevActive != nil && prevActive.ConfigID != res.ConfigID {
		prevConfigID = prevActive.ConfigID
	}

	rec := port.StrategyConfigRecord{
		ConfigID:               res.ConfigID,
		Source:                 source,
		Mode:                   string(p.Mode), // port boundary: enum → string (R1 guardrail)
		Symbol:                 cfg.Symbol,
		Enabled:                cfg.Enabled,
		MarketRegimeType:       string(cfg.MarketRegime.Type),
		MarketRegimeConfidence: cfg.MarketRegime.Confidence,
		StrategyName:           string(cfg.Strategy.Name),
		ValidFrom:              cfg.ValidFrom,
		ValidUntil:             cfg.ValidUntil,
		RawYAML:                string(raw),
		Status:                 port.StrategyConfigStatusActive,
	}

	// 手動トリガで同一 hour 内に走った時、Claude が hour-aligned な config_id を
	// 再利用すると 23505 UNIQUE 違反になる。衝突したら -r<hex> を付けて retry。
	// 3 回試して全部衝突するのは事実上ありえないが念のため上限を切る。
	for attempt := 0; attempt < 3; attempt++ {
		var promoteErr error
		if p.ConfigPromoter != nil {
			promoteErr = p.ConfigPromoter.PromoteActive(ctx, prevConfigID, rec)
		} else {
			// Test fallback: best-effort 2 文 (空白期間あり)
			if prevConfigID != "" {
				_ = p.StrategyRepo.MarkExpired(ctx, prevConfigID)
			}
			promoteErr = p.StrategyRepo.Insert(ctx, rec)
		}

		if promoteErr == nil {
			break
		}
		if !errors.Is(promoteErr, port.ErrDuplicateConfigID) || attempt == 2 {
			return res, fmt.Errorf("promote active strategy_configs: %w", promoteErr)
		}
		// 新しい config_id を生成して retry
		newID := suffixConfigID(rec.ConfigID)
		rec.ConfigID = newID
		rec.RawYAML = rewriteConfigIDInYAML(rec.RawYAML, newID)
		res.ConfigID = newID
		if cfg != nil {
			cfg.ConfigID = newID
		}
		raw = []byte(rec.RawYAML)
	}

	// Write pass events with the FINAL config_id
	// (post suffix-retry) so validation_events.config_id matches the
	// promoted strategy_configs.config_id. Doing this before the retry loop
	// would leave audit rows pointing at a config_id that no longer exists.
	p.persistValidationEvents(ctx, res.ConfigID, all)

	// DB is the source of truth from here on. YAML is an "outbox" artifact
	// for human inspection / legacy tooling — write best-effort but DO NOT
	// fail the promotion if it errors. LoadActiveFromDB is the new startup
	// path so YAML staleness no longer creates a split-brain on restart.
	if p.ArtifactStore != nil {
		if err := p.ArtifactStore.PromoteActive(ctx, raw); err != nil {
			if p.Logger != nil {
				p.Logger.Warn("active_yaml_write_failed_db_authoritative",
					"err", err, "config_id", rec.ConfigID,
					"hint", "DB row is authoritative; YAML will be rewritten on next promote")
			}
			// fall through — DB commit already succeeded.
		}
	}

	res.Promoted = true
	return res, nil
}

func (p *Promoter) insertRejectedRow(ctx context.Context, configID string, raw []byte, reason string, cfg *config.StrategyConfig, source string) error {
	if source == "" {
		source = p.Source
	}
	rec := port.StrategyConfigRecord{
		ConfigID:     configID,
		Source:       source,
		Mode:         string(p.Mode), // port boundary: enum → string (R1 guardrail)
		Status:       port.StrategyConfigStatusRejected,
		RawYAML:      string(raw),
		RejectReason: reason,
		ValidFrom:    p.Clock(),
		ValidUntil:   p.Clock(),
	}
	// cfg == nil means the raw input failed to
	// parse. Populate ParseFailureRaw so the adapter writes a
	// strategy_config_parse_failures junction row (raw_input audit trail).
	if cfg == nil {
		rec.ParseFailureRaw = string(raw)
	}
	if cfg != nil {
		rec.Symbol = cfg.Symbol
		rec.Enabled = cfg.Enabled
		rec.MarketRegimeType = string(cfg.MarketRegime.Type)
		rec.MarketRegimeConfidence = cfg.MarketRegime.Confidence
		rec.StrategyName = string(cfg.Strategy.Name)
		if !cfg.ValidFrom.IsZero() {
			rec.ValidFrom = cfg.ValidFrom
		}
		if !cfg.ValidUntil.IsZero() {
			rec.ValidUntil = cfg.ValidUntil
		}
	}
	return p.StrategyRepo.Insert(ctx, rec)
}

func (p *Promoter) persistValidationEvents(ctx context.Context, configID string, all *config.ValidationResult) {
	// Group errors by ValidationType.
	failsByType := map[config.ValidationType][]string{}
	for _, e := range all.Errors {
		failsByType[e.Type] = append(failsByType[e.Type], e.Error())
	}
	for _, typ := range []config.ValidationType{
		config.ValidationSchema, config.ValidationHardLimit,
		config.ValidationSemantic, config.ValidationRisk,
	} {
		fails := failsByType[typ]
		if len(fails) == 0 {
			// Surface insert errors instead of
			// swallowing them — a silent FK violation here would make the audit
			// trail vanish without any operator-visible signal.
			if err := p.recordValidationEvent(ctx, configID, string(typ), "pass", ""); err != nil && p.Logger != nil {
				p.Logger.Warn("promote_record_validation_event_failed",
					"config_id", configID, "type", string(typ), "status", "pass", "err", err)
			}
			continue
		}
		// Concatenate failure messages for that type.
		msg := strings.Join(fails, "; ")
		if err := p.recordValidationEvent(ctx, configID, string(typ), "fail", msg); err != nil && p.Logger != nil {
			p.Logger.Warn("promote_record_validation_event_failed",
				"config_id", configID, "type", string(typ), "status", "fail", "err", err)
		}
	}
}

func (p *Promoter) recordValidationEvent(ctx context.Context, configID, vtype, status, msg string) error {
	if p.ValidationRepo == nil {
		return nil
	}
	return p.ValidationRepo.Insert(ctx, port.ConfigValidationEvent{
		ConfigID:       configID,
		ValidationType: vtype,
		Status:         status,
		Message:        msg,
	})
}

// LoadActive reads the active YAML artifact (if present) and parses it.
// Returns (nil, nil) when no artifact exists — that's a clean "no active
// config yet" state. TTL is checked separately by the caller
// (StrategyConfig.IsActive).
//
// DEPRECATED for startup wiring: use LoadActiveFromDB instead. No production
// caller remains (tests only); kept for tools without DB access.
func (p *Promoter) LoadActive() (*config.StrategyConfig, error) {
	if p.ArtifactStore == nil {
		return nil, nil
	}
	body, err := p.ArtifactStore.ReadActive(context.Background())
	if err != nil {
		return nil, fmt.Errorf("read active artifact: %w", err)
	}
	if body == nil {
		return nil, nil
	}
	return config.ParseStrategyConfig(body)
}

// LoadActiveFromDB resolves the current active config from the DB row for
// the bot's (Symbol, Mode) — the authoritative SoT. Returns (nil, nil) when no such row exists.
//
// Use this at startup. There is NO YAML fallback at runtime — if
// the DB query fails or the row's raw_yaml can't be parsed, startup must
// fail closed so the bot does not enter the price loop with an outdated
// active config from a stale on-disk YAML.
func (p *Promoter) LoadActiveFromDB(ctx context.Context, symbol string) (*config.StrategyConfig, error) {
	rec, err := p.StrategyRepo.GetActive(ctx, symbol, string(p.Mode))
	if err != nil {
		return nil, fmt.Errorf("db get active: %w", err)
	}
	if rec == nil {
		return nil, nil
	}
	cfg, err := config.ParseStrategyConfig([]byte(rec.RawYAML))
	if err != nil {
		return nil, fmt.Errorf("parse active.raw_yaml (config_id=%s): %w", rec.ConfigID, err)
	}
	// Validate the active config at startup. frozen configs are
	// SQL-direct-INSERTed, bypassing promotion-time validation, so this is the
	// first time the running bot actually checks them. Static-only (no runtime
	// risk pass) so emergency_stop/daily_loss state can't brick startup; TTL is
	// exempt for strategy_computed so the frozen far-future
	// valid_until passes. Caller (loadActiveConfigsForBundles) fails closed on
	// Live, warns on Paper. nil Validator = skip (back-compat / tests).
	if p.Validator != nil {
		if res := p.Validator.ValidateStatic(cfg, p.AllowedStrategies...); !res.OK() {
			return nil, fmt.Errorf("active config failed startup validation (config_id=%s): %s", rec.ConfigID, res.Summary())
		}
	}
	return cfg, nil
}

// rawYAMLEqual is the canonical comparator for "is this re-promoted YAML
// the same content?". Trims trailing whitespace (the YAML emitter may add
// a final newline) and normalises line endings so a no-op re-promote is
// recognised as such even if the file went through a different writer.
func rawYAMLEqual(a, b string) bool {
	return trimTrailWS(a) == trimTrailWS(b)
}

func trimTrailWS(s string) string {
	for len(s) > 0 {
		c := s[len(s)-1]
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			s = s[:len(s)-1]
			continue
		}
		break
	}
	return s
}

// configIDLineRE は YAML 中の `config_id: "<...>"` (クォート有無問わず) を
// 1 行マッチさせる。クォートの種類は ", ', もしくは無しを許容する。
var configIDLineRE = regexp.MustCompile(`(?m)^([ \t]*config_id[ \t]*:[ \t]*)("[^"\n]*"|'[^'\n]*'|[^\n]+)$`)

// suffixConfigID は 23505 衝突時に新しい一意 ID を作る。既に -rXXXX 付きなら
// 末尾の hex 部分だけを差し替える。
func suffixConfigID(orig string) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	suf := "-r" + hex.EncodeToString(b[:])
	// 既存の -rXXXX を剥がしてから付け直す (再 retry で長くなり続けるのを防ぐ)。
	if loc := regexp.MustCompile(`-r[0-9a-f]{6}$`).FindStringIndex(orig); loc != nil {
		orig = orig[:loc[0]]
	}
	return orig + suf
}

// rewriteConfigIDInYAML は raw YAML の config_id 行を newID に置換する。
// 元の値部分のクォートスタイルは保持しない (常に "..." で書き直す)。
func rewriteConfigIDInYAML(raw, newID string) string {
	replaced := configIDLineRE.ReplaceAllString(raw, `${1}"`+newID+`"`)
	if replaced == raw {
		// マッチしなかった (= raw に config_id 行が無い) — 念のため先頭に追加。
		return "config_id: \"" + newID + "\"\n" + raw
	}
	return replaced
}

// Note: file I/O (tempfile + rename) lives in
// adapter/artifact.FileStrategyConfigStore.PromoteActive.
// Promoter is port-only.
