package runtime

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"errors"
	"net"
	"os"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/resolver/dns"
	"google.golang.org/grpc/status"
)

const keyHeader = "x-governance-key"

func init() {
	// Headless Kubernetes Services change addresses during pod replacement.
	dns.SetMinResolutionInterval(2 * time.Second)
}

type rpcConfig struct {
	key    string
	tlsDir string
}

type serviceCredential struct {
	key    string
	secure bool
}

func (c serviceCredential) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{keyHeader: c.key}, nil
}

func (c serviceCredential) RequireTransportSecurity() bool { return c.secure }

func rpcAuthentication(key string) grpc.UnaryServerInterceptor {
	expected := sha256.Sum256([]byte(key))
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, next grpc.UnaryHandler) (any, error) {
		values := metadata.ValueFromIncomingContext(ctx, keyHeader)
		if len(values) != 1 {
			return nil, status.Error(codes.Unauthenticated, "internal credential required")
		}
		received := sha256.Sum256([]byte(values[0]))
		if subtle.ConstantTimeCompare(received[:], expected[:]) != 1 {
			return nil, status.Error(codes.Unauthenticated, "invalid internal credential")
		}
		return next(ctx, req)
	}
}

func (c rpcConfig) server() (*grpc.Server, error) {
	opts := []grpc.ServerOption{grpc.UnaryInterceptor(rpcAuthentication(c.key)), grpc.MaxRecvMsgSize(128 << 10)}
	if c.tlsDir != "" {
		certificate, err := tls.LoadX509KeyPair(c.tlsDir+"/tls.crt", c.tlsDir+"/tls.key")
		if err != nil {
			return nil, err
		}
		opts = append(opts, grpc.Creds(credentials.NewTLS(&tls.Config{
			Certificates: []tls.Certificate{certificate}, MinVersion: tls.VersionTLS12,
		})))
	}
	return grpc.NewServer(opts...), nil
}

func (c rpcConfig) dial(address string) (*grpc.ClientConn, error) {
	if host, port, err := net.SplitHostPort(address); err != nil || host == "" || port == "" {
		return nil, errors.New("RPC address must be host:port")
	}
	var transport credentials.TransportCredentials = insecure.NewCredentials()
	if c.tlsDir != "" {
		name := os.Getenv("RPC_TLS_SERVER_NAME")
		if name == "" {
			return nil, errors.New("RPC_TLS_SERVER_NAME is required with RPC_TLS_DIR")
		}
		var err error
		transport, err = credentials.NewClientTLSFromFile(c.tlsDir+"/ca.crt", name)
		if err != nil {
			return nil, err
		}
	}
	return grpc.NewClient("dns:///"+address, grpc.WithTransportCredentials(transport),
		grpc.WithPerRPCCredentials(serviceCredential{key: c.key, secure: c.tlsDir != ""}),
		grpc.WithDefaultServiceConfig(`{"loadBalancingConfig":[{"round_robin":{}}]}`),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(2<<20), grpc.WaitForReady(true)))
}
