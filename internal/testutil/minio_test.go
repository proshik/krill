package testutil

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/proshik/krill/internal/backup"
)

// validBucket is S3's bucket naming rule as far as these names can break it.
var validBucket = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,61}[a-z0-9]$`)

func TestNewMinioSharedServerWithPerTestBuckets(t *testing.T) {
	ctx := context.Background()
	first := NewMinio(t)

	// Two tests asking for the same base each get a bucket of their own,
	// and both can create it on the one server: with a fixed name the
	// second CreateBucket would fail with BucketAlreadyOwnedByYou.
	var names []string
	for _, sub := range []string{"one", "two", strings.Repeat("Long Name/", 10)} {
		t.Run(sub, func(t *testing.T) {
			m := NewMinio(t)
			if m != first {
				t.Fatalf("NewMinio = %+v, want the shared server %+v", m, first)
			}
			name := m.Bucket(t, "test")
			if !validBucket.MatchString(name) {
				t.Fatalf("bucket name %q is not a valid S3 bucket name", name)
			}
			if !strings.HasPrefix(name, "test-") {
				t.Errorf("bucket name %q does not start with its base", name)
			}
			d := backup.Destination{Endpoint: m.Endpoint, Bucket: name, Region: m.Region, AccessKey: m.AccessKey, SecretKey: m.SecretKey}
			if err := backup.CreateBucket(ctx, d, true); err != nil {
				t.Fatalf("create bucket %q: %v", name, err)
			}
			names = append(names, name)
		})
	}
	if len(names) != 3 || names[0] == names[1] || names[1] == names[2] || names[0] == names[2] {
		t.Fatalf("bucket names are not distinct: %q", names)
	}
}

func TestBucketName(t *testing.T) {
	for _, tc := range []struct {
		base, test string
		n          int64
		want       string
	}{
		{"test", "TestS3Roundtrip", 1, "test-1-tests3roundtrip"},
		{"krill-state-rt", "TestRT/sub_case with spaces", 12, "krill-state-rt-12-testrt-sub-case-with-spaces"},
		{"__", "Test", 3, "bucket-3-test"},
		{"x", "", 4, "x-4"},
	} {
		if got := bucketName(tc.base, tc.test, tc.n); got != tc.want {
			t.Errorf("bucketName(%q, %q, %d) = %q, want %q", tc.base, tc.test, tc.n, got, tc.want)
		}
	}
	// A long base and test name still keep the counter and the length limit,
	// and never end on a hyphen.
	for i := int64(1); i < 2000; i += 97 {
		got := bucketName(strings.Repeat("ab-", 30), strings.Repeat("Ä/x-", 40), i)
		if !validBucket.MatchString(got) {
			t.Fatalf("bucketName = %q, not a valid S3 bucket name", got)
		}
		if !strings.Contains(got, "-"+strconv.FormatInt(i, 10)+"-") {
			t.Fatalf("bucketName = %q lost its counter %d", got, i)
		}
	}
}
