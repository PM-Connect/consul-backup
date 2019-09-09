package main

import (
	"bytes"
	"flag"
	"fmt"
	"io/ioutil"
	"net/url"
	"os"
	"time"
	oldLogger "log"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/s3"
	consul "github.com/hashicorp/consul/api"
	consulServer "github.com/hashicorp/consul/agent"
	consulServerConfig "github.com/hashicorp/consul/agent/config"
	log "github.com/sirupsen/logrus"
)

// Target is the target.
type Target struct {
	Type string
	Base string
	Path string
	Options url.Values
}

func main() {
	consulAddr := flag.String("consul-addr", "", "The address of the consul server, including protocol (http/https)")
	consulTLSSkipVerify := flag.Bool("consul-tls-skip-verify", false, "Skip verifying the consul tls connection.")
	targetURI := flag.String("target", "", "The target to send the backup to. Format: {provider}://{path_on_provider} (eg, s3://my-bucket/consul-snapshots")

	flag.Parse()

	if len(*consulAddr) == 0 {
		envConsulAddr := os.Getenv("CONSUL_ADDR")
		consulAddr = &envConsulAddr
	}

	if len(*targetURI) == 0 {
		envTargetURI := os.Getenv("TARGET_URI")
		targetURI = &envTargetURI
	}

	parsedConsulAddr, err := url.ParseRequestURI(*consulAddr)
	if err != nil || parsedConsulAddr.Scheme == "" || parsedConsulAddr.Hostname() == "" {
		log.Errorf("provided consul url is invalid, got '%s'", *consulAddr)
		os.Exit(1)
	}

	parsedTargetURI, err := url.ParseRequestURI(*targetURI)
	if err != nil || parsedTargetURI.Scheme == "" || parsedTargetURI.Host == "" {
		log.Errorf("provided target url is invalid, got '%s'", *targetURI)
		os.Exit(1)
	}

	log.Infof("consul host: %s", *consulAddr)
	log.Infof("target: %s", *targetURI)

	target := &Target{
		Type:    parsedTargetURI.Scheme,
		Base:    parsedTargetURI.Host,
		Path:    parsedTargetURI.Path,
		Options: parsedTargetURI.Query(),
	}

	consulClient, err := consul.NewClient(&consul.Config{
		Address: *consulAddr,
		TLSConfig: consul.TLSConfig{
			InsecureSkipVerify: *consulTLSSkipVerify,
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

	consulAgent, err := getConsulAgent()

	if err != nil {
		log.Errorf("error starting dummy consul agent to test snapshot: %s", err)
		os.Exit(1)
	}

	log.Info("verifying snapshot by restoring to dummy consul server")

	err = consulAgent.Start()

	if err != nil {
		log.Errorf("error starting dummy consul agent to test snapshot: %s", err)
		os.Exit(1)
	}

	dummyConsulClient, err := consul.NewClient(&consul.Config{
		Address: "http://localhost:8500",
		TLSConfig: consul.TLSConfig{
			InsecureSkipVerify: *consulTLSSkipVerify,
		},
	})

	if err != nil {
		log.Errorf("error creating dummy consul client to test snapshot: %s", err)
		os.Exit(1)
	}

	log.Info("waiting for consul server to become ready")
	time.Sleep(time.Second * 2)

	reader := bytes.NewReader(snapshot)

	err = dummyConsulClient.Snapshot().Restore(nil, reader)

	if err != nil {
		log.Errorf("error restoring snapshot to dummy consul agent: %s", err)
		os.Exit(1)
	}

	snapshotKvs, _, err := dummyConsulClient.KV().List("/", nil)
	liveKvs, _, err := consulClient.KV().List("/", nil)

	var snapshotKeys []string
	var snapshotTotalBytes int64
	var liveTotalBytes int64

	for _, kv := range snapshotKvs {
		snapshotTotalBytes += int64(len(kv.Value))
		snapshotKeys = append(snapshotKeys, kv.Key)
	}

	for _, kv := range liveKvs {
		liveTotalBytes += int64(len(kv.Value))
		if !contains(snapshotKeys, kv.Key) {
			log.Errorf("key %s was not found in the snapshot", kv.Key)
			os.Exit(1)
		}
	}

	if liveTotalBytes < snapshotTotalBytes - 1000 || liveTotalBytes > snapshotTotalBytes + 1000 {
		log.Errorf("different snapshot kv size detected, got %d expected %d", snapshotTotalBytes, liveTotalBytes)
		os.Exit(1)
	}

	log.Infof("verified all keys are contained within the snapshot, got %d keys", len(snapshotKeys))

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
		retries++
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

func getConsulAgent() (*consulServer.Agent, error) {
	devMode := true
	builder, err := consulServerConfig.NewBuilder(consulServerConfig.Flags{
		DevMode: &devMode,
	})

	if err != nil {
		return nil, err
	}

	rt, err := builder.Build()

	if err != nil {
		return nil, err
	}

	l := oldLogger.New(ioutil.Discard, "", 0)

	return consulServer.New(&rt, l)
}

func contains(s []string, e string) bool {
	for _, a := range s {
		if a == e {
			return true
		}
	}
	return false
}