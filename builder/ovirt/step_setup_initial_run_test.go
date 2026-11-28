package ovirt

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

// findImageOnFakeEngine runs findImage against a fake engine that answers the
// disk search with the given XML. It returns the result and the search query
// the engine received.
func findImageOnFakeEngine(t *testing.T, name, disksXML string) (id string, search string, err error) {
	t.Helper()
	var mu sync.Mutex
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch path := strings.TrimPrefix(r.URL.Path, "/ovirt-engine/api"); {
		case r.URL.Path == "/ovirt-engine/sso/oauth/token":
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"access_token":"token"}`)
		case path == "":
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(w, `<api></api>`)
		case path == "/disks":
			mu.Lock()
			search = r.URL.Query().Get("search")
			mu.Unlock()
			w.Header().Set("Content-Type", "application/xml")
			_, _ = fmt.Fprint(w, disksXML)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL + "/ovirt-engine/api")
	if err != nil {
		t.Fatal(err)
	}
	config := &Config{}
	config.ovirtURL = u
	config.Username = "admin@internal"
	config.Password = "password"
	config.SkipCertValidation = true

	conn, err := ovirtConnect(config, new(multistep.BasicStateBag))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()

	id, err = (&stepSetupInitialRun{}).findImage(conn, name)
	mu.Lock()
	defer mu.Unlock()
	return id, search, err
}

func TestFindImage_QuotesName(t *testing.T) {
	id, search, err := findImageOnFakeEngine(t, "debian 13 DVD.iso",
		`<disks><disk id="d1"><name>debian 13 DVD.iso</name></disk></disks>`)
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}
	if id != "d1" {
		t.Fatalf("expected disk d1, got %q", id)
	}
	if want := `disk_content_type=iso and name="debian 13 DVD.iso"`; search != want {
		t.Fatalf("expected search %q, got %q", want, search)
	}
}

func TestFindImage_NotFound(t *testing.T) {
	_, _, err := findImageOnFakeEngine(t, "missing.iso", `<disks></disks>`)
	if err == nil || !strings.Contains(err.Error(), "could not find ISO image 'missing.iso'") {
		t.Fatalf("expected a not-found error, got: %v", err)
	}
}

func TestFindImage_SeveralMatches(t *testing.T) {
	_, _, err := findImageOnFakeEngine(t, "debian.iso",
		`<disks><disk id="d1"><name>debian.iso</name></disk><disk id="d2"><name>debian.iso</name></disk></disks>`)
	if err == nil {
		t.Fatal("expected an error when several ISO images share the name")
	}
	for _, want := range []string{"found 2 ISO images", "d1", "d2"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("expected the error to contain %q, got: %s", want, err)
		}
	}
}

func TestFindImage_RejectsDoubleQuote(t *testing.T) {
	_, search, err := findImageOnFakeEngine(t, `bad"name.iso`, `<disks></disks>`)
	if err == nil || !strings.Contains(err.Error(), "double quotes") {
		t.Fatalf("expected an invalid-name error, got: %v", err)
	}
	if search != "" {
		t.Fatalf("expected no search request, got %q", search)
	}
}
