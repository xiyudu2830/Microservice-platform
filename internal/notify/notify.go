package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"

	pb "github.com/xiyudu2830/Microservice-platform/api/governance/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	pb.UnimplementedNotifyServer
	address, host, from string
	username, password  string
	requireTLS          bool
}

func New(address, from, username, password string, requireTLS bool) (*Server, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil || host == "" || !plainAddress(from) || (username == "") != (password == "") {
		return nil, errors.New("invalid SMTP address, sender, or credentials")
	}
	return &Server{address: address, host: host, from: from, username: username, password: password,
		requireTLS: requireTLS || username != ""}, nil
}

func plainAddress(value string) bool {
	address, err := mail.ParseAddress(value)
	return err == nil && address.Address == value && len(value) <= 254 &&
		!strings.ContainsAny(value, "\r\n")
}

func (s *Server) Send(ctx context.Context, req *pb.Notification) (*pb.Empty, error) {
	if !plainAddress(req.To) || strings.TrimSpace(req.Subject) == "" || len(req.Subject) > 200 ||
		strings.ContainsAny(req.Subject, "\r\n\x00") || len(req.Body) == 0 || len(req.Body) > 8192 {
		return nil, status.Error(codes.InvalidArgument, "invalid email address, subject or body")
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := s.deliver(ctx, req); err != nil {
		// SMTP replies may contain email addresses or message contents.
		slog.Warn("SMTP delivery failed", "error_type", fmt.Sprintf("%T", err))
		if ctx.Err() != nil {
			return nil, status.FromContextError(ctx.Err()).Err()
		}
		return nil, status.Error(codes.Unavailable, "SMTP delivery failed")
	}
	return &pb.Empty{}, nil
}

func (s *Server) deliver(ctx context.Context, req *pb.Notification) error {
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", s.address)
	if err != nil {
		return err
	}
	defer conn.Close()
	deadline, _ := ctx.Deadline()
	if err := conn.SetDeadline(deadline); err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()

	client, err := smtp.NewClient(conn, s.host)
	if err != nil {
		return err
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		if err := client.StartTLS(&tls.Config{ServerName: s.host, MinVersion: tls.VersionTLS12}); err != nil {
			return err
		}
	} else if s.requireTLS {
		return errors.New("SMTP server does not support required STARTTLS")
	}
	if s.username != "" {
		if err := client.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return err
		}
	}
	if err := client.Mail(s.from); err != nil {
		return err
	}
	if err := client.Rcpt(req.To); err != nil {
		return err
	}
	writer, err := client.Data()
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(writer, "From: %s\r\nTo: %s\r\nSubject: %s\r\nDate: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\n"+
		"Content-Transfer-Encoding: quoted-printable\r\n\r\n",
		s.from, req.To, mime.QEncoding.Encode("UTF-8", req.Subject), time.Now().UTC().Format(time.RFC1123Z))
	if err != nil {
		return err
	}
	body := quotedprintable.NewWriter(writer)
	if _, err := body.Write([]byte(req.Body)); err != nil {
		return err
	}
	if err := body.Close(); err != nil {
		return err
	}
	// The DATA reply acknowledges acceptance. A subsequent QUIT error must
	// not turn an accepted message into a failed delivery.
	if err := writer.Close(); err != nil {
		return err
	}
	_ = client.Quit()
	return nil
}
