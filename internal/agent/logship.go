package agent

import (
	"context"
	"log"

	"github.com/opensandbox/opensandbox/internal/logship"
	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// LogshipConfig captures the parameters needed to ship sandbox session
// logs to the configured backend (Axiom). The forwarder reads this at
// flush time so it can stamp each event with the right sandbox/org IDs.
//
// IngestToken empty = log shipping disabled. The worker decides whether
// to call ConfigureLogship at all based on its own AXIOM_INGEST_TOKEN
// env; passing an empty token here is a defensive no-op path.
type LogshipConfig struct {
	IngestToken string
	Dataset     string
	SandboxID   string
	OrgID       string
}

// ConfigureLogship is called by the worker once per sandbox boot to
// hand the agent its log-shipping configuration. Phase 0 just stores
// the config; Phase 1 wires it into the forwarder's Activate path.
func (s *Server) ConfigureLogship(ctx context.Context, req *pb.ConfigureLogshipRequest) (*pb.ConfigureLogshipResponse, error) {
	cfg := LogshipConfig{
		IngestToken: req.IngestToken,
		Dataset:     req.Dataset,
		SandboxID:   req.SandboxId,
		OrgID:       req.OrgId,
	}
	s.logshipMu.Lock()
	s.logshipCfg = cfg
	s.logshipMu.Unlock()

	if cfg.IngestToken == "" {
		log.Printf("agent: ConfigureLogship received empty ingest_token (kill-switch); shipping disabled")
	} else {
		log.Printf("agent: ConfigureLogship received (sandbox_id=%s, dataset=%s)", cfg.SandboxID, cfg.Dataset)
		if s.Shipper != nil {
			s.Shipper.Activate(logship.Config{
				IngestToken: cfg.IngestToken,
				Dataset:     cfg.Dataset,
				SandboxID:   cfg.SandboxID,
				OrgID:       cfg.OrgID,
			})
		}
	}
	return &pb.ConfigureLogshipResponse{}, nil
}

// LogshipConfig returns the current log-shipping configuration.
// Returns the zero value if ConfigureLogship has not yet been called.
func (s *Server) GetLogshipConfig() LogshipConfig {
	s.logshipMu.RLock()
	defer s.logshipMu.RUnlock()
	return s.logshipCfg
}
