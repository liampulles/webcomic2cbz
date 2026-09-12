package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"
	"time"

	"github.com/rs/zerolog/log"
)

type Source interface {
	Name() string
	FetchConcurrency() int
	Available() ([]int, error)
	Fetch(int) (string, error)
}

type sourceSetupFn func(SourceConfig, Config, string) (bool, Source, error)

var sourceSetups []sourceSetupFn = []sourceSetupFn{
	tryHTTPDirectSource,
	tryImgFilesSource,
	tryHTMLTemplateScan,
}

// --- HTTPDirectSource

type HTTPDirectSource struct {
	LatestRuleSourcer

	imgDir           string
	homepage         string
	urlTemplate      *template.Template
	basenameTemplate *template.Template
}

func tryHTTPDirectSource(srcCfg SourceConfig, cfg Config, dir string) (bool, Source, error) {
	if srcCfg.Httpdirect.URLFormat == "" {
		return false, nil, nil
	}

	latestRule, err := newLatestRule(srcCfg.Httpdirect.LatestRule, cfg)
	if err != nil {
		return true, nil, err
	}

	urlTemplate, err := template.New("url").Parse(srcCfg.Httpdirect.URLFormat)
	if err != nil {
		return true, nil, fmt.Errorf("url template: %w", err)
	}

	basenameTemplate, err := template.New("basename").Parse(srcCfg.Httpdirect.BasenameFormat)
	if err != nil {
		return true, nil, fmt.Errorf("basename format: %w", err)
	}

	source := &HTTPDirectSource{
		LatestRuleSourcer: LatestRuleSourcer{latestRule},
		imgDir:            dir,
		homepage:          cfg.Homepage,
		urlTemplate:       urlTemplate,
		basenameTemplate:  basenameTemplate,
	}
	return true, source, nil
}

var _ Source = &HTTPDirectSource{}

func (h *HTTPDirectSource) Name() string {
	return "HTTPDirectSource"
}

func (h *HTTPDirectSource) FetchConcurrency() int {
	return 20
}

func (h *HTTPDirectSource) Fetch(idx int) (string, error) {
	// Figure out url
	var b strings.Builder
	data := struct {
		Idx int
	}{
		Idx: idx,
	}
	err := h.urlTemplate.Execute(&b, data)
	if err != nil {
		return "", err
	}
	url := b.String()

	// Figure out path
	var b2 strings.Builder
	err = h.basenameTemplate.Execute(&b2, data)
	if err != nil {
		return "", err
	}
	base := b2.String()
	path := filepath.Join(h.imgDir, base)

	err = fetchHTTPImg(url, path)
	if err != nil {
		return "", err
	}

	return path, nil
}

func fetchHTTPImg(url string, path string) error {
	// Download to a temp file
	tmpPath, err := getTempFile(url)
	if err != nil {
		return err
	}

	// Move it to the comic dir with an appropriate name
	// (so it can potentially be used by image source)

	// Create a file
	of, err := os.Create(path)
	if err != nil {
		return err
	}
	defer of.Close()

	// Open the temporary file
	inf, err := os.Open(tmpPath)
	if err != nil {
		return err
	}
	defer inf.Close()

	// Copy
	_, err = io.Copy(of, inf)
	if err != nil {
		return err
	}

	// Delete the temp file (but don't fail if we can't)
	os.Remove(tmpPath)

	log.Info().
		Str("path", path).
		Msg("fetch succeeded.")
	return nil
}

// --- HTMLTemplateScan

type HTMLTemplateScan struct {
	LatestRuleSourcer

	imgDir           string
	homepage         string
	urlTemplate      *template.Template
	basenameTemplate *template.Template
	imgRe            *regexp.Regexp
}

func tryHTMLTemplateScan(srcCfg SourceConfig, cfg Config, dir string) (bool, Source, error) {
	if srcCfg.Htmltemplatescan.URLFormat == "" {
		return false, nil, nil
	}

	latestRule, err := newLatestRule(srcCfg.Htmltemplatescan.LatestRule, cfg)
	if err != nil {
		return true, nil, err
	}

	urlTemplate, err := template.New("url").Parse(srcCfg.Htmltemplatescan.URLFormat)
	if err != nil {
		return true, nil, fmt.Errorf("url template: %w", err)
	}

	basenameTemplate, err := template.New("basename").Parse(srcCfg.Htmltemplatescan.BasenameFormat)
	if err != nil {
		return true, nil, fmt.Errorf("basename format: %w", err)
	}

	imgRe, err := regexp.Compile(srcCfg.Htmltemplatescan.ImgRegex)
	if err != nil {
		return true, nil, fmt.Errorf("img regex: %w", err)
	}

	source := &HTMLTemplateScan{
		LatestRuleSourcer: LatestRuleSourcer{latestRule},
		imgDir:            dir,
		homepage:          cfg.Homepage,
		urlTemplate:       urlTemplate,
		basenameTemplate:  basenameTemplate,
		imgRe:             imgRe,
	}
	return true, source, nil
}

