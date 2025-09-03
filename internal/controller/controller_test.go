package controller

import (
	"testing"
)

func TestParseLabels_NoPrefix(t *testing.T) {
	c := &Controller{cfg: Config{}}
	in := map[string]string{
		"s3.enabled": "true",
		"s3.bucket":  "b",
		"foo":        "bar",
	}
	m := c.parseLabels(in)
	if m["s3.enabled"] != "true" || m["s3.bucket"] != "b" {
		t.Fatalf("unexpected parse: %#v", m)
	}
	if _, ok := m["foo"]; ok {
		t.Fatalf("unexpected key passed through")
	}
}

func TestParseLabels_WithPrefix(t *testing.T) {
	c := &Controller{cfg: Config{LabelPrefix: "org"}}
	in := map[string]string{
		"org/s3.enabled": "true",
		"s3.enabled":     "false",
	}
	m := c.parseLabels(in)
	if m["s3.enabled"] != "true" {
		t.Fatalf("prefixed should override: %#v", m)
	}
}

func TestValidateConfig_Minimal(t *testing.T) {
	vr := ValidateConfig(Config{S3Endpoint: "http://s3", Mountpoint: "/mnt/s3", MounterImage: "rclone/rclone"})
	if !vr.OK {
		t.Fatalf("expected OK, got: %#v", vr)
	}
}
