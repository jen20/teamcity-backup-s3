package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ec2"
	"github.com/aws/aws-sdk-go/service/kms"
	"github.com/aws/aws-sdk-go/service/s3"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"
)

func main() {
	teamcityBaseURL := os.Getenv("TEAMCITY_BASE_URL")
	if teamcityBaseURL == "" {
		teamcityBaseURL = "http://localhost:8111"
	}
	teamcityDataDir := os.Getenv("TEAMCITY_DATA_DIR")
	if teamcityDataDir == "" {
		teamcityDataDir = filepath.Join("/var", "lib", "teamcity")
	}

	metadataClient, err := getMetadataClient()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring EC2 metadata client: %s", err)
		os.Exit(1)
	}

	region, err := metadataClient.Region()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error obtaining region from metadata client: %s\n", err)
		os.Exit(1)
	}

	ec2Client, err := getEC2Client(region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring EC2 client: %s\n", err)
		os.Exit(1)
	}

	s3Client, err := getS3Client(region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring S3 client: %s\n", err)
		os.Exit(1)
	}

	s3Uploader, err := getS3Uploader(region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring S3 uploader: %s\n", err)
		os.Exit(1)
	}

	kmsClient, err := getKMSClient(region)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error configuring KMS client: %s\n", err)
		os.Exit(1)
	}

	instanceID, err := metadataClient.GetMetadata("instance-id")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error obtaining instance ID from metadata client: %s\n", err)
		os.Exit(1)
	}

	destinationPrefix, err := getEC2TagValue(ec2Client, instanceID, "teamcity:backup:destination_prefix")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error obtaining TeamCity Backup Destination Prefix from EC2 client: %s\n", err)
		os.Exit(1)
	}
	destinationBucket, destinationKeyPrefix, err := parseS3Path(destinationPrefix)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}

	credentialsKey, err := getEC2TagValue(ec2Client, instanceID, "teamcity:backup:credentials_key")
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error obtaining TeamCity Credentials Path from EC2 client: %s\n", err)
		os.Exit(1)
	}

	credentialsBucket, credentialsKeyPath, err := parseS3Path(credentialsKey)
	if err != nil {
		fmt.Fprint(os.Stderr, err)
		os.Exit(1)
	}

	credentialsObject, err := s3Client.GetObject(&s3.GetObjectInput{
		Bucket: &credentialsBucket,
		Key:    &credentialsKeyPath,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error downloading decryptedCredentials credentialsKeyPath from S3: %s", err)
		os.Exit(1)
	}
	credentialsCiphertext, err := ioutil.ReadAll(credentialsObject.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading downloaded decryptedCredentials decryptedCredentials from S3: %s", err)
		os.Exit(1)
	}

	decryptOutput, err := kmsClient.Decrypt(&kms.DecryptInput{
		CiphertextBlob: credentialsCiphertext,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error decrypting downloaded credentails: %s", err)
		os.Exit(1)
	}

	decryptedCredentials := &struct {
		User     string `json:"user"`
		Password string `json:"password"`
	}{}

	err = json.Unmarshal(decryptOutput.Plaintext, &decryptedCredentials)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Decrypted credentials must be a JSON object with keys %q and %q\n", "user", "password")
		os.Exit(1)
	}

	startBackupRequestParams := url.Values{}
	startBackupRequestParams.Set("addTimestamp", "true")
	startBackupRequestParams.Set("fileName", "TeamCity_Backup")
	startBackupRequestParams.Set("includeBuildLogs", "true")
	startBackupRequestParams.Set("includeConfigs", "true")
	startBackupRequestParams.Set("includeDatabase", "false")
	startBackupRequestParams.Set("includePersonalChangers", "true")

	startBackupRequest, err := http.NewRequest(http.MethodPost, fmt.Sprintf("%s/httpAuth/app/rest/server/backup", teamcityBaseURL), nil)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error constructing backup request: %s\n", err)
		os.Exit(1)
	}
	startBackupRequest.SetBasicAuth(decryptedCredentials.User, decryptedCredentials.Password)
	startBackupRequest.URL.RawQuery = startBackupRequestParams.Encode()

	startBackupResponse, err := http.DefaultClient.Do(startBackupRequest)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error on startBackupResponse from TeamCity: %d\n", startBackupResponse.StatusCode)
		os.Exit(1)
	}
	defer startBackupResponse.Body.Close()
	body, err := ioutil.ReadAll(startBackupResponse.Body)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error reading backupStatusResponse body: %s\n", err)
		os.Exit(1)
	}
	backupLocation := filepath.Join(teamcityDataDir, "backup", strings.TrimSpace(string(body)))
	fmt.Printf("Started TeamCity backup to %s\n", backupLocation)

	for {
		backupStatusRequest, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/httpAuth/app/rest/server/backup", teamcityBaseURL), nil)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error constructing status request: %s\n", err)
			os.Exit(1)
		}
		backupStatusRequest.SetBasicAuth(decryptedCredentials.User, decryptedCredentials.Password)

		backupStatusResponse, err := http.DefaultClient.Do(backupStatusRequest)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error on backupStatusRequest from TeamCity: %d\n", backupStatusResponse.StatusCode)
			os.Exit(1)
		}
		body, err := ioutil.ReadAll(backupStatusResponse.Body)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error reading backupStatusResponse body: %s\n", err)
			os.Exit(1)
		}
		backupStatusResponse.Body.Close()

		if strings.TrimSpace(string(body)) != "Idle" {
			fmt.Println("Waiting for TeamCity backup to complete...")
			time.Sleep(5 * time.Second)
			continue
		}

		break
	}

	_, backupFilename := filepath.Split(backupLocation)
	reader, err := os.Open(backupLocation)
	defer os.Remove(backupLocation)
	defer reader.Close()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Cannot open backup file %q: %s", backupLocation, err)
		os.Exit(1)
	}

	uploadKey := fmt.Sprintf("%s/%s", destinationKeyPrefix, backupFilename)
	fmt.Printf("Uploading TeamCity backup to: %s\n", uploadKey)

	uploadResult, err := s3Uploader.Upload(&s3manager.UploadInput{
		Bucket: aws.String(destinationBucket),
		Key:    aws.String(uploadKey),
		Body:   reader,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error uploading backup to S3: %s\n", err)
		os.Exit(1)
	}

	fmt.Printf("Uploaded TeamCity backup to: %s\n", uploadResult.Location)
}

