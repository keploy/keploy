// The app under test: an S3 client (aws-sdk-go-v2) whose every call is signed
// with SigV4. GET /run puts an object, reads it back, and reads it again
// through a presigned URL with net/http; it answers what it read, so the
// replay compares its response too. GET /healthz answers 200.
//
// Env: S3_ENDPOINT (LocalStack, path-style), S3_BUCKET, AWS_REGION and the
// static AWS_ACCESS_KEY_ID / AWS_SECRET_ACCESS_KEY: each call's Authorization
// carries the key id, the date, the region and the service in its credential
// scope, and a signature over the request at the time it is made.
package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func main() {
	ctx := context.Background()
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "aws config:", err)
		os.Exit(1)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(os.Getenv("S3_ENDPOINT"))
		o.UsePathStyle = true
	})
	bucket := os.Getenv("S3_BUCKET")

	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "ok") })
	http.HandleFunc("/run", func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Query().Get("key")
		body := "hello " + key
		if _, err := client.PutObject(r.Context(), &s3.PutObjectInput{Bucket: &bucket, Key: &key, Body: strings.NewReader(body)}); err != nil {
			http.Error(w, "put: "+err.Error(), http.StatusBadGateway)
			return
		}
		got, err := client.GetObject(r.Context(), &s3.GetObjectInput{Bucket: &bucket, Key: &key})
		if err != nil {
			http.Error(w, "get: "+err.Error(), http.StatusBadGateway)
			return
		}
		read, _ := io.ReadAll(got.Body)
		_ = got.Body.Close()
		url, err := s3.NewPresignClient(client).PresignGetObject(r.Context(), &s3.GetObjectInput{Bucket: &bucket, Key: &key}, s3.WithPresignExpires(15*time.Minute))
		if err != nil {
			http.Error(w, "presign: "+err.Error(), http.StatusBadGateway)
			return
		}
		resp, err := http.Get(url.URL)
		if err != nil {
			http.Error(w, "presigned get: "+err.Error(), http.StatusBadGateway)
			return
		}
		viaURL, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		fmt.Fprintf(w, `{"get":%q,"presigned":%q,"status":%d}`, read, viaURL, resp.StatusCode)
	})
	if err := http.ListenAndServe(":8091", nil); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
