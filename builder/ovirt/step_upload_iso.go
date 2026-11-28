package ovirt

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	ovirtsdk4 "github.com/ovirt/go-ovirt/v4"
)

// isoDiskIDKey is the state key of the ISO disk to boot from, set when the ISO
// was uploaded (or found) by stepUploadISO.
const isoDiskIDKey = "iso_disk_id"

// isoPathKey is the state key of the local path of the downloaded ISO.
const isoPathKey = "iso_path"

// maxImageSizeSlack is how much larger than the ISO a disk may be and still
// be compared with it. Storage rounds a disk up, by at most one LVM extent on
// block storage (128 MiB by default).
const maxImageSizeSlack = 1024 * 1024 * 1024

// stepUploadISO makes the downloaded ISO available as an ISO disk on the
// storage domain. A disk of the same name is reused if its content matches.
type stepUploadISO struct {
	diskID string
	// created is set once this step created the disk.
	created bool
	// complete is set once the upload was finalized.
	complete bool
}

func (s *stepUploadISO) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("config").(*Config)
	ui := state.Get("ui").(packer.Ui)
	isoPath := state.Get(isoPathKey).(string)

	halt := func(err error) multistep.StepAction {
		ui.Error(err.Error())
		state.Put("error", err)
		return multistep.ActionHalt
	}

	conn, err := ovirtConnect(config, state)
	if err != nil {
		return halt(err)
	}

	file, err := os.Open(isoPath)
	if err != nil {
		return halt(fmt.Errorf("could not open the ISO: %w", err))
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return halt(fmt.Errorf("could not stat the ISO: %w", err))
	}
	size := info.Size()

	storageDomainID, err := (&stepAddDisk{}).findStorageDomain(conn, config.StorageDomain)
	if err != nil {
		return halt(err)
	}

	name := config.RemoteCacheDiskName
	existing, err := findISODisks(conn, name, storageDomainID)
	if err != nil {
		return halt(err)
	}
	if len(existing) > 1 {
		return halt(fmt.Errorf("found %d ISO disks named '%s' on storage domain '%s', remove the duplicates or set remote_cache_disk_name",
			len(existing), name, config.StorageDomain))
	}

	client := newImageioClient(config)
	defer client.CloseIdleConnections()

	if len(existing) == 1 {
		diskID := existing[0].MustId()
		if !config.RemoteCacheOverwrite {
			if status, _ := existing[0].Status(); status != ovirtsdk4.DISKSTATUS_OK {
				return halt(fmt.Errorf("ISO disk '%s' (%s) is '%s', it can not be reused", name, diskID, status))
			}
			ui.Sayf("Found ISO disk '%s' (%s) on storage domain '%s', comparing it with the downloaded ISO...",
				name, diskID, config.StorageDomain)
			same, err := isoDiskMatches(ctx, conn, client, diskID, file, size)
			if err != nil {
				return halt(fmt.Errorf("could not verify ISO disk '%s' (%s): %w", name, diskID, err))
			}
			if !same {
				return halt(fmt.Errorf("ISO disk '%s' (%s) on storage domain '%s' differs from the downloaded ISO; "+
					"set remote_cache_overwrite to replace it or use another remote_cache_disk_name",
					name, diskID, config.StorageDomain))
			}
			ui.Sayf("Using existing ISO disk '%s' (%s)", name, diskID)
			state.Put(isoDiskIDKey, diskID)
			return multistep.ActionContinue
		}

		ui.Sayf("Removing existing ISO disk '%s' (%s) (remote_cache_overwrite)...", name, diskID)
		if err := removeDisk(ctx, conn, diskID); err != nil {
			return halt(fmt.Errorf("could not remove ISO disk '%s' (%s): %w", name, diskID, err))
		}
	}

	ui.Sayf("Creating ISO disk '%s' on storage domain '%s'...", name, config.StorageDomain)
	diskResp, err := conn.SystemService().DisksService().Add().Disk(
		ovirtsdk4.NewDiskBuilder().
			Name(name).
			Description("Uploaded by Packer").
			ContentType(ovirtsdk4.DISKCONTENTTYPE_ISO).
			Format(ovirtsdk4.DISKFORMAT_RAW).
			ProvisionedSize(size).
			Sparse(false).
			StorageDomainsOfAny(ovirtsdk4.NewStorageDomainBuilder().Id(storageDomainID).MustBuild()).
			MustBuild(),
	).Send()
	if err != nil {
		return halt(fmt.Errorf("could not create the ISO disk: %w", err))
	}
	s.diskID = diskResp.MustDisk().MustId()
	s.created = true

	stateChange := StateChangeConf{
		Pending:   []string{string(ovirtsdk4.DISKSTATUS_LOCKED)},
		Target:    []string{string(ovirtsdk4.DISKSTATUS_OK)},
		Refresh:   DiskStateRefreshFunc(conn, s.diskID),
		StepState: state,
	}
	if _, err := WaitForState(&stateChange); err != nil {
		return halt(fmt.Errorf("failed waiting for ISO disk (%s) to reach status OK: %w", s.diskID, err))
	}

	transfer, err := startImageTransfer(ctx, conn, s.diskID, ovirtsdk4.IMAGETRANSFERDIRECTION_UPLOAD)
	if err != nil {
		return halt(err)
	}
	transferID := transfer.MustId()

	imageURL, err := imageioURL(ctx, client, transfer)
	if err != nil {
		cancelImageTransfer(conn, transferID)
		return halt(err)
	}

	ui.Sayf("Uploading ISO (%s)...", formatSize(size))
	progress := &uploadProgress{reader: file, total: size, ui: ui, step: 10}
	if err := imageioUpload(ctx, client, imageURL, progress, size); err != nil {
		cancelImageTransfer(conn, transferID)
		return halt(fmt.Errorf("could not upload the ISO: %w", err))
	}

	ui.Say("Finalizing the ISO upload...")
	if err := finalizeImageTransfer(ctx, conn, transferID, s.diskID); err != nil {
		return halt(err)
	}
	s.complete = true

	ui.Sayf("Uploaded ISO disk '%s' (%s)", name, s.diskID)
	state.Put(isoDiskIDKey, s.diskID)
	return multistep.ActionContinue
}

