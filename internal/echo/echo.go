package echo

import (
	"context"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
)

type Server struct {
	pb.UnimplementedWorkloadServer
	Name string
}

func (s *Server) Invoke(_ context.Context, req *pb.Invocation) (*pb.InvocationResult, error) {
	return &pb.InvocationResult{RequestId: req.RequestId, Backend: s.Name, Message: req.Message}, nil
}
