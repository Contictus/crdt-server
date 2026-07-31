// Package blob stores document bytes in S3-compatible object storage.
//
// It exists because of what history costs. A version is a whole document, and
// the default is twenty-four of them per document; at a hundred thousand
// documents that is a database holding terabytes of blobs it never queries, with
// the vacuum pressure and the backup times that implies. Object storage is
// roughly twenty times cheaper per byte and does not have to be backed up by the
// same machinery as the rows.
//
// What stays in PostgreSQL: everything that is queried. The document row, the
// owner, the name, the sequence numbers, the state vectors, the update log. What
// moves here: the snapshot and the version payloads, which are only ever read
// whole, by primary key, and never joined against anything.
//
// # What this client is not
//
// It is four requests - PUT, GET, DELETE, HEAD - signed with SigV4 written out
// in sigv4.go. That is a deliberate trade and these are the things it gives up,
// stated here rather than discovered later:
//
//   - No IRSA, no EC2 instance roles, no SSO, no shared config file. Credentials
//     come from flags or from AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY /
//     AWS_SESSION_TOKEN. On Kubernetes that means a Secret rather than a service
//     account annotation.
//   - No multipart upload. A single PUT is capped at 5 GB by S3, and a Yjs
//     snapshot that large is a different problem than this one.
//   - No presigned URLs, no bucket management, no lifecycle configuration.
//
// If those matter more than the dependency count, the interface here is small
// enough that an SDK-backed implementation is a drop-in.
package blob

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// ErrNotFound means the object is not there. It is distinguished from every
// other failure because a missing object and an unreachable bucket call for
// opposite responses: one is a document that was deleted, the other is an
// outage.
var ErrNotFound = errors.New("blob: no such object")

// ErrNoBucket means the bucket itself is not there - a typo in -s3-bucket, a
// bucket in another region, or one nobody created.
//
// S3 answers that with the same 404 it uses for a missing object, and reading
// them as the same thing is wrong in both directions. The message points at the
// key when the problem is the configuration, which sends whoever is debugging
// to look for a document that was never the issue. Worse, a missing object is
// benign in two places - Delete treats it as success, Exists reports false -
// so a mistyped bucket would have every write fail while every delete reported
// success, silently, forever.
//
// Found by running the tests against a MinIO whose bucket had been wiped: the
// failures said "no such object" for a bucket that did not exist.
var ErrNoBucket = errors.New("blob: no such bucket")

// maxObject bounds what will be read back into memory. A snapshot is a whole
// document, and a document that has grown past this is a problem to notice
// rather than an allocation to make.
const maxObject = 256 << 20

// DefaultTimeout bounds one request.
const DefaultTimeout = 30 * time.Second

// Config configures an S3 client.
type Config struct {
	// Bucket is required.
	Bucket string
	// Region is required; S3-compatible services that do not have regions
	// conventionally accept "us-east-1".
	Region string
	// Endpoint points at something that is not AWS - MinIO, R2, Backblaze,
	// Ceph. Empty means AWS, addressed virtual-host style.
	Endpoint string
	// Prefix namespaces every key, so one bucket can hold more than one
	// deployment.
	Prefix string
	// PathStyle addresses objects as <endpoint>/<bucket>/<key> rather than
	// <bucket>.<endpoint>/<key>. It is forced on when Endpoint is set, because
	// that is what every S3-compatible service supports and what AWS is moving
	// away from.
	PathStyle bool

	Credentials Credentials
	Timeout     time.Duration
	Client      *http.Client

	// now is injectable so a signature can be checked against a fixed clock.
	now func() time.Time
}

// A Client talks to one bucket. It is safe for concurrent use.
type Client struct {
	cfg    Config
	base   *url.URL
	client *http.Client
}

