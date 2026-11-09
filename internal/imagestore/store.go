// Package imagestore copies Shoko images into an S3-compatible bucket and hands
// out presigned links to them. Silo only downloads artwork from public
// addresses, so a Shoko server on a private network cannot be fetched
// directly; a bucket with a public endpoint can.
package imagestore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// LinkLifetime is how long a presigned link stays valid. Silo caches an image
// as soon as it resolves the link, and resolves stored paths again when it
// needs them, so a week, the longest S3 allows, is plenty.
const LinkLifetime = 7 * 24 * time.Hour

// Config names the bucket. Objects are written with path-style addressing,
// which Garage, MinIO and AWS all accept.
type Config struct {
	Endpoint        string
	Region          string
	Bucket          string
	Prefix          string
	AccessKeyID     string
	SecretAccessKey string
	HTTPClient      *http.Client
}

// Source downloads one image from Shoko.
type Source func(ctx context.Context, imageID string) ([]byte, string, error)

// Store copies each image once and remembers which ones the bucket holds.
type Store struct {
	client *minio.Client
	bucket string
	prefix string
	source Source

	mu      sync.Mutex
	present map[string]bool
	pending map[string]*upload
}

type upload struct {
	done chan struct{}
	err  error
}

func New(cfg Config, source Source) (*Store, error) {
	if source == nil {
		return nil, errors.New("imagestore: image source is required")
	}
	endpoint, err := url.Parse(strings.TrimSpace(cfg.Endpoint))
	if err != nil || endpoint.Host == "" || (endpoint.Scheme != "http" && endpoint.Scheme != "https") {
		return nil, errors.New("imagestore: endpoint must be an http or https URL")
	}
	if endpoint.Path != "" && endpoint.Path != "/" {
		return nil, errors.New("imagestore: endpoint must not have a path")
	}
	if strings.TrimSpace(cfg.Bucket) == "" {
		return nil, errors.New("imagestore: bucket is required")
	}
	region := strings.TrimSpace(cfg.Region)
	if region == "" {
		region = "us-east-1"
	}
	options := &minio.Options{
		Creds:        credentials.NewStaticV4(strings.TrimSpace(cfg.AccessKeyID), strings.TrimSpace(cfg.SecretAccessKey), ""),
		Secure:       endpoint.Scheme == "https",
		Region:       region,
		BucketLookup: minio.BucketLookupPath,
		// Silo waits on image resolution, so a failing bucket must not hold it
		// up with long retries; the caller falls back to Shoko's own URL.
		MaxRetries: 1,
	}
	if cfg.HTTPClient != nil {
		options.Transport = cfg.HTTPClient.Transport
	}
	client, err := minio.New(endpoint.Host, options)
	if err != nil {
		return nil, fmt.Errorf("imagestore: %w", err)
	}
	return &Store{
		client:  client,
		bucket:  strings.TrimSpace(cfg.Bucket),
		prefix:  strings.Trim(strings.TrimSpace(cfg.Prefix), "/"),
		source:  source,
		present: make(map[string]bool),
		pending: make(map[string]*upload),
	}, nil
}

// Check confirms the bucket exists and the key can reach it.
func (s *Store) Check(ctx context.Context) error {
	exists, err := s.client.BucketExists(ctx, s.bucket)
	if err != nil {
		return fmt.Errorf("imagestore: cannot reach bucket %q: %w", s.bucket, err)
	}
	if !exists {
		return fmt.Errorf("imagestore: bucket %q does not exist", s.bucket)
	}
	return nil
}

// URL copies the image into the bucket if it is not there yet and returns a
// presigned link to it.
func (s *Store) URL(ctx context.Context, imageID string) (string, error) {
	key := s.key(imageID)
	if err := s.ensure(ctx, key, imageID); err != nil {
		return "", err
	}
	link, err := s.client.PresignedGetObject(ctx, s.bucket, key, LinkLifetime, nil)
	if err != nil {
		return "", fmt.Errorf("imagestore: presign %s: %w", key, err)
	}
	return link.String(), nil
}

func (s *Store) key(imageID string) string {
	key := "shoko/" + imageID
	if s.prefix != "" {
		key = s.prefix + "/" + key
	}
	return key
}

// ensure uploads key once. Concurrent requests for the same image wait for the
// first upload instead of starting their own.
func (s *Store) ensure(ctx context.Context, key, imageID string) error {
	s.mu.Lock()
	if s.present[key] {
		s.mu.Unlock()
		return nil
	}
	if running, ok := s.pending[key]; ok {
		s.mu.Unlock()
		select {
		case <-running.done:
			return running.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	current := &upload{done: make(chan struct{})}
	s.pending[key] = current
	s.mu.Unlock()

	current.err = s.copy(ctx, key, imageID)
	s.mu.Lock()
	delete(s.pending, key)
	if current.err == nil {
		s.present[key] = true
	}
	s.mu.Unlock()
	close(current.done)
	return current.err
}

func (s *Store) copy(ctx context.Context, key, imageID string) error {
	if _, err := s.client.StatObject(ctx, s.bucket, key, minio.StatObjectOptions{}); err == nil {
		return nil
	} else if minio.ToErrorResponse(err).Code != "NoSuchKey" && minio.ToErrorResponse(err).StatusCode != http.StatusNotFound {
		return fmt.Errorf("imagestore: check %s: %w", key, err)
	}
	data, contentType, err := s.source(ctx, imageID)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, s.bucket, key, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{
		ContentType:  contentType,
		CacheControl: "public, max-age=86400",
	})
	if err != nil {
		return fmt.Errorf("imagestore: upload %s: %w", key, err)
	}
	return nil
}
