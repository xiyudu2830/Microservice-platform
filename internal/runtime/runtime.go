package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"github.com/xiyudu2830/Microservice-platform/internal/audit"
	"github.com/xiyudu2830/Microservice-platform/internal/auth"
	"github.com/xiyudu2830/Microservice-platform/internal/broker"
	"github.com/xiyudu2830/Microservice-platform/internal/echo"
	"github.com/xiyudu2830/Microservice-platform/internal/notify"
	"github.com/xiyudu2830/Microservice-platform/internal/registry"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
)

type app struct {
	rpc         *grpc.Server
	public      http.Handler
	healthCheck func(context.Context) error
	cleanup     func()
}

func Env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}

func Run(ctx context.Context, role string) error {
	config := rpcConfig{key: os.Getenv("INTERNAL_RPC_KEY"), tlsDir: os.Getenv("RPC_TLS_DIR")}
	if len(config.key) < 32 || len(config.key) > 512 {
		return errors.New("INTERNAL_RPC_KEY must contain 32 to 512 characters")
	}
	for _, c := range config.key {
		if c < '!' || c > '~' {
			return errors.New("INTERNAL_RPC_KEY must use printable ASCII without spaces")
		}
	}
	a := &app{cleanup: func() {}}
	defer func() { a.cleanup() }()
	startup, cancel := context.WithTimeout(ctx, 12*time.Second)
	err := a.setup(ctx, startup, role, config)
	cancel()
	if err != nil {
		return err
	}

	healthAddr := Env("HEALTH_ADDR", ":8081")
	listener, err := net.Listen("tcp", healthAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	address := Env("RPC_ADDR", ":9090")
	if a.public != nil {
		address = Env("HTTP_ADDR", ":8080")
	}
	service, err := net.Listen("tcp", address)
	if err != nil {
		return err
	}
	defer service.Close()

	var ready atomic.Bool
	health := httpServer(healthHandler(&ready, a.healthCheck))
	public := httpServer(a.public)
	errs := make(chan error, 2)
	go func() { errs <- health.Serve(listener) }()
	if a.public != nil {
		go func() { errs <- public.Serve(service) }()
	} else {
		go func() { errs <- a.rpc.Serve(service) }()
	}
	ready.Store(true)
	slog.Info("service started", "address", address, "health_address", healthAddr)

	var serveErr error
	select {
	case <-ctx.Done():
	case serveErr = <-errs:
	}
	ready.Store(false)
	shutdown, stop := context.WithTimeout(context.Background(), 12*time.Second)
	defer stop()
	if a.rpc != nil {
		done := make(chan struct{})
		go func() {
			a.rpc.GracefulStop()
			close(done)
		}()
		select {
		case <-done:
		case <-shutdown.Done():
			a.rpc.Stop()
		}
	} else if err := public.Shutdown(shutdown); err != nil {
		_ = public.Close()
	}
	if err := health.Shutdown(shutdown); err != nil {
		_ = health.Close()
	}
	return serveErr
}

func httpServer(handler http.Handler) *http.Server {
	return &http.Server{Handler: handler, ReadHeaderTimeout: 3 * time.Second,
		ReadTimeout: 7 * time.Second, WriteTimeout: 14 * time.Second, IdleTimeout: 30 * time.Second,
		MaxHeaderBytes: 16 << 10}
}

func healthHandler(ready *atomic.Bool, check func(context.Context) error) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /livez", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if !ready.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
		defer cancel()
		if check != nil && check(ctx) != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	})
	return mux
}

func backends() (map[string]string, error) {
	result := make(map[string]string)
	for entry := range strings.SplitSeq(os.Getenv("WORKLOADS"), ",") {
		name, target, ok := strings.Cut(entry, "=")
		if !ok || !registry.ValidLabel(name) {
			return nil, errors.New("WORKLOADS must be name=host:port pairs")
		}
		host, port, err := net.SplitHostPort(target)
		number, portErr := strconv.Atoi(port)
		if err != nil || host == "" || portErr != nil || number < 1 || number > 65535 {
			return nil, fmt.Errorf("invalid WORKLOADS address for %s", name)
		}
		if _, exists := result[name]; exists {
			return nil, fmt.Errorf("duplicate workload %s", name)
		}
		result[name] = target
	}
	return result, nil
}

func (a *app) setup(processCtx, ctx context.Context, role string, config rpcConfig) error {
	if role == "broker" {
		return a.setupBroker(config)
	}
	rpc, err := config.server()
	if err != nil {
		return err
	}
	a.rpc = rpc
	switch role {
	case "auth":
		return a.setupAuth(processCtx, ctx)
	case "registry", "audit":
		return a.setupMongo(ctx, role)
	case "notify":
		requireTLS, err := strconv.ParseBool(Env("SMTP_REQUIRE_TLS", "true"))
		if err != nil {
			return errors.New("SMTP_REQUIRE_TLS must be true or false")
		}
		service, err := notify.New(os.Getenv("SMTP_ADDR"), os.Getenv("SMTP_FROM"),
			os.Getenv("SMTP_USERNAME"), os.Getenv("SMTP_PASSWORD"), requireTLS)
		if err != nil {
			return err
		}
		pb.RegisterNotifyServer(rpc, service)
	case "echo":
		name := os.Getenv("WORKLOAD_NAME")
		if !registry.ValidLabel(name) {
			return errors.New("WORKLOAD_NAME must be a valid backend name")
		}
		pb.RegisterWorkloadServer(rpc, &echo.Server{Name: name})
	default:
		return fmt.Errorf("unknown service role %q", role)
	}
	return nil
}

