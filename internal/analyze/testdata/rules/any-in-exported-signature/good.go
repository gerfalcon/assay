package good

import "context"

// Variadic ...any is the pass-through idiom behind fmt.Printf, SQL driver args
// and structured logging. Flagging it produced pure noise on a real pgx wrapper,
// which is why it is exempt.
func Exec(ctx context.Context, sql string, args ...any) error { return nil }

func Logf(format string, args ...any) {}

// Unexported is out of scope: the blast radius stops at the package.
func unexportedAny(x any) {}

// A non-empty interface states a requirement, which is the whole point.
func Read(r interface{ Read([]byte) (int, error) }) {}

func Concrete(s string, n int) error { return nil }
