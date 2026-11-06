package dbservice

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"regexp"
	"strings"
)

// Buckets on a managed S3 instance (engine minio — Silo or MinIO) are managed
// with `mc` run inside the instance's own container through Engine.Exec: the
// image ships it, Exec already reaches a container on any node, and nothing new
// is needed in Krill to speak MinIO's admin API. The root credentials travel in
// the exec's environment (MC_HOST_<alias>), never in argv or a config file.
// mc's own output can echo a secret key back, so it is parsed for its status
// and never logged or returned verbatim.

// bucketNameRe — S3 bucket naming as MinIO enforces it, minus dots (which
// break virtual-host addressing and TLS for no benefit here).
var bucketNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

// ValidBucketName reports whether name is a bucket name Krill creates or adopts.
func ValidBucketName(name string) bool {
	return bucketNameRe.MatchString(name) && !strings.Contains(name, "--")
}

// accessKeyRe — the access keys Krill generates or adopts.
var accessKeyRe = regexp.MustCompile(`^[A-Za-z0-9._-]{3,128}$`)

// ValidAccessKey reports whether key is an access key Krill accepts.
func ValidAccessKey(key string) bool { return accessKeyRe.MatchString(key) }

// BucketPolicyName is the canned policy granting one bucket; shared by every
// principal Krill gives access to that bucket.
func BucketPolicyName(bucket string) string { return "krill-bucket-" + bucket }

