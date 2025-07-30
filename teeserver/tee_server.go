package teeserver

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"

	"github.com/jianlinjiang/trusted-launcher/agent"
	"github.com/jianlinjiang/trusted-launcher/internal/logging"
	"github.com/jianlinjiang/trusted-launcher/spec"
)

// TeeServer is a server that can be called from a container through a unix
// socket file.
type TeeServer struct {
	server      *http.Server
	netListener net.Listener
}

type attestHandler struct {
	ctx         context.Context
	attestAgent agent.AttestationAgent
	// defaultTokenFile string
	logger     logging.Logger
	launchSpec spec.LaunchSpec
}

// New takes in a socket and start to listen to it, and create a server
func New(ctx context.Context, unixSock string, a agent.AttestationAgent, logger logging.Logger, launchSpec spec.LaunchSpec) (*TeeServer, error) {
	var err error
	nl, err := net.Listen("unix", unixSock)
	if err != nil {
		return nil, fmt.Errorf("cannot listen to the socket [%s]: %v", unixSock, err)
	}

	teeServer := TeeServer{
		netListener: nl,
		server: &http.Server{
			Handler: (&attestHandler{
				ctx:         ctx,
				attestAgent: a,
				logger:      logger,
				launchSpec:  launchSpec,
			}).Handler(),
		},
	}
	return &teeServer, nil
}

// Handler creates a multiplexer for the server.
func (a *attestHandler) Handler() http.Handler {
	mux := http.NewServeMux()
	// to test default token: curl --unix-socket <socket> http://localhost/v1/token
	// to test custom token:
	// curl -d '{"audience":"<aud>", "nonces":["<nonce1>"]}' -H "Content-Type: application/json" -X POST
	//   --unix-socket /tmp/container_launcher/teeserver.sock http://localhost/v1/token

	mux.HandleFunc("/v1/attest", a.getAttestationReport)
	return mux
}

func (a *attestHandler) getAttestationReport(w http.ResponseWriter, r *http.Request) {
	nonceStr := r.URL.Query().Get("nonce")
	if nonceStr == "" {
		http.Error(w, "missing 'nonce' query parameter", http.StatusBadRequest)
		return
	}

	decodedNonce, err := base64.URLEncoding.DecodeString(nonceStr)
	if err != nil {
		http.Error(w, "invalid 'nonce' format: not a base64 string", http.StatusBadRequest)
		return
	}

	if len(decodedNonce) != 64 {
		http.Error(w, fmt.Sprintf("invalid 'nonce' length: expected 64 bytes, got %d", len(decodedNonce)), http.StatusBadRequest)
		return
	}

	var nonce [64]byte
	copy(nonce[:], decodedNonce)

	report, err := a.attestAgent.Attest(a.ctx, nonce)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get attestation report: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	if _, err := w.Write(report); err != nil {
		http.Error(w, fmt.Sprintf("failed to write response: %v", err), http.StatusInternalServerError)
	}
}

// Serve starts the server, will block until the server shutdown.
func (s *TeeServer) Serve() error {
	return s.server.Serve(s.netListener)
}

// Shutdown will terminate the server and the underlying listener.
func (s *TeeServer) Shutdown(ctx context.Context) error {
	err := s.server.Shutdown(ctx)
	err2 := s.netListener.Close()

	if err != nil {
		return err
	}
	if err2 != nil {
		return err2
	}
	return nil
}
