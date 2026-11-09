package imagestore

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// fakeS3 is the part of the S3 API the store uses: bucket and object HEAD,
// and object PUT, with path-style addressing.
type fakeS3 struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
	types   map[string]string
	puts    atomic.Int32
}

func newFakeS3(bucket string) *fakeS3 {
	return &fakeS3{bucket: bucket, objects: map[string][]byte{}, types: map[string]string{}}
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	bucket, key, _ := strings.Cut(path, "/")
	if bucket != f.bucket {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case r.Method == http.MethodHead && key == "":
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodHead:
		data, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Length", strconv.Itoa(len(data)))
		w.Header().Set("ETag", `"etag"`)
		w.Header().Set("Last-Modified", "Mon, 09 Nov 2026 00:00:00 GMT")
		w.WriteHeader(http.StatusOK)
	case r.Method == http.MethodPut:
		data, _ := io.ReadAll(r.Body)
		if strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
			data = decodeChunked(data)
		}
		f.objects[key] = data
		f.types[key] = r.Header.Get("Content-Type")
		f.puts.Add(1)
		w.Header().Set("ETag", `"etag"`)
		w.WriteHeader(http.StatusOK)
	default:
		w.WriteHeader(http.StatusMethodNotAllowed)
	}
}

// decodeChunked strips aws-chunked framing: "<hex size>;chunk-signature=...\r\n<data>\r\n", ending with a zero-size chunk.
func decodeChunked(body []byte) []byte {
	var out []byte
	rest := string(body)
	for {
		header, after, ok := strings.Cut(rest, "\r\n")
		if !ok {
			return out
		}
		sizeText, _, _ := strings.Cut(header, ";")
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil || size == 0 || int(size) > len(after) {
			return out
		}
		out = append(out, after[:size]...)
		rest = strings.TrimPrefix(after[size:], "\r\n")
	}
}

func newStore(t *testing.T, server *httptest.Server, source Source) *Store {
	t.Helper()
	store, err := New(Config{
		Endpoint:        server.URL,
		Region:          "garage",
		Bucket:          "shoko-images",
		Prefix:          "/silo/",
		AccessKeyID:     "key",
		SecretAccessKey: "secret",
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

const imageID = "9f8c7d6e-5b4a-4c3d-8e2f-1a0b9c8d7e6f"

func TestURLCopiesAnImageOnceAndSignsALinkToIt(t *testing.T) {
	s3 := newFakeS3("shoko-images")
	server := httptest.NewServer(s3)
	defer server.Close()
	var downloads atomic.Int32
	store := newStore(t, server, func(_ context.Context, id string) ([]byte, string, error) {
		downloads.Add(1)
		return []byte("poster bytes for " + id), "image/jpeg", nil
	})

	var wg sync.WaitGroup
	links := make([]string, 8)
	for i := range links {
		wg.Add(1)
		go func() {
			defer wg.Done()
			link, err := store.URL(context.Background(), imageID)
			if err != nil {
				t.Error(err)
			}
			links[i] = link
		}()
	}
	wg.Wait()

	if downloads.Load() != 1 || s3.puts.Load() != 1 {
		t.Fatalf("downloads = %d, uploads = %d, want one each", downloads.Load(), s3.puts.Load())
	}
	key := "silo/shoko/" + imageID
	if string(s3.objects[key]) != "poster bytes for "+imageID || s3.types[key] != "image/jpeg" {
		t.Fatalf("stored %q as %q", s3.objects[key], s3.types[key])
	}
	link, err := url.Parse(links[0])
	if err != nil {
		t.Fatal(err)
	}
	if link.Path != "/shoko-images/"+key || link.Query().Get("X-Amz-Signature") == "" || link.Query().Get("X-Amz-Expires") != "604800" {
		t.Fatalf("link = %s", links[0])
	}
}

func TestAnImageAlreadyInTheBucketIsNotCopiedAgain(t *testing.T) {
	s3 := newFakeS3("shoko-images")
	s3.objects["silo/shoko/"+imageID] = []byte("stored earlier")
	server := httptest.NewServer(s3)
	defer server.Close()
	store := newStore(t, server, func(context.Context, string) ([]byte, string, error) {
		t.Fatal("Shoko was asked for an image the bucket already holds")
		return nil, "", nil
	})

	if _, err := store.URL(context.Background(), imageID); err != nil {
		t.Fatal(err)
	}
	if s3.puts.Load() != 0 {
		t.Fatalf("uploads = %d, want 0", s3.puts.Load())
	}
}

func TestAFailedDownloadIsRetriedNextTime(t *testing.T) {
	server := httptest.NewServer(newFakeS3("shoko-images"))
	defer server.Close()
	var calls atomic.Int32
	store := newStore(t, server, func(context.Context, string) ([]byte, string, error) {
		if calls.Add(1) == 1 {
			return nil, "", errors.New("shoko is restarting")
		}
		return []byte("poster"), "image/png", nil
	})

	if _, err := store.URL(context.Background(), imageID); err == nil {
		t.Fatal("a failed download produced a link")
	}
	if _, err := store.URL(context.Background(), imageID); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 {
		t.Fatalf("downloads = %d, want 2", calls.Load())
	}
}

func TestCheckReportsAMissingBucket(t *testing.T) {
	server := httptest.NewServer(newFakeS3("other-bucket"))
	defer server.Close()
	store := newStore(t, server, func(context.Context, string) ([]byte, string, error) { return nil, "", nil })

	if err := store.Check(context.Background()); err == nil || !strings.Contains(err.Error(), "shoko-images") {
		t.Fatalf("err = %v, want a missing bucket", err)
	}
}

func TestNewRejectsAnEndpointWithAPath(t *testing.T) {
	_, err := New(Config{Endpoint: "https://s3.example.com/bucket", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s"},
		func(context.Context, string) ([]byte, string, error) { return nil, "", nil })
	if err == nil {
		t.Fatal("an endpoint with a path was accepted")
	}
}
