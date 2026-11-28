package ovirt

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"path"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

const (
	TemplateSource = "template"
	ISOSource      = "iso"
)

const BlankTemplateUUID = "00000000-0000-0000-0000-000000000000"

// SourceConfig contains the various source properties for an oVirt image
type SourceConfig struct {
	Cluster string `mapstructure:"cluster"`

	SourceType string `mapstructure:"source_type"`

	SourceTemplateName    string `mapstructure:"source_template_name"`
	SourceTemplateVersion int64  `mapstructure:"source_template_version"`
	SourceTemplateID      string `mapstructure:"source_template_id"`

	// SourceISO is the name of an ISO disk that already exists on oVirt.
	SourceISO string `mapstructure:"source_iso"`

	// ISOConfig holds the standard Packer ISO options (iso_url, iso_urls,
	// iso_checksum, iso_target_path, iso_target_extension). When set, the ISO
	// is downloaded and uploaded to storage_domain instead of using source_iso.
	commonsteps.ISOConfig `mapstructure:",squash"`

	// RemoteCacheDiskName is the name of the ISO disk on oVirt. Defaults to
	// the file name of the first ISO URL.
	RemoteCacheDiskName string `mapstructure:"remote_cache_disk_name"`
	// RemoteCacheOverwrite replaces an ISO disk of that name on the storage
	// domain instead of verifying and reusing it.
	RemoteCacheOverwrite bool `mapstructure:"remote_cache_overwrite"`
	// RemoteCacheCleanup removes the ISO disk after the build, if this build
	// uploaded it.
	RemoteCacheCleanup bool `mapstructure:"remote_cache_cleanup"`

	CDName    string            `mapstructure:"cd_name"`
	CDFiles   []string          `mapstructure:"cd_files"`
	CDContent map[string]string `mapstructure:"cd_content"`
}

// Prepare performs basic validation on the SourceConfig
func (c *SourceConfig) Prepare(ctx *interpolate.Context) []error {
	var errs []error

	if c.Cluster == "" {
		c.Cluster = "Default"
	}
	if c.CDName == "" {
		c.CDName = "UNATTEND"
	}

	switch c.SourceType {
	case "":
		log.Printf("Using default source_type: %s", c.SourceType)
		c.SourceType = "template"
		fallthrough
	case TemplateSource:
		log.Println("Using template source type")
		if (c.SourceTemplateName != "") && (c.SourceTemplateVersion < 1) {
			c.SourceTemplateVersion = 1
			log.Printf("Using default source_template_version: %d", c.SourceTemplateVersion)
		}
		if c.SourceTemplateID != "" {
			if _, err := uuid.Parse(c.SourceTemplateID); err != nil {
				errs = append(errs, fmt.Errorf("invalid source_template_id: %s", c.SourceTemplateID))
			}
		}
		if (c.SourceTemplateName != "") && (c.SourceTemplateID != "") {
			errs = append(errs, errors.New("conflict: set either source_template_name or source_template_id"))
		}
		if (c.SourceTemplateName == "") && (c.SourceTemplateID == "") {
			errs = append(errs, errors.New("source_template_name or source_template_id must be specified"))
		}

	case ISOSource:
		log.Println("Using ISO source type")
		c.SourceTemplateName = "Blank"
		c.SourceTemplateID = BlankTemplateUUID
		switch {
		case c.SourceISO != "" && c.downloadsISO():
			errs = append(errs, errors.New("conflict: set either source_iso or iso_url/iso_urls"))
		case c.SourceISO == "" && !c.downloadsISO():
			errs = append(errs, errors.New("source_iso or iso_url/iso_urls must be specified"))
		}

	default:
		errs = append(errs, fmt.Errorf("invalid source_type: %s", c.SourceType))
	}

	if c.downloadsISO() && c.SourceType == TemplateSource {
		errs = append(errs, errors.New(`iso_url and iso_urls require source_type "iso"`))
	}
	if !c.downloadsISO() && (c.RemoteCacheDiskName != "" || c.RemoteCacheOverwrite || c.RemoteCacheCleanup) {
		errs = append(errs, errors.New("remote_cache_disk_name, remote_cache_overwrite and remote_cache_cleanup require iso_url or iso_urls"))
	}

	if len(errs) > 0 {
		return errs
	}

	return nil
}

// downloadsISO reports whether the ISO comes from iso_url/iso_urls rather than
// from a disk that already exists on oVirt.
func (c *SourceConfig) downloadsISO() bool {
	return c.RawSingleISOUrl != "" || len(c.ISOUrls) > 0
}

// PrepareISO validates the ISO download options and sets the default name of
// the ISO disk on oVirt. It is a no-op unless iso_url or iso_urls is set.
func (c *SourceConfig) PrepareISO(ctx *interpolate.Context) (warnings []string, errs []error) {
	if !c.downloadsISO() {
		return nil, nil
	}

	warnings, errs = c.ISOConfig.Prepare(ctx)
	if len(errs) > 0 {
		return warnings, errs
	}

	if c.RemoteCacheDiskName == "" {
		name, err := isoFileName(c.ISOUrls[0])
		if err != nil {
			errs = append(errs, fmt.Errorf("could not derive remote_cache_disk_name from %q, set it explicitly: %w", c.ISOUrls[0], err))
			return warnings, errs
		}
		c.RemoteCacheDiskName = name
	}
	// The oVirt search syntax has no escape for a double quote inside a
	// quoted value, so such a disk could never be found again.
	if strings.Contains(c.RemoteCacheDiskName, `"`) {
		errs = append(errs, errors.New("remote_cache_disk_name must not contain double quotes"))
	}

	return warnings, errs
}

// isoFileName returns the file name of an ISO URL or local path.
func isoFileName(rawURL string) (string, error) {
	p := rawURL
	if u, err := url.Parse(rawURL); err == nil && (u.Path != "" || u.Host != "") {
		p = u.Path
	}
	name := path.Base(filepath.ToSlash(p))
	if name == "" || name == "." || name == "/" {
		return "", errors.New("no file name in the URL")
	}
	return name, nil
}
