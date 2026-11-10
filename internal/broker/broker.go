package broker

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"github.com/xiyudu2830/Microservice-platform/internal/registry"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	Auth      pb.AuthClient
	Registry  pb.RegistryClient
	Audit     pb.AuditClient
	Notify    pb.NotifyClient
	Workloads map[string]pb.WorkloadClient
}

type result struct {
	code int
	body any
}

type endpoint func(*http.Request, *pb.Identity, string) (result, error)

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/login", s.endpoint("", s.login))
	mux.HandleFunc("POST /v1/users", s.endpoint("admin", s.createUser))
	mux.HandleFunc("PUT /v1/routes/{service}", s.endpoint("admin", s.putRoute))
	mux.HandleFunc("GET /v1/routes", s.endpoint("member", s.listRoutes))
	mux.HandleFunc("POST /v1/invoke/{service}", s.endpoint("member", s.invoke))
	mux.HandleFunc("POST /v1/notifications", s.endpoint("admin", s.notify))
	mux.HandleFunc("GET /v1/audit", s.endpoint("admin", s.listAudit))
	return mux
}

func (s *Server) endpoint(role string, action endpoint) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var raw [16]byte
		rand.Read(raw[:])
		requestID := hex.EncodeToString(raw[:])
		started := time.Now()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		r.Body = http.MaxBytesReader(w, r.Body, 64<<10)

		identity, err := s.authorize(r, role)
		var res result
		if err == nil {
			res, err = action(r, identity, requestID)
		}
		if err != nil {
			res = errorResult(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Request-Id", requestID)
		if res.code == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", "Bearer")
		}

		event := &pb.AuditEvent{RequestId: requestID, ActorId: identity.GetId(), Method: r.Method,
			Path: strings.TrimPrefix(r.Pattern, r.Method+" "), Status: int32(res.code),
			UnixMillis: started.UnixMilli(), DurationMillis: time.Since(started).Milliseconds()}
		if session, ok := res.body.(*pb.Session); ok {
			event.ActorId = session.GetIdentity().GetId()
		}
		auditCtx, stop := context.WithTimeout(context.WithoutCancel(ctx), 750*time.Millisecond)
		defer stop()
		if _, err := s.Audit.Record(auditCtx, event); err != nil {
			slog.Warn("audit write failed", "request_id", requestID, "code", status.Code(err).String())
		}
		slog.Info("request", "request_id", requestID, "actor_id", event.ActorId,
			"method", event.Method, "path", event.Path, "status", res.code, "duration_ms", event.DurationMillis)
		w.WriteHeader(res.code)
		if res.body != nil {
			if err := json.NewEncoder(w).Encode(res.body); err != nil {
				slog.Debug("response write failed", "request_id", requestID, "error", err)
			}
		}
	}
}

func (s *Server) authorize(r *http.Request, role string) (*pb.Identity, error) {
	if role == "" {
		return nil, nil
	}
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || token == "" {
		return nil, status.Error(codes.Unauthenticated, "bearer token required")
	}
	identity, err := s.Auth.Verify(r.Context(), &pb.VerifyRequest{Token: token})
	if err != nil {
		return nil, err
	}
	if (role == "admin" && identity.Role != "admin") ||
		(identity.Role != "admin" && identity.Role != "viewer") {
		return identity, status.Error(codes.PermissionDenied, "insufficient permissions")
	}
	return identity, nil
}

func decode(r *http.Request, body any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	err := decoder.Decode(body)
	if err == nil {
		err = decoder.Decode(new(any))
		if errors.Is(err, io.EOF) {
			return nil
		}
	}
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return status.Error(codes.ResourceExhausted, "request body too large")
	}
	return status.Error(codes.InvalidArgument, "expected one valid JSON object")
}

func (s *Server) login(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	var req pb.LoginRequest
	if err := decode(r, &req); err != nil {
		return result{}, err
	}
	res, err := s.Auth.Login(r.Context(), &req)
	return result{http.StatusOK, res}, err
}

