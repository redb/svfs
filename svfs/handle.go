package svfs

import (
	"fmt"
	"io"
	"time"

	"bazil.org/fuse"
	"bazil.org/fuse/fs"
	"golang.org/x/net/context"
)

// ObjectHandle represents an open object handle, similarly to
// file handles.
type ObjectHandle struct {
	target        *Object
	rd            io.ReadSeeker
	wd            io.WriteCloser
	create        bool
	truncate      bool
	nonce         string
	wroteSegment  bool
	segmentID     uint
	uploaded      uint64
	segmentPrefix string
	segmentPath   string
}

// Read gets a swift object data for a request within the current context.
// The request size is always honored. We open the file on the first write.
func (fh *ObjectHandle) Read(ctx context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) (err error) {
	if fh.rd == nil {
		fh.rd, err = newReader(fh)
		if err != nil {
			return err
		}
	}
	fh.rd.Seek(req.Offset, 0)
	resp.Data = make([]byte, req.Size)
	io.ReadFull(fh.rd, resp.Data)
	return nil
}

// Release frees the file handle, closing all readers/writers in use.
func (fh *ObjectHandle) Release(ctx context.Context, req *fuse.ReleaseRequest) error {
	if fh.rd != nil {
		if closer, ok := fh.rd.(io.Closer); ok {
			closer.Close()
		}
	}
	if fh.wd != nil {
		defer fh.target.m.Unlock()
		fh.wd.Close()
		if Encryption {
			headers := map[string]string{
				ObjectSizeHeader:  fmt.Sprintf("%d", fh.target.so.Bytes),
				ObjectNonceHeader: fh.nonce,
			}
			h := fh.target.sh.ObjectMetadata().Headers(ObjectMetaHeader)
			for k, v := range headers {
				fh.target.sh[k] = v
				h[k] = v
			}
			err := SwiftConnection.ObjectUpdate(fh.target.c.Name, fh.target.path, h)
			if err != nil {
				return fmt.Errorf("Failed to update object crypto headers")
			}
		}
		ChangeCache.Remove(fh.target.c.Name, fh.target.path)
	}
	return nil
}

// Write pushes data to a swift object.
// If we detect that we are writing more data than the configured
// segment size, then the first object we were writing to is moved
// to the segment container and named accordingly to DLO conventions.
// Remaining data will be split into segments sequentially until
// file handle release is called. If we are overwriting an object
// we handle segment deletion, and object creation.
func (fh *ObjectHandle) Write(ctx context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) (err error) {
	// Truncating the file, first write
	if !fh.create && !fh.truncate {
		if fh.target.segmented {
			err = deleteSegments(fh.target.cs.Name, fh.target.sh[ManifestHeader])
			if err != nil {
				return err
			}
			fh.target.segmented = false
		}
		fh.truncate = true
		fh.target.so.Bytes = 0
		fh.wd, err = newWriter(fh.target.c.Name, fh.target.so.Name, &fh.nonce)
		if err != nil {
			return err
		}
	}

	// Write first segment or file with size smaller than
	// a segment size
	if fh.uploaded+uint64(len(req.Data)) <= uint64(SegmentSize) {
		// File size is less than the size of a segment
		// or we didn't fill the current segment yet.
		if _, err := fh.wd.Write(req.Data); err != nil {
			return err
		}
		fh.uploaded += uint64(len(req.Data))
		fh.target.so.Bytes += int64(len(req.Data))
		goto EndWrite
	}

	// File size is greater than the size of a segment
	// Move it to the segment directory and start writing
	// next segment.
	if fh.uploaded+uint64(len(req.Data)) > uint64(SegmentSize) {
		if !fh.wroteSegment {
			fh.wd.Close()
			fh.segmentPrefix = fmt.Sprintf("%s/%d", fh.target.path, time.Now().Unix())
			fh.segmentPath = segmentPath(fh.segmentPrefix, &fh.segmentID)

			err := SwiftConnection.ObjectMove(fh.target.c.Name, fh.target.path, fh.target.cs.Name, fh.segmentPath)
			if err != nil {
				return err
			}

			fh.wroteSegment = true
			createManifest(fh.target.c.Name, fh.target.cs.Name+"/"+fh.segmentPrefix, fh.target.path)
			fh.target.segmented = true
		}

		fh.wd.Close()
		fh.wd, err = initSegment(fh.target.cs.Name, fh.segmentPrefix, &fh.segmentID, fh.target.so, req.Data, &fh.uploaded, &fh.nonce)

		if err != nil {
			return err
		}

		goto EndWrite
	}

EndWrite:
	resp.Size = len(req.Data)
	return nil
}

var (
	_ fs.Handle         = (*ObjectHandle)(nil)
	_ fs.HandleReleaser = (*ObjectHandle)(nil)
	_ fs.HandleReader   = (*ObjectHandle)(nil)
	_ fs.HandleWriter   = (*ObjectHandle)(nil)
)
