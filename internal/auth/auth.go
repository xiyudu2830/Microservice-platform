package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	_ "embed"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"golang.org/x/crypto/bcrypt"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

//go:embed schema.sql
var schema string

const passwordCost = 12

type Server struct {
	pb.UnimplementedAuthServer
	db        *pgxpool.Pool
	dummyHash []byte
}

func New(ctx context.Context, db *pgxpool.Pool, bootstrapEmail, bootstrapPassword string) (*Server, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte("unused dummy password"), passwordCost)
	if err != nil {
		return nil, err
	}
	// Serialize DDL across replicas, including the initial install.
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(820301)"); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, schema); err != nil {
		return nil, err
	}
	if bootstrapEmail != "" || bootstrapPassword != "" {
		email, err := validateUser(bootstrapEmail, bootstrapPassword, "admin")
		if err != nil {
			return nil, err
		}
		var existingRole string
		err = tx.QueryRow(ctx, "SELECT role FROM users WHERE email = $1", email).Scan(&existingRole)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			passwordHash, err := bcrypt.GenerateFromPassword([]byte(bootstrapPassword), passwordCost)
			if err != nil {
				return nil, err
			}
			if _, err := tx.Exec(ctx, `INSERT INTO users (id,email,password_hash,role)
				VALUES (gen_random_uuid(),$1,$2,'admin')`, email, passwordHash); err != nil {
				return nil, err
			}
		case err != nil:
			return nil, err
		case existingRole != "admin":
			return nil, errors.New("bootstrap email belongs to a non-admin account")
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return &Server{db: db, dummyHash: hash}, nil
}

func validateUser(email, password, role string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	address, err := mail.ParseAddress(email)
	if err != nil || address.Address != email || len(email) > 254 {
		return "", status.Error(codes.InvalidArgument, "enter a valid email address")
	}
	if len(password) < 12 || len(password) > 72 {
		return "", status.Error(codes.InvalidArgument, "password must be 12 to 72 bytes")
	}
	if role != "admin" && role != "viewer" {
		return "", status.Error(codes.InvalidArgument, "role must be admin or viewer")
	}
	return email, nil
}

func (s *Server) CreateUser(ctx context.Context, req *pb.CreateUserRequest) (*pb.Identity, error) {
	email, err := validateUser(req.Email, req.Password, req.Role)
	if err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), passwordCost)
	if err != nil {
		return nil, status.Error(codes.Internal, "password hashing failed")
	}
	identity := &pb.Identity{Role: req.Role}
	err = s.db.QueryRow(ctx, `INSERT INTO users (id,email,password_hash,role)
		VALUES (gen_random_uuid(),$1,$2,$3) RETURNING id::text`, email, hash, req.Role).Scan(&identity.Id)
	var pgErr *pgconn.PgError
	switch {
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		return nil, status.Error(codes.AlreadyExists, "user already exists")
	case err != nil:
		return nil, status.Error(codes.Unavailable, "user store unavailable")
	default:
		return identity, nil
	}
}

func (s *Server) Login(ctx context.Context, req *pb.LoginRequest) (*pb.Session, error) {
	if len(req.Email) > 254 || len(req.Password) > 72 || req.Password == "" {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}
	email := strings.ToLower(strings.TrimSpace(req.Email))
	identity := &pb.Identity{}
	var hash []byte
	err := s.db.QueryRow(ctx, "SELECT id::text,role,password_hash FROM users WHERE email=$1", email).
		Scan(&identity.Id, &identity.Role, &hash)
	missing := errors.Is(err, pgx.ErrNoRows)
	switch {
	case missing:
		hash = s.dummyHash
	case err != nil:
		return nil, status.Error(codes.Unavailable, "user store unavailable")
	}
	if bcrypt.CompareHashAndPassword(hash, []byte(req.Password)) != nil || missing {
		return nil, status.Error(codes.Unauthenticated, "invalid credentials")
	}

	raw := make([]byte, 32)
	rand.Read(raw)
	token := base64.RawURLEncoding.EncodeToString(raw)
	tokenHash := sha256.Sum256(raw)
	session := &pb.Session{Token: token, Identity: identity}
	err = s.db.QueryRow(ctx, `INSERT INTO sessions (token_hash,user_id,expires_at)
		VALUES ($1,$2::uuid,CURRENT_TIMESTAMP + interval '1 hour')
		RETURNING floor(extract(epoch FROM expires_at))::bigint`, tokenHash[:], identity.Id).
		Scan(&session.ExpiresUnix)
	if err != nil {
		return nil, status.Error(codes.Unavailable, "session store unavailable")
	}
	return session, nil
}

func (s *Server) Verify(ctx context.Context, req *pb.VerifyRequest) (*pb.Identity, error) {
	if len(req.Token) != base64.RawURLEncoding.EncodedLen(32) {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired session")
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(req.Token)
	if err != nil {
		return nil, status.Error(codes.Unauthenticated, "invalid or expired session")
	}
	hash := sha256.Sum256(raw)
	identity := &pb.Identity{}
	err = s.db.QueryRow(ctx, `SELECT u.id::text,u.role FROM sessions s
		JOIN users u ON u.id=s.user_id WHERE s.token_hash=$1 AND s.expires_at>CURRENT_TIMESTAMP`,
		hash[:]).Scan(&identity.Id, &identity.Role)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		return nil, status.Error(codes.Unauthenticated, "invalid or expired session")
	case err != nil:
		return nil, status.Error(codes.Unavailable, "session store unavailable")
	default:
		return identity, nil
	}
}

func (s *Server) PruneSessions(ctx context.Context) error {
	_, err := s.db.Exec(ctx, "DELETE FROM sessions WHERE expires_at <= CURRENT_TIMESTAMP")
	return err
}
