// Package cachegrant builds the IAM documents that let a CI task role use the
// shared CI caches: the S3 cache bucket (Go build cache, archive caches) and
// the BuildKit warm-cache repository in a container registry, and the
// repository policy that admits those roles to the repository when it lives in
// another account.
//
// It has no provider in it: every function returns plain maps that marshal with
// encoding/json (which orders keys, so equal inputs give equal bytes). ARNs are
// built at run time from the partition and the names the caller passes.
package cachegrant

import (
	"errors"
	"fmt"
)

// Partition is the default ARN partition.
const Partition = "aws"

// BuildkitPushPull are the actions of a push and a pull of the BuildKit
// warm-cache repository, and nothing else. The shared builders push
// `--cache-to type=registry` with the CLIENT's registry credentials, so a
// runner's identity needs the push.
func BuildkitPushPull() []string {
	return []string{
		"ecr:PutImage", "ecr:InitiateLayerUpload", "ecr:UploadLayerPart", "ecr:CompleteLayerUpload",
		"ecr:BatchCheckLayerAvailability", "ecr:BatchGetImage", "ecr:GetDownloadUrlForLayer",
		"ecr:DescribeRepositories", "ecr:ListImages", "ecr:DescribeImages",
	}
}

// BucketARN is the ARN of an S3 bucket.
func BucketARN(partition, bucket string) string { return "arn:" + partition + ":s3:::" + bucket }

// RepositoryARN is the ARN of an ECR repository.
func RepositoryARN(partition, region, account, repository string) string {
	return fmt.Sprintf("arn:%s:ecr:%s:%s:repository/%s", partition, region, account, repository)
}

// TaskRole is the grant of a task role that uses the cache bucket under some
// prefixes and the BuildKit cache repository.
type TaskRole struct {
	// Partition is the ARN partition; default Partition.
	Partition string
	// Bucket is the cache bucket name.
	Bucket string
	// Prefixes are the key prefixes the role may use, each ending in "/"
	// (such as "gocache/"). At least one.
	Prefixes []string
	// BuildkitRepositoryARN is the BuildKit cache repository the role pushes
	// and pulls (see [RepositoryARN]). Required.
	BuildkitRepositoryARN string
}

// Statements returns the role's identity-policy statements, in order. The
// bucket list is limited to the prefixes (a missing key is then a 404, not a
// 403); objects are read, written and their multipart uploads managed, never
// deleted (the bucket's lifecycle ages them out). The registry side is the one
// repository plus the account-wide authorization token, which cannot be scoped.
func (t TaskRole) Statements() ([]map[string]any, error) {
	if t.Bucket == "" || len(t.Prefixes) == 0 || t.BuildkitRepositoryARN == "" {
		return nil, errors.New("cachegrant: TaskRole needs Bucket, Prefixes and BuildkitRepositoryARN")
	}

	partition := t.Partition
	if partition == "" {
		partition = Partition
	}

	bucket := BucketARN(partition, t.Bucket)

	var lists, objects []string

	for _, p := range t.Prefixes {
		lists = append(lists, p+"*")
		objects = append(objects, bucket+"/"+p+"*")
	}

	list := allow("CacheList", "s3:ListBucket", bucket)
	list["Condition"] = map[string]any{"StringLike": map[string]any{"s3:prefix": lists}}

	return []map[string]any{
		list,
		allow("CacheObjects",
			[]string{"s3:GetObject", "s3:PutObject", "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"}, objects),
		allow("EcrAuth", "ecr:GetAuthorizationToken", "*"),
		allow("EcrBuildkitCache", BuildkitPushPull(), t.BuildkitRepositoryARN),
	}, nil
}

// RepositoryPolicy is the repository policy of the BuildKit cache repository
// in the registry account: the other half of a cross-account push (a
// cross-account ECR grant needs the repository's policy and the caller's
// identity policy; either alone grants nothing). It admits the roles of
// roleAccount whose ARN matches roleARNPattern (such as
// "<role ARN prefix>*-task") to push and pull, and grants
// nobody else anything. The principal is the account root narrowed by
// aws:PrincipalArn, because a repository policy cannot name a role pattern as a
// principal.
func RepositoryPolicy(partition, sid, roleAccount, roleARNPattern string) map[string]any {
	if partition == "" {
		partition = Partition
	}

	return map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{{
			"Sid":       sid,
			"Effect":    "Allow",
			"Principal": map[string]any{"AWS": "arn:" + partition + ":iam::" + roleAccount + ":root"},
			"Action":    BuildkitPushPull(),
			"Condition": map[string]any{"ArnLike": map[string]any{"aws:PrincipalArn": roleARNPattern}},
		}},
	}
}

func allow(sid string, action, resource any) map[string]any {
	return map[string]any{"Sid": sid, "Effect": "Allow", "Action": action, "Resource": resource}
}
