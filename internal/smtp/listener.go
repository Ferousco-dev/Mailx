package smtp

import (
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/Ferousco-dev/mailx/internal/mail"
)

// Server admits and serves a bounded number of concurrent SMTP connections.
// Its configuration and admission slots belong only to this instance.
type Server struct {
	config Config
	sink   func(Session, mail.Message) error
	slots  chan struct{}
}

// NewServer creates an SMTP server with a finite per-instance connection cap.
func NewServer(config Config, sink func(Session, mail.Message) error) (*Server, error) {
	normalized, err := config.normalized()
	if err != nil {
		return nil, err
	}
	return &Server{
		config: normalized,
		sink:   sink,
		slots:  make(chan struct{}, normalized.MaxConnections),
	}, nil
}

// Serve accepts connections until listener closes or returns an error.
// Admission is decided before an SMTP handler goroutine is created.
func (s *Server) Serve(listener net.Listener) error {
	var backoff time.Duration
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			var netErr net.Error
			if errors.As(err, &netErr) && netErr.Timeout() {
				if backoff == 0 {
					backoff = 5 * time.Millisecond
				} else if backoff < time.Second {
					backoff *= 2
				}
				time.Sleep(backoff)
				continue
			}
			return fmt.Errorf("accept SMTP connection: %w", err)
		}
		backoff = 0
		if !s.tryAcquire() {
			// Immediate close keeps overload handling non-blocking and prevents
			// rejected connections from creating more goroutines or wait queues.
			_ = conn.Close()
			continue
		}
		go s.handle(conn)
	}
}

func (s *Server) tryAcquire() bool {
	select {
	case s.slots <- struct{}{}:
		return true
	default:
		return false
	}
}

func (s *Server) handle(conn net.Conn) {
	defer func() { <-s.slots }()
	_ = HandleConnectionWithConfig(conn, s.config, s.sink)
}