func (a *app) setupAuth(processCtx, ctx context.Context) error {
	dsn := os.Getenv("POSTGRES_DSN")
	if dsn == "" {
		return errors.New("POSTGRES_DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return errors.New("invalid PostgreSQL configuration")
	}
	cleanup := make(chan struct{})
	a.cleanup = func() { close(cleanup); pool.Close() }
	a.healthCheck = pool.Ping
	service, err := auth.New(ctx, pool, os.Getenv("BOOTSTRAP_EMAIL"), os.Getenv("BOOTSTRAP_PASSWORD"))
	if err != nil {
		slog.Error("auth initialization failed", "error_type", fmt.Sprintf("%T", err))
		return errors.New("failed to initialize auth schema or bootstrap account")
	}
	pb.RegisterAuthServer(a.rpc, service)
	go func() {
		ticker := time.NewTicker(15 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-processCtx.Done():
				return
			case <-cleanup:
				return
			case <-ticker.C:
				pruneCtx, cancel := context.WithTimeout(processCtx, 5*time.Second)
				if err := service.PruneSessions(pruneCtx); err != nil {
					slog.Warn("session cleanup failed")
				}
				cancel()
			}
		}
	}()
	return nil
}

func (a *app) setupMongo(ctx context.Context, role string) error {
	uri := os.Getenv("MONGO_URI")
	if uri == "" {
		return errors.New("MONGO_URI is required")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetServerSelectionTimeout(3 * time.Second))
	if err != nil {
		return errors.New("invalid MongoDB configuration")
	}
	a.cleanup = func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = client.Disconnect(ctx)
	}
	a.healthCheck = func(ctx context.Context) error { return client.Ping(ctx, readpref.Primary()) }
	if err := a.healthCheck(ctx); err != nil {
		return errors.New("MongoDB unavailable")
	}
	if role == "registry" {
		targets, err := backends()
		if err != nil {
			return err
		}
		database := client.Database(Env("MONGO_DATABASE", "governance_registry"))
		pb.RegisterRegistryServer(a.rpc, registry.New(database.Collection("routes"), targets))
	} else {
		database := client.Database(Env("MONGO_DATABASE", "governance_audit"))
		service, err := audit.New(ctx, database.Collection("events"))
		if err != nil {
			return errors.New("failed to initialize audit retention index")
		}
		pb.RegisterAuditServer(a.rpc, service)
	}
	return nil
}

func (a *app) setupBroker(config rpcConfig) error {
	targets, err := backends()
	if err != nil {
		return err
	}
	var conns []*grpc.ClientConn
	a.cleanup = func() {
		for _, conn := range conns {
			_ = conn.Close()
		}
	}
	connect := func(address string) (*grpc.ClientConn, error) {
		conn, err := config.dial(address)
		if err == nil {
			conns = append(conns, conn)
			conn.Connect()
		}
		return conn, err
	}
	authConn, err := connect(os.Getenv("AUTH_ADDR"))
	if err != nil {
		return fmt.Errorf("AUTH_ADDR: %w", err)
	}
	registryConn, err := connect(os.Getenv("REGISTRY_ADDR"))
	if err != nil {
		return fmt.Errorf("REGISTRY_ADDR: %w", err)
	}
	auditConn, err := connect(os.Getenv("AUDIT_ADDR"))
	if err != nil {
		return fmt.Errorf("AUDIT_ADDR: %w", err)
	}
	notifyConn, err := connect(os.Getenv("NOTIFY_ADDR"))
	if err != nil {
		return fmt.Errorf("NOTIFY_ADDR: %w", err)
	}
	service := &broker.Server{
		Auth: pb.NewAuthClient(authConn), Registry: pb.NewRegistryClient(registryConn),
		Audit: pb.NewAuditClient(auditConn), Notify: pb.NewNotifyClient(notifyConn),
		Workloads: make(map[string]pb.WorkloadClient, len(targets)),
	}
	for name, address := range targets {
		conn, err := connect(address)
		if err != nil {
			return fmt.Errorf("workload %s: %w", name, err)
		}
		service.Workloads[name] = pb.NewWorkloadClient(conn)
	}
	a.public = service.Handler()
	a.healthCheck = func(context.Context) error {
		if authConn.GetState() != connectivity.Ready || registryConn.GetState() != connectivity.Ready {
			return errors.New("core RPC channels are not ready")
		}
		return nil
	}
	return nil
}
