package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

// ReadFile reads a file from the VM filesystem.
func (s *Server) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	path := resolvePath(req.Path)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return &pb.ReadFileResponse{Content: data}, nil
}

// WriteFile writes content to a file in the VM filesystem.
func (s *Server) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	path := resolvePath(req.Path)

	// Ensure parent directory exists
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", dir, err)
	}

	mode := os.FileMode(req.Mode)
	if mode == 0 {
		mode = 0644
	}

	if err := os.WriteFile(path, req.Content, mode); err != nil {
		return nil, fmt.Errorf("write %s: %w", path, err)
	}
	return &pb.WriteFileResponse{}, nil
}

// ListDir lists entries in a directory.
func (s *Server) ListDir(ctx context.Context, req *pb.ListDirRequest) (*pb.ListDirResponse, error) {
	path := resolvePath(req.Path)
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("readdir %s: %w", path, err)
	}

	result := make([]*pb.DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		result = append(result, &pb.DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
			Size:  info.Size(),
			Path:  filepath.Join(path, e.Name()),
		})
	}
	return &pb.ListDirResponse{Entries: result}, nil
}

// MakeDir creates a directory (with parents).
func (s *Server) MakeDir(ctx context.Context, req *pb.MakeDirRequest) (*pb.MakeDirResponse, error) {
	path := resolvePath(req.Path)
	if err := os.MkdirAll(path, 0755); err != nil {
		return nil, fmt.Errorf("mkdir %s: %w", path, err)
	}
	return &pb.MakeDirResponse{}, nil
}

// Remove removes a file or directory (recursively).
func (s *Server) Remove(ctx context.Context, req *pb.RemoveRequest) (*pb.RemoveResponse, error) {
	path := resolvePath(req.Path)
	if err := os.RemoveAll(path); err != nil {
		return nil, fmt.Errorf("remove %s: %w", path, err)
	}
	return &pb.RemoveResponse{}, nil
}

// Exists checks if a path exists.
func (s *Server) Exists(ctx context.Context, req *pb.ExistsRequest) (*pb.ExistsResponse, error) {
	path := resolvePath(req.Path)
	_, err := os.Stat(path)
	return &pb.ExistsResponse{Exists: err == nil}, nil
}

// Stat returns file metadata.
func (s *Server) Stat(ctx context.Context, req *pb.StatRequest) (*pb.StatResponse, error) {
	path := resolvePath(req.Path)
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat %s: %w", path, err)
	}
	return &pb.StatResponse{
		Name:    info.Name(),
		IsDir:   info.IsDir(),
		Size:    info.Size(),
		Mode:    info.Mode().String(),
		ModTime: info.ModTime().UTC().Format("2006-01-02T15:04:05Z"),
		Path:    path,
	}, nil
}

// resolvePath ensures paths are rooted in /workspace for relative paths.
func resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join("/workspace", path)
}
