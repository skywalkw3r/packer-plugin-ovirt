package ovirt

import (
	"strings"
	"testing"
)

func TestSourceConfig_Prepare_name(t *testing.T) {
	sc := testSourceConfig()
	errs := sc.Prepare(nil)
	if errs != nil {
		t.Fatal("should not fail to initialize minimal source config")
	}

	sc = testSourceConfig()
	sc.SourceTemplateName = ""
	errs = sc.Prepare(nil)
	if errs == nil {
		t.Fatal("should not accept empty name")
	}
}

func TestSourceConfig_Prepare_type(t *testing.T) {
	sc := testSourceConfig()
	sc.SourceType = "foo"
	errs := sc.Prepare(nil)
	if errs == nil {
		t.Fatal("should not accept invalid type")
	}
}

func TestSourceConfig_Prepare_template(t *testing.T) {
	sc := testTemplateSourceConfig()
	errs := sc.Prepare(nil)
	if errs != nil {
		t.Fatal("should not fail to initialize template source config")
	}

	sc = testTemplateSourceConfig()
	sc.SourceTemplateVersion = 42
	errs = sc.Prepare(nil)
	if errs != nil {
		t.Fatal("should not fail to accept template version")
	}

	sc = testTemplateSourceConfig()
	sc.SourceTemplateName = ""
	errs = sc.Prepare(nil)
	if errs == nil {
		t.Fatal("should not accept empty template name")
	}

	sc = testTemplateSourceConfig()
	sc.SourceTemplateName = ""
	sc.SourceTemplateID = "foo"
	errs = sc.Prepare(nil)
	if errs == nil {
		t.Fatal("should not accept invalid template uuid")
	}

	sc = testTemplateSourceConfig()
	sc.SourceTemplateName = ""
	sc.SourceTemplateID = "c2867299-28ea-48a2-922a-805b999fcb2d"
	errs = sc.Prepare(nil)
	if errs != nil {
		t.Fatal("should not fail when template id is given")
	}

	sc = testTemplateSourceConfig()
	sc.SourceTemplateID = "c2867299-28ea-48a2-922a-805b999fcb2d"
	errs = sc.Prepare(nil)
	if errs == nil {
		t.Fatal("should fail when both template name and id are given")
	}
}

func testSourceConfig() SourceConfig {
	return SourceConfig{
		SourceTemplateName: "foo",
	}
}

func testTemplateSourceConfig() SourceConfig {
	return SourceConfig{
		SourceType:         "template",
		SourceTemplateName: "foo",
	}
}

// isoTestConfig returns a minimal raw configuration for an ISO build, with
// the given options added or, for a nil value, removed.
func isoTestConfig(extra map[string]any) map[string]any {
	raw := map[string]any{
		"ovirt_url":      "https://engine.example.com/ovirt-engine/api",
		"username":       "admin@internal",
		"password":       "password",
		"communicator":   "none",
		"source_type":    "iso",
		"storage_domain": "data",
		"iso_url":        "https://example.com/isos/debian-13.iso?mirror=1",
		"iso_checksum":   "sha256:e41eeaffa4fdd64fbf07fc8b0d18a1b5f15ba9743a72c222008f8fd0b6463355",
	}
	for k, v := range extra {
		if v == nil {
			delete(raw, k)
		} else {
			raw[k] = v
		}
	}
	return raw
}

func TestConfig_ISOURL(t *testing.T) {
	c, warnings, err := NewConfig(isoTestConfig(nil))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	if !c.downloadsISO() {
		t.Fatal("expected the ISO to be downloaded")
	}
	if len(c.ISOUrls) != 1 || c.ISOUrls[0] != "https://example.com/isos/debian-13.iso?mirror=1" {
		t.Fatalf("unexpected ISO URLs: %v", c.ISOUrls)
	}
	if c.RemoteCacheDiskName != "debian-13.iso" {
		t.Fatalf("expected the disk name to default to the file name, got %q", c.RemoteCacheDiskName)
	}
	if c.SourceTemplateID != BlankTemplateUUID {
		t.Fatalf("expected an ISO build to start from the Blank template, got %q", c.SourceTemplateID)
	}
}