func (s *Server) createUser(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	var req pb.CreateUserRequest
	if err := decode(r, &req); err != nil {
		return result{}, err
	}
	res, err := s.Auth.CreateUser(r.Context(), &req)
	return result{http.StatusCreated, res}, err
}

func (s *Server) putRoute(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	var body struct {
		DefaultBackend string     `json:"default_backend"`
		Rules          []*pb.Rule `json:"rules"`
	}
	if err := decode(r, &body); err != nil {
		return result{}, err
	}
	req := &pb.Route{Service: r.PathValue("service"), DefaultBackend: body.DefaultBackend, Rules: body.Rules}
	res, err := s.Registry.Put(r.Context(), req)
	return result{http.StatusOK, res}, err
}

func (s *Server) listRoutes(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	res, err := s.Registry.List(r.Context(), &pb.Empty{})
	if err != nil {
		return result{}, err
	}
	routes := res.Routes
	if routes == nil {
		routes = []*pb.Route{}
	}
	return result{http.StatusOK, map[string]any{"routes": routes}}, nil
}

func (s *Server) invoke(r *http.Request, identity *pb.Identity, requestID string) (result, error) {
	service, region := r.PathValue("service"), r.Header.Get("X-Region")
	if !registry.ValidLabel(service) || (region != "" && !registry.ValidLabel(region)) {
		return result{}, status.Error(codes.InvalidArgument, "invalid service name or region")
	}
	var req struct {
		Message string `json:"message"`
	}
	if err := decode(r, &req); err != nil {
		return result{}, err
	}
	if len(req.Message) == 0 || len(req.Message) > 4096 {
		return result{}, status.Error(codes.InvalidArgument, "message must be 1 to 4096 bytes")
	}
	route, err := s.Registry.Get(r.Context(), &pb.RouteKey{Service: service})
	if err != nil {
		return result{}, err
	}
	backend := selectBackend(route, identity.Role, region)
	client, ok := s.Workloads[backend]
	if !ok {
		return result{}, status.Error(codes.Unavailable, "configured backend unavailable")
	}
	res, err := client.Invoke(r.Context(), &pb.Invocation{RequestId: requestID, Message: req.Message})
	return result{http.StatusOK, res}, err
}

func selectBackend(route *pb.Route, role, region string) string {
	for _, rule := range route.Rules {
		if (rule.Role == "" || rule.Role == role) && (rule.Region == "" || rule.Region == region) {
			return rule.Backend
		}
	}
	return route.DefaultBackend
}

func (s *Server) notify(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	var req pb.Notification
	if err := decode(r, &req); err != nil {
		return result{}, err
	}
	_, err := s.Notify.Send(r.Context(), &req)
	return result{http.StatusNoContent, nil}, err
}

func (s *Server) listAudit(r *http.Request, _ *pb.Identity, _ string) (result, error) {
	res, err := s.Audit.List(r.Context(), &pb.Empty{})
	if err != nil {
		return result{}, err
	}
	events := res.Events
	if events == nil {
		events = []*pb.AuditEvent{}
	}
	return result{http.StatusOK, map[string]any{"events": events}}, nil
}

func errorResult(err error) result {
	st := status.FromContextError(err)
	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		st = status.Convert(err)
	}
	code, message := http.StatusInternalServerError, "internal error"
	switch st.Code() {
	case codes.InvalidArgument:
		code, message = http.StatusBadRequest, st.Message()
	case codes.Unauthenticated:
		code, message = http.StatusUnauthorized, st.Message()
	case codes.PermissionDenied:
		code, message = http.StatusForbidden, st.Message()
	case codes.NotFound:
		code, message = http.StatusNotFound, st.Message()
	case codes.AlreadyExists:
		code, message = http.StatusConflict, st.Message()
	case codes.ResourceExhausted:
		code, message = http.StatusRequestEntityTooLarge, "request body too large"
	case codes.Unavailable:
		code, message = http.StatusServiceUnavailable, "upstream unavailable"
	case codes.DeadlineExceeded, codes.Canceled:
		code, message = http.StatusGatewayTimeout, "upstream timed out"
	}
	return result{code, map[string]string{"error": message}}
}
