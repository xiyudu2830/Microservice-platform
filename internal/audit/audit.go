package audit

import (
	"context"
	"time"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	pb.UnimplementedAuditServer
	collection *mongo.Collection
}

type eventDocument struct {
	RequestID string    `bson:"request_id"`
	ActorID   string    `bson:"actor_id"`
	Method    string    `bson:"method"`
	Path      string    `bson:"path"`
	Status    int32     `bson:"status"`
	At        time.Time `bson:"at"`
	Duration  int64     `bson:"duration_ms"`
}

func New(ctx context.Context, collection *mongo.Collection) (*Server, error) {
	_, err := collection.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "at", Value: 1}},
		Options: options.Index().SetExpireAfterSeconds(7 * 24 * 60 * 60),
	})
	if err != nil {
		return nil, err
	}
	return &Server{collection: collection}, nil
}

func (s *Server) Record(ctx context.Context, event *pb.AuditEvent) (*pb.Empty, error) {
	_, err := s.collection.InsertOne(ctx, eventDocument{
		RequestID: event.RequestId, ActorID: event.ActorId, Method: event.Method,
		Path: event.Path, Status: event.Status, At: time.UnixMilli(event.UnixMillis).UTC(),
		Duration: event.DurationMillis,
	})
	if err != nil {
		return nil, status.Error(codes.Unavailable, "audit store unavailable")
	}
	return &pb.Empty{}, nil
}

func (s *Server) List(ctx context.Context, _ *pb.Empty) (*pb.AuditEvents, error) {
	cursor, err := s.collection.Find(ctx, bson.D{},
		options.Find().SetSort(bson.D{{Key: "at", Value: -1}, {Key: "_id", Value: -1}}).SetLimit(100))
	if err != nil {
		return nil, status.Error(codes.Unavailable, "audit store unavailable")
	}
	var docs []eventDocument
	if err := cursor.All(ctx, &docs); err != nil {
		return nil, status.Error(codes.Unavailable, "audit store unavailable")
	}
	res := &pb.AuditEvents{Events: make([]*pb.AuditEvent, 0, len(docs))}
	for _, doc := range docs {
		res.Events = append(res.Events, &pb.AuditEvent{
			RequestId: doc.RequestID, ActorId: doc.ActorID, Method: doc.Method, Path: doc.Path,
			Status: doc.Status, UnixMillis: doc.At.UnixMilli(), DurationMillis: doc.Duration,
		})
	}
	return res, nil
}
