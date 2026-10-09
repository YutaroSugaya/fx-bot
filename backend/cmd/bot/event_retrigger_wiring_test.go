package main

import (
	"io"
	"log/slog"
	"testing"

	"fx-bot/backend/internal/adapter/broker"
	"fx-bot/backend/internal/config"
)

// event_retrigger 配線: OCO fill は RUNTIME reconcile だけが検知する
// live の通常決済経路なので、buildRuntimeReconcile は OnPositionClosed フックを
// 必ず貫通させること。startup reconcile は boot 時の後始末 (どうせ 90 秒後に
// 初回サイクルが走る) なので配線しない — 誤って boot 中に発火させない。
func TestBuildRuntimeReconcile_WiresOnPositionClosed(t *testing.T) {
	called := 0
	hook := func(symbol, reason string) { called++ }
	r := buildRuntimeReconcile(reconcileWiringDeps{
		Broker:           &broker.MockBroker{},
		Repos:            newWiringTestRepos(),
		Symbol:           "USD_JPY",
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveMode:         config.ModeLiveConfig,
		OnPositionClosed: hook,
	})
	if r.OnPositionClosed == nil {
		t.Fatal("runtime Reconcile.OnPositionClosed must be wired (broker OCO fills are THE live close path for the event retrigger)")
	}
	r.OnPositionClosed("USD_JPY", "take_profit")
	if called != 1 {
		t.Fatalf("hook passthrough broken: called=%d", called)
	}
}

func TestBuildStartupReconcile_LeavesOnPositionClosedNil(t *testing.T) {
	r := buildStartupReconcile(reconcileWiringDeps{
		Broker:           &broker.MockBroker{},
		Repos:            newWiringTestRepos(),
		Symbol:           "USD_JPY",
		Logger:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		LiveMode:         config.ModeLiveConfig,
		OnPositionClosed: func(symbol, reason string) {},
	})
	if r.OnPositionClosed != nil {
		t.Fatal("startup Reconcile must NOT fire the event retrigger (boot-time cleanup; the first scheduled cycle runs ~90s later anyway)")
	}
}
