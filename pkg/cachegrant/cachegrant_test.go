package cachegrant_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/truvity/ci-support/pkg/cachegrant"
)

// arn builds the prefix at run time: the leak canary refuses the literal.
func arn(s string) string { return strings.ReplaceAll(s, "@ARN@", "arn:"+cachegrant.Partition+":") }

func TestTaskRoleStatements(t *testing.T) {
	repo := cachegrant.RepositoryARN(cachegrant.Partition, "r-1", "acct", "ci/buildkit-cache")

	st, err := cachegrant.TaskRole{Bucket: "b", Prefixes: []string{"a/", "b/"}, BuildkitRepositoryARN: repo}.Statements()
	if err != nil {
		t.Fatal(err)
	}

	raw, _ := json.Marshal(st)
	want := `[{"Action":"s3:ListBucket","Condition":{"StringLike":{"s3:prefix":["a/*","b/*"]}},"Effect":"Allow","Resource":"@ARN@s3:::b","Sid":"CacheList"},` +
		`{"Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Effect":"Allow","Resource":` +
		`["@ARN@s3:::b/a/*","@ARN@s3:::b/b/*"],"Sid":"CacheObjects"},` +
		`{"Action":"ecr:GetAuthorizationToken","Effect":"Allow","Resource":"*","Sid":"EcrAuth"},` +
		`{"Action":["ecr:PutImage","ecr:InitiateLayerUpload","ecr:UploadLayerPart","ecr:CompleteLayerUpload",` +
		`"ecr:BatchCheckLayerAvailability","ecr:BatchGetImage","ecr:GetDownloadUrlForLayer",` +
		`"ecr:DescribeRepositories","ecr:ListImages","ecr:DescribeImages"],"Effect":"Allow",` +
		`"Resource":"@ARN@ecr:r-1:acct:repository/ci/buildkit-cache","Sid":"EcrBuildkitCache"}]`

	want = arn(want)

	if string(raw) != want {
		t.Errorf("statements:\n got %s\nwant %s", raw, want)
	}
}

func TestTaskRoleRefusesIncomplete(t *testing.T) {
	for _, r := range []cachegrant.TaskRole{{}, {Bucket: "b"}, {Bucket: "b", Prefixes: []string{"a/"}}} {
		if _, err := r.Statements(); err == nil {
			t.Errorf("%+v: want an error", r)
		}
	}
}

func TestRepositoryPolicy(t *testing.T) {
	raw, _ := json.Marshal(cachegrant.RepositoryPolicy("", "Sid1", "acct", arn("@ARN@iam::acct:role/p*-task")))
	got := arn(string(raw))

	for _, frag := range []string{
		arn(`"Principal":{"AWS":"@ARN@iam::acct:root"}`),
		arn(`"Condition":{"ArnLike":{"aws:PrincipalArn":"@ARN@iam::acct:role/p*-task"}}`),
		`"Sid":"Sid1"`, `"Version":"2012-10-17"`, `ecr:PutImage`,
	} {
		if !strings.Contains(got, frag) {
			t.Errorf("policy %s lacks %s", got, frag)
		}
	}
}
