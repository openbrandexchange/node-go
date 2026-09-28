package node

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

// TestSFTPImportSource imports from an SFTP folder when OBX_TEST_SFTP
// (user:password@host:port) is set, for example with
// docker run -p 2222:22 atmoz/sftp brand:brandpass:1001::upload
func TestSFTPImportSource(t *testing.T) {
	spec := os.Getenv("OBX_TEST_SFTP")
	if spec == "" {
		t.Skip("OBX_TEST_SFTP not set")
	}
	userPass, hostPort, _ := strings.Cut(spec, "@")
	user, pass, _ := strings.Cut(userPass, ":")
	host, portStr, _ := strings.Cut(hostPort, ":")
	port, _ := strconv.Atoi(portStr)
	conn, err := ssh.Dial("tcp", hostPort, &ssh.ClientConfig{User: user, Auth: []ssh.AuthMethod{ssh.Password(pass)}, HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 10 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	cl, err := sftp.NewClient(conn)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(specDir(t), "kestrel")
	put := func(remote, local string) {
		data, _ := os.ReadFile(local)
		f, err := cl.Create(remote)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = f.Write(data)
		f.Close()
	}
	_ = cl.MkdirAll("/upload/export/images")
	put("/upload/export/products.csv", filepath.Join(dir, "products.csv"))
	imgs, _ := filepath.Glob(filepath.Join(dir, "images", "*.png"))
	for _, p := range imgs {
		put("/upload/export/images/"+filepath.Base(p), p)
	}
	cl.Close()
	conn.Close()

	n := newTestNode(t)
	admin := n.setup()
	src := admin.must(201, "POST", "/admin/api/import-sources", map[string]any{"name": "SFTP", "kind": "sftp",
		"schedule":    map[string]any{"frequency": "daily", "time": "04:00"},
		"config":      map[string]any{"host": host, "port": port, "username": user, "path": "/upload/export"},
		"credentials": map[string]any{"password": pass}})
	test := admin.must(200, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/test", nil)
	if test["ok"] != true || test["host_key_fingerprint"] == nil {
		t.Fatalf("test %v", test)
	}
	run := admin.must(202, "POST", "/admin/api/import-sources/"+src["id"].(string)+"/run", nil)
	n.app.RunJobsNow(context.Background())
	done := admin.must(200, "GET", "/admin/api/imports/"+run["id"].(string), nil)
	if done["status"] != "succeeded" || done["counts"].(map[string]any)["errors"].(float64) != 0 {
		t.Fatalf("%v", done)
	}
	got := admin.must(200, "GET", "/admin/api/import-sources/"+src["id"].(string), nil)
	if got["config"].(map[string]any)["host_key_fingerprint"] == nil {
		t.Fatal("host key was not pinned on first use")
	}
}
