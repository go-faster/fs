package fs

import "context"

// CustomerKey is an SSE-C key: the 256-bit key a client sends with every
// request on an object it encrypts with its own key. The server uses it for
// the duration of the request and never stores it.
//
// It travels on the request's context rather than in each request type,
// because reads take no request struct: the S3 layer attaches it with
// WithCustomerKey, and a storage backend that supports SSE-C reads it with
// CustomerKeyFrom on every write, upload part and read of the object.
// A backend that does not must refuse a write carrying one with
// ErrUnsupportedOperation, never store the object in the clear.
//
// ponytail: context, not a field; a GetObjectRequest if reads grow more
// per-request options.
type CustomerKey []byte

type customerKeyCtx struct{}

// WithCustomerKey returns ctx carrying k.
func WithCustomerKey(ctx context.Context, k CustomerKey) context.Context {
	return context.WithValue(ctx, customerKeyCtx{}, k)
}

// CustomerKeyFrom returns the customer key ctx carries, or nil.
func CustomerKeyFrom(ctx context.Context) CustomerKey {
	k, _ := ctx.Value(customerKeyCtx{}).(CustomerKey)

	return k
}
