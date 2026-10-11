package model

import "context"

type inputAudioSupportKey struct{}

// WithInputAudioSupport marks tool execution with the active model profile's
// audio capability. Tools use this to preserve audio only for capable models.
func WithInputAudioSupport(ctx context.Context, supported bool) context.Context {
	return context.WithValue(ctx, inputAudioSupportKey{}, supported)
}

func InputAudioSupported(ctx context.Context) bool {
	supported, _ := ctx.Value(inputAudioSupportKey{}).(bool)
	return supported
}
