package broker

import (
	"context"
	"time"
)

// ResolveExecution / ResolveSettleLegs が共有する「ctx 期限まで一定間隔で
// polling、条件成立で return、エラーは即 return、ctx done で ctx.Err()」の
// パターンを共通化する。
//
// 命名:
//   - pollUntilOrCtxDone[T]: fn が done=true を返すまで interval ごとに呼ぶ。
//     fn 内の error は loop を短絡。ctx done なら ctx.Err()。
//
// Note: 名前を pollUntilOrCtxDone と冗長にしているのは、より一般的な
// "retry" との混同を避けるため。retry はエラーが続く間リトライ、poll は
// 「結果が出るまで待つ」セマンティクスで両者は別物。

// pollUntilOrCtxDone は fn を interval ごとに呼び、fn が done=true で
// 値を返すまで待つ。fn 内の error は即 short-circuit。
// ctx 期限到来時は zero 値 + ctx.Err() を返す。
//
// fn は ctx を受け取る (poll 内部の各呼び出しに親 ctx を伝播するため)。
func pollUntilOrCtxDone[T any](
	ctx context.Context,
	interval time.Duration,
	fn func(ctx context.Context) (T, bool, error),
) (T, error) {
	var zero T
	for {
		v, done, err := fn(ctx)
		if err != nil {
			return zero, err
		}
		if done {
			return v, nil
		}
		select {
		case <-ctx.Done():
			return zero, ctx.Err()
		case <-time.After(interval):
		}
	}
}