var _ Source = &HTMLTemplateScan{}

func (h *HTMLTemplateScan) Name() string {
	return "HTMLTemplateScan"
}

func (h *HTMLTemplateScan) FetchConcurrency() int {
	return 20
}

func (h *HTMLTemplateScan) Fetch(idx int) (string, error) {
	// Figure out html page url
	var b strings.Builder
	data := struct {
		Idx int
	}{
		Idx: idx,
	}
	err := h.urlTemplate.Execute(&b, data)
	if err != nil {
		return "", err
	}
	htmlURL := b.String()

	// Resolve comic http from regex
	htmlBytes, err := getBytes(htmlURL)
	if err != nil {
		return "", err
	}
	matches := h.imgRe.FindSubmatch(htmlBytes)
	if matches == nil {
		return "", fmt.Errorf("img regex: %w", err)
	}
	url := string(matches[1])

	// Figure out file path
	var b2 strings.Builder
	err = h.basenameTemplate.Execute(&b2, data)
	if err != nil {
		return "", err
	}
	base := b2.String()
	path := filepath.Join(h.imgDir, base)

	// Download
	err = fetchHTTPImg(url, path)
	if err != nil {
		return "", err
	}

	return path, nil
}

// --- ImgFilesSource

type ImgFilesSource struct {
	imgDir           string
	basenameGlob     string
	basenameTemplate *template.Template
	idxRegex         *regexp.Regexp
}

func tryImgFilesSource(srcCfg SourceConfig, cfg Config, dir string) (bool, Source, error) {
	if srcCfg.Imgfiles.BasenameFormat == "" {
		return false, nil, nil
	}

	basenameTemplate, err := template.New("basename").Parse(srcCfg.Imgfiles.BasenameFormat)
	if err != nil {
		return true, nil, fmt.Errorf("basename format: %w", err)
	}

	idxRegex, err := regexp.Compile(srcCfg.Imgfiles.IdxRegex)
	if err != nil {
		return true, nil, fmt.Errorf("regexp format: %w", err)
	}

	source := &ImgFilesSource{
		imgDir:           dir,
		basenameGlob:     srcCfg.Imgfiles.BasenameGlob,
		basenameTemplate: basenameTemplate,
		idxRegex:         idxRegex,
	}

	return true, source, nil
}

var _ Source = &ImgFilesSource{}

func (i *ImgFilesSource) Name() string {
	return "ImgFilesSource"
}

func (i *ImgFilesSource) FetchConcurrency() int {
	return 5
}

func (i *ImgFilesSource) Available() ([]int, error) {
	// Glob the image directory
	glob := filepath.Join(i.imgDir, i.basenameGlob)
	found, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("glob: %w", err)
	}

	// Keep the base names
	bases := make([]string, len(found))
	for i, item := range found {
		bases[i] = strings.TrimSuffix(filepath.Base(item), filepath.Ext(item))
	}

	// Filter by regex, getting idx part
	var filtered []string
	for _, base := range bases {
		match := i.idxRegex.FindStringSubmatch(base)
		if match != nil {
			filtered = append(filtered, match[1])
		}
	}

	// Convert idx part to int
	available := make([]int, len(filtered))
	for i, idxStr := range filtered {
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return nil, fmt.Errorf("strconv: the idx_regex in your config is not selecting integers properly, correct it")
		}
		available[i] = idx
	}

	return available, nil
}

func (i *ImgFilesSource) Fetch(idx int) (string, error) {
	// Template basename
	var b strings.Builder
	data := struct {
		Idx int
	}{
		Idx: idx,
	}
	err := i.basenameTemplate.Execute(&b, data)
	if err != nil {
		return "", fmt.Errorf("basename tmpl: %w", err)
	}
	base := b.String()

	// Double check the file exists.
	path := filepath.Join(i.imgDir, base)
	_, err = os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("file problem: %s - %w", path, err)
	}

	return path, nil
}

// --- LatestRule

type LatestRule interface {
	Latest() (int, error)
}

type latestRuleSetupFn func(LatestRuleConfig, Config) (bool, LatestRule, error)

var latestRuleSetups []latestRuleSetupFn = []latestRuleSetupFn{
	tryHomepageRegexLatestRule,
}

type LatestRuleSourcer struct {
	rule LatestRule
}

func (l LatestRuleSourcer) Available() ([]int, error) {
	latest, err := l.rule.Latest()
	if err != nil {
		return nil, err
	}

	available := make([]int, latest)
	// We assume webcomics start at idx 1.
	for i := 1; i <= latest; i++ {
		available[i-1] = i
	}
	return available, nil
}

func newLatestRule(latestConfig LatestRuleConfig, cfg Config) (LatestRule, error) {
	// Try each
	for _, setup := range latestRuleSetups {
		applies, rule, err := setup(latestConfig, cfg)
		if !applies {
			continue
		}
		if err != nil {
			return nil, err
		}
		return rule, nil
	}

	return nil, errors.New("no latest rule configured- adjust config.")
}

