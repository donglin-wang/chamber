// Package hostprobe provides reusable host-assumption rules.
package hostprobe

import "context"

type Rule struct {
	name string

	run           func(context.Context, Rule) []string
	goos          func() string
	euid          func() int
	currentUser   func() (string, error)
	readFile      func(string) ([]byte, error)
	lookPath      func(string) (string, error)
	commandOutput func(context.Context, string, ...string) ([]byte, error)
}

func (r Rule) Name() string {
	return r.name
}

func (r Rule) Check(ctx context.Context) []string {
	if r.run == nil {
		return nil
	}
	return r.run(ctx, r.withDefaults())
}
