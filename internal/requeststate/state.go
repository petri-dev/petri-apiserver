package requeststate

import "context"

type State struct {
	Issuer, Subject string
	Name, Result    string
}

type key struct{}

func WithContext(ctx context.Context, state *State) context.Context {
	return context.WithValue(ctx, key{}, state)
}

func FromContext(ctx context.Context) *State {
	state, _ := ctx.Value(key{}).(*State)
	return state
}
