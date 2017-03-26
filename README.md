# TeamCity Backup to S3

`teamcity-backup-s3` is a utility written in Go which creates backups of TeamCity and uploads them to S3. It assumes TeamCity is running on an EC2 instance, and the backup utility is running on the teamcity server.

The utility runs as a one-time process, and can be invoked with `cron` or systemd timers in order to run scheduled backups. Rotation should be managed with a lifecycle policy on the S3 bucket into which the backups are written.

## Configuration

Configuration comes from both environment variables and tags on the EC2 instance:

- Environment: `TEAMCITY_BASE_URL` - an address from which the TeamCity server can be reached from the same instance. Defaults to `http://localhost:8111`
- Environment: `TEAMCITY_DATA_DIR` - the data directory for TeamCity in which backups are written. Defaults to `/var/lib/teamcity`
- Tag: `teamcity:backup:credentials_key` - the path in S3 to a KMS-encrypted JSON file containing a username and password for a TeamCity user with permissions to run backup jobs (currently this requires administrative permissions). Example: `s3://my-secret-items/teamcity-credentials.key.enc`. See below for more information on encryption.
- Tag: `teamcity:backup:destination_prefix` - a path prefix in S3 with which to construct they keys of each backup. Example: `s3://my-backups/teamcity`.

## Encrypting Credentials

Unfortunately administrative credentials are required to access the TeamCity endpoint which creates backups. Consequently we encrypt these credentials using KMS, and put the ciphertext in an S3 bucket.

The (unencrypted) credentials file must be structured as follows:

```json
{
	"user": "teamcity_username",
	"password": "teamcity_password"
}
```

It can be encrypted using the following command, assuming a KMS key already exists and the user making the API call has permission to use the `kms:Encrypt` operation on the key:

```
aws kms encrypt \
	--region <region> \
	--key-id <kms-key-id> \
	--plaintext "$(cat credentials.json)" \
	--query CiphertextBlob \
	--output text | base64 --decode > "credentials.json.enc"
```

The file `credentials.json.enc` can then be uploaded to S3, and the full path used as the value for the `teamcity:backup:credentials_key` tag on the instance running this utility.

## Configuration with systemd

Example Service File:

```
[Unit]
Description=Back up TeamCity server to S3
Requires=network-online.target teamcity.service
After=network-online.target

[Service]
Type=simple
ExecStart=/usr/bin/teamcity-backup-s3
User=teamcity
Group=teamcity
```

Example Timer file:

```
[Unit]
Description=Back up TeamCity server to S3

[Timer]
OnBootSec=300
OnUnitActiveSec=1h

[Install]
WantedBy=timers.target
```

## Minimal EC2 Role

The following policy grants sufficient permissions for the backup utility to operate, assuming the various ARN placeholders are filled in:

```json
{
    "Version": "2012-10-17",
    "Statement": [
        {
            "Sid": "AllowDescribeTags",
            "Effect": "Allow",
            "Action": [
                "ec2:DescribeTags"
            ],
            "Resource": [
                "*"
            ]
        },
        {
            "Sid": "AllowDownloadCredentials",
            "Effect": "Allow",
            "Action": [
                "s3:GetObject"
            ],
            "Resource": [
                "<Credentials Bucket ARN>/path/to/credentials"
            ]
        },
        {
            "Sid": "AllowDecryptCredentials",
            "Effect": "Allow",
            "Action": [
                "kms:Decrypt"
            ],
            "Resource": [
                "<KMS Key ARN>"
            ]
        },
        {
            "Sid": "AllowUploadBackups",
            "Effect": "Allow",
            "Action": [
                "s3:AbortMultipartUpload",
                "s3:ListMultipartUploadParts",
                "s3:PutObject"
            ],
            "Resource": [
                "arn:aws:s3:::evision-temp/backups/*"
            ]
        }
    ]
}
```