// Cleanup removes the ISO disk if the upload did not complete, or if
// remote_cache_cleanup asks for it. A disk that was reused is never removed.
func (s *stepUploadISO) Cleanup(state multistep.StateBag) {
	if !s.created {
		return
	}
	config := state.Get("config").(*Config)
	ui := state.Get("ui").(packer.Ui)
	if s.complete && !config.RemoteCacheCleanup {
		return
	}

	conn, err := ovirtConnect(config, state)
	if err != nil {
		ui.Error(err.Error())
		return
	}

	ui.Sayf("Removing ISO disk '%s' (%s)...", config.RemoteCacheDiskName, s.diskID)
	// The build context may already be cancelled, the removal should still run.
	if err := removeDisk(context.Background(), conn, s.diskID); err != nil {
		ui.Error(fmt.Sprintf("could not remove ISO disk '%s' (%s), remove it manually: %s",
			config.RemoteCacheDiskName, s.diskID, err))
	}
}

// findISODisks returns the ISO disks with exactly that name on the storage
// domain.
func findISODisks(conn *ovirtsdk4.Connection, name, storageDomainID string) ([]*ovirtsdk4.Disk, error) {
	log.Printf("Searching for ISO disk '%s' on storage domain %s", name, storageDomainID)
	resp, err := conn.SystemService().DisksService().List().
		Search(fmt.Sprintf(`disk_content_type=iso and name="%s"`, name)).
		Send()
	if err != nil {
		return nil, fmt.Errorf("error while searching for ISO disk '%s': %w", name, err)
	}

	var found []*ovirtsdk4.Disk
	for _, disk := range resp.MustDisks().Slice() {
		// The search ignores case and expands wildcards.
		if diskName, _ := disk.Name(); diskName != name {
			continue
		}
		domains, ok := disk.StorageDomains()
		if !ok {
			continue
		}
		for _, domain := range domains.Slice() {
			if id, _ := domain.Id(); id == storageDomainID {
				found = append(found, disk)
				break
			}
		}
	}
	return found, nil
}

