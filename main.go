package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	consul "github.com/hashicorp/consul/api"
	log "github.com/sirupsen/logrus"
)

type Target struct {
	Type string
	Base string
	Path string
	Options url.Values
}

func main() {
	consulAddr := flag.String("consul-addr", "", "The address of the consul server, including protocol (http/https)")
	consulTlsSkipVerify := flag.Bool("consul-tls-skip-verify", false, "Skip verifying the consul tls connection.")
	targetUri := flag.String("target", "", "The target to send the backup to. Format: {provider}://{path_on_provider} (eg, s3://my-bucket/consul-snapshots")

	flag.Parse()

	if len(*consulAddr) == 0 {
		envConsulAddr := os.Getenv("CONSUL_ADDR")
		consulAddr = &envConsulAddr
	}

	if len(*targetUri) == 0 {
		envTargetUri := os.Getenv("TARGET_URI")
		targetUri = &envTargetUri
	}

	parsedConsulAddr, err := url.ParseRequestURI(*consulAddr)
	if err != nil || parsedConsulAddr.Scheme == "" || parsedConsulAddr.Hostname() == "" {
		log.Errorf("provided consul url is invalid, got '%s'", *consulAddr)
		os.Exit(1)
	}

	parsedTargetUri, err := url.ParseRequestURI(*targetUri)
	if err != nil || parsedTargetUri.Scheme == "" || parsedTargetUri.Host == "" {
		log.Errorf("provided target url is invalid, got '%s'", *targetUri)
		os.Exit(1)
	}

	log.Infof("consul host: %s", *consulAddr)
	log.Infof("target: %s", *targetUri)

	target := &Target{
		Type: parsedTargetUri.Scheme,
		Base: parsedTargetUri.Host,
		Path: parsedTargetUri.Path,
		Options: parsedTargetUri.Query(),
	}

	consulClient, err := consul.NewClient(&consul.Config{
		Address: *consulAddr,
		TLSConfig: consul.TLSConfig{
			InsecureSkipVerify: *consulTlsSkipVerify,
		},
	})

	if err != nil {
		log.Errorf("error creating consul client: %s", err)
		os.Exit(1)
	}

	data, _, err := consulClient.Snapshot().Save(nil)

	if err != nil {
		log.Errorf("error fetching consul snapshot: %s", err)
		os.Exit(1)
	}

	snapshot, err := ioutil.ReadAll(data)

	log.Infof("got snapshot of %d bytes", len(snapshot))

	if err != nil {
		log.Errorf("error reading consul snapshot: %s", err)
		os.Exit(1)
	}

	snapshotKey := fmt.Sprintf("%d.snap", time.Now().Unix())

	switch target.Type {
	case "s3":
		log.Infof("uploading snapshot to s3")
		err = sendToS3(target, &snapshotKey, &snapshot)
	default:
		err = fmt.Errorf("target type of %s is not supported", target.Type)
	}

	if err != nil {
		log.Errorf("error uploading to s3: %s", err)
		os.Exit(1)
	}
}

func sendToS3(target *Target, snapshotKey *string, snapshot *[]byte) error {
	sess, err := session.NewSession(&aws.Config{
		Region: aws.String(target.Options.Get("region")),
	})

	if err != nil {
		return err
	}

	svc := s3.New(sess)

	s3Path := fmt.Sprintf("%s/%s", target.Path, *snapshotKey)

	retries := 0

	_, err = svc.PutObject(&s3.PutObjectInput{
		Bucket: &target.Base,
		Body: bytes.NewReader(*snapshot),
		Key: &s3Path,
	})

	for err != nil && retries < 3 {
		retries += 1
		log.Warnf("error uploading to aws, retrying in 5 seconds for retry %d/%d", retries, 3)
		time.Sleep(time.Second * 5)
		_, err = svc.PutObject(&s3.PutObjectInput{
			Bucket: &target.Base,
			Body: bytes.NewReader(*snapshot),
			Key: &s3Path,
		})
	}

	if err != nil {
		return err
	}

	log.Infof("saved snapshot to bucket %s at path %s", target.Base, s3Path)

	return nil
}