package api

import (
	"context"
	"log"
	"net/http"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/pkg/types"
	pb "github.com/opensandbox/opensandbox/proto/worker"
)

func (s *Server) runCommand(c echo.Context) error {
	id := c.Param("id")

	// Server mode: dispatch to worker via gRPC
	if s.workerRegistry != nil {
		return s.runCommandRemote(c, id)
	}

	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	var cfg types.ProcessConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	if cfg.Command == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "cmd is required",
		})
	}

	var result *types.ProcessResult
	var execErr error

	start := time.Now()

	routeOp := func(ctx context.Context) error {
		result, execErr = s.manager.Exec(ctx, id, cfg)
		return execErr
	}

	// Route through sandbox router (handles auto-wake, rolling timeout reset)
	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "exec", routeOp); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
		}
	} else {
		if err := routeOp(c.Request().Context()); err != nil {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"error": err.Error(),
			})
		}
	}

	durationMs := int(time.Since(start).Milliseconds())

	// Log command to per-sandbox SQLite
	if s.sandboxDBs != nil {
		sdb, dbErr := s.sandboxDBs.Get(id)
		if dbErr == nil {
			stdoutLen := len(result.Stdout)
			stderrLen := len(result.Stderr)
			_ = sdb.LogCommand(cfg.Command, cfg.Args, cfg.Cwd, result.ExitCode, durationMs, stdoutLen, stderrLen)
		}
	}

	return c.JSON(http.StatusOK, result)
}

func (s *Server) runCommandRemote(c echo.Context, sandboxID string) error {
	if s.store == nil {
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "database not configured",
		})
	}

	var cfg types.ProcessConfig
	if err := c.Bind(&cfg); err != nil {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "invalid request body: " + err.Error(),
		})
	}

	if cfg.Command == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "cmd is required",
		})
	}

	// Look up which worker has this sandbox
	session, err := s.store.GetSandboxSession(c.Request().Context(), sandboxID)
	if err != nil {
		return c.JSON(http.StatusNotFound, map[string]string{
			"error": "sandbox not found",
		})
	}

	client, err := s.workerRegistry.GetWorkerClient(session.WorkerID)
	if err != nil {
		log.Printf("commands: worker %s unreachable for exec: %v", session.WorkerID, err)
		return c.JSON(http.StatusServiceUnavailable, map[string]string{
			"error": "worker unreachable",
		})
	}

	timeout := 30 * time.Second
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}
	grpcCtx, cancel := context.WithTimeout(c.Request().Context(), timeout)
	defer cancel()

	resp, err := client.ExecCommand(grpcCtx, &pb.ExecCommandRequest{
		SandboxId: sandboxID,
		Command:   cfg.Command,
		Args:      cfg.Args,
		Envs:      cfg.Env,
		Cwd:       cfg.Cwd,
		Timeout:   int32(cfg.Timeout),
	})
	if err != nil {
		log.Printf("commands: gRPC exec failed for %s: %v", sandboxID, err)
		return c.JSON(http.StatusInternalServerError, map[string]string{
			"error": err.Error(),
		})
	}

	result := &types.ProcessResult{
		ExitCode: int(resp.ExitCode),
		Stdout:   resp.Stdout,
		Stderr:   resp.Stderr,
	}

	return c.JSON(http.StatusOK, result)
}
