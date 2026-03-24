package agent

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"

	pb "github.com/opensandbox/opensandbox/proto/agent"
)

const streamChunkSize = 256 * 1024 // 256KB per gRPC chunk

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

// ReadFileStream streams a file in 256KB chunks over gRPC.
func (s *Server) ReadFileStream(req *pb.ReadFileStreamRequest, stream pb.SandboxAgent_ReadFileStreamServer) error {
	path := resolvePath(req.Path)
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	buf := make([]byte, streamChunkSize)
	first := true
	for {
		n, err := f.Read(buf)
		if n > 0 {
			chunk := &pb.FileChunk{Data: buf[:n]}
			if first {
				chunk.TotalSize = info.Size()
				first = false
			}
			if sendErr := stream.Send(chunk); sendErr != nil {
				return fmt.Errorf("send chunk: %w", sendErr)
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	return nil
}

// WriteFileStream receives a file in chunks over gRPC and writes it to disk.
func (s *Server) WriteFileStream(stream pb.SandboxAgent_WriteFileStreamServer) error {
	// First message carries path and mode
	msg, err := stream.Recv()
	if err != nil {
		return fmt.Errorf("recv first chunk: %w", err)
	}

	path := resolvePath(msg.Path)
	mode := os.FileMode(msg.Mode)
	if mode == 0 {
		mode = 0644
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}

	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return fmt.Errorf("create %s: %w", path, err)
	}
	defer f.Close()

	var total int64
	// Write first message's data
	if len(msg.Data) > 0 {
		n, err := f.Write(msg.Data)
		if err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		total += int64(n)
	}

	// Receive remaining chunks
	for {
		msg, err := stream.Recv()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("recv chunk: %w", err)
		}
		if len(msg.Data) > 0 {
			n, err := f.Write(msg.Data)
			if err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			total += int64(n)
		}
	}

	return stream.SendAndClose(&pb.WriteFileStreamResponse{BytesWritten: total})
}

// resolvePath ensures paths are rooted in /root for relative paths.
func resolvePath(path string) string {
	if filepath.IsAbs(path) {
		return filepath.Clean(path)
	}
	return filepath.Join("/root", path)
}