var s3PathMatch = regexp.MustCompile(`s3://([^/]*)/(.*)`)

func parseS3Path(path string) (string, string, error) {
	matches := s3PathMatch.FindAllStringSubmatch(path, -1)
	if len(matches) != 1 {
		return "", "", fmt.Errorf("Path %q is not in the format %q", path, "s3://<bucket>/path/to/key")
	}
	if len(matches[0]) != 3 {
		return "", "", fmt.Errorf("Path %q is not in the format %q", path, "s3://<bucket>/path/to/key")
	}

	return matches[0][1], matches[0][2], nil
}

func getEC2TagValue(client *ec2.EC2, instanceID string, tagName string) (string, error) {
	describeTagsOutput, err := client.DescribeTags(&ec2.DescribeTagsInput{
		Filters: []*ec2.Filter{
			{
				Name:   aws.String("resource-type"),
				Values: []*string{aws.String("instance")},
			},
			{
				Name:   aws.String("resource-id"),
				Values: []*string{aws.String(instanceID)},
			},
			{
				Name:   aws.String("key"),
				Values: []*string{aws.String(tagName)},
			},
		},
	})
	if err != nil {
		return "", err
	}

	if len(describeTagsOutput.Tags) == 0 {
		return "", fmt.Errorf("No tags named %s present on instance %s", tagName, instanceID)
	}

	if len(describeTagsOutput.Tags) > 1 {
		return "", fmt.Errorf("Multiple tag values for tag named %s present on instance %s", tagName, instanceID)
	}

	if valuePtr := describeTagsOutput.Tags[0].Value; valuePtr != nil {
		return *valuePtr, nil
	}

	return "", nil
}

func getMetadataClient() (*ec2metadata.EC2Metadata, error) {
	sess, err := session.NewSession()
	if err != nil {
		return nil, err
	}

	return ec2metadata.New(sess), nil
}

func getEC2Client(region string) (*ec2.EC2, error) {
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region: aws.String(region),
		},
	})
	if err != nil {
		return nil, err
	}

	return ec2.New(sess), nil
}

func getS3Client(region string) (*s3.S3, error) {
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region: aws.String(region),
		},
	})
	if err != nil {
		return nil, err
	}

	return s3.New(sess), nil
}

func getS3Uploader(region string) (*s3manager.Uploader, error) {
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region: aws.String(region),
		},
	})
	if err != nil {
		return nil, err
	}

	return s3manager.NewUploader(sess), nil
}

func getKMSClient(region string) (*kms.KMS, error) {
	sess, err := session.NewSessionWithOptions(session.Options{
		Config: aws.Config{
			Region: aws.String(region),
		},
	})
	if err != nil {
		return nil, err
	}

	return kms.New(sess), nil
}
