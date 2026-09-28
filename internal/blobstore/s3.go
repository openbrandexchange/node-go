package blobstore

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// S3 stores files in an S3-compatible bucket.
type S3 struct {
	client *minio.Client
	bucket string
	prefix string
	desc   string
}

func firstEnv(keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
	}
	return ""
}

func newS3(u *url.URL) (*S3, error) {
	q := u.Query()
	bucket := u.Host
	if bucket == "" {
		return nil, fmt.Errorf("blob URL needs a bucket: s3://bucket/prefix")
	}
	endpoint := q.Get("endpoint")
	if endpoint == "" {
		endpoint = firstEnv("OBX_S3_ENDPOINT")
	}
	region := q.Get("region")
	if region == "" {
		region = firstEnv("OBX_S3_REGION", "AWS_REGION")
	}
	switch u.Scheme {
	case "gs":
		if endpoint == "" {
			endpoint = "https://storage.googleapis.com"
		}
		if region == "" {
			region = "auto"
		}
	case "r2":
		if endpoint == "" {
			return nil, fmt.Errorf("r2:// needs ?endpoint=https://<account>.r2.cloudflarestorage.com")
		}
		if region == "" {
			region = "auto"
		}
	}
	if endpoint == "" {
		if region == "" {
			region = "us-east-1"
		}
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	eu, err := url.Parse(endpoint)
	if err != nil || eu.Host == "" {
		return nil, fmt.Errorf("invalid S3 endpoint %q", endpoint)
	}
	pathStyle := q.Get("path_style") == "true" || q.Get("path_style") == "1"
	lookup := minio.BucketLookupAuto
	if pathStyle {
		lookup = minio.BucketLookupPath
	}
	access := firstEnv("OBX_S3_ACCESS_KEY_ID", "AWS_ACCESS_KEY_ID")
	secret := firstEnv("OBX_S3_SECRET_ACCESS_KEY", "AWS_SECRET_ACCESS_KEY")
	var creds *credentials.Credentials
	if access != "" {
		creds = credentials.NewStaticV4(access, secret, firstEnv("AWS_SESSION_TOKEN"))
	} else {
		creds = credentials.NewChainCredentials([]credentials.Provider{&credentials.EnvAWS{}, &credentials.IAM{}})
	}
	client, err := minio.New(eu.Host, &minio.Options{
		Creds:        creds,
		Secure:       eu.Scheme != "http",
		Region:       region,
		BucketLookup: lookup,
	})
	if err != nil {
		return nil, err
	}
	prefix := strings.Trim(u.Path, "/")
	if prefix != "" {
		prefix += "/"
	}
	return &S3{client: client, bucket: bucket, prefix: prefix, desc: fmt.Sprintf("%s://%s/%s (%s)", u.Scheme, bucket, prefix, eu.Host)}, nil
}

// Put uploads an object.
func (s *S3) Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error {
	if contentType == "" {
		contentType = "application/octet-stream"
	}
	_, err := s.client.PutObject(ctx, s.bucket, s.prefix+key, r, size, minio.PutObjectOptions{ContentType: contentType})
	return err
}

type s3Object struct {
	*minio.Object
	size int64
}

func (o *s3Object) Size() int64 { return o.size }

// Open opens an object (lazy ranged reads).
func (s *S3) Open(ctx context.Context, key string) (Object, error) {
	obj, err := s.client.GetObject(ctx, s.bucket, s.prefix+key, minio.GetObjectOptions{})
	if err != nil {
		return nil, err
	}
	st, err := obj.Stat()
	if err != nil {
		obj.Close()
		if minio.ToErrorResponse(err).Code == "NoSuchKey" {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &s3Object{Object: obj, size: st.Size}, nil
}

// Exists stats an object.
func (s *S3) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.StatObject(ctx, s.bucket, s.prefix+key, minio.StatObjectOptions{})
	if err == nil {
		return true, nil
	}
	if minio.ToErrorResponse(err).Code == "NoSuchKey" {
		return false, nil
	}
	return false, err
}

// Delete removes an object.
func (s *S3) Delete(ctx context.Context, key string) error {
	return s.client.RemoveObject(ctx, s.bucket, s.prefix+key, minio.RemoveObjectOptions{})
}

// DeletePrefix removes all objects under prefix.
func (s *S3) DeletePrefix(ctx context.Context, prefix string) error {
	for obj := range s.client.ListObjects(ctx, s.bucket, minio.ListObjectsOptions{Prefix: s.prefix + prefix, Recursive: true}) {
		if obj.Err != nil {
			return obj.Err
		}
		if err := s.client.RemoveObject(ctx, s.bucket, obj.Key, minio.RemoveObjectOptions{}); err != nil {
			return err
		}
	}
	return nil
}

// Describe returns bucket and endpoint.
func (s *S3) Describe() string { return s.desc }
