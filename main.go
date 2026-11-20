// gesso deploys a directory of static files to a Cloudflare Worker.
//
//	gesso -name example-site ./public
//
// Assets are addressed by content, so a second deploy of unchanged files
// uploads nothing. No config file, no state, no bundling.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	apiBase   = "https://api.cloudflare.com/client/v4"
	configDir = ".config/gesso"
	maxFile   = 25 << 20
	maxFiles  = 20000
)

// neverPartOfTheSite covers build and dependency directories that no static
// site serves. Anything else is the site's own business: list it in .siteignore.
var neverPartOfTheSite = []string{"__pycache__", "node_modules", "*.pyc"}

type asset struct {
	rel   string // "/index.html", as it appears in the manifest
	local string
	size  int64
	key   string
	ctype string
}

func main() {
	name := flag.String("name", "", "the Worker to deploy to")
	dry := flag.Bool("dry-run", false, "print what would be uploaded and exit")
	compat := flag.String("compat", time.Now().UTC().Format("2006-01-02"), "compatibility_date sent with the deploy")
	flag.Usage = func() {
		fmt.Fprint(os.Stderr, `usage: gesso -name <worker> [-dry-run] [-compat YYYY-MM-DD] <directory>

Deploys <directory> to the Cloudflare Worker <worker> as static assets. Files
are addressed by content, so a second deploy of unchanged files uploads
nothing. No config file, no state, no bundling.

flags:
`)
		flag.PrintDefaults()
		fmt.Fprint(os.Stderr, `
credentials, first found wins:
  CLOUDFLARE_API_TOKEN   or  ~/.config/gesso/token       (needs Workers Scripts:Edit)
  CLOUDFLARE_ACCOUNT_ID  or  ~/.config/gesso/account-id

ignored: dotfiles and dot-directories, __pycache__, node_modules, *.pyc, and one
path.Match pattern per line in <directory>/.siteignore. A top-level _headers is
sent to Workers as configuration, not published as a file. A top-level
_redirects is dropped with a warning: no redirect config is sent.

refused: a directory holding functions/, _worker.js or _routes.json needs a
Worker script; gesso serves static assets only.

examples:
  gesso -name example-site ./public
  gesso -name example-site -dry-run ./public
  gesso -name example-site -compat 2026-11-20 ./public
`)
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if *name == "" {
		fatal("missing -name: the Worker to deploy to")
	}
	root, err := filepath.Abs(flag.Arg(0))
	if err != nil {
		fatal(err.Error())
	}
	assets, skipped, headers, err := collect(root)
	if err != nil {
		fatal(err.Error())
	}
	if len(assets) == 0 {
		fatal("nothing to deploy in " + root)
	}

	if *dry {
		var total int64
		for _, a := range assets {
			total += a.size
		}
		fmt.Printf("%d files, %d bytes -> worker %s\n", len(assets), total, *name)
		for _, a := range assets {
			fmt.Printf("  %-24s %s %9d  %s\n", a.rel, a.key, a.size, a.ctype)
		}
		if headers != "" {
			fmt.Printf("  _headers %d bytes of config\n", len(headers))
		}
		for _, s := range skipped {
			fmt.Printf("  skipped %s\n", s)
		}
		return
	}

	token, err := credential("CLOUDFLARE_API_TOKEN", "token")
	if err != nil {
		fatal(err.Error())
	}
	account, err := credential("CLOUDFLARE_ACCOUNT_ID", "account-id")
	if err != nil {
		fatal(err.Error())
	}
	uploaded, err := newClient(account, token).deploy(*name, assets, headers, *compat)
	if err != nil {
		fatal(err.Error())
	}
	fmt.Printf("deployed %d files (%d uploaded) to %s\n", len(assets), uploaded, *name)
}

// collect walks root and returns the files to publish, the ones deliberately
// left out, and the contents of a top-level _headers, which Workers takes as
// config rather than as a file.
func collect(root string) ([]asset, []string, string, error) {
	info, err := os.Stat(root)
	if err != nil {
		return nil, nil, "", err
	}
	if !info.IsDir() {
		return nil, nil, "", fmt.Errorf("%s is not a directory", root)
	}
	for _, needsWorker := range []string{"functions", "_worker.js", "_routes.json"} {
		if _, err := os.Stat(filepath.Join(root, needsWorker)); err == nil {
			return nil, nil, "", fmt.Errorf("%s means this site needs a Worker script; gesso serves static files only", needsWorker)
		}
	}
	headers := ""
	if b, err := os.ReadFile(filepath.Join(root, "_headers")); err == nil {
		headers = string(b)
	}
	if _, err := os.Stat(filepath.Join(root, "_redirects")); err == nil {
		fmt.Fprintln(os.Stderr, "gesso: _redirects is not sent to Workers; its rules will not apply")
	}
	ignore, err := ignorePatterns(root)
	if err != nil {
		return nil, nil, "", err
	}

	var assets []asset
	var skipped []string
	err = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		if ignored(rel, ignore) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			skipped = append(skipped, rel)
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if rel == "_headers" || rel == "_redirects" {
			return nil
		}
		content, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if len(content) > maxFile {
			return fmt.Errorf("%s is %d bytes, over the 25 MiB asset limit", rel, len(content))
		}
		if len(assets) == maxFiles {
			return fmt.Errorf("more than %d files", maxFiles)
		}
		assets = append(assets, asset{
			rel:   "/" + rel,
			local: p,
			size:  int64(len(content)),
			key:   assetKey(p, content),
			ctype: contentType(p),
		})
		return nil
	})
	if err != nil {
		return nil, nil, "", err
	}
	return assets, skipped, headers, nil
}

