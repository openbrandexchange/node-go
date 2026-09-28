package node

import (
	"bytes"
	"context"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// TestS3ImportSource runs a scheduled import from an S3-compatible bucket
// when OBX_TEST_S3_ENDPOINT (plus OBX_S3_ACCESS_KEY_ID/SECRET) is set.
func TestS3ImportSource(t *testing.T) {
	ep := os.Getenv("OBX_TEST_S3_ENDPOINT")
	if ep == "" {
		t.Skip("OBX_TEST_S3_ENDPOINT not set")
	}
	key, secret := os.Getenv("OBX_S3_ACCESS_KEY_ID"), os.Getenv("OBX_S3_SECRET_ACCESS_KEY")
	u, _ := url.Parse(ep)
	cl, err := minio.New(u.Host, &minio.Options{Creds: credentials.NewStaticV4(key, secret, ""), Secure: u.Scheme == "https"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_ = cl.MakeBucket(ctx, "obx-source", minio.MakeBucketOptions{})
	dir := filepath.Join(specDir(t), "kestrel")
	put := func(name, path string) {
		data, _ := os.ReadFile(path)
		if _, err := cl.PutObject(ctx, "obx-source", "export/"+name, bytes.NewReader(data), int64(len(data)), minio.PutObjectOptions{}); err != nil {
			t.Fatal(err)
		}
	}
	put("products.csv", filepath.Join(dir, "products.csv"))
	put("assets.csv", filepath.Join(dir, "assets.csv"))
	imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*.png"))
	for _, p := range imgs {
		put("images/"+filepath.Base(p), p)
	}
	n := newTestNode(t)
	admin := n.setup()
	src := admin.must(201, "POST", "/admin/api/import-sources", map[string]any{"name": "Bucket", "kind": "s3",
		"schedule":    map[string]any{"frequency": "manual"},
		"config":      map[string]any{"endpoint": ep, "region": "us-east-1", "bucket": "obx-source", "prefix": "export/", "path_style": true},
		"credentials": map[string]any{"access_key_id": key, "secret_access_key": secret}})
	test := admin.must(200, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/test", nil)
	if test["ok"] != true || len(test["files"].([]any)) != 29 {
		t.Fatalf("test %v", test)
	}
	run := admin.must(202, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/run", nil)
	n.app.RunJobsNow(ctx)
	done := admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
	c := done["counts"].(map[string]any)
	if done["status"] != "succeeded" || c["errors"].(float64) != 0 || c["created"].(float64) != 1+8+40+27 {
		t.Fatalf("%v", done)
	}
}
