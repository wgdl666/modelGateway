package worker

import (
	"context"
	"errors"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss"
	osscredentials "github.com/aliyun/alibabacloud-oss-go-sdk-v2/oss/credentials"
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/wgdl666/modelGateway/internal/config"
	"github.com/wgdl666/modelGateway/internal/protocol"
)

type resultUploader struct {
	config config.NativeResults
	put    func(ctx context.Context, key, contentType string, body io.ReadSeeker, size int64) error
	get    func(ctx context.Context, key string) (io.ReadCloser, int64, error)
}

// ResultStore 读取本网关写入结果桶的成品。调用方仍然要核对对象属于哪一次请求。
type ResultStore struct{ *resultUploader }

func OpenResultStore(cfg config.NativeResults) (*ResultStore, error) {
	store, err := newResultUploader(cfg)
	if err != nil {
		return nil, err
	}
	return &ResultStore{store}, nil
}

func (s *ResultStore) Get(ctx context.Context, object protocol.Object) (io.ReadCloser, int64, error) {
	if s == nil || s.resultUploader == nil || s.get == nil {
		return nil, 0, errors.New("result storage is not configured")
	}
	if object.Provider != s.config.Provider || object.Region != s.config.Region || object.Bucket != s.config.Bucket ||
		object.Key == "" || strings.Contains(object.Key, "..") || strings.HasPrefix(object.Key, "/") {
		return nil, 0, errors.New("object is outside configured result storage")
	}
	return s.get(ctx, object.Key)
}

// 桶和凭据由网关部署指定，业务请求只能得到生成文件的引用。
func newResultUploader(cfg config.NativeResults) (*resultUploader, error) {
	if cfg.Bucket == "" || cfg.Region == "" {
		return nil, errors.New("native_results requires region and bucket")
	}
	if cfg.Endpoint != "" {
		u, err := url.Parse(cfg.Endpoint)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" ||
			u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
			return nil, errors.New("invalid native_results endpoint")
		}
	}
	uploader := &resultUploader{config: cfg}
	switch cfg.Provider {
	case "oss":
		if cfg.Endpoint == "" {
			return nil, errors.New("native_results OSS endpoint is required")
		}
		client := oss.NewClient(oss.LoadDefaultConfig().WithRegion(cfg.Region).WithEndpoint(cfg.Endpoint).
			WithCredentialsProvider(osscredentials.NewEnvironmentVariableCredentialsProvider()))
		uploader.put = func(ctx context.Context, key, contentType string, body io.ReadSeeker, size int64) error {
			_, err := client.PutObject(ctx, &oss.PutObjectRequest{
				Bucket: oss.Ptr(cfg.Bucket), Key: oss.Ptr(key), Body: body,
				ContentType: oss.Ptr(contentType), ContentLength: oss.Ptr(size),
			})
			return err
		}
		uploader.get = func(ctx context.Context, key string) (io.ReadCloser, int64, error) {
			result, err := client.GetObject(ctx, &oss.GetObjectRequest{Bucket: oss.Ptr(cfg.Bucket), Key: oss.Ptr(key)})
			if err != nil {
				return nil, 0, err
			}
			if result.ContentLength <= 0 {
				_ = result.Body.Close()
				return nil, 0, errors.New("result object has no length")
			}
			return result.Body, result.ContentLength, nil
		}
	case "s3":
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		sdk, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region),
			awsconfig.WithRequestChecksumCalculation(aws.RequestChecksumCalculationWhenRequired))
		if err != nil {
			return nil, errors.New("cannot load native_results S3 configuration")
		}
		client := s3.NewFromConfig(sdk, func(options *s3.Options) {
			if cfg.Endpoint != "" {
				options.BaseEndpoint = aws.String(cfg.Endpoint)
				options.UsePathStyle = true
			}
		})
		uploader.put = func(ctx context.Context, key, contentType string, body io.ReadSeeker, size int64) error {
			_, err := client.PutObject(ctx, &s3.PutObjectInput{
				Bucket: aws.String(cfg.Bucket), Key: aws.String(key), Body: body,
				ContentType: aws.String(contentType), ContentLength: aws.Int64(size),
			})
			return err
		}
		uploader.get = func(ctx context.Context, key string) (io.ReadCloser, int64, error) {
			result, err := client.GetObject(ctx, &s3.GetObjectInput{Bucket: aws.String(cfg.Bucket), Key: aws.String(key)})
			if err != nil {
				return nil, 0, err
			}
			if result.ContentLength == nil || *result.ContentLength <= 0 {
				_ = result.Body.Close()
				return nil, 0, errors.New("result object has no length")
			}
			return result.Body, *result.ContentLength, nil
		}
	default:
		return nil, errors.New("native_results provider must be oss or s3")
	}
	return uploader, nil
}

func (u *resultUploader) upload(ctx context.Context, key, contentType string, body io.ReadSeeker, size int64) (protocol.Object, error) {
	if err := u.put(ctx, key, contentType, body, size); err != nil {
		return protocol.Object{}, err
	}
	return protocol.Object{Provider: u.config.Provider, Region: u.config.Region, Bucket: u.config.Bucket, Key: key}, nil
}