// ignorePatterns is the built-in list plus one pattern per line from
// .siteignore. Patterns use path.Match syntax and are tried against the whole
// relative path and against the base name.
func ignorePatterns(root string) ([]string, error) {
	patterns := append([]string{}, neverPartOfTheSite...)
	b, err := os.ReadFile(filepath.Join(root, ".siteignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return patterns, nil
		}
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

func ignored(rel string, patterns []string) bool {
	for _, part := range strings.Split(rel, "/") {
		if strings.HasPrefix(part, ".") {
			return true
		}
	}
	base := path.Base(rel)
	for _, p := range patterns {
		if ok, _ := path.Match(p, rel); ok {
			return true
		}
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}

var contentTypeOverride = map[string]string{
	".md":          "text/markdown; charset=utf-8",
	".txt":         "text/plain; charset=utf-8",
	".typ":         "text/plain; charset=utf-8",
	".webmanifest": "application/manifest+json",
}

func contentType(file string) string {
	ext := strings.ToLower(filepath.Ext(file))
	if ct, ok := contentTypeOverride[ext]; ok {
		return ct
	}
	if ct := mime.TypeByExtension(ext); ct != "" {
		return ct
	}
	return "application/octet-stream"
}

// assetKey is the content-addressed name the asset store files a blob under.
// Cloudflare's own client uses BLAKE3 over base64(contents)+extension, cut to
// 32 hex chars. The server keys on the string alone, so any digest works as
// long as it is stable and per-extension.
//
// braid: sha256 keys, swap to blake3 when a deploy proves the API rejects them.
func assetKey(name string, content []byte) string {
	h := sha256.New()
	h.Write([]byte(base64.StdEncoding.EncodeToString(content)))
	h.Write([]byte(strings.TrimPrefix(filepath.Ext(name), ".")))
	return hex.EncodeToString(h.Sum(nil))[:32]
}

// credential reads from the environment, then from ~/.config/gesso.
func credential(env, file string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(env)); v != "" {
		return v, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(home, configDir, file)
	b, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("set %s or write %s", env, path)
	}
	if v := strings.TrimSpace(string(b)); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("%s is empty", path)
}

type client struct {
	account string
	token   string
	http    *http.Client
}

func newClient(account, token string) *client {
	return &client{account: account, token: token, http: &http.Client{Timeout: 2 * time.Minute}}
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  []apiError      `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

func (c *client) call(method, apiPath string, body io.Reader, ctype string) (json.RawMessage, error) {
	return c.callAs(c.token, method, apiPath, body, ctype)
}

// callAs sends with the given bearer token. Asset uploads authenticate with the
// short-lived upload token from the session, not the account API token.
func (c *client) callAs(token, method, apiPath string, body io.Reader, ctype string) (json.RawMessage, error) {
	req, err := http.NewRequest(method, apiBase+apiPath, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("%s %s: %s: %s", method, apiPath, resp.Status, snippet(raw))
	}
	if !env.Success || resp.StatusCode >= 300 {
		var msgs []string
		for _, e := range env.Errors {
			msgs = append(msgs, fmt.Sprintf("%d %s", e.Code, e.Message))
		}
		if len(msgs) == 0 {
			msgs = append(msgs, snippet(raw))
		}
		return nil, fmt.Errorf("%s %s: %s", method, apiPath, strings.Join(msgs, "; "))
	}
	return env.Result, nil
}

func snippet(b []byte) string {
	if len(b) > 300 {
		b = b[:300]
	}
	return strings.TrimSpace(string(b))
}

type manifestEntry struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

type uploadSession struct {
	JWT     string     `json:"jwt"`
	Buckets [][]string `json:"buckets"`
}

// deploy uploads the assets the store is missing, then creates the Worker
// version that serves them. Returns the version id and how many files went up.
func (c *client) deploy(name string, assets []asset, headers, compat string) (int, error) {
	manifest := make(map[string]manifestEntry, len(assets))
	byKey := make(map[string]asset, len(assets))
	for _, a := range assets {
		manifest[a.rel] = manifestEntry{Hash: a.key, Size: a.size}
		byKey[a.key] = a
	}
	body, err := json.Marshal(map[string]any{"manifest": manifest})
	if err != nil {
		return 0, err
	}
	raw, err := c.call("POST",
		fmt.Sprintf("/accounts/%s/workers/scripts/%s/assets-upload-session", c.account, name),
		bytes.NewReader(body), "application/json")
	if err != nil {
		return 0, err
	}
	var session uploadSession
	if err := json.Unmarshal(raw, &session); err != nil {
		return 0, err
	}

	completion := session.JWT
	uploaded := 0
	for _, bucket := range session.Buckets {
		for _, key := range bucket {
			a, ok := byKey[key]
			if !ok {
				return uploaded, fmt.Errorf("server asked for an unknown hash %s", key)
			}
			jwt, err := c.upload(a, session.JWT)
			if err != nil {
				return uploaded, err
			}
			if jwt != "" {
				completion = jwt
			}
			uploaded++
			fmt.Printf("  uploaded %s\n", a.rel)
		}
	}
	if completion == "" {
		return uploaded, fmt.Errorf("upload finished with no completion token")
	}
	return uploaded, c.putScript(name, completion, headers, compat)
}

func (c *client) upload(a asset, jwt string) (string, error) {
	f, err := os.Open(a.local)
	if err != nil {
		return "", err
	}
	defer f.Close()
	raw, err := c.callAs(jwt, "POST", fmt.Sprintf("/accounts/%s/workers/assets/upload/%s", c.account, a.key), f, a.ctype)
	if err != nil {
		return "", err
	}
	var res struct {
		JWT string `json:"jwt"`
	}
	json.Unmarshal(raw, &res)
	return res.JWT, nil
}

// putScript creates the version. With no user script the whole payload is one
// metadata field: the asset token plus the optional _headers config.
func (c *client) putScript(name, jwt, headers, compat string) error {
	config := map[string]any{}
	if headers != "" {
		config["_headers"] = headers
	}
	metadata := map[string]any{
		"assets": map[string]any{"jwt": jwt, "config": config},
	}
	if compat != "" {
		metadata["compatibility_date"] = compat
	}
	meta, err := json.Marshal(metadata)
	if err != nil {
		return err
	}

	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	part, err := mw.CreateFormField("metadata")
	if err != nil {
		return err
	}
	if _, err := part.Write(meta); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}

	_, err = c.call("PUT", fmt.Sprintf("/accounts/%s/workers/scripts/%s", c.account, name), &buf, mw.FormDataContentType())
	return err
}

func fatal(msg string) {
	fmt.Fprintln(os.Stderr, "gesso: "+msg)
	os.Exit(1)
}
