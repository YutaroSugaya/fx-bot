// Migration runner for fx-bot. Minimalist, no external libraries.
//
// Usage:
//
//	go run ./cmd/migrate up      -- apply all pending migrations
//	go run ./cmd/migrate down    -- roll back the most-recent applied migration
//	go run ./cmd/migrate status  -- print applied versions
//
// WARNING: `down` runs the newest applied migration's down file with no
// confirmation prompt. Down files are destructive (rolling back 0001 removes
// every table, including the trade history). Take a backup first and never
// run it against a database whose history you need.
//
// Migration files live in MIGRATIONS_DIR (default: migrations) and must be
// named "<version>_<name>.up.sql" and "<version>_<name>.down.sql" where
// version is a positive integer. Both up and down files are required for
// every version.
//
// Environment:
//
//	DATABASE_URL       postgres DSN (required)
//	MIGRATIONS_DIR     directory holding *.sql files (default: migrations)
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"fx-bot/backend/internal/config"
)

type migration struct {
	Version  int64
	Name     string
	UpPath   string
	DownPath string
}

var fileRE = regexp.MustCompile(`^(\d+)_(.+)\.(up|down)\.sql$`)

func loadMigrations(dir string) ([]migration, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read migrations dir %q: %w", dir, err)
	}
	type key struct {
		v int64
		n string
	}
	upMap := map[key]string{}
	downMap := map[key]string{}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := fileRE.FindStringSubmatch(e.Name())
		if m == nil {
			continue
		}
		v, err := strconv.ParseInt(m[1], 10, 64)
		if err != nil {
			return nil, fmt.Errorf("parse version in %q: %w", e.Name(), err)
		}
		k := key{v: v, n: m[2]}
		switch m[3] {
		case "up":
			upMap[k] = filepath.Join(dir, e.Name())
		case "down":
			downMap[k] = filepath.Join(dir, e.Name())
		}
	}
	var out []migration
	for k, up := range upMap {
		down, ok := downMap[k]
		if !ok {
			return nil, fmt.Errorf("missing down file for %d_%s", k.v, k.n)
		}
		out = append(out, migration{Version: k.v, Name: k.n, UpPath: up, DownPath: down})
	}
	for k := range downMap {
		if _, ok := upMap[k]; !ok {
			return nil, fmt.Errorf("missing up file for %d_%s", k.v, k.n)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

func ensureMigrationTable(ctx context.Context, pool *pgxpool.Pool) error {
	const q = `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    BIGINT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`
	_, err := pool.Exec(ctx, q)
	if err != nil {
		return fmt.Errorf("ensure schema_migrations: %w", err)
	}
	return nil
}

func appliedVersions(ctx context.Context, pool *pgxpool.Pool) (map[int64]time.Time, error) {
	rows, err := pool.Query(ctx, `SELECT version, applied_at FROM schema_migrations ORDER BY version`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()
	out := map[int64]time.Time{}
	for rows.Next() {
		var v int64
		var at time.Time
		if err := rows.Scan(&v, &at); err != nil {
			return nil, err
		}
		out[v] = at
	}
	return out, rows.Err()
}

func applyOne(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	body, err := os.ReadFile(m.UpPath)
	if err != nil {
		return fmt.Errorf("read up file: %w", err)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("run up %d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1)`, m.Version); err != nil {
			return fmt.Errorf("record %d: %w", m.Version, err)
		}
		return nil
	})
}

func revertOne(ctx context.Context, pool *pgxpool.Pool, m migration) error {
	body, err := os.ReadFile(m.DownPath)
	if err != nil {
		return fmt.Errorf("read down file: %w", err)
	}
	return pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, string(body)); err != nil {
			return fmt.Errorf("run down %d: %w", m.Version, err)
		}
		if _, err := tx.Exec(ctx, `DELETE FROM schema_migrations WHERE version = $1`, m.Version); err != nil {
			return fmt.Errorf("delete %d: %w", m.Version, err)
		}
		return nil
	})
}

func up(ctx context.Context, pool *pgxpool.Pool, migs []migration) error {
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	pending := 0
	for _, m := range migs {
		if _, ok := applied[m.Version]; ok {
			continue
		}
		fmt.Printf("applying %d_%s ...\n", m.Version, m.Name)
		if err := applyOne(ctx, pool, m); err != nil {
			return err
		}
		pending++
	}
	if pending == 0 {
		fmt.Println("no pending migrations")
	}
	return nil
}

func down(ctx context.Context, pool *pgxpool.Pool, migs []migration) error {
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	// find latest applied that has a corresponding migration entry
	for i := len(migs) - 1; i >= 0; i-- {
		m := migs[i]
		if _, ok := applied[m.Version]; ok {
			fmt.Printf("reverting %d_%s ...\n", m.Version, m.Name)
			return revertOne(ctx, pool, m)
		}
	}
	fmt.Println("nothing to revert")
	return nil
}

func status(ctx context.Context, pool *pgxpool.Pool, migs []migration) error {
	applied, err := appliedVersions(ctx, pool)
	if err != nil {
		return err
	}
	if len(migs) == 0 {
		fmt.Println("(no migration files)")
		return nil
	}
	for _, m := range migs {
		if at, ok := applied[m.Version]; ok {
			fmt.Printf("[x] %d_%s\tapplied %s\n", m.Version, m.Name, at.Format(time.RFC3339))
		} else {
			fmt.Printf("[ ] %d_%s\tpending\n", m.Version, m.Name)
		}
	}
	return nil
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: migrate <up|down|status>")
		os.Exit(2)
	}
	cmd := strings.ToLower(os.Args[1])

	_ = config.LoadDotEnv(".env")
	dsn := config.MustEnv("DATABASE_URL")
	dir := config.Env("MIGRATIONS_DIR", "migrations")

	migs, err := loadMigrations(dir)
	if err != nil {
		fmt.Fprintln(os.Stderr, "load migrations:", err)
		os.Exit(1)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		fmt.Fprintln(os.Stderr, "pgx connect:", err)
		os.Exit(1)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "pgx ping:", err)
		os.Exit(1)
	}
	if err := ensureMigrationTable(ctx, pool); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	var runErr error
	switch cmd {
	case "up":
		runErr = up(ctx, pool, migs)
	case "down":
		runErr = down(ctx, pool, migs)
	case "status":
		runErr = status(ctx, pool, migs)
	default:
		runErr = errors.New("unknown subcommand: " + cmd)
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}
