package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"go-test/s3"
)

func main() {
	configPath := flag.String("config", "", "JSON configuration file (defaults to ./config.json)")
	file := flag.String("file", "", "local file to upload")
	key := flag.String("key", "", "destination object key (defaults to filename)")
	abort := flag.Bool("abort", false, "abort the saved upload for -key")
	flag.Parse()
	if *key == "" && *file != "" {
		*key = filepath.Base(*file)
	}
	if *key == "" || (!*abort && *file == "") {
		flag.Usage()
		os.Exit(2)
	}
	c, err := s3.LoadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	uploader, err := s3.New(c)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *abort {
		if err := uploader.Abort(ctx, *key); err != nil {
			log.Fatal(err)
		}
		fmt.Println("Upload aborted")
		return
	}
	result, err := uploader.UploadFile(ctx, *file, *key, func(p s3.Progress) {
		fmt.Fprintf(os.Stderr, "\r%.1f%% (%d/%d bytes)", p.Percentage, p.BytesWritten, p.ContentLength)
	})
	fmt.Fprintln(os.Stderr)
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("Uploaded s3://%s/%s (%s)\n", result.Bucket, result.Key, result.ETag)
}
