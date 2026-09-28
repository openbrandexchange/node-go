// Package blobstore stores files (image originals, renditions, zips) on the
// local disk or in an S3-compatible bucket (AWS S3, Cloudflare R2, MinIO,
// Google Cloud Storage through its S3 interoperability endpoint).
package blobstore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("blob not found")

// Object is an open stored file. It supports seeking so it can be served
// with Range requests.
type Object interface {
	io.ReadSeekCloser
	Size() int64
}

// Store is a key/value file store.
type Store interface {
	// Put stores the bytes read from r under key (overwriting).
	Put(ctx context.Context, key string, r io.Reader, size int64, contentType string) error
	// Open opens key for reading.
	Open(ctx context.Context, key string) (Object, error)
	// Exists reports whether key exists.
	Exists(ctx context.Context, key string) (bool, error)
	// Delete removes key (no error when missing).
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every key below prefix.
	DeletePrefix(ctx context.Context, prefix string) error
	// Describe returns a short human description (no secrets).
	Describe() string
}

// Open returns a store for a URL:
//
//	file:///var/lib/obx/blobs          local directory
//	s3://bucket/prefix?region=eu-west-1&endpoint=https://...&path_style=true
//	gs://bucket/prefix                 Google Cloud Storage (S3 interoperability, HMAC keys)
//
// S3 credentials come from OBX_S3_ACCESS_KEY_ID / OBX_S3_SECRET_ACCESS_KEY
// (or AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY).
func Open(raw string) (Store, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("invalid OBX_BLOB_URL: %w", err)
	}
	switch u.Scheme {
	case "file", "":
		path := u.Path
		if u.Scheme == "" {
			path = raw
		}
		if u.Host != "" && u.Host != "localhost" { // file://relative/dir
			path = filepath.Join(u.Host, u.Path)
		}
		return NewLocal(path)
	case "s3", "gs", "r2":
		return newS3(u)
	}
	return nil, fmt.Errorf("unsupported OBX_BLOB_URL scheme %q (use file://, s3:// or gs://)", u.Scheme)
}

// Local stores files in a directory.
type Local struct{ root string }

// NewLocal creates the directory if needed.
func NewLocal(root string) (*Local, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(abs, 0o700); err != nil {
		return nil, err
	}
	return &Local{root: abs}, nil
}

// Root is the directory the files live in.
func (l *Local) Root() string { return l.root }

func (l *Local) path(key string) (string, error) {
	clean := filepath.Clean("/" + key)
	if strings.Contains(key, "..") || clean == "/" {
		return "", fmt.Errorf("invalid key")
	}
	return filepath.Join(l.root, filepath.FromSlash(clean)), nil
}

// Put writes atomically (temp file + rename).
func (l *Local) Put(_ context.Context, key string, r io.Reader, _ int64, _ string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(p), ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, p)
}

type localObject struct {
	*os.File
	size int64
}

func (o *localObject) Size() int64 { return o.size }

// Open opens a file.
func (l *Local) Open(_ context.Context, key string) (Object, error) {
	p, err := l.path(key)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(p)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &localObject{File: f, size: st.Size()}, nil
}

// Exists checks for a file.
func (l *Local) Exists(_ context.Context, key string) (bool, error) {
	p, err := l.path(key)
	if err != nil {
		return false, err
	}
	_, err = os.Stat(p)
	if err == nil {
		return true, nil
	}
	if os.IsNotExist(err) {
		return false, nil
	}
	return false, err
}

// Delete removes a file.
func (l *Local) Delete(_ context.Context, key string) error {
	p, err := l.path(key)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// DeletePrefix removes a directory tree.
func (l *Local) DeletePrefix(_ context.Context, prefix string) error {
	p, err := l.path(strings.TrimSuffix(prefix, "/"))
	if err != nil {
		return err
	}
	return os.RemoveAll(p)
}

// Describe returns the directory.
func (l *Local) Describe() string { return "local directory " + l.root }

// BlobKey is the storage key of a content-addressed blob.
func BlobKey(sha string) string { return "blobs/" + sha[:2] + "/" + sha }
