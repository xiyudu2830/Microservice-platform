package runtime

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestRPCAuthentication(t *testing.T) {
	const key = "test-key-for-the-internal-interceptor"
	interceptor := rpcAuthentication(key)
	for _, test := range []struct {
		name       string
		values     []string
		authorized bool
	}{
		{"missing", nil, false},
		{"incorrect", []string{"wrong"}, false},
		{"ambiguous", []string{key, key}, false},
		{"valid", []string{key}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := metadata.NewIncomingContext(context.Background(), metadata.MD{keyHeader: test.values})
			called := false
			_, err := interceptor(ctx, nil, nil, func(context.Context, any) (any, error) {
				called = true
				return nil, nil
			})
			if test.authorized {
				if err != nil || !called {
					t.Fatalf("expected authorized call, called=%t err=%v", called, err)
				}
			} else if status.Code(err) != codes.Unauthenticated || called {
				t.Fatalf("expected authentication failure, called=%t err=%v", called, err)
			}
		})
	}
}
