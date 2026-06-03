package backup

import (
	"context"
	"io"
	"sort"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/feature/s3/manager"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Destination is the S3 target (mapped from the destinations row).
type Destination struct {
	Endpoint  string
	Bucket    string
	Region    string
	AccessKey string
	SecretKey string
}

// Object is one stored backup.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

func clientFor(ctx context.Context, d Destination) (*s3.Client, error) {
	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(d.Region),
		awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(d.AccessKey, d.SecretKey, "")),
	)
	if err != nil {
		return nil, err
	}
	return s3.NewFromConfig(cfg, func(o *s3.Options) {
		if d.Endpoint != "" {
			o.BaseEndpoint = &d.Endpoint
			o.UsePathStyle = true
		}
	}), nil
}

// CheckAccess verifies credentials + bucket via HeadBucket.
func CheckAccess(ctx context.Context, d Destination) error {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return err
	}
	_, err = cl.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &d.Bucket})
	return err
}

// CreateBucket creates the bucket (used in tests / first-time setup).
func CreateBucket(ctx context.Context, d Destination) error {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return err
	}
	_, err = cl.CreateBucket(ctx, &s3.CreateBucketInput{Bucket: &d.Bucket})
	return err
}

func Upload(ctx context.Context, d Destination, key string, r io.Reader) error {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return err
	}
	_, err = manager.NewUploader(cl).Upload(ctx, &s3.PutObjectInput{Bucket: &d.Bucket, Key: &key, Body: r})
	return err
}

func List(ctx context.Context, d Destination, prefix string) ([]Object, error) {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return nil, err
	}
	var out []Object
	p := s3.NewListObjectsV2Paginator(cl, &s3.ListObjectsV2Input{Bucket: &d.Bucket, Prefix: &prefix})
	for p.HasMorePages() {
		page, err := p.NextPage(ctx)
		if err != nil {
			return nil, err
		}
		for _, o := range page.Contents {
			obj := Object{Key: *o.Key}
			if o.Size != nil {
				obj.Size = *o.Size
			}
			if o.LastModified != nil {
				obj.LastModified = *o.LastModified
			}
			out = append(out, obj)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastModified.After(out[j].LastModified) })
	return out, nil
}

func Download(ctx context.Context, d Destination, key string) (io.ReadCloser, error) {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return nil, err
	}
	o, err := cl.GetObject(ctx, &s3.GetObjectInput{Bucket: &d.Bucket, Key: &key})
	if err != nil {
		return nil, err
	}
	return o.Body, nil
}

func Delete(ctx context.Context, d Destination, key string) error {
	cl, err := clientFor(ctx, d)
	if err != nil {
		return err
	}
	_, err = cl.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: &d.Bucket, Key: &key})
	return err
}