// --- HomepageRegexLatestRule

type HomepageRegexLatestRule struct {
	homepage string
	re       *regexp.Regexp
}

var _ LatestRule = &HomepageRegexLatestRule{}

func (r *HomepageRegexLatestRule) Latest() (int, error) {
	bytes, err := getBytes(r.homepage)
	if err != nil {
		return 0, err
	}

	found := r.re.FindSubmatch(bytes)
	if found == nil {
		return 0, errors.New("homepage regex: no match found, check your config")
	}

	latest, err := strconv.Atoi(string(found[1]))
	if err != nil {
		return 0, errors.New("homepage regex: not selecting an integer, check your config")
	}

	return latest, nil
}

func tryHomepageRegexLatestRule(latestConfig LatestRuleConfig, cfg Config) (bool, LatestRule, error) {
	if latestConfig.HomepageRegex == "" {
		return false, nil, nil
	}

	re, err := regexp.Compile(latestConfig.HomepageRegex)
	if err != nil {
		return true, nil, fmt.Errorf("homepage regex: %w", err)
	}

	rule := &HomepageRegexLatestRule{
		homepage: cfg.Homepage,
		re:       re,
	}
	return true, rule, nil
}

// --- General

type Sourced struct {
	Idx  int
	Path string
}

func SourceWebcomics(cfg Config, existing []int, dir string) []Sourced {
	found := make([]int, len(existing))
	copy(found, existing)
	sources := resolveSources(cfg, dir)
	var sourced []Sourced

	// Go in prescribed order of sources...
	for _, source := range sources {
		// See what is available for this source
		available, err := source.Available()
		if err != nil {
			log.Err(err).
				Str("source", source.Name()).
				Msg("available failed - going to next source.")
			continue
		}

		// What are we missing that the source has?
		missing := Diff(available, found)

		// Try and fetch what we can.
		someSourced, _ := DoConcurrentProcess(source.FetchConcurrency(), missing, func(idx int) (Sourced, error) {
			path, err := source.Fetch(idx)
			if err != nil {
				log.Err(err).
					Str("source", source.Name()).
					Int("idx", idx).
					Msg("fetch failed - skipping this image.")
				return Sourced{}, err
			}
			return Sourced{Idx: idx, Path: path}, err
		})
		for _, item := range someSourced {
			found = append(found, item.Idx)
			sourced = append(sourced, item)
		}
	}
	return sourced
}

func resolveSources(cfg Config, dir string) []Source {
	var sources []Source
	for _, sourceCfg := range cfg.Source {
		for _, fn := range sourceSetups {
			isInCfg, source, err := fn(sourceCfg, cfg, dir)
			if !isInCfg {
				continue
			}
			if err != nil {
				log.Err(err).
					Str("dir", dir).
					Msg("invalid config detected - will skip sourcing for this webcomic.")
				return nil
			}
			sources = append(sources, source)
			break
		}
	}
	return sources
}

// Returns base - sub, in set terms
func Diff[T comparable](base, sub []T) []T {
	subSet := make(map[T]bool, len(sub))
	for _, item := range sub {
		subSet[item] = true
	}

	seen := make(map[T]bool, len(base))
	var diff []T
	for _, item := range base {
		if seen[item] {
			continue
		}
		seen[item] = true
		if !subSet[item] {
			diff = append(diff, item)
		}
	}

	return diff
}

var client = &http.Client{
	Timeout: 15 * time.Second,
}

func prepGet(url string) (*http.Request, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}

	req.Header.Set("User-Agent", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "en-ZA,en;q=0.9")
	req.Header.Set("Upgrade-Insecure-Requests", "1")
	req.Header.Set("Sec-Fetch-Dest", "document")
	req.Header.Set("Sec-Fetch-Mode", "navigate")
	req.Header.Set("Sec-Fetch-Site", "none")
	req.Header.Set("Sec-Fetch-User", "?1")

	return req, nil
}

func getBytes(url string) ([]byte, error) {
	req, err := prepGet(url)
	if err != nil {
		return nil, err
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, fmt.Errorf("http: %d", resp.StatusCode)
	}

	bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("http read: %w", err)
	}

	return bytes, nil
}

func getTempFile(url string) (string, error) {
	req, err := prepGet(url)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("http: %w", err)
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", fmt.Errorf("http: %d", resp.StatusCode)
	}

	err = os.MkdirAll(httpDirectTempDir(), 0755)
	if err != nil {
		return "", fmt.Errorf("temp dir create: %w", err)
	}

	f, err := os.CreateTemp(httpDirectTempDir(), "")
	if err != nil {
		return "", fmt.Errorf("temp file create: %w", err)
	}
	defer f.Close()

	_, err = io.Copy(f, resp.Body)
	if err != nil {
		return "", fmt.Errorf("temp copy: %w", err)
	}

	return f.Name(), nil
}

func httpDirectTempDir() string {
	return filepath.Join(os.TempDir(), "webcomic2cbz_httpdirect")
}
