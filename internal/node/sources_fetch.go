package node

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"

	"github.com/openbrandexchange/node-go/internal/db"
	"github.com/openbrandexchange/node-go/internal/importfmt"
	"github.com/openbrandexchange/node-go/internal/problem"
	"github.com/openbrandexchange/node-go/internal/safehttp"
)

type remoteFile struct {
	Name     string
	Size     int64
	Modified *string
	Tag      string
}

type fetcher interface {
	list(ctx context.Context) ([]remoteFile, string, error)
	open(ctx context.Context, f remoteFile) (io.ReadCloser, error)
	hostKey() string
	close()
}

func (a *App) fetcherFor(s *sourceRow) (fetcher, *problem.Problem) {
	creds := a.sourceCreds(s)
	switch s.Kind {
	case "https_url":
		return &httpsFetcher{a: a, cfg: s.Config, creds: creds}, nil
	case "sftp":
		return &sftpFetcher{a: a, s: s, cfg: s.Config, creds: creds}, nil
	case "s3":
		return &s3Fetcher{a: a, cfg: s.Config, creds: creds}, nil
	}
	return nil, problem.New("validation_failed", "Unknown source kind %s.", s.Kind)
}

func fingerprintOf(files []remoteFile) string {
	sort.Slice(files, func(i, j int) bool { return files[i].Name < files[j].Name })
	h := sha256.New()
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00%s\n", f.Name, f.Size, deref(f.Modified), f.Tag)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ---- https ----------------------------------------------------------------------------

type httpsFetcher struct {
	a     *App
	cfg   map[string]any
	creds sourceCreds
	files map[string]*os.File
}

func (f *httpsFetcher) get(ctx context.Context, raw string) (*os.File, int64, string, error) {
	if err := f.a.Policy.CheckURL(ctx, raw, "https"); err != nil {
		return nil, 0, "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, 0, "", err
	}
	req.Header.Set("User-Agent", Software+"/"+Version)
	for k, v := range f.creds.Headers {
		req.Header.Set(k, v)
	}
	resp, err := f.a.Policy.Client(10*time.Minute, 5, "https").Do(req)
	if err != nil {
		return nil, 0, "", fmt.Errorf("download failed: %s", scrubErr(err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, 0, "", fmt.Errorf("the server answered HTTP %d", resp.StatusCode)
	}
	tmp, sha, n, err := spool(resp.Body, MaxImportBytes)
	if err != nil {
		return nil, 0, "", err
	}
	return tmp, n, sha, nil
}

func (f *httpsFetcher) list(ctx context.Context) ([]remoteFile, string, error) {
	f.files = map[string]*os.File{}
	var out []remoteFile
	for _, k := range []string{"url", "images_url"} {
		raw, _ := f.cfg[k].(string)
		if raw == "" {
			continue
		}
		tmp, n, sha, err := f.get(ctx, raw)
		if err != nil {
			return nil, "", err
		}
		head := make([]byte, 4)
		_, _ = io.ReadFull(tmp, head)
		_, _ = tmp.Seek(0, io.SeekStart)
		name := path.Base(strings.SplitN(raw, "?", 2)[0])
		isZip := bytes.Equal(head, []byte("PK\x03\x04"))
		switch {
		case k == "images_url":
			name = "images.zip"
		case isZip && !strings.HasSuffix(strings.ToLower(name), ".zip"):
			name = "package.zip"
		case !isZip && !strings.HasSuffix(strings.ToLower(name), ".csv"):
			name = "products.csv"
		}
		f.files[name] = tmp
		out = append(out, remoteFile{Name: name, Size: n, Tag: sha})
	}
	return out, fingerprintOf(append([]remoteFile(nil), out...)), nil
}

func (f *httpsFetcher) open(_ context.Context, rf remoteFile) (io.ReadCloser, error) {
	tmp := f.files[rf.Name]
	if tmp == nil {
		return nil, errors.New("not downloaded")
	}
	_, _ = tmp.Seek(0, io.SeekStart)
	return io.NopCloser(tmp), nil
}

func (f *httpsFetcher) hostKey() string { return "" }

func (f *httpsFetcher) close() {
	for _, t := range f.files {
		t.Close()
	}
}

// ---- sftp ------------------------------------------------------------------------------

type sftpFetcher struct {
	a      *App
	s      *sourceRow
	cfg    map[string]any
	creds  sourceCreds
	conn   *ssh.Client
	client *sftp.Client
	seen   string
}

func (f *sftpFetcher) connect(ctx context.Context) error {
	if f.client != nil {
		return nil
	}
	host, _ := f.cfg["host"].(string)
	user, _ := f.cfg["username"].(string)
	port := 22
	switch p := f.cfg["port"].(type) {
	case float64:
		port = int(p)
	case int:
		port = p
	}
	if err := f.a.Policy.CheckHost(ctx, host); err != nil {
		return err
	}
	var auths []ssh.AuthMethod
	if f.creds.PrivateKey != nil {
		var signer ssh.Signer
		var err error
		if f.creds.Password != nil {
			signer, err = ssh.ParsePrivateKeyWithPassphrase([]byte(*f.creds.PrivateKey), []byte(*f.creds.Password))
		} else {
			signer, err = ssh.ParsePrivateKey([]byte(*f.creds.PrivateKey))
		}
		if err != nil {
			return errors.New("the private key cannot be read")
		}
		auths = append(auths, ssh.PublicKeys(signer))
	}
	if f.creds.Password != nil && f.creds.PrivateKey == nil {
		auths = append(auths, ssh.Password(*f.creds.Password))
	}
	pinned, _ := f.cfg["host_key_fingerprint"].(string)
	cfg := &ssh.ClientConfig{
		User: user, Auth: auths, Timeout: 15 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			fp := ssh.FingerprintSHA256(key)
			f.seen = fp
			if pinned != "" && pinned != fp {
				return fmt.Errorf("host key %s does not match the pinned %s", fp, pinned)
			}
			return nil
		},
	}
	d := f.a.Policy.Dialer()
	raw, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return fmt.Errorf("cannot connect: %s", scrubErr(err))
	}
	c, chans, reqs, err := ssh.NewClientConn(raw, host, cfg)
	if err != nil {
		raw.Close()
		return fmt.Errorf("ssh login failed: %v", err)
	}
	f.conn = ssh.NewClient(c, chans, reqs)
	cl, err := sftp.NewClient(f.conn)
	if err != nil {
		f.conn.Close()
		return fmt.Errorf("sftp failed: %v", err)
	}
	f.client = cl
	if pinned == "" && f.s != nil && f.seen != "" {
		// Trust on first use: pin the key we saw.
		f.cfg["host_key_fingerprint"] = f.seen
		_, _ = f.a.DB.Exec(ctx, `UPDATE import_sources SET config = ? WHERE id = ?`, string(mustJSON(withoutKind(f.cfg))), f.s.ID)
	}
	return nil
}

func withoutKind(m map[string]any) map[string]any {
	out := cloneDoc(m)
	delete(out, "kind")
	return out
}

func (f *sftpFetcher) list(ctx context.Context) ([]remoteFile, string, error) {
	if err := f.connect(ctx); err != nil {
		return nil, "", err
	}
	root, _ := f.cfg["path"].(string)
	st, err := f.client.Stat(root)
	if err != nil {
		return nil, "", fmt.Errorf("path %s not found", root)
	}
	var out []remoteFile
	add := func(p string, fi os.FileInfo) {
		m := fi.ModTime().UTC().Format(time.RFC3339)
		out = append(out, remoteFile{Name: p, Size: fi.Size(), Modified: &m})
	}
	if !st.IsDir() {
		add(root, st)
	} else {
		w := f.client.Walk(root)
		n := 0
		for w.Step() {
			if w.Err() != nil || w.Stat().IsDir() {
				continue
			}
			if n++; n > maxZipEntries {
				break
			}
			if !importfmt.Ignored(w.Path()) {
				add(w.Path(), w.Stat())
			}
		}
	}
	return out, fingerprintOf(append([]remoteFile(nil), out...)), nil
}

func (f *sftpFetcher) open(ctx context.Context, rf remoteFile) (io.ReadCloser, error) {
	if err := f.connect(ctx); err != nil {
		return nil, err
	}
	return f.client.Open(rf.Name)
}

func (f *sftpFetcher) hostKey() string { return f.seen }

func (f *sftpFetcher) close() {
	if f.client != nil {
		f.client.Close()
	}
	if f.conn != nil {
		f.conn.Close()
	}
}

// ---- s3 ---------------------------------------------------------------------------------

type s3Fetcher struct {
	a      *App
	cfg    map[string]any
	creds  sourceCreds
	client *minio.Client
}

func (f *s3Fetcher) connect(ctx context.Context) error {
	if f.client != nil {
		return nil
	}
	region, _ := f.cfg["region"].(string)
	endpoint, _ := f.cfg["endpoint"].(string)
	if endpoint == "" {
		if region == "" {
			region = "us-east-1"
		}
		endpoint = "https://s3." + region + ".amazonaws.com"
	}
	if err := f.a.Policy.CheckURL(ctx, endpoint, "https"); err != nil {
		return err
	}
	u, _ := url.Parse(endpoint)
	lookup := minio.BucketLookupAuto
	if ps, _ := f.cfg["path_style"].(bool); ps {
		lookup = minio.BucketLookupPath
	}
	var creds *credentials.Credentials
	if f.creds.AccessKeyID != nil {
		creds = credentials.NewStaticV4(*f.creds.AccessKeyID, deref(f.creds.SecretAccessKey), "")
	} else {
		creds = credentials.NewStaticV4("", "", "")
	}
	tr := &http.Transport{DialContext: f.a.Policy.Dialer().DialContext, TLSHandshakeTimeout: 15 * time.Second, ResponseHeaderTimeout: 60 * time.Second}
	cl, err := minio.New(u.Host, &minio.Options{Creds: creds, Secure: u.Scheme != "http", Region: region, BucketLookup: lookup, Transport: tr})
	if err != nil {
		return err
	}
	f.client = cl
	return nil
}

func (f *s3Fetcher) list(ctx context.Context) ([]remoteFile, string, error) {
	if err := f.connect(ctx); err != nil {
		return nil, "", err
	}
	bucket, _ := f.cfg["bucket"].(string)
	prefix, _ := f.cfg["prefix"].(string)
	var out []remoteFile
	for obj := range f.client.ListObjects(ctx, bucket, minio.ListObjectsOptions{Prefix: prefix, Recursive: true}) {
		if obj.Err != nil {
			return nil, "", fmt.Errorf("listing failed: %v", obj.Err)
		}
		if strings.HasSuffix(obj.Key, "/") || importfmt.Ignored(obj.Key) {
			continue
		}
		m := obj.LastModified.UTC().Format(time.RFC3339)
		out = append(out, remoteFile{Name: obj.Key, Size: obj.Size, Modified: &m, Tag: obj.ETag})
		if len(out) > maxZipEntries {
			break
		}
	}
	return out, fingerprintOf(append([]remoteFile(nil), out...)), nil
}

func (f *s3Fetcher) open(ctx context.Context, rf remoteFile) (io.ReadCloser, error) {
	if err := f.connect(ctx); err != nil {
		return nil, err
	}
	bucket, _ := f.cfg["bucket"].(string)
	return f.client.GetObject(ctx, bucket, rf.Name, minio.GetObjectOptions{})
}

func (f *s3Fetcher) hostKey() string { return "" }
func (f *s3Fetcher) close()          {}

// ---- turning a remote folder into import files -------------------------------------------

// fetchSource downloads a source into staged import files. It returns
// unchanged=true when the remote files match the last successful run.
func (a *App) fetchSource(ctx context.Context, jobID string, p *importPayload) (unchanged bool, prob *problem.Problem) {
	s, err := a.loadSource(ctx, p.SourceID)
	if err != nil || s == nil {
		return false, problem.New("not_found", "The import source was deleted.")
	}
	f, pr := a.fetcherFor(s)
	if pr != nil {
		return false, pr
	}
	defer f.close()
	fctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	files, fp, err := f.list(fctx)
	if err != nil {
		code := "service_unavailable"
		if errors.Is(err, safehttp.ErrUnsafe) {
			code = "unsafe_url"
		}
		return false, problem.New(code, "Could not read the source: %s", err.Error())
	}
	if !p.Force && s.Fingerprint != nil && *s.Fingerprint == fp {
		return true, nil
	}
	p.Fingerprint = fp
	var csvs []remoteFile
	var zips []remoteFile
	stage := func(rf remoteFile, field, name string) *problem.Problem {
		rc, err := f.open(fctx, rf)
		if err != nil {
			return problem.New("service_unavailable", "Could not download %s: %s", rf.Name, scrubErr(err))
		}
		defer rc.Close()
		imf, err := a.stageFile(ctx, jobID, field, name, rc, MaxImportBytes)
		if err != nil {
			return problem.As(err)
		}
		p.Files = append(p.Files, *imf)
		return nil
	}
	for _, rf := range files {
		base := path.Base(rf.Name)
		lower := strings.ToLower(base)
		switch {
		case strings.HasSuffix(lower, ".csv"):
			csvs = append(csvs, rf)
		case strings.HasSuffix(lower, ".zip"):
			zips = append(zips, rf)
		case importfmt.IsImage(base):
			if pr := stage(rf, "images", base); pr != nil {
				return false, pr
			}
		}
	}
	var productsCSV, assetsCSV *remoteFile
	var others []remoteFile
	for i := range csvs {
		switch strings.ToLower(path.Base(csvs[i].Name)) {
		case "products.csv":
			productsCSV = &csvs[i]
		case "assets.csv":
			assetsCSV = &csvs[i]
		default:
			others = append(others, csvs[i])
		}
	}
	if productsCSV == nil && len(others) == 1 {
		productsCSV = &others[0]
	}
	if productsCSV != nil {
		if pr := stage(*productsCSV, "csv", "products.csv"); pr != nil {
			return false, pr
		}
	}
	if assetsCSV != nil {
		if pr := stage(*assetsCSV, "assets_csv", "assets.csv"); pr != nil {
			return false, pr
		}
	}
	for _, z := range zips {
		field := "images"
		if strings.Contains(strings.ToLower(path.Base(z.Name)), "package") || (productsCSV == nil && len(zips) == 1) {
			field = "package"
		}
		if pr := stage(z, field, path.Base(z.Name)); pr != nil {
			return false, pr
		}
	}
	if len(p.Files) == 0 {
		return false, problem.New("import_invalid", "The source contains no products.csv, zip or images.")
	}
	return false, nil
}

func (a *App) recordFingerprint(ctx context.Context, sourceID, fp string) {
	if fp == "" {
		return
	}
	_, _ = a.DB.Exec(ctx, `UPDATE import_sources SET last_fingerprint = ?, updated_at = updated_at WHERE id = ?`, fp, sourceID)
}

var _ = base64.StdEncoding
var _ = db.Fmt
