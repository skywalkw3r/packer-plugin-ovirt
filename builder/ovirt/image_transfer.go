package ovirt

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	ovirtsdk4 "github.com/ovirt/go-ovirt/v4"
)

// These are variables so that the tests do not have to wait.
var (
	// imageTransferPollInterval is the delay between two status checks.
	imageTransferPollInterval = time.Second
	// imageTransferTimeout bounds the wait for a transfer to start or finish,
	// and for a disk to be removed.
	imageTransferTimeout = 2 * time.Minute
	// imageioConnectTimeout bounds the check that an imageio URL is reachable.
	imageioConnectTimeout = 10 * time.Second
)

// imageioChecksumAlgorithm is the digest requested for the block checksum.
const imageioChecksumAlgorithm = "sha256"

// newImageioClient returns the HTTP client used to talk to ovirt-imageio. Like
// the API connection it uses the system CA pool unless insecure_skip_tls_verify
// is set, and no HTTP proxy.
func newImageioClient(config *Config) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: config.SkipCertValidation},
		},
	}
}

func isNotFound(err error) bool {
	var notFound *ovirtsdk4.NotFoundError
	return errors.As(err, &notFound)
}

// redactImageioURL hides the ticket ID in an ovirt-imageio URL
// (https://host:port/images/<ticket-id>/...). The ticket ID is the only
// credential for the transfer, so it must not reach errors or logs.
func redactImageioURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "(ovirt-imageio URL)"
	}
	before, rest, ok := strings.Cut(u.Path, "/images/")
	if !ok {
		return u.Redacted()
	}
	_, suffix, _ := strings.Cut(rest, "/")
	u.Path = before + "/images/REDACTED"
	if suffix != "" {
		u.Path += "/" + suffix
	}
	u.RawPath = ""
	return u.Redacted()
}

// imageioDo sends a request to ovirt-imageio. A transport error names the URL,
// so the ticket ID is hidden in it.
func imageioDo(client *http.Client, req *http.Request) (*http.Response, error) {
	resp, err := client.Do(req)
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		urlErr.URL = redactImageioURL(urlErr.URL)
	}
	return resp, err
}

// sleepContext waits for d, or returns early with the context's error.
func sleepContext(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

// startImageTransfer creates an image transfer for the disk and waits until it
// is ready to transfer data. On failure the transfer is cancelled.
func startImageTransfer(ctx context.Context, conn *ovirtsdk4.Connection, diskID string,
	direction ovirtsdk4.ImageTransferDirection) (*ovirtsdk4.ImageTransfer, error) {
	transfersService := conn.SystemService().ImageTransfersService()

	addResp, err := transfersService.Add().ImageTransfer(
		ovirtsdk4.NewImageTransferBuilder().
			DiskBuilder(ovirtsdk4.NewDiskBuilder().Id(diskID)).
			Direction(direction).
			Format(ovirtsdk4.DISKFORMAT_RAW).
			MustBuild(),
	).Send()
	if err != nil {
		return nil, fmt.Errorf("could not create the image transfer for disk %s: %w", diskID, err)
	}
	transferID := addResp.MustImageTransfer().MustId()
	log.Printf("Created %s image transfer %s for disk %s", direction, transferID, diskID)

	transferService := transfersService.ImageTransferService(transferID)
	deadline := time.Now().Add(imageTransferTimeout)
	for {
		getResp, err := transferService.Get().Send()
		if err != nil {
			if isNotFound(err) {
				return nil, fmt.Errorf("image transfer %s was removed", transferID)
			}
			cancelImageTransfer(conn, transferID)
			return nil, fmt.Errorf("could not get image transfer %s: %w", transferID, err)
		}
		transfer := getResp.MustImageTransfer()

		switch phase := transfer.MustPhase(); phase {
		case ovirtsdk4.IMAGETRANSFERPHASE_TRANSFERRING:
			return transfer, nil
		case ovirtsdk4.IMAGETRANSFERPHASE_INITIALIZING:
			// Not ready yet.
		default:
			cancelImageTransfer(conn, transferID)
			return nil, fmt.Errorf("image transfer %s is in unexpected phase '%s'", transferID, phase)
		}

		if time.Now().After(deadline) {
			cancelImageTransfer(conn, transferID)
			return nil, fmt.Errorf("timed out waiting for image transfer %s to start", transferID)
		}
		if err := sleepContext(ctx, imageTransferPollInterval); err != nil {
			cancelImageTransfer(conn, transferID)
			return nil, err
		}
	}
}

// cancelImageTransfer cancels a transfer. For an upload the engine then
// removes the disk.
func cancelImageTransfer(conn *ovirtsdk4.Connection, transferID string) {
	log.Printf("Cancelling image transfer %s", transferID)
	service := conn.SystemService().ImageTransfersService().ImageTransferService(transferID)
	if _, err := service.Cancel().Send(); err != nil && !isNotFound(err) {
		log.Printf("Could not cancel image transfer %s: %s", transferID, err)
	}
}

// finalizeImageTransfer finalizes a transfer and waits for the engine to
// finish it, which releases the disk.
func finalizeImageTransfer(ctx context.Context, conn *ovirtsdk4.Connection, transferID, diskID string) error {
	transferService := conn.SystemService().ImageTransfersService().ImageTransferService(transferID)
	if _, err := transferService.Finalize().Send(); err != nil {
		return fmt.Errorf("could not finalize image transfer %s: %w", transferID, err)
	}

	deadline := time.Now().Add(imageTransferTimeout)
	for {
		getResp, err := transferService.Get().Send()
		switch {
		case isNotFound(err):
			// Engines before 4.4.7 remove the transfer as soon as it ends, so
			// the disk status has to tell how it went.
			diskResp, err := conn.SystemService().DisksService().DiskService(diskID).Get().Send()
			if err != nil {
				if isNotFound(err) {
					return fmt.Errorf("image transfer %s failed: disk %s was removed", transferID, diskID)
				}
				return fmt.Errorf("could not get disk %s: %w", diskID, err)
			}
			if status := diskResp.MustDisk().MustStatus(); status != ovirtsdk4.DISKSTATUS_OK {
				return fmt.Errorf("image transfer %s failed: disk %s is '%s'", transferID, diskID, status)
			}
			return nil
		case err != nil:
			return fmt.Errorf("could not get image transfer %s: %w", transferID, err)
		}

		phase := getResp.MustImageTransfer().MustPhase()
		switch phase {
		case ovirtsdk4.IMAGETRANSFERPHASE_FINISHED_SUCCESS:
			return nil
		case ovirtsdk4.IMAGETRANSFERPHASE_FINISHED_FAILURE:
			return fmt.Errorf("image transfer %s failed", transferID)
		}

		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for image transfer %s to finish, phase '%s'", transferID, phase)
		}
		if err := sleepContext(ctx, imageTransferPollInterval); err != nil {
			return err
		}
	}
}

