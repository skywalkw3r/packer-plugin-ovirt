# Build from an ISO that Packer downloads and uploads to oVirt.

source "ovirt-ovirt" "debian" {
  ovirt_url = "https://ovirt.example.com/ovirt-engine/api"
  username  = "admin@internal"
  password  = "password"

  source_type    = "iso"
  iso_url        = "https://cdimage.debian.org/debian-cd/current/amd64/iso-dvd/debian-13.4.0-amd64-DVD-1.iso"
  iso_checksum   = "sha256:e41eeaffa4fdd64fbf07fc8b0d18a1b5f15ba9743a72c222008f8fd0b6463355"
  storage_domain = "data"

  # Optional. The ISO disk on oVirt is named after the file by default, and
  # kept after the build so that the next build can reuse it.
  # remote_cache_disk_name = "debian-13-DVD.iso"
  # remote_cache_overwrite = false
  # remote_cache_cleanup   = false

  disk_size = 20
  network   = "ovirtmgmt"

  boot_wait    = "10s"
  boot_command = ["<esc><wait>", "auto url=http://192.168.0.1/preseed.cfg<enter>"]

  ssh_username     = "packer"
  ssh_password     = "packer"
  shutdown_command = "sudo shutdown -P now"
  remove_cdrom     = true
  template_name    = "debian-13"
}

build {
  sources = ["source.ovirt-ovirt.debian"]
}