// isoDiskMatches reports whether the disk holds the same data as the local
// ISO. It compares ovirt-imageio's block checksum of the disk, which the host
// computes without sending the data, with the same checksum of the file.
func isoDiskMatches(ctx context.Context, conn *ovirtsdk4.Connection, client *http.Client,
	diskID string, file io.ReadSeeker, size int64) (same bool, err error) {
	transfer, err := startImageTransfer(ctx, conn, diskID, ovirtsdk4.IMAGETRANSFERDIRECTION_DOWNLOAD)
	if err != nil {
		return false, err
	}
	defer func() {
		// Finalizing releases the disk. Nothing was changed, so a failure here
		// only matters if the comparison itself went fine.
		if ferr := finalizeImageTransfer(context.Background(), conn, transfer.MustId(), diskID); ferr != nil && err == nil {
			same, err = false, ferr
		}
	}()

	imageURL, err := imageioURL(ctx, client, transfer)
	if err != nil {
		return false, err
	}

	remoteSize, err := imageioImageSize(ctx, client, imageURL)
	if err != nil {
		return false, err
	}
	if remoteSize < size || remoteSize-size > maxImageSizeSlack {
		log.Printf("ISO disk %s has %d bytes, the ISO has %d bytes", diskID, remoteSize, size)
		return false, nil
	}

	remoteChecksum, blockSize, err := imageioChecksum(ctx, client, imageURL)
	if err != nil {
		return false, err
	}

	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}
	localChecksum, err := blockChecksum(file, remoteSize, sha256.New, blockSize)
	if err != nil {
		return false, fmt.Errorf("could not compute the checksum of the ISO: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return false, err
	}

	log.Printf("ISO disk %s block checksum %s, local ISO %s", diskID, remoteChecksum, localChecksum)
	return remoteChecksum == localChecksum, nil
}

// removeDisk removes a disk and waits until it is gone. A disk that does not
// exist, or that the engine removes on its own after a cancelled upload, is
// not an error.
func removeDisk(ctx context.Context, conn *ovirtsdk4.Connection, diskID string) error {
	diskService := conn.SystemService().DisksService().DiskService(diskID)
	deadline := time.Now().Add(imageTransferTimeout)
	requested := false
	var lastErr error

	for {
		resp, err := diskService.Get().Send()
		if isNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}

		// A locked disk can not be removed yet.
		if status, _ := resp.MustDisk().Status(); !requested && status != ovirtsdk4.DISKSTATUS_LOCKED {
			_, err := diskService.Remove().Send()
			switch {
			case isNotFound(err):
				return nil
			case err != nil:
				lastErr = err
			default:
				requested = true
			}
		}

		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("timed out waiting for disk %s to be removed", diskID)
		}
		if err := sleepContext(ctx, imageTransferPollInterval); err != nil {
			return err
		}
	}
}

// uploadProgress reports the upload through the UI every step percent.
type uploadProgress struct {
	reader io.Reader
	total  int64
	ui     packer.Ui
	step   int64

	sent     int64
	reported int64
}

func (p *uploadProgress) Read(b []byte) (int, error) {
	n, err := p.reader.Read(b)
	p.sent += int64(n)
	if p.total > 0 {
		if percent := p.sent * 100 / p.total / p.step * p.step; percent > p.reported && percent < 100 {
			p.reported = percent
			p.ui.Sayf("Uploaded %d%% (%s of %s)", percent, formatSize(p.sent), formatSize(p.total))
		}
	}
	return n, err
}

// formatSize renders a byte count for the UI.
func formatSize(bytes int64) string {
	const (
		mib = 1024 * 1024
		gib = 1024 * mib
	)
	if bytes >= gib {
		return fmt.Sprintf("%.1f GiB", float64(bytes)/gib)
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/mib)
}
