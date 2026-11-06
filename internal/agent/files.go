package agent

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"

	"github.com/Lazypilottt/forkbox/internal/agentwire"
	"github.com/Lazypilottt/forkbox/proto/agentpb"
)

func absPath(p string) error {
	if !filepath.IsAbs(p) {
		return fmt.Errorf("%w: path %q must be absolute", fs.ErrInvalid, p)
	}
	return nil
}

func readFile(c net.Conn, r *agentpb.ReadFileRequest) error {
	if err := absPath(r.Path); err != nil {
		return writeErr(c, err)
	}
	f, err := os.Open(r.Path)
	if err != nil {
		return writeErr(c, err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return writeErr(c, err)
	}
	if !st.Mode().IsRegular() {
		return writeErr(c, fmt.Errorf("%w: %s is not a regular file", fs.ErrInvalid, r.Path))
	}
	if err := agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_FileHeader{FileHeader: &agentpb.FileHeader{
		Size: uint64(st.Size()), Mode: uint32(st.Mode().Perm()),
	}}}); err != nil {
		return err
	}
	h := sha256.New()
	buf := make([]byte, agentwire.ChunkSize)
	var size uint64
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			size += uint64(n)
			if err := agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Chunk{Chunk: &agentpb.Chunk{Data: buf[:n]}}}); err != nil {
				return err
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return writeErr(c, rerr)
		}
	}
	return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_FileDone{FileDone: &agentpb.FileDone{Size: size, Sha256: h.Sum(nil)}}})
}

// writeFile receives chunks into a temporary file next to the target and
// renames it into place only once the upload is complete, so readers never
// see a partial file.
func writeFile(c net.Conn, r *agentpb.WriteFileRequest) error {
	if err := absPath(r.Path); err != nil {
		return writeErr(c, err)
	}
	mode := fs.FileMode(r.Mode).Perm()
	if mode == 0 {
		mode = 0o644
	}
	dir := filepath.Dir(r.Path)
	if r.MakeParents {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return writeErr(c, err)
		}
	}
	tmp, err := os.CreateTemp(dir, ".fbx-upload-*")
	if err != nil {
		return writeErr(c, err)
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	h := sha256.New()
	var size uint64
	for {
		req := &agentpb.Request{}
		if err := agentwire.Read(c, req); err != nil {
			return err // host went away mid-upload
		}
		ch := req.GetChunk()
		if ch == nil {
			return writeErr(c, fmt.Errorf("%w: write_file expects chunk frames, got %T", fs.ErrInvalid, req.Op))
		}
		if len(ch.Data) > 0 {
			if _, err := tmp.Write(ch.Data); err != nil {
				return writeErr(c, err)
			}
			h.Write(ch.Data)
			size += uint64(len(ch.Data))
		}
		if ch.Eof {
			break
		}
	}
	if err := tmp.Chmod(mode); err != nil {
		return writeErr(c, err)
	}
	if err := tmp.Close(); err != nil {
		return writeErr(c, err)
	}
	if err := os.Rename(tmp.Name(), r.Path); err != nil {
		return writeErr(c, err)
	}
	ok = true
	return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_FileDone{FileDone: &agentpb.FileDone{Size: size, Sha256: h.Sum(nil)}}})
}

func stat(c net.Conn, r *agentpb.StatRequest) error {
	if err := absPath(r.Path); err != nil {
		return writeErr(c, err)
	}
	resp := &agentpb.StatResponse{}
	st, err := os.Stat(r.Path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return writeErr(c, err)
	default:
		resp.Exists = true
		resp.Size = uint64(st.Size())
		resp.Mode = uint32(st.Mode().Perm())
		resp.IsDir = st.IsDir()
		resp.MtimeUnixNano = st.ModTime().UnixNano()
	}
	return agentwire.Write(c, &agentpb.Response{Msg: &agentpb.Response_Stat{Stat: resp}})
}
