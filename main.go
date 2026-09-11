package main

import (
	"archive/zip"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	"github.com/rs/zerolog/log"
)

// --- Regex

var configRegex = regexp.MustCompile(`^webcomic2cbz\.(?:yml|yaml)$`)
var cbzRegex = regexp.MustCompile(`^.*\.cbz$`)
var idxImageRegex = regexp.MustCompile(`^(\d+)\.(?:jpg|png|jpeg|gif|bmp|webp)$`)

// --- Lock keys

func cbzDirLockKey(dir string) string {
	return filepath.Join(dir, "cbz-dir.lock")
}

func cbzFileLockKey(cbzPath string) string {
	return fmt.Sprintf("%s.lock", cbzPath)
}

// --- Main logic

func main() {
	// Get current working dir
	root, err := os.Getwd()
	if err != nil {
		panic(fmt.Errorf("could not determine working dir: %w", err))
	}

	// Look for config files
	log.Info().Str("root", root).Msg("scanning for config...")
	found, err := findFilesRecursive(root, configRegex)
	if err != nil {
		log.Err(err).Str("root", root).Msg("error encountered whilst scanning - continuing.")
	}

	// Handle concurrently
	var jobs []Job
	for _, path := range found {
		jobs = append(jobs, configProcessJob(path))
	}
	DoConcurrent(5, jobs)
}

// Responsible for reading and parsing a found config file, and looking for relevant cbz
// files in the enclosed directory.
func configProcessJob(cfgPath string) Job {
	return func() {
		// Read the config
		log.Info().Str("path", cfgPath).Msg("reading config...")
		cfg, err := ParseConfig(cfgPath)
		if err != nil {
			log.Err(err).Str("path", cfgPath).Msg("invalid config or bad file perms - skipping.")
			return
		}

		// Scan for matching CBZ files and upsert ComicInfo for them. This needs to lock cbzs
		// for the dir.
		cfgDir := path.Dir(cfgPath)
		all, err := os.ReadDir(cfgDir)
		if err != nil {
			log.Err(err).Str("path", cfgPath).Msg("could not read cfg dir - continuing.")
		}
		cbzItems, err := filterMatchingCBZ(cfgDir, cfg, all)
		if err != nil {
			return
		}

		// Update the ComicInfo.xml for the existing CBZs associated to a webcomic, and also
		// track what webcomics we actually have.
		cbzImgIdxs, err := DoConcurrentProcess(5, cbzItems, func(cbz cbzItem) ([]int, error) {
			imgIdxs, err := comicInfoUpsertSingle(cbz.Path, cbz.Number, cfg)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", cbz.Path, err)
			}
			return imgIdxs, nil
		})
		if err != nil {
			log.Err(err).Str("path", cfgPath).Msg("could not read some cbzs - skipping webcomic.")
			return
		}
		var allIdx []int
		for _, imgIdxs := range cbzImgIdxs {
			allIdx = append(allIdx, imgIdxs...)
		}

		// Source missing webcomics
		sourced := SourceWebcomics(cfg, allIdx, cfgDir)
		log.Info().Interface("sourced", sourced).Msg("sourced some images")
	}
}

// --- Helper funcs

func comicInfoUpsertSingle(cbzPath string, number int, cfg Config) ([]int, error) {
	// Read the cbz
	log.Info().Str("path", cbzPath).Msg("reading cbz...")
	imageIdx, err := readCBZImageIdx(cbzPath)
	if err != nil {
		return nil, err
	}

	// Update ComicInfo
	info := CreateComicInfo(cfg, number)
	err = UpsertComicInfo(cbzPath, info)
	if err != nil {
		return nil, err
	}

	return imageIdx, nil
}

// Locking is up to the caller.
func readCBZImageIdx(cbzPath string) ([]int, error) {
	// Open the CBZ file
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	// Find embedded images, with an idx like title
	var found []int
	for _, f := range zr.File {
		// Filter out irrelevant listings
		if f.FileInfo().IsDir() {
			continue
		}
		if !idxImageRegex.MatchString(f.Name) {
			continue
		}

		// Get the idx part
		idxStr := idxImageRegex.FindStringSubmatch(f.Name)
		idx, err := strconv.Atoi(idxStr[1])
		if err != nil {
			return nil, err
		}
		found = append(found, idx)
	}

	return found, nil
}

type cbzItem struct {
	Path   string
	Number int
}

func filterMatchingCBZ(cfgDir string, cfg Config, all []os.DirEntry) ([]cbzItem, error) {
	// Generate hypothetical titles up to 999 volumes, as a match set.
	matchSet := make(map[string]int, 999)
	for i := 1; i < 1000; i++ {
		name, err := cbzTitle(cfgDir, cfg, i)
		if err != nil {
			return nil, err
		}
		matchSet[name] = i
	}

	// See which match
	var keep []cbzItem
	for _, entry := range all {
		if entry.IsDir() {
			continue
		}

		name := entry.Name()
		base := strings.TrimSuffix(name, path.Ext(name))
		number, ok := matchSet[base]
		if !ok {
			continue
		}
		keep = append(keep, cbzItem{path.Join(cfgDir, name), number})
	}

	return keep, nil
}

func cbzTitle(cfgDir string, cfg Config, volume int) (string, error) {
	// TODO: Could possibly cache built templates, if needed.
	// Define the template
	tmpl, err := template.New("cbz").Parse(cfg.Cbz.NamingTemplate)
	if err != nil {
		log.Err(err).
			Str("cfg_dir", cfgDir).
			Msg("invalid cbz.naming_template - please fix this. skipping this dir.")
		return "", err
	}

	// Build template data
	data := struct {
		Title  string
		Volume int
	}{
		Title:  cfg.Title,
		Volume: volume,
	}

	// Execute the template
	var b strings.Builder
	err = tmpl.Execute(&b, data)
	if err != nil {
		log.Err(err).
			Str("cfg_dir", cfgDir).
			Msg("invalid cbz.naming_template - please fix this. skipping this dir.")
		return "", err
	}

	return b.String(), nil
}

func findFilesRecursive(root string, nameRegex *regexp.Regexp) ([]string, error) {
	var found []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}

		// We got a match! Enqueue it.
		if nameRegex.MatchString(d.Name()) {
			found = append(found, path)
			return nil
		}

		return nil
	})

	return found, err
}

// ---
