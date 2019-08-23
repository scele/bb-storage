package blobstore

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/buildbarn/bb-storage/pkg/util"

	"golang.org/x/net/context/ctxhttp"
	"golang.org/x/sync/errgroup"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type remoteBlobAccess struct {
	address string
	prefix  string
}

func convertHTTPUnexpectedStatus(resp *http.Response) error {
	return status.Errorf(codes.Unknown, "Unexpected status code from remote cache: %d - %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

// NewRemoteBlobAccess for use of HTTP/1.1 cache backend.
//
// See: https://docs.bazel.build/versions/master/remote-caching.html#http-caching-protocol
func NewRemoteBlobAccess(address, prefix string) BlobAccess {
	return &remoteBlobAccess{
		address: address,
		prefix:  prefix,
	}
}

func (ba *remoteBlobAccess) Get(ctx context.Context, digest *util.Digest) (int64, io.ReadCloser, error) {
	url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
	resp, err := ctxhttp.Get(ctx, http.DefaultClient, url)
	if err != nil {
		fmt.Printf("Error getting digest. %s\n", err)
		return 0, nil, err
	}

	switch resp.StatusCode {
	case http.StatusNotFound:
		resp.Body.Close()
		return 0, nil, status.Error(codes.NotFound, url)
	case http.StatusOK:
		return resp.ContentLength, resp.Body, nil
	default:
		resp.Body.Close()
		return 0, nil, convertHTTPUnexpectedStatus(resp)
	}
}

func (ba *remoteBlobAccess) Put(ctx context.Context, digest *util.Digest, sizeBytes int64, r io.ReadCloser) error {
	url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
	req, err := http.NewRequest(http.MethodPut, url, r)
	if err != nil {
		r.Close()
		return err
	}
	req.ContentLength = sizeBytes
	_, err = ctxhttp.Do(ctx, http.DefaultClient, req)
	return err
}

func (ba *remoteBlobAccess) Delete(ctx context.Context, digest *util.Digest) error {
	return status.Error(codes.Unimplemented, "Bazel HTTP caching protocol does not support object deletion")
}

func (ba *remoteBlobAccess) FindMissing(ctx context.Context, digests []*util.Digest) ([]*util.Digest, error) {
	g, ctx := errgroup.WithContext(ctx)
	in := make(chan *util.Digest, 10)
	out := make(chan *util.Digest, 10)

	workers := 512
	if len(digests) < workers {
		workers = len(digests)
	}

	for i := 0; i < workers; i++ {
		g.Go(func() error {
			for digest := range in {
				url := fmt.Sprintf("%s/%s/%s", ba.address, ba.prefix, digest.GetHashString())
				resp, err := ctxhttp.Head(ctx, http.DefaultClient, url)
				if err != nil {
					return err
				}

				switch resp.StatusCode {
				case http.StatusNotFound:
					out <- digest
				case http.StatusOK:
					// No fallthrough
				default:
					return convertHTTPUnexpectedStatus(resp)
				}
			}
			return nil
		})
	}
	go func() {
		for _, digest := range digests {
			in <- digest
		}
		close(in)
		g.Wait()
		close(out)
	}()

	var missing []*util.Digest
	for digest := range out {
		missing = append(missing, digest)
	}

	if err := g.Wait(); err != nil {
		return nil, err
	}
	return missing, nil
}
