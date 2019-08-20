package blobstore

import (
	"context"
	"fmt"
	"io"

	"github.com/buildbarn/bb-storage/pkg/util"
	"github.com/hashicorp/go-multierror"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type tieredBlobAccess struct {
	tiers []BlobAccess
}

type readCloser struct {
	io.Reader
	io.Closer
}

type multiCloser struct {
	cs []io.Closer
}

func MultiCloser(c ...io.Closer) io.Closer {
	m := &multiCloser{}
	m.cs = append(m.cs, c...)
	return m
}

func (m *multiCloser) Close() error {
	var first error
	for _, c := range m.cs {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

type multiWriteCloser struct {
	io.Writer
	cs []io.Closer
}

func MultiWriteCloser(ws ...io.Writer) io.WriteCloser {
	m := &multiWriteCloser{Writer: io.MultiWriter(ws...)}
	for _, w := range ws {
		if c, ok := w.(io.Closer); ok {
			m.cs = append(m.cs, c)
		}
	}
	return m
}

func (m *multiWriteCloser) Close() error {
	var first error
	for _, c := range m.cs {
		if err := c.Close(); err != nil && first == nil {
			first = err
		}
	}
	return first
}

// NewTieredBlobAccess creates a BlobAccess that implements multiple cache
// tiers.
func NewTieredBlobAccess(tiers []BlobAccess) BlobAccess {
	return &tieredBlobAccess{
		tiers: tiers,
	}
}

func (ba *tieredBlobAccess) Get(ctx context.Context, digest *util.Digest) (int64, io.ReadCloser, error) {
	for i, t := range ba.tiers {
		size, r, err := t.Get(ctx, digest)
		if err == nil {
			var writers []io.Writer
			for j := 0; j < i; j++ {
				// Store the read data in the lower cache tiers.
				pr, pw := io.Pipe()
				writers = append(writers, pw)
				go func(t BlobAccess) {
					err = t.Put(ctx, digest, size, pr)
					if err != nil {
						fmt.Printf("Error writing")
					}
				}(ba.tiers[j])
			}
			multiWriter := MultiWriteCloser(writers...)
			tee := readCloser{
				io.TeeReader(r, multiWriter),
				MultiCloser(r, multiWriter),
			}
			return size, tee, err
		}
	}
	return 0, nil, status.Error(codes.NotFound, "Failed to get blob")
}

func (ba *tieredBlobAccess) Put(ctx context.Context, digest *util.Digest, sizeBytes int64, r io.ReadCloser) error {
	var writers []io.Writer
	results := make(chan error, len(ba.tiers))
	for _, t := range ba.tiers {
		pr, pw := io.Pipe()
		writers = append(writers, pw)
		go func() {
			results <- t.Put(ctx, digest, sizeBytes, pr)
		}()
	}
	multiWriter := MultiWriteCloser(writers...)
	go func() {
		io.Copy(multiWriter, r)
		multiWriter.Close()
		r.Close()
	}()

	var err error
	for range ba.tiers {
		multierror.Append(err, <-results)
	}
	close(results)
	return err
}

func (ba *tieredBlobAccess) Delete(ctx context.Context, digest *util.Digest) error {
	results := make(chan error, len(ba.tiers))
	for _, t := range ba.tiers {
		go func() {
			results <- t.Delete(ctx, digest)
		}()
	}
	var err error
	for range ba.tiers {
		multierror.Append(err, <-results)
	}
	close(results)
	return err
}

func (ba *tieredBlobAccess) FindMissing(ctx context.Context, digests []*util.Digest) ([]*util.Digest, error) {
	var err error
	for _, t := range ba.tiers {
		digests, err = t.FindMissing(ctx, digests)
		if err != nil {
			return nil, err
		}
		if len(digests) == 0 {
			break
		}
	}
	return digests, nil
}
