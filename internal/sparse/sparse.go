// Package sparse implements a block-level sparse file archive format.
//
// Large ext4 disk images (e.g., 20GB workspace.ext4) are mostly zeros (sparse).
// tar.zst compresses well but on extraction writes the full 20GB — slow.
// This format stores only non-zero 4KB blocks with their offsets, making both
// archive size and restore time proportional to actual content, not disk size.
//
// Format (.sparse.zst):
//   - All data is wrapped in a zstd stream
//   - Header: magic [8]byte "OSBSPAR1" + fileSize uint64 (little-endian)
//   - Blocks: repeated (offset uint64 + data [4096]byte) for each non-zero block
//   - EOF: end of zstd stream
//
// Restore:
//  1. truncate file to fileSize (instant — creates sparse file)
//  2. Read blocks, pwrite each at its offset (only writes real data)
package sparse

import (
	"encoding/binary"
	"fmt"
	"io"
	"log"
	"os"
	"time"

	"github.com/klauspost/compress/zstd"
)

const (
	BlockSize = 4096
	Magic     = "OSBSPAR1"
)

// Create scans srcPath for non-zero blocks and writes a sparse archive to dstPath.
// Returns the number of non-zero blocks written.
func Create(srcPath, dstPath string) (int, error) {
	src, err := os.Open(srcPath)
	if err != nil {
		return 0, fmt.Errorf("open source: %w", err)
	}
	defer src.Close()

	info, err := src.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat source: %w", err)
	}
	fileSize := uint64(info.Size())

	dst, err := os.Create(dstPath)
	if err != nil {
		return 0, fmt.Errorf("create archive: %w", err)
	}
	defer dst.Close()

	zw, err := zstd.NewWriter(dst, zstd.WithEncoderLevel(zstd.SpeedDefault))
	if err != nil {
		return 0, fmt.Errorf("create zstd writer: %w", err)
	}

	// Write header: magic + file size
	if _, err := zw.Write([]byte(Magic)); err != nil {
		zw.Close()
		return 0, fmt.Errorf("write magic: %w", err)
	}
	var sizeBuf [8]byte
	binary.LittleEndian.PutUint64(sizeBuf[:], fileSize)
	if _, err := zw.Write(sizeBuf[:]); err != nil {
		zw.Close()
		return 0, fmt.Errorf("write file size: %w", err)
	}

	// Scan blocks
	buf := make([]byte, BlockSize)
	var offsetBuf [8]byte
	blocks := 0
	var offset uint64

	for offset = 0; offset < fileSize; offset += BlockSize {
		n, err := io.ReadFull(src, buf)
		if err != nil && err != io.ErrUnexpectedEOF && err != io.EOF {
			zw.Close()
			return 0, fmt.Errorf("read block at offset %d: %w", offset, err)
		}
		if n == 0 {
			break
		}

		// Check if block is all zeros
		if isZero(buf[:n]) {
			continue
		}

		// Write offset + data
		binary.LittleEndian.PutUint64(offsetBuf[:], offset)
		if _, err := zw.Write(offsetBuf[:]); err != nil {
			zw.Close()
			return 0, fmt.Errorf("write offset: %w", err)
		}
		if _, err := zw.Write(buf[:n]); err != nil {
			zw.Close()
			return 0, fmt.Errorf("write block data: %w", err)
		}
		blocks++
	}

	if err := zw.Close(); err != nil {
		return 0, fmt.Errorf("close zstd: %w", err)
	}
	return blocks, nil
}

// Restore reads a sparse archive and reconstructs the original file at dstPath.
// The file is created as a sparse file (truncated to full size, only non-zero blocks written).
func Restore(archivePath, dstPath string) error {
	t0 := time.Now()

	src, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("open archive: %w", err)
	}
	defer src.Close()

	zr, err := zstd.NewReader(src)
	if err != nil {
		return fmt.Errorf("create zstd reader: %w", err)
	}
	defer zr.Close()

	// Read header
	var header [8]byte
	if _, err := io.ReadFull(zr, header[:]); err != nil {
		return fmt.Errorf("read magic: %w", err)
	}
	if string(header[:]) != Magic {
		return fmt.Errorf("invalid magic: %q (expected %q)", header[:], Magic)
	}

	var sizeBuf [8]byte
	if _, err := io.ReadFull(zr, sizeBuf[:]); err != nil {
		return fmt.Errorf("read file size: %w", err)
	}
	fileSize := binary.LittleEndian.Uint64(sizeBuf[:])

	// Create sparse file
	dst, err := os.Create(dstPath)
	if err != nil {
		return fmt.Errorf("create output file: %w", err)
	}
	defer dst.Close()

	if err := dst.Truncate(int64(fileSize)); err != nil {
		return fmt.Errorf("truncate to %d: %w", fileSize, err)
	}

	// Read and write blocks
	var offsetBuf [8]byte
	buf := make([]byte, BlockSize)
	blocks := 0

	for {
		// Read offset
		_, err := io.ReadFull(zr, offsetBuf[:])
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			break
		}
		if err != nil {
			return fmt.Errorf("read block offset: %w", err)
		}
		offset := binary.LittleEndian.Uint64(offsetBuf[:])

		// Read block data
		n, err := io.ReadFull(zr, buf)
		if err != nil && err != io.ErrUnexpectedEOF {
			return fmt.Errorf("read block data at offset %d: %w", offset, err)
		}

		// Write at offset (pwrite — doesn't affect file position for other blocks)
		if _, err := dst.WriteAt(buf[:n], int64(offset)); err != nil {
			return fmt.Errorf("write block at offset %d: %w", offset, err)
		}
		blocks++
	}

	log.Printf("sparse: restored %s (%d blocks, %d MB apparent, %dms)",
		dstPath, blocks, fileSize/1024/1024, time.Since(t0).Milliseconds())
	return nil
}

// isZero returns true if all bytes in b are zero.
func isZero(b []byte) bool {
	for _, v := range b {
		if v != 0 {
			return false
		}
	}
	return true
}