// imageioURL returns the first URL of the transfer that answers. The transfer
// URL talks to the host directly; the proxy URL goes through the engine and
// works when the hosts cannot be reached from where Packer runs.
func imageioURL(ctx context.Context, client *http.Client, transfer *ovirtsdk4.ImageTransfer) (string, error) {
	var urls []string
	if u, ok := transfer.TransferUrl(); ok && u != "" {
		urls = append(urls, u)
	}
	if u, ok := transfer.ProxyUrl(); ok && u != "" {
		urls = append(urls, u)
	}
	if len(urls) == 0 {
		return "", errors.New("the image transfer has neither a transfer URL nor a proxy URL")
	}

	var lastErr error
	for _, u := range urls {
		if err := imageioOptions(ctx, client, u); err != nil {
			log.Printf("Could not reach %s: %s", redactImageioURL(u), err)
			lastErr = err
			continue
		}
		return u, nil
	}
	return "", fmt.Errorf("could not reach ovirt-imageio: %w", lastErr)
}

func imageioOptions(ctx context.Context, client *http.Client, imageURL string) error {
	ctx, cancel := context.WithTimeout(ctx, imageioConnectTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodOptions, imageURL, nil)
	if err != nil {
		return err
	}
	resp, err := imageioDo(client, req)
	if err != nil {
		return err
	}
	return imageioCheckResponse(resp, true)
}

// imageioCheckResponse turns a non-200 answer into an error carrying the
// server's message. With closeBody it also drains and closes the body.
func imageioCheckResponse(resp *http.Response, closeBody bool) error {
	if resp.StatusCode != http.StatusOK {
		message, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		_ = resp.Body.Close()
		return fmt.Errorf("%s %s: %s: %s", resp.Request.Method, redactImageioURL(resp.Request.URL.String()),
			resp.Status, strings.TrimSpace(string(message)))
	}
	if closeBody {
		_, _ = io.Copy(io.Discard, resp.Body)
		return resp.Body.Close()
	}
	return nil
}

// imageioUpload sends size bytes from body as the whole image.
func imageioUpload(ctx context.Context, client *http.Client, imageURL string, body io.Reader, size int64) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, imageURL, io.NopCloser(body))
	if err != nil {
		return err
	}
	req.ContentLength = size
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := imageioDo(client, req)
	if err != nil {
		return err
	}
	return imageioCheckResponse(resp, true)
}

func imageioGetJSON(ctx context.Context, client *http.Client, url string, result any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := imageioDo(client, req)
	if err != nil {
		return err
	}
	if err := imageioCheckResponse(resp, false); err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	return json.NewDecoder(resp.Body).Decode(result)
}

// imageioImageSize returns the size of the image behind a transfer, as
// ovirt-imageio sees it. It can be larger than the size the disk was created
// with, because storage rounds sizes up.
func imageioImageSize(ctx context.Context, client *http.Client, imageURL string) (int64, error) {
	var extents []struct {
		Start  int64 `json:"start"`
		Length int64 `json:"length"`
	}
	if err := imageioGetJSON(ctx, client, imageURL+"/extents", &extents); err != nil {
		return 0, fmt.Errorf("could not get the image extents: %w", err)
	}
	var size int64
	for _, extent := range extents {
		size = max(size, extent.Start+extent.Length)
	}
	return size, nil
}

// imageioChecksum asks ovirt-imageio for the block checksum of the image and
// returns it with the block size it was computed with. See blockChecksum.
func imageioChecksum(ctx context.Context, client *http.Client, imageURL string) (string, int, error) {
	var result struct {
		Checksum  string `json:"checksum"`
		Algorithm string `json:"algorithm"`
		BlockSize int    `json:"block_size"`
	}
	url := imageURL + "/checksum?algorithm=" + imageioChecksumAlgorithm
	if err := imageioGetJSON(ctx, client, url, &result); err != nil {
		return "", 0, fmt.Errorf("could not get the image checksum: %w", err)
	}
	if result.Algorithm != imageioChecksumAlgorithm || result.Checksum == "" || result.BlockSize <= 0 {
		return "", 0, fmt.Errorf("unexpected checksum answer from ovirt-imageio: algorithm '%s', block size %d",
			result.Algorithm, result.BlockSize)
	}
	return result.Checksum, result.BlockSize, nil
}
