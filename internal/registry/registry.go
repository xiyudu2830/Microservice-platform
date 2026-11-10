package registry

import (
	"context"
	"errors"
	"regexp"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var label = regexp.MustCompile(`^[a-z][a-z0-9-]{0,39}$`)

type Server struct {
	pb.UnimplementedRegistryServer
	collection *mongo.Collection
	backends   map[string]string
}

type ruleDocument struct {
	Role    string `bson:"role"`
	Region  string `bson:"region"`
	Backend string `bson:"backend"`
}

type routeDocument struct {
	Service        string         `bson:"_id"`
	DefaultBackend string         `bson:"default_backend"`
	Rules          []ruleDocument `bson:"rules"`
}

func New(collection *mongo.Collection, backends map[string]string) *Server {
	return &Server{collection: collection, backends: backends}
}

func ValidLabel(value string) bool {
	return label.MatchString(value)
}

func Validate(route *pb.Route, backends map[string]string) error {
	if !ValidLabel(route.Service) {
		return status.Error(codes.InvalidArgument, "invalid service name")
	}
	if _, ok := backends[route.DefaultBackend]; !ok {
		return status.Error(codes.InvalidArgument, "unknown default backend")
	}
	if len(route.Rules) > 32 {
		return status.Error(codes.InvalidArgument, "a route can have at most 32 rules")
	}
	type match struct{ role, region string }
	seen := make(map[match]bool, len(route.Rules))
	for _, rule := range route.Rules {
		if rule == nil || (rule.Role != "" && rule.Role != "admin" && rule.Role != "viewer") ||
			(rule.Region != "" && !ValidLabel(rule.Region)) || (rule.Role == "" && rule.Region == "") {
			return status.Error(codes.InvalidArgument, "a rule needs a valid role or region")
		}
		if _, ok := backends[rule.Backend]; !ok {
			return status.Error(codes.InvalidArgument, "unknown rule backend")
		}
		key := match{rule.Role, rule.Region}
		if seen[key] {
			return status.Error(codes.InvalidArgument, "duplicate rule match")
		}
		seen[key] = true
	}
	return nil
}

func (s *Server) Put(ctx context.Context, req *pb.Route) (*pb.Route, error) {
	if err := Validate(req, s.backends); err != nil {
		return nil, err
	}
	doc := routeDocument{Service: req.Service, DefaultBackend: req.DefaultBackend}
	for _, rule := range req.Rules {
		doc.Rules = append(doc.Rules, ruleDocument{Role: rule.Role, Region: rule.Region, Backend: rule.Backend})
	}
	_, err := s.collection.ReplaceOne(ctx, bson.M{"_id": req.Service}, doc, options.Replace().SetUpsert(true))
	if mongo.IsDuplicateKeyError(err) {
		// A concurrent first writer may have inserted the same key.
		_, err = s.collection.ReplaceOne(ctx, bson.M{"_id": req.Service}, doc)
	}
	if err != nil {
		return nil, status.Error(codes.Unavailable, "registry store unavailable")
	}
	return req, nil
}

func (s *Server) Get(ctx context.Context, req *pb.RouteKey) (*pb.Route, error) {
	var doc routeDocument
	err := s.collection.FindOne(ctx, bson.M{"_id": req.Service}).Decode(&doc)
	switch {
	case errors.Is(err, mongo.ErrNoDocuments):
		return nil, status.Error(codes.NotFound, "service is not registered")
	case err != nil:
		return nil, status.Error(codes.Unavailable, "registry store unavailable")
	default:
		return doc.route(), nil
	}
}

func (s *Server) List(ctx context.Context, _ *pb.Empty) (*pb.Routes, error) {
	cursor, err := s.collection.Find(ctx, bson.D{}, options.Find().SetSort(bson.D{{Key: "_id", Value: 1}}).SetLimit(100))
	if err != nil {
		return nil, status.Error(codes.Unavailable, "registry store unavailable")
	}
	var docs []routeDocument
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, status.Error(codes.Unavailable, "registry store unavailable")
	}
	res := &pb.Routes{Routes: make([]*pb.Route, 0, len(docs))}
	for _, doc := range docs {
		res.Routes = append(res.Routes, doc.route())
	}
	return res, nil
}

func (doc routeDocument) route() *pb.Route {
	route := &pb.Route{Service: doc.Service, DefaultBackend: doc.DefaultBackend, Rules: make([]*pb.Rule, 0, len(doc.Rules))}
	for _, rule := range doc.Rules {
		route.Rules = append(route.Rules, &pb.Rule{Role: rule.Role, Region: rule.Region, Backend: rule.Backend})
	}
	return route
}
