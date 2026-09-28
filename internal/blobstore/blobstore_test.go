package blobstore

import (
	"bytes"
	"context"
	"io"
	"net/url"
	"os"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

func roundTrip(t *testing.T, s Store) {
	ctx := context.Background()
	data := []byte("hello blob store")
	if err := s.Put(ctx, "blobs/ab/abc", bytes.NewReader(data), int64(len(data)), "text/plain"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.Exists(ctx, "blobs/ab/abc"); err != nil || !ok {
		t.Fatalf("exists: %v %v", ok, err)
	}
	o, err := s.Open(ctx, "blobs/ab/abc")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Seek(6, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	rest, _ := io.ReadAll(o)
	o.Close()
	if string(rest) != "blob store" || o.Size() != int64(len(data)) {
		t.Fatalf("read %q size %d", rest, o.Size())
	}
	if _, err := s.Open(ctx, "blobs/zz/missing"); err != ErrNotFound {
		t.Fatalf("missing: %v", err)
	}
	if err := s.DeletePrefix(ctx, "blobs/"); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.Exists(ctx, "blobs/ab/abc"); ok {
		t.Fatal("not deleted")
	}
}

func TestLocal(t *testing.T) {
	s, err := Open("file://" + t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s)
	if err := s.Put(context.Background(), "../escape", bytes.NewReader(nil), 0, ""); err == nil {
		t.Fatal("path escape accepted")
	}
}

// TestS3 runs against an S3-compatible server when OBX_TEST_S3_ENDPOINT is
// set (for example http://127.0.0.1:8333 with SeaweedFS or MinIO).
func TestS3(t *testing.T) {
	ep := os.Getenv("OBX_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("OBX_TEST_S3_ENDPOINT not set")
	}
	u, _ := url.Parse(ep)
	cl, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(os.Getenv("OBX_S3_ACCESS_KEY_ID"), os.Getenv("OBX_S3_SECRET_ACCESS_KEY"), ""), Secure: u.Scheme == "https"})
	if err != nil {
		t.Fatal(err)
	}
	_ = cl.MakeBucket(context.Background(), "obx-test", minio.MakeBucketOptions{})
	s, err := Open("s3://obx-test/prefix?endpoint=" + url.QueryEscape(ep) + "&path_style=true&region=us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	roundTrip(t, s)
}
