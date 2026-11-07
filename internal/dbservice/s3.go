package dbservice

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
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
// read back and delete an object in it. The probe object has a random name
// checked to be free first, so no existing object is touched.
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
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return err
	}
	probe := "k/" + b.Name + "/.krill-import-check-" + hex.EncodeToString(rnd[:])
	if exists, err := s.mcObjectExists(ctx, inst, probe); err != nil {
		return err
	} else if exists {
		return errors.New("import probe name already taken; retry")
	}
	if err := s.mcAs(ctx, inst, b.AccessKey, b.SecretKey, "pipe", strings.NewReader("krill"), "pipe", probe); err != nil {
		var me *mcError
		if errors.As(err, &me) {
			return ErrBucketCredentials
		}
		return err
	}
	// Whatever the read shows, the probe goes again: it is ours alone.
	defer func() {
		cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), provisionCompensateTimeout)
		defer cancel()
		if err := s.mc(cctx, inst, "rm probe", nil, "rm", probe); err != nil {
			slog.Warn("import bucket: probe object not removed", "bucket", b.Name, "err", err)
		}
	}()
	var got bytes.Buffer
	if err := s.engine.Exec(ctx, inst.AppName, []string{"mc", "cat", probe},
		[]string{"MC_HOST_k=" + mcHost(b.AccessKey, b.SecretKey)}, nil, &got); err != nil || got.String() != "krill" {
		return ErrBucketCredentials
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

// mcObjectExists reports whether an object exists, from mc's own answer: a
// "success" status is yes, "Object does not exist" is no, anything else is an
// error — never a guess, since a wrong "no" would overwrite someone's object.
func (s *Service) mcObjectExists(ctx context.Context, inst Instance, path string) (bool, error) {
	var out bytes.Buffer
	err := s.engine.Exec(ctx, inst.AppName, []string{"mc", "--json", "stat", path},
		[]string{"MC_HOST_k=" + mcHost(inst.Superuser, inst.SuperuserPassword)}, nil, &out)
	res, parsed := lastMCResult(out.Bytes())
	switch {
	case parsed && res.Status == "success" && err == nil:
		return true, nil
	case parsed && res.Status == "error" && strings.Contains(res.Error.Cause.Message, "does not exist"):
		return false, nil
	}
	if cerr := ctx.Err(); cerr != nil {
		return false, cerr
	}
	return false, &mcError{Op: "stat", Message: "could not tell whether the probe object exists"}
}

// RemoveEmptyBucket removes a bucket only if it holds no objects (mc rb
// without --force): for undoing a bucket Krill just created, where anything
// written into it meanwhile must survive.
func (s *Service) RemoveEmptyBucket(ctx context.Context, inst Instance, bucket string) error {
	if s.engine == nil {
		return nil
	}
	if !ValidBucketName(bucket) {
		return errors.New("invalid bucket name")
	}
	return s.mc(ctx, inst, "rb", nil, "rb", "k/"+bucket)
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

// ObjectEntry is one object of a bucket.
type ObjectEntry struct {
	Key  string
	Size int64
}

// ErrTooManyObjects is returned by ListObjects past its limit.
var ErrTooManyObjects = errors.New("the bucket holds more objects than a snapshot may")

// ListObjects lists every object of a bucket (mc ls --recursive), parsing the
// listing as it streams in and failing once it passes limit objects, so a
// huge bucket costs a bounded amount of memory in Krill.
func (s *Service) ListObjects(ctx context.Context, inst Instance, bucket string, limit int) ([]ObjectEntry, error) {
	if !ValidBucketName(bucket) {
		return nil, errors.New("invalid bucket name")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pr, pw := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := s.engine.Exec(ctx, inst.AppName, []string{"mc", "--json", "ls", "--recursive", "k/" + bucket},
			[]string{"MC_HOST_k=" + mcHost(inst.Superuser, inst.SuperuserPassword)}, nil, pw)
		pw.CloseWithError(err)
		done <- err
	}()
	var entries []ObjectEntry
	var listErr error
	sc := bufio.NewScanner(pr)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var e struct {
			Status string `json:"status"`
			Type   string `json:"type"`
			Key    string `json:"key"`
			Size   int64  `json:"size"`
		}
		if json.Unmarshal(line, &e) != nil {
			continue
		}
		if e.Status == "error" {
			res, _ := lastMCResult(line)
			listErr = &mcError{Op: "ls", Message: res.Error.Message + ": " + res.Error.Cause.Message, Code: res.Error.Cause.Error.Code}
			break
		}
		if e.Type == "file" {
			if len(entries) >= limit {
				listErr = fmt.Errorf("%w (%d)", ErrTooManyObjects, limit)
				break
			}
			entries = append(entries, ObjectEntry{Key: e.Key, Size: e.Size})
		}
	}
	if listErr == nil {
		listErr = sc.Err()
	}
	if listErr != nil {
		cancel()
		pr.CloseWithError(listErr)
		<-done
		return nil, listErr
	}
	if err := <-done; err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, fmt.Errorf("mc ls: %w", err)
	}
	return entries, nil
}

// ReadObject streams one object of a bucket to w (mc cat).
func (s *Service) ReadObject(ctx context.Context, inst Instance, bucket, key string, w io.Writer) error {
	if !ValidBucketName(bucket) {
		return errors.New("invalid bucket name")
	}
	return s.engine.Exec(ctx, inst.AppName, []string{"mc", "cat", "k/" + bucket + "/" + key},
		[]string{"MC_HOST_k=" + mcHost(inst.Superuser, inst.SuperuserPassword)}, nil, w)
}

// WriteObject stores r as one object of a bucket (mc pipe).
func (s *Service) WriteObject(ctx context.Context, inst Instance, bucket, key string, r io.Reader) error {
	if !ValidBucketName(bucket) {
		return errors.New("invalid bucket name")
	}
	return s.mc(ctx, inst, "pipe", r, "pipe", "k/"+bucket+"/"+key)
}
