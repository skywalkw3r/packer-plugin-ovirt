package ovirt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
)

const fakeChecksumBlockSize = 4096

type fakeDisk struct {
	name    string
	domain  string
	status  string
	content []byte
}

type fakeTransfer struct {
	diskID    string
	direction string
	phase     string
}

// fakeOvirt is a fake engine and a fake ovirt-imageio daemon on one server. It
// implements the calls stepUploadISO makes and records what happened.
type fakeOvirt struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	disks     map[string]*fakeDisk
	transfers map[string]*fakeTransfer
	nextID    int

	// Behaviour switches.
	failUpload       bool // the daemon answers PUT with a 500
	deadTransferURL  bool // only the proxy URL of a transfer answers
	roundUpImageSize int  // the daemon reports images rounded up to this size
	forgetTransfers  bool // like engines before 4.4.7, a finished transfer is gone
	finalizeFails    bool // finalizing ends in finished_failure and an illegal disk
	stuckTransfers   bool // a new transfer is paused by the system

	// What happened.
	searches   []string
	createdXML []string
	removed    []string
	cancelled  []string
	finalized  []string
	puts       int
	checksums  int
}

func newFakeOvirt(t *testing.T) *fakeOvirt {
	t.Helper()
	f := &fakeOvirt{
		t:         t,
		disks:     map[string]*fakeDisk{},
		transfers: map[string]*fakeTransfer{},
	}
	f.srv = httptest.NewTLSServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeOvirt) addDisk(id, name, domain string, content []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.disks[id] = &fakeDisk{name: name, domain: domain, status: "ok", content: content}
}

func (f *fakeOvirt) diskXML(id string, d *fakeDisk) string {
	return fmt.Sprintf(`<disk id="%s"><name>%s</name><status>%s</status><content_type>iso</content_type>`+
		`<storage_domains><storage_domain id="%s"/></storage_domains></disk>`, id, d.name, d.status, d.domain)
}

func (f *fakeOvirt) transferXML(id string, tr *fakeTransfer) string {
	transferURL := f.srv.URL + "/images/" + id
	if f.deadTransferURL {
		// Nothing listens on port 1.
		transferURL = "https://127.0.0.1:1/images/" + id
	}
	return fmt.Sprintf(`<image_transfer id="%s"><phase>%s</phase><direction>%s</direction>`+
		`<transfer_url>%s</transfer_url><proxy_url>%s</proxy_url></image_transfer>`,
		id, tr.phase, tr.direction, transferURL, f.srv.URL+"/images/"+id)
}

// image returns the disk content as the daemon exposes it.
func (f *fakeOvirt) image(d *fakeDisk) []byte {
	if f.roundUpImageSize <= 0 || len(d.content)%f.roundUpImageSize == 0 {
		return d.content
	}
	padded := make([]byte, (len(d.content)/f.roundUpImageSize+1)*f.roundUpImageSize)
	copy(padded, d.content)
	return padded
}

