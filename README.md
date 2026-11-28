# oVirt packer.io builder

This builder plugin extends [packer.io](https://packer.io) to support building
images for [oVirt](https://www.ovirt.org).

Based on:

* [cwilloughby-bw/packer-builder-ovirt](https://github.com/cwilloughby-bw/packer-builder-ovirt)
* [ganto/packer-builder-ovirt](https://github.com/ganto/packer-builder-ovirt)

## ISO builds

With `source_type = "iso"` the VM boots from an ISO disk. Either name a disk
that already exists on oVirt with `source_iso`, or let Packer download the ISO
and upload it to `storage_domain` (see
[examples/iso_url.pkr.hcl](examples/iso_url.pkr.hcl)):

| Option | Description |
| --- | --- |
| `iso_url`, `iso_urls` | Where to download the ISO from. Cannot be combined with `source_iso`. |
| `iso_checksum` | Checksum of the ISO, e.g. `sha256:...`, or `none`. |
| `iso_target_path`, `iso_target_extension` | Where the ISO is cached on the machine running Packer. |
| `remote_cache_disk_name` | Name of the ISO disk on oVirt. Defaults to the file name of the ISO. |
| `remote_cache_overwrite` | Replace an ISO disk of that name instead of reusing it. Defaults to `false`. |
| `remote_cache_cleanup` | Remove the ISO disk after the build, if this build uploaded it. Defaults to `false`. |

If an ISO disk named `remote_cache_disk_name` already exists on the storage
domain, it is reused when its content matches the downloaded ISO. The content
is compared with the block checksum that ovirt-imageio computes on the host, so
the disk is not downloaded. A disk with other content fails the build, unless
`remote_cache_overwrite` is set.

The upload goes to ovirt-imageio on a host, or through the engine's proxy if
the host cannot be reached. It follows `insecure_skip_tls_verify`.

## Development

### Prerequisites

To compile this plugin you must have a working Go compiler setup. Follow the
[official instructions](https://golang.org/doc/install) or use your local
package manager to install Go on your system.

### Compile the plugin

```shell
go build
```

If the build was successful, you should now have the `builder-ovirt`.
