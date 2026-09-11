package main

import (
	"archive/zip"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"github.com/rs/zerolog/log"
)

// --- QIDs

const scanQID = "scan"
const configProcessQID = "config_process"
const comicInfoUpsertQID = "comic_info_upsert"

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
	wd, err := os.Getwd()
	if err != nil {
		panic(fmt.Errorf("could not determine working dir: %w", err))
	}

	// Define a few "global" queues.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	cancel := DefineQueues(ctx,
		QueueDefinition{scanQID, 1},
		QueueDefinition{configProcessQID, 10},
		QueueDefinition{comicInfoUpsertQID, 10},
	)

	// Enqueue the scan job
	Enqueue(scanQID, scanJob(wd))
	cancel()

	// Wait for all queues
	QueuesWait()
}

// Scan for webcomic2cbz.yml files and enqueue them.
func scanJob(root string) Job {
	return func(ctx context.Context) {
		// Enqueue any found config files
		log.Info().Str("root", root).Msg("scanning for config...")
		found, err := findFilesRecursive(root, configRegex)
		if err != nil {
			log.Err(err).Str("root", root).Msg("error encountered whilst scanning - continuing.")
		}
		for _, path := range found {
			Enqueue(configProcessQID, configProcessJob(path))
		}
	}
}

// Responsible for reading and parsing a found config file, and looking for relevant cbz
// files in the enclosed directory.
func configProcessJob(cfgPath string) Job {
	return func(ctx context.Context) {
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
		var all []os.DirEntry
		lockErr := WithRLockFile(cbzDirLockKey(cfgDir), 1*time.Minute, func() {
			all, err = os.ReadDir(cfgDir)
		})
		if lockErr != nil {
			log.Err(lockErr).Str("path", cfgPath).Msg("could not acquire cbzDirs lock - continuing.")
		}
		if err != nil {
			log.Err(err).Str("path", cfgPath).Msg("could not read cfg dir - continuing.")
		}
		allCBZ, err := filterMatchingCBZ(cfgPath, cfg, all)
		if err != nil {
			return
		}
		Enqueue(comicInfoUpsertQID, comicInfoUpsertJob(allCBZ, cfg))
	}
}

// Update the ComicInfo.xml for the existing CBZs associated to a webcomic, and also
// track what webcomics we actually have.
func comicInfoUpsertJob(cbzPaths map[string]int, cfg Config) Job {
	return func(ctx context.Context) {
		var allIdx []int
		for cbzPath, number := range cbzPaths {
			cbzIdx, err := comicInfoUpsertSingle(cbzPath, number, cfg)
			if err != nil {
				log.Err(err).Str("path", cbzPath).Msg("could not read cbz - skipping webcomic.")
				return
			}
			allIdx = append(allIdx, cbzIdx...)
		}
	}
}

// --- Helper funcs

func comicInfoUpsertSingle(cbzPath string, number int, cfg Config) ([]int, error) {
	// Read the cbz
	var imageIdx []int
	var err error
	lockErr := WithRLockFile(cbzFileLockKey(cbzPath), 1*time.Minute, func() {
		imageIdx, err = readCBZImageIdx(cbzPath)
		if err != nil {
			return
		}
	})
	if lockErr != nil {
		return nil, err
	}
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
		idxStr := idxImageRegex.FindString(f.Name)
		idx, err := strconv.Atoi(idxStr)
		if err != nil {
			return nil, err
		}
		found = append(found, idx)
	}

	return found, nil
}

func filterMatchingCBZ(cfgPath string, cfg Config, all []os.DirEntry) (map[string]int, error) {
	// Generate hypothetical titles up to 999 volumes, as a match set.
	matchSet := make(map[string]int, 999)
	for i := 1; i < 1000; i++ {
		name, err := cbzTitle(cfgPath, cfg, i)
		if err != nil {
			return nil, err
		}
		matchSet[name] = i
	}

	// See which match
	keep := make(map[string]int)
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
		keep[path.Join(cfgPath, name)] = number
	}

	return keep, nil
}

func cbzTitle(cfgPath string, cfg Config, volume int) (string, error) {
	// TODO: Could possibly cache built templates, if needed.
	// Define the template
	tmpl, err := template.New("cbz").Parse(cfg.Cbz.NamingTemplate)
	if err != nil {
		log.Err(err).
			Str("cfg_path", cfgPath).
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
			Str("cfg_path", cfgPath).
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
