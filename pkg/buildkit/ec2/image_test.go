package ec2_test

import (
	"testing"

	"github.com/pulumi/pulumi/sdk/v3/go/pulumi"
)

const (
	pinnedImage = "ami-0pinned0123456789"
	// resolved is what the recorder's mock answers for any SSM parameter.
	resolvedImage = "ami-0example"
)

// launchTemplateImage is the image id the recorded launch template carries.
func launchTemplateImage(t *testing.T, rec *recorder) any {
	t.Helper()

	var got any

	n := 0

	for _, x := range rec.all {
		if x["type"] == "aws:ec2/launchTemplate:LaunchTemplate" {
			got = x["inputs"].(map[string]any)["imageId"]
			n++
		}
	}

	if n != 1 {
		t.Fatalf("want one launch template, got %d", n)
	}

	return got
}

// Without ImageID the launch template follows the public SSM parameter, as
// before.
func TestBuilderImageFollowsSSMByDefault(t *testing.T) {
	rec, _, err := run(t, onDemandArgs())
	if err != nil {
		t.Fatal(err)
	}

	if got := launchTemplateImage(t, rec); got != resolvedImage || rec.ssmReads == 0 {
		t.Errorf("image %v, %d parameter reads: want %s from the parameter", got, rec.ssmReads, resolvedImage)
	}
}

// ImageID pins the instance's AMI: the id is used as given and no parameter
// is read, so a new Amazon image cannot change the plan.
func TestBuilderImageIDPins(t *testing.T) {
	a := onDemandArgs()
	a.ImageID = pulumi.String(pinnedImage)

	rec, _, err := run(t, a)
	if err != nil {
		t.Fatal(err)
	}

	if got := launchTemplateImage(t, rec); got != pinnedImage {
		t.Errorf("image %v, want %s", got, pinnedImage)
	}

	if rec.ssmReads != 0 {
		t.Errorf("a pinned image read %d SSM parameters", rec.ssmReads)
	}
}

// An empty literal is not a pin: a caller may pass a configuration value
// straight through.
func TestBuilderEmptyImageIDIsNotAPin(t *testing.T) {
	a := onDemandArgs()
	a.ImageID = pulumi.String("")

	rec, _, err := run(t, a)
	if err != nil {
		t.Fatal(err)
	}

	if got := launchTemplateImage(t, rec); got != resolvedImage || rec.ssmReads == 0 {
		t.Errorf("image %v, %d parameter reads: want %s from the parameter", got, rec.ssmReads, resolvedImage)
	}
}

func TestProxyImageFollowsSSMByDefault(t *testing.T) {
	rec, _, err := runProxy(t, proxyArgs())
	if err != nil {
		t.Fatal(err)
	}

	if got := launchTemplateImage(t, rec); got != resolvedImage || rec.ssmReads == 0 {
		t.Errorf("image %v, %d parameter reads: want %s from the parameter", got, rec.ssmReads, resolvedImage)
	}
}

func TestProxyImageIDPins(t *testing.T) {
	a := proxyArgs()
	a.ImageID = pulumi.String(pinnedImage)

	rec, _, err := runProxy(t, a)
	if err != nil {
		t.Fatal(err)
	}

	if got := launchTemplateImage(t, rec); got != pinnedImage {
		t.Errorf("image %v, want %s", got, pinnedImage)
	}

	if rec.ssmReads != 0 {
		t.Errorf("a pinned image read %d SSM parameters", rec.ssmReads)
	}
}
