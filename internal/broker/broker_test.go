package broker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"google.golang.org/grpc"
)

func TestSelectBackend(t *testing.T) {
	route := &pb.Route{
		DefaultBackend: "standard",
		Rules: []*pb.Rule{
			{Role: "viewer", Region: "us-east", Backend: "priority"},
			{Region: "us-east", Backend: "standard"},
			{Role: "admin", Backend: "priority"},
		},
	}
	for _, test := range []struct{ name, role, region, want string }{
		{"both fields match", "viewer", "us-east", "priority"},
		{"first of two matches", "admin", "us-east", "standard"},
		{"role wildcard on region", "admin", "", "priority"},
		{"no match", "viewer", "eu-west", "standard"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := selectBackend(route, test.role, test.region); got != test.want {
				t.Fatalf("backend = %q, want %q", got, test.want)
			}
		})
	}
}

type authClient struct{ pb.AuthClient }

func (authClient) Verify(context.Context, *pb.VerifyRequest, ...grpc.CallOption) (*pb.Identity, error) {
	return &pb.Identity{Id: "user-id", Role: "viewer"}, nil
}

type auditClient struct {
	pb.AuditClient
	event *pb.AuditEvent
}

func (a *auditClient) Record(_ context.Context, event *pb.AuditEvent, _ ...grpc.CallOption) (*pb.Empty, error) {
	a.event = event
	return &pb.Empty{}, nil
}

type registryClient struct{ pb.RegistryClient }

func (registryClient) Get(context.Context, *pb.RouteKey, ...grpc.CallOption) (*pb.Route, error) {
	return &pb.Route{DefaultBackend: "standard", Rules: []*pb.Rule{
		{Role: "viewer", Region: "us-east", Backend: "priority"},
	}}, nil
}

type workloadClient struct{ pb.WorkloadClient }

func (workloadClient) Invoke(_ context.Context, req *pb.Invocation, _ ...grpc.CallOption) (*pb.InvocationResult, error) {
	return &pb.InvocationResult{RequestId: req.RequestId, Backend: "priority", Message: req.Message}, nil
}

func TestBrokerAuthorizationRoutingAndAudit(t *testing.T) {
	audit := &auditClient{}
	s := &Server{Auth: authClient{}, Audit: audit, Registry: registryClient{},
		Workloads: map[string]pb.WorkloadClient{"priority": workloadClient{}}}
	handler := s.Handler()

	send := func(method, path, token, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("X-Region", "us-east")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		return res
	}

	if res := send("POST", "/v1/invoke/demo", "", `{"message":"hello"}`); res.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated invocation status = %d", res.Code)
	}
	if res := send("PUT", "/v1/routes/demo", "viewer-token", `{}`); res.Code != http.StatusForbidden {
		t.Fatalf("viewer write status = %d", res.Code)
	}
	if audit.event.ActorId != "user-id" || audit.event.Status != http.StatusForbidden {
		t.Fatalf("denied request was not attributed: %v", audit.event)
	}

	res := send("POST", "/v1/invoke/demo", "viewer-token", `{"message":"hello"}`)
	var body pb.InvocationResult
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if res.Code != http.StatusOK || body.Backend != "priority" || body.Message != "hello" {
		t.Fatalf("unexpected invocation response: %d %s", res.Code, res.Body)
	}
	if body.RequestId == "" || res.Header().Get("X-Request-Id") != body.RequestId ||
		audit.event.RequestId != body.RequestId || audit.event.Path != "/v1/invoke/{service}" {
		t.Fatalf("request and audit did not correlate: %v", audit.event)
	}
}