// bucketPolicy grants every S3 action on the bucket and its objects, and
// nothing else (no other bucket, no listing of buckets, no admin API).
func bucketPolicy(bucket string) string {
	return fmt.Sprintf(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:*"],"Resource":["arn:aws:s3:::%[1]s","arn:aws:s3:::%[1]s/*"]}]}`, bucket)
}

// ErrMCMissing is returned when the instance's image has no mc.
var ErrMCMissing = errors.New("the S3 server's image has no mc: bucket management needs it (the default Silo image ships it)")

// ErrBucketMissing is returned by ImportBucket for a bucket that does not exist.
var ErrBucketMissing = errors.New("bucket does not exist on the server")

// ErrBucketCredentials is returned by ImportBucket when the given principal
// cannot use the bucket.
var ErrBucketCredentials = errors.New("the access key cannot read and write this bucket")

// mcResult is the part of `mc --json` output Krill reads.
type mcResult struct {
	Status string `json:"status"`
	Error  struct {
		Message string `json:"message"`
		Cause   struct {
			Message string `json:"message"`
			Error   struct {
				Code string `json:"Code"`
			} `json:"error"`
		} `json:"cause"`
	} `json:"error"`
}

// mcError is a failed mc command, carrying only mc's own message (never its
// whole output, which may contain a secret key).
type mcError struct {
	Op      string
	Message string
	Code    string
}

func (e *mcError) Error() string {
	msg := e.Message
	if msg == "" {
		msg = "failed"
	}
	return "mc " + e.Op + ": " + msg
}

// mcHost is the MC_HOST_<alias> value addressing the server inside its own
// container with the given credentials.
func mcHost(user, pass string) string {
	u := url.URL{Scheme: "http", Host: "127.0.0.1:9000", User: url.UserPassword(user, pass)}
	return u.String()
}

// mc runs `mc --json args...` in the instance's container as the root user,
// with stdin, and returns the parsed result of the last JSON line.
func (s *Service) mc(ctx context.Context, inst Instance, op string, stdin io.Reader, args ...string) error {
	return s.mcAs(ctx, inst, inst.Superuser, inst.SuperuserPassword, op, stdin, args...)
}

// mcAs is mc with another principal's credentials (to prove what it can do).
func (s *Service) mcAs(ctx context.Context, inst Instance, user, pass, op string, stdin io.Reader, args ...string) error {
	var out bytes.Buffer
	cmd := append([]string{"mc", "--json"}, args...)
	err := s.engine.Exec(ctx, inst.AppName, cmd, []string{"MC_HOST_k=" + mcHost(user, pass)}, stdin, &out)
	res, parsed := lastMCResult(out.Bytes())
	if err == nil && (!parsed || res.Status != "error") {
		return nil
	}
	if !parsed {
		text := out.String()
		if strings.Contains(text, "executable file not found") || strings.Contains(text, "mc: not found") ||
			(err != nil && strings.Contains(err.Error(), "executable file not found")) {
			return ErrMCMissing
		}
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("mc %s: %w", op, cerr)
		}
		// Engine.Exec reports a non-zero exit with the whole argv, which for
		// `admin user add` holds the new secret key: that text must not reach a
		// flash message or a log. Failures before the command ran (no running
		// task, an unreachable node) carry no argv and are kept.
		if err != nil && !strings.Contains(err.Error(), " exited ") {
			return fmt.Errorf("mc %s: %w", op, err)
		}
		return &mcError{Op: op, Message: "the command failed without a result"}
	}
	msg := res.Error.Message
	if c := res.Error.Cause.Message; c != "" {
		msg += ": " + c
	}
	return &mcError{Op: op, Message: msg, Code: res.Error.Cause.Error.Code}
}

// lastMCResult parses the last JSON object mc printed.
func lastMCResult(out []byte) (mcResult, bool) {
	lines := bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	for i := len(lines) - 1; i >= 0; i-- {
		line := bytes.TrimSpace(lines[i])
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var r mcResult
		if json.Unmarshal(line, &r) == nil && r.Status != "" {
			return r, true
		}
	}
	return mcResult{}, false
}

// Bucket is a bucket and the principal linked apps use.
type Bucket struct {
	Name      string
	AccessKey string
	SecretKey string
}

// CreateBucket creates the bucket, its canned policy and a principal holding
// only that policy. On a failure part-way it removes what it created, so a
// retry starts clean. Engine-gated (nil engine → no-op, tests only).
func (s *Service) CreateBucket(ctx context.Context, inst Instance, b Bucket) error {
	if s.engine == nil {
		return nil
	}
	if !ValidBucketName(b.Name) || !ValidAccessKey(b.AccessKey) {
		return errors.New("invalid bucket name or access key")
	}
	if err := s.mc(ctx, inst, "mb", nil, "mb", "k/"+b.Name); err != nil {
		return err
	}
	if err := s.grantPrincipal(ctx, inst, b.Name, b.AccessKey, b.SecretKey); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionCompensateTimeout)
		defer cancel()
		if rerr := s.mc(cctx, inst, "rb", nil, "rb", "k/"+b.Name); rerr != nil {
			slog.Error("create bucket: compensating bucket removal failed", "bucket", b.Name, "err", rerr)
		}
		return err
	}
	return nil
}

// grantPrincipal ensures the bucket's policy and adds a principal holding it,
// removing the principal again if attaching the policy fails.
func (s *Service) grantPrincipal(ctx context.Context, inst Instance, bucket, accessKey, secretKey string) error {
	if err := s.mc(ctx, inst, "admin policy create", strings.NewReader(bucketPolicy(bucket)),
		"admin", "policy", "create", "k", BucketPolicyName(bucket), "/dev/stdin"); err != nil {
		return err
	}
	if err := s.mc(ctx, inst, "admin user add", nil, "admin", "user", "add", "k", accessKey, secretKey); err != nil {
		return err
	}
	if err := s.mc(ctx, inst, "admin policy attach", nil, "admin", "policy", "attach", "k", BucketPolicyName(bucket), "--user", accessKey); err != nil {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionCompensateTimeout)
		defer cancel()
		if rerr := s.mc(cctx, inst, "admin user remove", nil, "admin", "user", "remove", "k", accessKey); rerr != nil {
			slog.Error("grant bucket principal: compensating user removal failed", "bucket", bucket, "access_key", accessKey, "err", rerr)
		}
		return err
	}
	return nil
}

// AddPrincipal adds a new principal for an existing bucket (a key rotation).
func (s *Service) AddPrincipal(ctx context.Context, inst Instance, bucket, accessKey, secretKey string) error {
	if s.engine == nil {
		return nil
	}
	if !ValidBucketName(bucket) || !ValidAccessKey(accessKey) {
		return errors.New("invalid bucket name or access key")
	}
	return s.grantPrincipal(ctx, inst, bucket, accessKey, secretKey)
}

// RemovePrincipal deletes a principal. A principal that no longer exists is
// not an error.
func (s *Service) RemovePrincipal(ctx context.Context, inst Instance, accessKey string) error {
	if s.engine == nil || accessKey == "" {
		return nil
	}
	err := s.mc(ctx, inst, "admin user remove", nil, "admin", "user", "remove", "k", accessKey)
	var me *mcError
	if errors.As(err, &me) && me.Code == "XMinioAdminNoSuchUser" {
		return nil
	}
	return err
}

// ImportBucket adopts an existing bucket and principal without recreating
// either: the bucket must exist, and the principal must be able to write,
// read and delete an object in it.
func (s *Service) ImportBucket(ctx context.Context, inst Instance, b Bucket) error {
	if s.engine == nil {
		return nil
	}
	if !ValidBucketName(b.Name) || !ValidAccessKey(b.AccessKey) {
		return errors.New("invalid bucket name or access key")
	}
	if err := s.mc(ctx, inst, "stat bucket", nil, "stat", "k/"+b.Name); err != nil {
		var me *mcError
		if errors.As(err, &me) {
			return ErrBucketMissing
		}
		return err
	}
	probe := "k/" + b.Name + "/.krill-import-check"
	if err := s.mcAs(ctx, inst, b.AccessKey, b.SecretKey, "pipe", strings.NewReader("krill"), "pipe", probe); err != nil {
		var me *mcError
		if errors.As(err, &me) {
			return ErrBucketCredentials
		}
		return err
	}
	if err := s.mcAs(ctx, inst, b.AccessKey, b.SecretKey, "rm", nil, "rm", probe); err != nil {
		var me *mcError
		if errors.As(err, &me) {
			return ErrBucketCredentials
		}
		return err
	}
	return nil
}

// RemoveBucket deletes the bucket with every object in it.
func (s *Service) RemoveBucket(ctx context.Context, inst Instance, bucket string) error {
	if s.engine == nil {
		return nil
	}
	if !ValidBucketName(bucket) {
		return errors.New("invalid bucket name")
	}
	return s.mc(ctx, inst, "rb", nil, "rb", "--force", "k/"+bucket)
}
