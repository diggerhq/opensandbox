package api

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/labstack/echo/v4"
	"github.com/opensandbox/opensandbox/pkg/types"
)

func (s *Server) readFile(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "path query parameter is required",
		})
	}

	var reader io.ReadCloser
	var totalSize int64

	routeOp := func(ctx context.Context) error {
		var err error
		reader, totalSize, err = s.manager.ReadFileStream(ctx, id, path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "readFile", routeOp); err != nil {
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
	defer reader.Close()

	resp := c.Response()
	resp.Header().Set("Content-Type", "application/octet-stream")
	if totalSize > 0 {
		resp.Header().Set("Content-Length", fmt.Sprintf("%d", totalSize))
	}
	resp.WriteHeader(http.StatusOK)
	_, err := io.Copy(resp.Writer, reader)
	return err
}

func (s *Server) writeFile(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "path query parameter is required",
		})
	}

	routeOp := func(ctx context.Context) error {
		_, err := s.manager.WriteFileStream(ctx, id, path, 0644, c.Request().Body)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "writeFile", routeOp); err != nil {
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

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) listDir(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		path = "/"
	}

	var entries []types.EntryInfo

	routeOp := func(ctx context.Context) error {
		var err error
		entries, err = s.manager.ListDir(ctx, id, path)
		return err
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "listDir", routeOp); err != nil {
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

	return c.JSON(http.StatusOK, entries)
}

func (s *Server) makeDir(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "path query parameter is required",
		})
	}

	routeOp := func(ctx context.Context) error {
		return s.manager.MakeDir(ctx, id, path)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "makeDir", routeOp); err != nil {
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

	return c.NoContent(http.StatusNoContent)
}

func (s *Server) removeFile(c echo.Context) error {
	if s.manager == nil {
		return c.JSON(http.StatusServiceUnavailable, errSandboxNotAvailable)
	}

	id := c.Param("id")
	path := c.QueryParam("path")
	if path == "" {
		return c.JSON(http.StatusBadRequest, map[string]string{
			"error": "path query parameter is required",
		})
	}

	routeOp := func(ctx context.Context) error {
		return s.manager.Remove(ctx, id, path)
	}

	if s.router != nil {
		if err := s.router.Route(c.Request().Context(), id, "removeFile", routeOp); err != nil {
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

	return c.NoContent(http.StatusNoContent)
}
