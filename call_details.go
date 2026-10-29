package rudderstackai

import "context"

// CallDetails are app-set model call metadata. Flags preserve explicit false.
type CallDetails struct {
	CallPurpose        string
	FallbackUsed, BYOK *bool
}

type callDetailsKey struct{}

// WithCallDetails scopes metadata to provider calls made with this context.
func WithCallDetails(ctx context.Context, details CallDetails) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	parent := CallDetailsFromContext(ctx)
	if details.CallPurpose == "" {
		details.CallPurpose = parent.CallPurpose
	}
	if details.FallbackUsed == nil {
		details.FallbackUsed = parent.FallbackUsed
	}
	if details.BYOK == nil {
		details.BYOK = parent.BYOK
	}
	return context.WithValue(ctx, callDetailsKey{}, details)
}

// CallDetailsFromContext returns metadata for custom provider integrations.
func CallDetailsFromContext(ctx context.Context) CallDetails {
	if ctx == nil {
		return CallDetails{}
	}
	value, _ := ctx.Value(callDetailsKey{}).(CallDetails)
	return value
}