func (f *fakeOvirt) handle(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()

	xml := func(body string) {
		w.Header().Set("Content-Type", "application/xml")
		_, _ = fmt.Fprint(w, body)
	}
	json := func(body string) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}

	if r.URL.Path == "/ovirt-engine/sso/oauth/token" {
		json(`{"access_token":"token"}`)
		return
	}

	// The imageio daemon.
	if rest, ok := strings.CutPrefix(r.URL.Path, "/images/"); ok {
		id, sub, _ := strings.Cut(rest, "/")
		tr := f.transfers[id]
		if tr == nil || tr.phase != "transferring" {
			http.Error(w, "no such ticket", http.StatusForbidden)
			return
		}
		disk := f.disks[tr.diskID]
		switch {
		case r.Method == http.MethodOptions && sub == "":
			json(`{"features": ["extents", "zero", "flush"]}`)
		case r.Method == http.MethodPut && sub == "":
			f.puts++
			body, _ := io.ReadAll(r.Body)
			if f.failUpload {
				http.Error(w, "disk full", http.StatusInternalServerError)
				return
			}
			if int64(len(body)) != r.ContentLength {
				http.Error(w, "short body", http.StatusBadRequest)
				return
			}
			disk.content = body
		case r.Method == http.MethodGet && sub == "extents":
			json(fmt.Sprintf(`[{"start": 0, "length": %d, "zero": false, "hole": false}]`, len(f.image(disk))))
		case r.Method == http.MethodGet && sub == "checksum":
			f.checksums++
			algorithm := r.URL.Query().Get("algorithm")
			if algorithm != "sha256" {
				http.Error(w, "unexpected algorithm "+algorithm, http.StatusBadRequest)
				return
			}
			image := f.image(disk)
			sum, err := blockChecksum(bytes.NewReader(image), int64(len(image)), sha256.New, fakeChecksumBlockSize)
			if err != nil {
				f.t.Error(err)
			}
			json(fmt.Sprintf(`{"checksum": "%s", "algorithm": "sha256", "block_size": %d}`, sum, fakeChecksumBlockSize))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
		return
	}

	// The engine API.
	path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api")
	switch {
	case path == "":
		xml(`<api></api>`)

	case path == "/clusters":
		xml(`<clusters><cluster id="c1"><name>Default</name></cluster></clusters>`)

	case path == "/storagedomains":
		xml(`<storage_domains><storage_domain id="sd1"><name>data</name></storage_domain></storage_domains>`)

	case path == "/disks" && r.Method == http.MethodGet:
		f.searches = append(f.searches, r.URL.Query().Get("search"))
		var b strings.Builder
		b.WriteString("<disks>")
		for id, d := range f.disks {
			b.WriteString(f.diskXML(id, d))
		}
		b.WriteString("</disks>")
		xml(b.String())

	case path == "/disks" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.createdXML = append(f.createdXML, string(body))
		name := between(string(body), "<name>", "</name>")
		f.nextID++
		id := fmt.Sprintf("new%d", f.nextID)
		f.disks[id] = &fakeDisk{name: name, domain: "sd1", status: "ok"}
		xml(f.diskXML(id, f.disks[id]))

	case strings.HasPrefix(path, "/disks/"):
		id := strings.TrimPrefix(path, "/disks/")
		d := f.disks[id]
		if d == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Method == http.MethodDelete {
			delete(f.disks, id)
			f.removed = append(f.removed, id)
			xml(`<action><status>complete</status></action>`)
			return
		}
		xml(f.diskXML(id, d))

	case path == "/imagetransfers" && r.Method == http.MethodPost:
		body, _ := io.ReadAll(r.Body)
		f.nextID++
		id := fmt.Sprintf("t%d", f.nextID)
		f.transfers[id] = &fakeTransfer{
			diskID:    between(string(body), `<disk id="`, `"`),
			direction: between(string(body), "<direction>", "</direction>"),
			// The first status check finds the transfer still initializing.
			phase: "initializing",
		}
		if f.stuckTransfers {
			f.transfers[id].phase = "paused_system"
		}
		xml(f.transferXML(id, f.transfers[id]))

	case strings.HasPrefix(path, "/imagetransfers/"):
		id, action, _ := strings.Cut(strings.TrimPrefix(path, "/imagetransfers/"), "/")
		tr := f.transfers[id]
		if tr == nil {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch action {
		case "":
			body := f.transferXML(id, tr)
			if tr.phase == "initializing" {
				tr.phase = "transferring"
			}
			xml(body)
		case "finalize":
			tr.phase = "finished_success"
			if f.finalizeFails {
				tr.phase = "finished_failure"
				f.disks[tr.diskID].status = "illegal"
			}
			if f.forgetTransfers {
				delete(f.transfers, id)
			}
			f.finalized = append(f.finalized, id)
			xml(`<action><status>complete</status></action>`)
		case "cancel":
			tr.phase = "cancelled_user"
			f.cancelled = append(f.cancelled, id)
			if tr.direction == "upload" {
				// The engine removes the disk of a cancelled upload.
				delete(f.disks, tr.diskID)
			}
			xml(`<action><status>complete</status></action>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}

	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func between(s, start, end string) string {
	_, rest, ok := strings.Cut(s, start)
	if !ok {
		return ""
	}
	value, _, _ := strings.Cut(rest, end)
	return value
}

// uploadISOState returns the state bag for a stepUploadISO run against the
// fake, with an ISO holding content.
func (f *fakeOvirt) uploadISOState(t *testing.T, content []byte) (*multistep.BasicStateBag, *Config) {
	t.Helper()

	oldInterval, oldConnect := imageTransferPollInterval, imageioConnectTimeout
	imageTransferPollInterval = time.Millisecond
	imageioConnectTimeout = 2 * time.Second
	t.Cleanup(func() { imageTransferPollInterval, imageioConnectTimeout = oldInterval, oldConnect })

	isoPath := filepath.Join(t.TempDir(), "test.iso")
	if err := os.WriteFile(isoPath, content, 0o600); err != nil {
		t.Fatal(err)
	}

	u, err := url.Parse(f.srv.URL + "/ovirt-engine/api")
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{}
	config.ovirtURL = u
	config.Username = "admin@internal"
	config.Password = "password"
	config.SkipCertValidation = true
	config.StorageDomain = "data"
	config.RemoteCacheDiskName = "test.iso"

	state := new(multistep.BasicStateBag)
	state.Put("config", config)
	state.Put("ui", packer.TestUi(t))
	state.Put(isoPathKey, isoPath)
	return state, config
}

// testISO is larger than a checksum block and not aligned to it.
func testISO() []byte {
	return bytes.Repeat([]byte("packer-ovirt-iso"), 700) // 11200 bytes
}

func TestStepUploadISO_Uploads(t *testing.T) {
	f := newFakeOvirt(t)
	// A disk of the same name on another storage domain is not ours.
	f.addDisk("other", "test.iso", "sd2", []byte("something else"))
	state, _ := f.uploadISOState(t, testISO())

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}
	step.Cleanup(state)

	f.mu.Lock()
	defer f.mu.Unlock()
	id, _ := state.Get(isoDiskIDKey).(string)
	disk := f.disks[id]
	if disk == nil || id == "other" {
		t.Fatalf("expected the new disk in the state, got %q", id)
	}
	if !bytes.Equal(disk.content, testISO()) {
		t.Fatalf("the uploaded data differs from the ISO (%d bytes uploaded)", len(disk.content))
	}
	if len(f.searches) != 1 || f.searches[0] != `disk_content_type=iso and name="test.iso"` {
		t.Fatalf("unexpected disk searches: %q", f.searches)
	}
	if len(f.createdXML) != 1 {
		t.Fatalf("expected one disk to be created, got %d", len(f.createdXML))
	}
	for _, want := range []string{
		"<name>test.iso</name>",
		"<content_type>iso</content_type>",
		"<format>raw</format>",
		fmt.Sprintf("<provisioned_size>%d</provisioned_size>", len(testISO())),
		"<sparse>false</sparse>",
		`<storage_domain id="sd1">`,
	} {
		if !strings.Contains(f.createdXML[0], want) {
			t.Errorf("expected the disk request to contain %s, got: %s", want, f.createdXML[0])
		}
	}
	if len(f.finalized) != 1 || len(f.cancelled) != 0 {
		t.Fatalf("expected one finalized transfer, got finalized=%v cancelled=%v", f.finalized, f.cancelled)
	}
	if len(f.removed) != 0 {
		t.Fatalf("expected the disk to be kept without remote_cache_cleanup, removed: %v", f.removed)
	}
}

func TestStepUploadISO_ReusesMatchingDisk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		roundUp int
	}{
		{"same size", 0},
		{"disk rounded up on storage", 4096},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeOvirt(t)
			f.roundUpImageSize = tc.roundUp
			f.addDisk("iso1", "test.iso", "sd1", testISO())
			state, _ := f.uploadISOState(t, testISO())

			step := &stepUploadISO{}
			if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
				t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
			}
			step.Cleanup(state)

			f.mu.Lock()
			defer f.mu.Unlock()
			if id, _ := state.Get(isoDiskIDKey).(string); id != "iso1" {
				t.Fatalf("expected the existing disk iso1, got %q", id)
			}
			if f.puts != 0 || len(f.createdXML) != 0 {
				t.Fatalf("expected no upload, got %d uploads and %d new disks", f.puts, len(f.createdXML))
			}
			if f.checksums != 1 {
				t.Fatalf("expected one checksum request, got %d", f.checksums)
			}
			if len(f.finalized) != 1 {
				t.Fatalf("expected the download transfer to be finalized, got %v", f.finalized)
			}
			if len(f.removed) != 0 {
				t.Fatalf("a reused disk must never be removed, removed: %v", f.removed)
			}
		})
	}
}

func TestStepUploadISO_CleanupNeverRemovesReusedDisk(t *testing.T) {
	f := newFakeOvirt(t)
	f.addDisk("iso1", "test.iso", "sd1", testISO())
	state, config := f.uploadISOState(t, testISO())
	config.RemoteCacheCleanup = true

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}
	step.Cleanup(state)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.removed) != 0 {
		t.Fatalf("a reused disk must never be removed, removed: %v", f.removed)
	}
}

func TestStepUploadISO_DifferentDiskFails(t *testing.T) {
	changed := testISO()
	changed[5000] ^= 0xff

	for name, existing := range map[string][]byte{
		"same size, different data": changed,
		"smaller disk":              testISO()[:8192],
	} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOvirt(t)
			f.addDisk("iso1", "test.iso", "sd1", existing)
			state, _ := f.uploadISOState(t, testISO())

			step := &stepUploadISO{}
			if action := step.Run(context.Background(), state); action != multistep.ActionHalt {
				t.Fatalf("expected ActionHalt, got %v", action)
			}
			step.Cleanup(state)

			err, _ := state.Get("error").(error)
			if err == nil || !strings.Contains(err.Error(), "differs from the downloaded ISO") ||
				!strings.Contains(err.Error(), "remote_cache_overwrite") {
				t.Fatalf("unexpected error: %v", err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.puts != 0 || len(f.removed) != 0 || len(f.createdXML) != 0 {
				t.Fatalf("expected the existing disk to be left alone: puts=%d removed=%v created=%d",
					f.puts, f.removed, len(f.createdXML))
			}
			if len(f.finalized) != 1 {
				t.Fatalf("expected the download transfer to be finalized, got %v", f.finalized)
			}
		})
	}
}

func TestStepUploadISO_Overwrite(t *testing.T) {
	f := newFakeOvirt(t)
	f.addDisk("iso1", "test.iso", "sd1", []byte("an older ISO"))
	state, config := f.uploadISOState(t, testISO())
	config.RemoteCacheOverwrite = true

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.removed) != 1 || f.removed[0] != "iso1" {
		t.Fatalf("expected iso1 to be removed, removed: %v", f.removed)
	}
	if f.checksums != 0 {
		t.Fatalf("expected no checksum request with remote_cache_overwrite, got %d", f.checksums)
	}
	id, _ := state.Get(isoDiskIDKey).(string)
	if disk := f.disks[id]; disk == nil || !bytes.Equal(disk.content, testISO()) {
		t.Fatalf("expected a new disk with the ISO, got %q", id)
	}
}

func TestStepUploadISO_SeveralDisksFail(t *testing.T) {
	f := newFakeOvirt(t)
	f.addDisk("iso1", "test.iso", "sd1", testISO())
	f.addDisk("iso2", "test.iso", "sd1", testISO())
	state, _ := f.uploadISOState(t, testISO())

	if action := (&stepUploadISO{}).Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("expected ActionHalt, got %v", action)
	}
	if err, _ := state.Get("error").(error); err == nil || !strings.Contains(err.Error(), "found 2 ISO disks") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestStepUploadISO_FailedUploadCleansUp(t *testing.T) {
	f := newFakeOvirt(t)
	f.failUpload = true
	state, _ := f.uploadISOState(t, testISO())

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("expected ActionHalt, got %v", action)
	}
	step.Cleanup(state)

	if err, _ := state.Get("error").(error); err == nil || !strings.Contains(err.Error(), "could not upload the ISO") ||
		!strings.Contains(err.Error(), "disk full") {
		t.Fatalf("unexpected error: %v", err)
	}
	// The ticket ID in the imageio URL is the transfer's credential.
	if err, _ := state.Get("error").(error); strings.Contains(err.Error(), "/images/t") ||
		!strings.Contains(err.Error(), "/images/REDACTED") {
		t.Fatalf("the error must not show the imageio ticket: %v", err)
	}
	if _, ok := state.GetOk(isoDiskIDKey); ok {
		t.Fatal("a failed upload must not leave a disk ID in the state")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancelled) != 1 {
		t.Fatalf("expected the transfer to be cancelled, got %v", f.cancelled)
	}
	if len(f.disks) != 0 {
		t.Fatalf("expected no disk to be left behind, got %d", len(f.disks))
	}
}

func TestStepUploadISO_FallsBackToProxyURL(t *testing.T) {
	f := newFakeOvirt(t)
	f.deadTransferURL = true
	state, _ := f.uploadISOState(t, testISO())

	if action := (&stepUploadISO{}).Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 1 {
		t.Fatalf("expected the upload to go through the proxy URL, got %d uploads", f.puts)
	}
}

func TestStepUploadISO_RemoteCacheCleanup(t *testing.T) {
	f := newFakeOvirt(t)
	state, config := f.uploadISOState(t, testISO())
	config.RemoteCacheCleanup = true

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}
	id, _ := state.Get(isoDiskIDKey).(string)
	step.Cleanup(state)

	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.removed) != 1 || f.removed[0] != id {
		t.Fatalf("expected the uploaded disk %q to be removed, removed: %v", id, f.removed)
	}
}

func TestUploadProgress(t *testing.T) {
	ui := packer.TestUi(t)
	p := &uploadProgress{reader: bytes.NewReader(make([]byte, 1000)), total: 1000, ui: ui, step: 10}
	buf := make([]byte, 100)
	for {
		if _, err := p.Read(buf); err == io.EOF {
			break
		} else if err != nil {
			t.Fatal(err)
		}
	}
	if p.sent != 1000 {
		t.Fatalf("expected 1000 bytes to be counted, got %d", p.sent)
	}
	// 100% is left to the "uploaded" message of the step.
	if p.reported != 90 {
		t.Fatalf("expected the last report at 90%%, got %d%%", p.reported)
	}
}

// TestStepSetupInitialRun_UsesUploadedISO checks that the CD-ROM is attached
// by the ID stepUploadISO left in the state, without searching by name.
func TestStepSetupInitialRun_UsesUploadedISO(t *testing.T) {
	var mu sync.Mutex
	var cdrom string
	var diskSearches int
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		xml := func(body string) {
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(w, body)
		}
		mu.Lock()
		defer mu.Unlock()
		switch path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api"); {
		case r.URL.Path == "/ovirt-engine/sso/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"token"}`)
		case path == "":
			xml(`<api></api>`)
		case path == "/disks":
			diskSearches++
			xml(`<disks></disks>`)
		case path == "/vms/vm1/cdroms" && r.Method == http.MethodPost:
			body, _ := io.ReadAll(r.Body)
			cdrom = string(body)
			xml(`<cdrom id="cd1"></cdrom>`)
		case path == "/vms/vm1/start":
			xml(`<action><status>complete</status></action>`)
		case path == "/vms/vm1":
			xml(`<vm id="vm1"><name>packer</name><status>up</status></vm>`)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	c, _, err := NewConfig(isoTestConfig(map[string]any{
		"ovirt_url":                srv.URL + "/ovirt-engine/api",
		"insecure_skip_tls_verify": true,
	}))
	if err != nil {
		t.Fatal(err)
	}

	state := new(multistep.BasicStateBag)
	state.Put("config", c)
	state.Put("ui", packer.TestUi(t))
	state.Put("vm_id", "vm1")
	state.Put(isoDiskIDKey, "iso1")

	step := &stepSetupInitialRun{Comm: &c.Comm}
	if action := step.Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}

	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(cdrom, `<file id="iso1">`) {
		t.Fatalf("expected the CD-ROM to use disk iso1, got: %s", cdrom)
	}
	if diskSearches != 0 {
		t.Fatalf("expected no disk search, got %d", diskSearches)
	}
}

// TestBuilderRun_DownloadsAndUploadsISO runs the builder with iso_url up to
// the VM creation, which the fake engine does not implement: the ISO has to
// be downloaded and uploaded before the VM is created.
func TestBuilderRun_DownloadsAndUploadsISO(t *testing.T) {
	f := newFakeOvirt(t)
	state, _ := f.uploadISOState(t, testISO())
	isoPath := state.Get(isoPathKey).(string)

	var b Builder
	if _, _, err := b.Prepare(isoTestConfig(map[string]any{
		"ovirt_url":                f.srv.URL + "/ovirt-engine/api",
		"insecure_skip_tls_verify": true,
		"iso_url":                  isoPath,
		"iso_checksum":             "none",
		"iso_target_path":          filepath.Join(t.TempDir(), "cache.iso"),
	})); err != nil {
		t.Fatalf("Prepare: %s", err)
	}

	_, err := b.Run(context.Background(), packer.TestUi(t), &packer.MockHook{})
	if err == nil || !strings.Contains(err.Error(), "could not find virtual machine template") {
		t.Fatalf("expected the build to fail at the VM creation, got: %v", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	if f.puts != 1 || len(f.disks) != 1 {
		t.Fatalf("expected one uploaded ISO disk, got %d uploads and %d disks", f.puts, len(f.disks))
	}
	for id, disk := range f.disks {
		if disk.name != "test.iso" || !bytes.Equal(disk.content, testISO()) {
			t.Fatalf("unexpected ISO disk %s: name %q, %d bytes", id, disk.name, len(disk.content))
		}
	}
	if len(f.removed) != 0 {
		t.Fatalf("a completed upload is kept for the next build, removed: %v", f.removed)
	}
}

func TestStepUploadISO_TransferGoneAfterFinalize(t *testing.T) {
	f := newFakeOvirt(t)
	f.forgetTransfers = true
	state, _ := f.uploadISOState(t, testISO())

	if action := (&stepUploadISO{}).Run(context.Background(), state); action != multistep.ActionContinue {
		t.Fatalf("expected ActionContinue, got %v (error: %v)", action, state.Get("error"))
	}
	if _, ok := state.GetOk(isoDiskIDKey); !ok {
		t.Fatal("expected the disk ID in the state")
	}
}

func TestStepUploadISO_FinalizeFailureCleansUp(t *testing.T) {
	for name, forget := range map[string]bool{"transfer reports the failure": false, "transfer gone, disk illegal": true} {
		t.Run(name, func(t *testing.T) {
			f := newFakeOvirt(t)
			f.finalizeFails = true
			f.forgetTransfers = forget
			state, _ := f.uploadISOState(t, testISO())

			step := &stepUploadISO{}
			if action := step.Run(context.Background(), state); action != multistep.ActionHalt {
				t.Fatalf("expected ActionHalt, got %v", action)
			}
			step.Cleanup(state)

			if err, _ := state.Get("error").(error); err == nil || !strings.Contains(err.Error(), "failed") {
				t.Fatalf("unexpected error: %v", err)
			}
			if _, ok := state.GetOk(isoDiskIDKey); ok {
				t.Fatal("a failed upload must not leave a disk ID in the state")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.disks) != 0 {
				t.Fatalf("expected the illegal disk to be removed, %d disks left", len(f.disks))
			}
		})
	}
}

func TestStepUploadISO_TransferDoesNotStart(t *testing.T) {
	f := newFakeOvirt(t)
	f.stuckTransfers = true
	state, _ := f.uploadISOState(t, testISO())

	step := &stepUploadISO{}
	if action := step.Run(context.Background(), state); action != multistep.ActionHalt {
		t.Fatalf("expected ActionHalt, got %v", action)
	}
	step.Cleanup(state)

	if err, _ := state.Get("error").(error); err == nil || !strings.Contains(err.Error(), "unexpected phase 'paused_system'") {
		t.Fatalf("unexpected error: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.cancelled) != 1 || f.puts != 0 || len(f.disks) != 0 {
		t.Fatalf("expected a cancelled transfer and no disk: cancelled=%v puts=%d disks=%d", f.cancelled, f.puts, len(f.disks))
	}
}
