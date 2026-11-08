package syncer

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var client = &http.Client{Timeout: 10 * time.Minute}

// backoff between attempts on network errors and 429/5xx answers.
var backoff = []time.Duration{1 * time.Second, 3 * time.Second}

// get fetches url. A github.com release download that keeps failing is fetched through the API
// asset endpoint instead, which is served separately and kept working when the release CDN
// answered 504 (issue #8). After one such failure the rest of the run goes straight to the API;
// if the API fails too (anonymous limit: 60 calls/hour), the normal link is tried again.
func get(url string) ([]byte, int, error) {
	repo, tag, name, isRelease := splitReleaseURL(url)
	if !isRelease || !cdnDown() {
		b, code, err := getRetry(url, "")
		if err == nil || code == 404 || !isRelease {
			return b, code, err
		}
		if code == 0 || code >= 500 {
			setCDNDown()
		}
	}
	api, err := assetAPIURL(repo, tag, name)
	if err == errNoAsset {
		return nil, 404, fmt.Errorf("%s: HTTP 404", url)
	}
	var b []byte
	code := 0
	if err == nil {
		if b, code, err = getRetry(api, "application/octet-stream"); err == nil {
			return b, code, nil
		}
	}
	// API unusable (e.g. the anonymous 60/hour limit): try the normal link again instead of stopping
	if b2, code2, err2 := getRetry(url, ""); err2 == nil {
		fetchMu.Lock()
		cdnFailed = false
		fetchMu.Unlock()
		return b2, code2, nil
	}
	return nil, code, err
}

var (
	fetchMu    sync.Mutex
	cdnFailed  bool
	assetLists = map[string]map[string]string{} // repo@tag -> asset name -> API url
)

func cdnDown() bool {
	fetchMu.Lock()
	defer fetchMu.Unlock()
	return cdnFailed
}

func setCDNDown() {
	fetchMu.Lock()
	cdnFailed = true
	fetchMu.Unlock()
}

func getRetry(url, accept string) (b []byte, code int, err error) {
	for i := 0; ; i++ {
		b, code, err = getOnce(url, accept, i > 0)
		if err == nil || !(code == 0 || code == 429 || code >= 500) || i >= len(backoff) {
			return
		}
		time.Sleep(backoff[i])
	}
}

func getOnce(url, accept string, retry bool) ([]byte, int, error) {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return nil, -1, err
	}
	req.Header.Set("User-Agent", "lichii-sync")
	if accept != "" {
		req.Header.Set("Accept", accept)
	}
	if retry { // don't get a cached error page back from a proxy
		req.Header.Set("Cache-Control", "no-cache")
		req.Header.Set("Pragma", "no-cache")
	}
	api := strings.HasPrefix(url, "https://api.github.com/")
	if api { // optional: a token lifts the API's 60 requests/hour limit
		if t := firstEnv("GITHUB_TOKEN", "GH_TOKEN"); t != "" {
			req.Header.Set("Authorization", "Bearer "+t)
		}
	}
	r, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer r.Body.Close()
	if r.StatusCode != 200 {
		err := fmt.Errorf("%s: HTTP %d", url, r.StatusCode)
		if api && (r.StatusCode == 403 || r.StatusCode == 429) {
			err = fmt.Errorf("%v - GitHub API limit reached (60/hour), try again later or set GITHUB_TOKEN", err)
		}
		return nil, r.StatusCode, err
	}
	b, err := ioutil.ReadAll(r.Body)
	if err != nil {
		return nil, 0, err // cut-off body: retryable
	}
	return b, 200, nil
}

func firstEnv(names ...string) string {
	for _, n := range names {
		if v := os.Getenv(n); v != "" {
			return v
		}
	}
	return ""
}

// splitReleaseURL parses https://github.com/<owner>/<repo>/releases/download/<tag>/<name>.
func splitReleaseURL(url string) (repo, tag, name string, ok bool) {
	const pre = "https://github.com/"
	if !strings.HasPrefix(url, pre) {
		return
	}
	p := strings.Split(strings.TrimPrefix(url, pre), "/")
	if len(p) != 6 || p[2] != "releases" || p[3] != "download" {
		return
	}
	return p[0] + "/" + p[1], p[4], p[5], true
}

// assetAPIURL looks up the API download URL of one release asset (one API call per release).
func assetAPIURL(repo, tag, name string) (string, error) {
	fetchMu.Lock()
	m, ok := assetLists[repo+"@"+tag]
	fetchMu.Unlock()
	if !ok {
		b, _, err := getRetry("https://api.github.com/repos/"+repo+"/releases/tags/"+tag, "")
		if err != nil {
			return "", err
		}
		var rel struct {
			Assets []struct {
				Name string `json:"name"`
				URL  string `json:"url"`
			} `json:"assets"`
		}
		if err := json.Unmarshal(b, &rel); err != nil {
			return "", err
		}
		m = map[string]string{}
		for _, a := range rel.Assets {
			m[a.Name] = a.URL
		}
		fetchMu.Lock()
		assetLists[repo+"@"+tag] = m
		fetchMu.Unlock()
	}
	if u, ok := m[name]; ok {
		return u, nil
	}
	return "", errNoAsset
}

var errNoAsset = fmt.Errorf("no such release file")