func TestConfig_ISOURL_Options(t *testing.T) {
	c, _, err := NewConfig(isoTestConfig(map[string]any{
		"iso_url":                nil,
		"iso_urls":               []string{"https://a.example.com/x.iso", "https://b.example.com/x.iso"},
		"remote_cache_disk_name": "custom.iso",
		"remote_cache_overwrite": true,
		"remote_cache_cleanup":   true,
	}))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(c.ISOUrls) != 2 || c.RemoteCacheDiskName != "custom.iso" || !c.RemoteCacheOverwrite || !c.RemoteCacheCleanup {
		t.Fatalf("options were not applied: %+v", c.SourceConfig)
	}
}

func TestConfig_ISOURL_ChecksumNoneWarns(t *testing.T) {
	_, warnings, err := NewConfig(isoTestConfig(map[string]any{"iso_checksum": "none"}))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if len(warnings) != 1 {
		t.Fatalf("expected the SDK's warning about a missing checksum, got %v", warnings)
	}
}

func TestConfig_ISOURL_Invalid(t *testing.T) {
	cases := map[string]struct {
		extra map[string]any
		want  string
	}{
		"with source_iso": {
			map[string]any{"source_iso": "debian.iso"},
			"set either source_iso or iso_url/iso_urls",
		},
		"neither source_iso nor iso_url": {
			map[string]any{"iso_url": nil, "iso_checksum": nil},
			"source_iso or iso_url/iso_urls must be specified",
		},
		"template build": {
			map[string]any{"source_type": "template", "source_template_name": "base"},
			`iso_url and iso_urls require source_type "iso"`,
		},
		"without storage_domain": {
			map[string]any{"storage_domain": nil},
			"storage_domain must be specified",
		},
		"without checksum": {
			map[string]any{"iso_checksum": nil},
			"checksum must be specified",
		},
		"iso_url and iso_urls": {
			map[string]any{"iso_urls": []string{"https://example.com/other.iso"}},
			"Only one of iso_url or iso_urls",
		},
		"double quote in the disk name": {
			map[string]any{"remote_cache_disk_name": `my "iso".iso`},
			"must not contain double quotes",
		},
		"URL without a file name": {
			map[string]any{"iso_url": "https://example.com/"},
			"set it explicitly",
		},
		"remote cache options with source_iso": {
			map[string]any{"iso_url": nil, "iso_checksum": nil, "source_iso": "debian.iso", "remote_cache_cleanup": true},
			"require iso_url or iso_urls",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := NewConfig(isoTestConfig(tc.extra))
			if err == nil {
				t.Fatal("expected an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected an error containing %q, got: %s", tc.want, err)
			}
		})
	}
}

func TestConfig_SourceISOStillWorks(t *testing.T) {
	c, _, err := NewConfig(isoTestConfig(map[string]any{
		"iso_url": nil, "iso_checksum": nil, "storage_domain": nil, "source_iso": "debian.iso",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if c.downloadsISO() || c.RemoteCacheDiskName != "" {
		t.Fatalf("source_iso must not trigger a download: %+v", c.SourceConfig)
	}
}

func TestISOFileName(t *testing.T) {
	for raw, want := range map[string]string{
		"https://example.com/isos/debian-13.iso":         "debian-13.iso",
		"https://example.com/isos/debian-13.iso?token=1": "debian-13.iso",
		"file:///var/isos/win%2011.iso":                  "win 11.iso",
		"/var/isos/local.iso":                            "local.iso",
		"relative/local.iso":                             "local.iso",
	} {
		got, err := isoFileName(raw)
		if err != nil || got != want {
			t.Errorf("isoFileName(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"https://example.com/", "https://example.com"} {
		if got, err := isoFileName(raw); err == nil {
			t.Errorf("isoFileName(%q) = %q, expected an error", raw, got)
		}
	}
}
