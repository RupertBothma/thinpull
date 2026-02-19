package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	awss3 "github.com/aws/aws-sdk-go-v2/service/s3"
)

type checkResult struct {
	Name   string
	Pass   bool
	Detail string
}

func main() {
	bucket := flag.String("bucket", "flyio-container-images", "S3 bucket to check")
	prefix := flag.String("prefix", "images/", "S3 prefix to list (minimal)")
	region := flag.String("region", "", "AWS region (optional; falls back to default chain)")
	timeout := flag.Duration("timeout", 20*time.Second, "per-operation timeout")
	flag.Parse()

	ctx := context.Background()

	loadOpts := []func(*config.LoadOptions) error{}
	if *region != "" {
		loadOpts = append(loadOpts, config.WithRegion(*region))
	}
	cfg, err := config.LoadDefaultConfig(ctx, loadOpts...)
	if err != nil {
		fmt.Printf("FATAL: failed to load AWS config: %v\n", err)
		os.Exit(2)
	}

	s3 := awss3.NewFromConfig(cfg)

	var results []checkResult

	// OPTIONAL: GetBucketLocation
	{
		ctxOp, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		_, err := s3.GetBucketLocation(ctxOp, &awss3.GetBucketLocationInput{Bucket: bucket})
		results = append(results, classify("s3:GetBucketLocation", err))
	}

	// REQUIRED: ListObjectsV2
	var firstKey string
	{
		ctxOp, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		out, err := s3.ListObjectsV2(ctxOp, &awss3.ListObjectsV2Input{Bucket: bucket, Prefix: prefix, MaxKeys: aws.Int32(1)})
		res := classify("s3:ListBucket", err)
		if err == nil {
			if len(out.Contents) > 0 && out.Contents[0].Key != nil {
				firstKey = *out.Contents[0].Key
				res.Detail = fmt.Sprintf("listed OK (sample key: %s)", firstKey)
			} else {
				res.Detail = "listed OK (no objects under prefix)"
			}
		}
		results = append(results, res)
	}

	// REQUIRED: HeadObject and GetObject
	if firstKey != "" {
		// HeadObject
		{
			ctxOp, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			_, err := s3.HeadObject(ctxOp, &awss3.HeadObjectInput{Bucket: bucket, Key: &firstKey})
			results = append(results, classify("s3:HeadObject", err))
		}
		// GetObject (range 0-0)
		{
			ctxOp, cancel := context.WithTimeout(ctx, *timeout)
			defer cancel()
			out, err := s3.GetObject(ctxOp, &awss3.GetObjectInput{Bucket: bucket, Key: &firstKey, Range: aws.String("bytes=0-0")})
			res := classify("s3:GetObject", err)
			if err == nil && out.Body != nil {
				_, _ = io.CopyN(io.Discard, out.Body, 1)
				out.Body.Close()
				res.Detail = "read 1 byte OK"
			}
			results = append(results, res)
		}
	}

	// Print summary
	fmt.Println("AWS S3 permission check summary:")
	missingRequired := 0
	for _, r := range results {
		status := "OK"
		if !r.Pass {
			if isRequired(r.Name) {
				status = "MISSING"
				missingRequired++
			} else {
				status = "OPTIONAL"
			}
		}
		if r.Detail != "" {
			fmt.Printf("- %-18s : %-8s — %s\n", r.Name, status, r.Detail)
		} else {
			fmt.Printf("- %-18s : %-8s\n", r.Name, status)
		}
	}

	if missingRequired > 0 {
		fmt.Printf("\nResult: %d required permission(s) missing.\n", missingRequired)
		os.Exit(1)
	}
	fmt.Println("\nResult: all required permissions present.")
}

func classify(name string, err error) checkResult {
	if err == nil {
		return checkResult{Name: name, Pass: true}
	}
	return checkResult{Name: name, Pass: false, Detail: strings.TrimSpace(err.Error())}
}

func isRequired(name string) bool {
	switch name {
	case "s3:ListBucket", "s3:HeadObject", "s3:GetObject":
		return true
	default:
		return false
	}
}
