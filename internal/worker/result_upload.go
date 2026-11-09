package worker

import (
	"context"
	"errors"
	"io"
	"net/url"
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
