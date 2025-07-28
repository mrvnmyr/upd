package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Struct for the .upd file
type UpdFile struct {
	Version int    `yaml:"upd.version"`
	URL     string `yaml:"url"`
}

// Walk upwards for .updignore, else current dir
func findProjectRoot() (string, error) {
	curDir, err := os.Getwd()
	if err != nil {
		return "", err
	}

	dir := curDir
	for {
		ignore := filepath.Join(dir, ".updignore")
		if _, err := os.Stat(ignore); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return curDir, nil // filesystem root reached, use current dir
		}
		dir = parent
	}
}

// fetchWithCache caches URLs by sha256(url).ext, respects ETag/Last-Modified if possible
func fetchWithCache(cacheDir, url string) (string, bool, error) {
	hash := sha256.Sum256([]byte(url))
	ext := filepath.Ext(url)
	if ext == "" || len(ext) > 8 {
		ext = ".dat"
	}
	cachePath := filepath.Join(cacheDir, hex.EncodeToString(hash[:])+ext)
	metaPath := cachePath + ".meta"

	// If cache exists, try conditional GET
	var etag, lastmod string
	if meta, err := os.ReadFile(metaPath); err == nil {
		lines := strings.Split(string(meta), "\n")
		for _, l := range lines {
			if strings.HasPrefix(l, "ETag: ") {
				etag = strings.TrimPrefix(l, "ETag: ")
			}
			if strings.HasPrefix(l, "Last-Modified: ") {
				lastmod = strings.TrimPrefix(l, "Last-Modified: ")
			}
		}
	}

	client := &http.Client{Timeout: 15 * time.Second}
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	if lastmod != "" {
		req.Header.Set("If-Modified-Since", lastmod)
	}

	resp, err := client.Do(req)
	if err != nil {
		// If we can't reach the server, use cache if available
		if _, statErr := os.Stat(cachePath); statErr == nil {
			return cachePath, true, nil
		}
		return "", false, err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		out, err := os.Create(cachePath)
		if err != nil {
			return "", false, err
		}
		_, err = io.Copy(out, resp.Body)
		out.Close()
		if err != nil {
			return "", false, err
		}
		etag := resp.Header.Get("ETag")
		lastmod := resp.Header.Get("Last-Modified")
		meta := fmt.Sprintf("ETag: %s\nLast-Modified: %s\n", etag, lastmod)
		_ = os.WriteFile(metaPath, []byte(meta), 0o644)
		return cachePath, false, nil
	case http.StatusNotModified:
		// Use cache
		return cachePath, true, nil
	default:
		return "", false, fmt.Errorf("http error: %s", resp.Status)
	}
}

// Reads/parses .upd, fetches and caches content, compares with basefile, updates if changed
func updateFile(projectRoot, updPath string) error {
	updData, err := os.ReadFile(updPath)
	if err != nil {
		return fmt.Errorf("reading .upd file: %w", err)
	}
	var upd UpdFile
	if err := yaml.Unmarshal(updData, &upd); err != nil {
		return fmt.Errorf("parsing .upd file: %w", err)
	}
	if upd.Version == 0 {
		return errors.New("every .upd file must set a non-zero 'upd.version' field")
	}
	if upd.URL == "" {
		return errors.New("no url field in .upd file")
	}

	// determine cacheDir
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("could not determine home directory: %w", err)
	}
	cacheDir := filepath.Join(home, ".cache", "upd", "urlcache")

	os.MkdirAll(cacheDir, 0o755)
	cachePath, cacheHit, err := fetchWithCache(cacheDir, upd.URL)
	if err != nil {
		return fmt.Errorf("fetching %s: %w", upd.URL, err)
	}

	// Figure out basefile (strip last .upd from filename)
	basefile := strings.TrimSuffix(updPath, ".upd")

	// Compare
	urlContent, err := os.ReadFile(cachePath)
	if err != nil {
		return fmt.Errorf("reading cache: %w", err)
	}
	baseContent, _ := os.ReadFile(basefile) // ignore error, treat as empty if not exists

	if string(urlContent) == string(baseContent) {
		fmt.Printf("%s already up to date (cache hit: %v)\n", basefile, cacheHit)
		return nil
	}

	// Update
	if err := os.WriteFile(basefile, urlContent, 0o644); err != nil {
		return fmt.Errorf("updating %s: %w", basefile, err)
	}
	fmt.Printf("Updated %s\n", basefile)
	return nil
}

func main() {
	projectRoot, err := findProjectRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding project root: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Project root: %s\n", projectRoot)

	err = filepath.WalkDir(projectRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".upd") {
			absPath, err := filepath.Abs(path)
			if err != nil {
				return err
			}
			return updateFile(projectRoot, absPath)
		}
		return nil
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Walk error: %v\n", err)
		os.Exit(1)
	}
}
