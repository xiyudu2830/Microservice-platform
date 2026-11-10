package registry

import (
	"testing"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestValidateRuleTargets(t *testing.T) {
	backends := map[string]string{"standard": "echo-standard:9090", "priority": "echo-priority:9090"}
	for _, test := range []struct {
		name  string
		rules []*pb.Rule
		want  codes.Code
	}{
		{"valid", []*pb.Rule{{Region: "us-east", Backend: "priority"}}, codes.OK},
		{"arbitrary endpoint", []*pb.Rule{{Region: "us-east", Backend: "other.example:1234"}}, codes.InvalidArgument},
		{"duplicate predicate", []*pb.Rule{{Region: "us-east", Backend: "standard"}, {Region: "us-east", Backend: "priority"}}, codes.InvalidArgument},
		{"null rule", []*pb.Rule{nil}, codes.InvalidArgument},
		{"use default for unconditional rules", []*pb.Rule{{Backend: "priority"}}, codes.InvalidArgument},
	} {
		t.Run(test.name, func(t *testing.T) {
			route := &pb.Route{Service: "demo", DefaultBackend: "standard", Rules: test.rules}
			if got := status.Code(Validate(route, backends)); got != test.want {
				t.Fatalf("validation code = %s, want %s", got, test.want)
			}
		})
	}
}