// New returns a client, or an error if the configuration cannot address a
// bucket.
func New(cfg Config) (*Client, error) {
	if cfg.Bucket == "" {
		return nil, errors.New("blob: no bucket")
	}
	if cfg.Region == "" {
		// A region is part of the signature's scope, so there is no sensible
		// default: guessing would produce a signature the service refuses with
		// a message about credentials rather than about configuration.
		return nil, errors.New("blob: no region")
	}
	if cfg.Credentials.AccessKeyID == "" {
		cfg.Credentials = credentialsFromEnvironment()
	}
	if cfg.Credentials.AccessKeyID == "" || cfg.Credentials.SecretAccessKey == "" {
		return nil, errors.New("blob: no credentials; set -s3-access-key and -s3-secret-key, or AWS_ACCESS_KEY_ID and AWS_SECRET_ACCESS_KEY")
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Client == nil {
		cfg.Client = &http.Client{Timeout: cfg.Timeout}
	}
	if cfg.now == nil {
		cfg.now = time.Now
	}

	base, err := endpointURL(cfg)
	if err != nil {
		return nil, err
	}
	return &Client{cfg: cfg, base: base, client: cfg.Client}, nil
}

// endpointURL works out the address objects hang off.
func endpointURL(cfg Config) (*url.URL, error) {
	if cfg.Endpoint == "" {
		// AWS, virtual-host style. Path style still works for existing buckets
		// there but AWS has said it is going away, so it is not the default for
		// a URL we are constructing today.
		host := cfg.Bucket + ".s3." + cfg.Region + ".amazonaws.com"
		if cfg.PathStyle {
			host = "s3." + cfg.Region + ".amazonaws.com"
		}
		return &url.URL{Scheme: "https", Host: host}, nil
	}
	raw := cfg.Endpoint
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("blob: bad endpoint: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("blob: endpoint %q names no host", cfg.Endpoint)
	}
	return u, nil
}

// credentialsFromEnvironment reads the three variables every S3 tool honours.
func credentialsFromEnvironment() Credentials {
	return Credentials{
		AccessKeyID:     os.Getenv("AWS_ACCESS_KEY_ID"),
		SecretAccessKey: os.Getenv("AWS_SECRET_ACCESS_KEY"),
		SessionToken:    os.Getenv("AWS_SESSION_TOKEN"),
	}
}

// objectURL builds the address of one key.
func (c *Client) objectURL(key string) *url.URL {
	u := *c.base
	path := c.cfg.Prefix + key
	// Path style, and every custom endpoint, put the bucket in the path.
	if c.cfg.Endpoint != "" || c.cfg.PathStyle {
		u.Path = "/" + c.cfg.Bucket + "/" + path
	} else {
		u.Path = "/" + path
	}
	return &u
}

// Put stores an object.
func (c *Client) Put(ctx context.Context, key string, body []byte) error {
	req, err := c.request(ctx, http.MethodPut, key, body)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return c.statusError("put", key, resp)
	}
	return nil
}

// Get reads an object.
func (c *Client) Get(ctx context.Context, key string) ([]byte, error) {
	req, err := c.request(ctx, http.MethodGet, key, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, err
	}
	defer drain(resp)
	if resp.StatusCode != http.StatusOK {
		return nil, c.statusError("get", key, resp)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxObject+1))
	if err != nil {
		return nil, fmt.Errorf("blob: get %s: %w", key, err)
	}
	if len(body) > maxObject {
		return nil, fmt.Errorf("blob: get %s: object is larger than %d bytes", key, maxObject)
	}
	return body, nil
}

// Delete removes an object. Deleting one that is not there is not an error, the
// same way it is not for S3 itself: the caller wanted it gone and it is gone.
func (c *Client) Delete(ctx context.Context, key string) error {
	req, err := c.request(ctx, http.MethodDelete, key, nil)
	if err != nil {
		return err
	}
	resp, err := c.do(req)
	if err != nil {
		return err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	}
	// A 404 is where "the object was already gone" and "the bucket does not
	// exist" arrive together. The first is success; the second means nothing
	// was deleted and nothing ever will be, and reporting it as success is how
	// a mistyped bucket stays undiscovered.
	if err := c.statusError("delete", key, resp); !errors.Is(err, ErrNotFound) {
		return err
	}
	return nil
}

// Exists reports whether an object is there, without reading it.
//
// It cannot distinguish a missing bucket from a missing object: S3 answers HEAD
// with a status and no body, and the body is the only thing that names the
// code. A false here therefore means "not readable", which is why nothing that
// decides to delete something uses it.
func (c *Client) Exists(ctx context.Context, key string) (bool, error) {
	req, err := c.request(ctx, http.MethodHead, key, nil)
	if err != nil {
		return false, err
	}
	resp, err := c.do(req)
	if err != nil {
		return false, err
	}
	defer drain(resp)
	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	}
	return false, c.statusError("head", key, resp)
}

// request builds and signs one request.
func (c *Client) request(ctx context.Context, method, key string, body []byte) (*http.Request, error) {
	if key == "" {
		return nil, errors.New("blob: empty key")
	}
	u := c.objectURL(key)
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("blob: %s %s: %w", strings.ToLower(method), key, err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	// A GET, HEAD or DELETE has no body, and its payload hash is the hash of
	// nothing - not a special constant.
	sign(req, c.cfg.Credentials, c.cfg.Region, c.cfg.now(), hexSHA256(body))
	return req, nil
}

func (c *Client) do(req *http.Request) (*http.Response, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("blob: %s: %w", strings.ToLower(req.Method), err)
	}
	return resp, nil
}

// statusError turns a refusal into something an operator can act on. S3's error
// bodies are XML naming the code; the first two hundred bytes carry it, and
// parsing XML to extract a string that is already legible is not worth a decoder.
func (c *Client) statusError(op, key string, resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
	detail := strings.TrimSpace(string(body))
	if resp.StatusCode == http.StatusNotFound {
		// A missing bucket and a missing object are both a 404; only the body
		// says which. HEAD has no body, so Exists cannot tell them apart and
		// says so where it is defined.
		if strings.Contains(detail, "<Code>NoSuchBucket</Code>") {
			return fmt.Errorf("%w: %s", ErrNoBucket, c.cfg.Bucket)
		}
		return fmt.Errorf("%w: %s", ErrNotFound, key)
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	return fmt.Errorf("blob: %s %s: %s: %s", op, key, resp.Status, detail)
}

// drain reads and closes a response body, so the connection goes back to the
// pool instead of being thrown away.
func drain(resp *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
}
