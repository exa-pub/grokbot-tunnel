package fakesand

import (
	"net/http"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/proto"
)

// The calls of the user-computer channel, as Call.RPC names them.
const (
	Watch    = "Watch"
	Poll     = "Poll"
	Submit   = "Submit"
	Withdraw = "Withdraw"
)

// Call is one call of the user-computer channel as the backend saw it.
type Call struct {
	RPC     string
	Header  http.Header
	Request proto.Message
}

// Calls returns the calls of the user-computer channel so far, in the order
// they came.
func (s *Server) Calls() []Call {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Call(nil), s.calls...)
}

// Fail makes the next n calls of rpc fail with code instead of being served.
func (s *Server) Fail(rpc string, n int, code connect.Code) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failures == nil {
		s.failures = map[string][]connect.Code{}
	}
	for range n {
		s.failures[rpc] = append(s.failures[rpc], code)
	}
}

// DropWatch ends every open watch stream, as a backend that restarts does.
func (s *Server) DropWatch() {
	s.mu.Lock()
	defer s.mu.Unlock()
	close(s.drop)
	s.drop = make(chan struct{})
}

// enter records a call and returns the failure it was told to answer with,
// if any.
func (s *Server) enter(rpc string, header http.Header, request proto.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, Call{RPC: rpc, Header: header.Clone(), Request: proto.Clone(request)})
	s.signal()
	if codes := s.failures[rpc]; len(codes) > 0 {
		s.failures[rpc] = codes[1:]
		return connect.NewError(codes[0], nil)
	}
	return nil
}
