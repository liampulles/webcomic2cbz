package main

import (
	"archive/zip"
	"encoding/xml"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/template"

	szip "github.com/STARRY-S/zip"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
)

// TODO: Create some common templating helpers
// TODO: Common zip operations
// TODO: Start with Number 0?
// TODO: Consistent filename formatting helpers

// --- Regex

var configRegex = regexp.MustCompile(`^webcomic2cbz\.(?:yml|yaml)$`)
var cbzRegex = regexp.MustCompile(`^.*\.cbz$`)
var idxImageRegex = regexp.MustCompile(`^(\d+)\.(?:jpg|png|jpeg|gif|bmp|webp)$`)
var imageRegex = regexp.MustCompile(`^.*\.(?:jpg|png|jpeg|gif|bmp|webp)$`)

// --- Main logic

func main() {
	log.Logger = log.Output(zerolog.ConsoleWriter{Out: os.Stderr})

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

		// Scan for matching CBZ files and upsert ComicInfo for them.
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
		// track what webcomics we actually have. If the existing CBZs don't line up with
		// our chunking config, then extract the images and delete them (they'll be recreated
		// later).
		var allIdx []int
		var keepCbz []cbzItem
		for _, cbz := range cbzItems {
			imgIdxs, ignoreCBZ, err := existingCBZProcess(cbz.Path, cbz.Number, cfg)
			if err != nil {
				log.Err(err).Str("path", cfgPath).Msg("could not read a cbz - skipping.")
			}
			if !ignoreCBZ {
				keepCbz = append(keepCbz, cbz)
				allIdx = append(allIdx, imgIdxs...)
			}
		}
		cbzItems = keepCbz

		// Source missing webcomics
		sourced := SourceWebcomics(cfg, allIdx, cfgDir)

		// We can short circuit now if we got nothing
		if len(sourced) == 0 {
			log.Info().
				Str("dir", cfgDir).
				Msgf("no new webcomics found for %s - shorting", cfg.Title)
			return
		}

		// Update existing cbz
		handled := updateExistingCBZs(cfg, cbzItems, sourced)

		// Create new cbz
		createNewCBZs(cfgDir, cfg, handled, sourced)
	}
}

// --- Helper funcs

func existingCBZProcess(cbzPath string, number int, cfg Config) ([]int, bool, error) {
	// Read the cbz
	imageIdx, others, err := readCBZImageIdx(cbzPath)
	if err != nil {
		return nil, true, err
	}

	// See if the found idx line up with our expectations. And if there are other images,
	// then we definitely want to trigger.
	start, end := issueIdxRange(number, cfg)
	var problemIdx []int
	for _, idx := range imageIdx {
		if idx >= start && idx <= end {
			continue
		}
		problemIdx = append(problemIdx, idx)
		// One is enough.
		break
	}
	if len(others) > 0 || len(problemIdx) > 0 {
		// Looks like at least one idx is unexpected - then
		// we'll extract the images and delete the cbz, and give
		// an ImgSource the possibility to use those images
		// to build a proper cbz.
		extractCBZIdxImages(cbzPath)
		err = os.Remove(cbzPath)
		if err != nil {
			log.Err(err).
				Str("path", cbzPath).
				Msg("could not delete the cbz - since this means we cannot build a consistent set, we must panic")
			panic(err)
		}
		// We report no files existing in this cbz - because it does not exist anymore.
		return nil, true, nil
	}

	// Update ComicInfo
	info := CreateComicInfo(cfg, number)
	err = UpsertComicInfo(cbzPath, info)
	if err != nil {
		return nil, false, err
	}

	return imageIdx, false, nil
}

func updateExistingCBZs(cfg Config, cbzItems []cbzItem, sourced []Sourced) []cbzItem {
	var pertinent []cbzItem
	for _, cbzItem := range cbzItems {
		pertaining, err := updateExistingCBZ(cfg, cbzItem, sourced)
		if pertaining {
			pertinent = append(pertinent, cbzItem)
		}
		if err != nil {
			log.Err(err).
				Str("cbz", cbzItem.Path).
				Msg("could not update this cbz, will continue to the next one")
		}
	}
	return pertinent
}

func updateExistingCBZ(cfg Config, cbzItem cbzItem, sourced []Sourced) (bool, error) {
	// What range of webcomics applies to this cbz?
	start, end := issueIdxRange(cbzItem.Number, cfg)

	// Filter sourced images by that
	var filtered []Sourced
	for _, item := range sourced {
		if item.Idx < start || item.Idx > end {
			continue
		}
		filtered = append(filtered, item)
	}

	// Short circuit if nothing to do here.
	if len(filtered) == 0 {
		return false, nil
	}

	// Open the zip for writing
	log.Info().Str("path", cbzItem.Path).Msg("updating with sourced images")
	f, err := os.OpenFile(cbzItem.Path, os.O_RDWR, 0)
	if err != nil {
		return true, err
	}
	defer f.Close()
	zu, err := szip.NewUpdater(f)
	if err != nil {
		return true, err
	}
	defer zu.Close()

	// Append each filtered image
	for _, item := range filtered {
		// What filename to use? We're putting it directly at the top level.
		ext := filepath.Ext(item.Path)
		base := fmt.Sprintf("%d%s", item.Idx, ext)

		// Get a reference to the embedded file in the zip
		ef, err := zu.Append(base, szip.APPEND_MODE_OVERWRITE)
		if err != nil {
			log.Err(err).
				Str("cbz", cbzItem.Path).
				Msgf("could not write image %d, will skip this one", item.Idx)
			continue
		}

		// Open the sourced file on disk
		inf, err := os.Open(item.Path)
		if err != nil {
			log.Err(err).
				Str("path", item.Path).
				Msg("could not open sourced image, will skip this one")
			continue
		}

		// Copy the bytes over
		_, err = io.Copy(ef, inf)
		if err != nil {
			log.Err(err).
				Str("cbz", cbzItem.Path).
				Str("img_path", item.Path).
				Msg("could not copy bytes over, will skip this one")
			continue
		}

		// Remove the sourced image
		inf.Close()
		err = os.Remove(item.Path)
		if err != nil {
			log.Err(err).
				Str("img_path", item.Path).
				Msg("could not remove the sourced image, will leave it as is")
			continue
		}
	}

	return true, nil
}

type numberToBuild struct {
	Number  int
	Sources []Sourced
}

func createNewCBZs(dir string, cfg Config, handled []cbzItem, sourced []Sourced) {
	// Which issue numbers have been handled
	handledNumbers := make(map[int]bool)
	for _, item := range handled {
		handledNumbers[item.Number] = true
	}

	// Given our sourced images, which numbers can we then make?
	numbersToBuildMap := make(map[int]numberToBuild)
	for _, item := range sourced {
		number := issueNumberForIdx(item.Idx, cfg)
		if handledNumbers[number] {
			continue
		}
		prospect := numbersToBuildMap[number]
		prospect.Number = number
		prospect.Sources = append(prospect.Sources, item)
		numbersToBuildMap[number] = prospect
	}

	// Go and make them
	for _, item := range numbersToBuildMap {
		err := createNewCBZ(dir, cfg, item)
		if err != nil {
			log.Err(err).
				Int("number", item.Number).
				Msg("could not create new cbz - continuing")
		}
	}
}

func createNewCBZ(dir string, cfg Config, item numberToBuild) error {
	// What is the path?
	name, err := cbzTitle(dir, cfg, item.Number)
	if err != nil {
		return err
	}
	path := filepath.Join(dir, name) + ".cbz"

	// Open file for writing
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zipW := zip.NewWriter(f)
	defer zipW.Close()

	// Write a ComicInfo.xml
	info := CreateComicInfo(cfg, item.Number)
	infoF, err := zipW.Create("ComicInfo.xml")
	if err != nil {
		return err
	}
	xmlE := xml.NewEncoder(infoF)
	xmlE.Indent("", "  ")
	err = xmlE.Encode(info)
	if err != nil {
		return err
	}

	// Write each sourced image
	for _, item := range item.Sources {
		// What filename to use? We're putting it directly at the top level.
		ext := filepath.Ext(item.Path)
		base := fmt.Sprintf("%d%s", item.Idx, ext)

		// Open the sourced image
		inf, err := os.Open(item.Path)
		if err != nil {
			log.Err(err).
				Str("path", item.Path).
				Msg("could not open sourced image, will skip this one")
			continue
		}

		// Open an embedded zip file
		zipF, err := zipW.Create(base)
		if err != nil {
			log.Err(err).
				Str("cbz", path).
				Msgf("could not write image %d, will skip this one", item.Idx)
			continue
		}

		// Copy the bytes over
		_, err = io.Copy(zipF, inf)
		if err != nil {
			log.Err(err).
				Str("cbz", path).
				Str("img_path", item.Path).
				Msg("could not copy bytes over, will skip this one")
			continue
		}

		// Remove the sourced image
		inf.Close()
		err = os.Remove(item.Path)
		if err != nil {
			log.Err(err).
				Str("img_path", item.Path).
				Msg("could not remove the sourced image, will leave it as is")
			continue
		}
	}

	log.Info().Str("path", path).Msg("finishing up new cbz...")
	return nil
}

// Inclusive on both sides
func issueIdxRange(number int, cfg Config) (int, int) {
	chunk := cfg.Cbz.ChunkSize
	start := ((number - 1) * chunk) + 1
	end := number * chunk
	return start, end
}

func issueNumberForIdx(idx int, cfg Config) int {
	chunk := cfg.Cbz.ChunkSize
	return ((idx - 1) / chunk) + 1
}

func extractCBZIdxImages(cbzPath string) {
	dir := filepath.Dir(cbzPath)

	// Open the CBZ file
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		log.Err(err).
			Str("path", cbzPath).
			Msg("could not open the cbz file - won't be able to save any images from it")
		return
	}
	defer zr.Close()

	// Find embedded images, with an idx like title
	for _, f := range zr.File {
		// Filter out irrelevant listings
		if f.FileInfo().IsDir() {
			continue
		}
		if !idxImageRegex.MatchString(f.Name) {
			continue
		}

		// "Open" the image in the zip
		zf, err := f.Open()
		if err != nil {
			log.Err(err).
				Str("path", cbzPath).
				Msg("could not open an image in the zip - will skip this one")
			continue
		}
		defer zf.Close()

		// Prep an output image
		outPath := filepath.Join(dir, f.Name)
		of, err := os.Create(outPath)
		if err != nil {
			log.Err(err).
				Str("path", outPath).
				Msg("could not create a file to extract an image - will skip this one")
			continue
		}
		defer of.Close()

		// Write to it
		_, err = io.Copy(of, zf)
		if err != nil {
			log.Err(err).
				Str("path", outPath).
				Msg("could not write image data - will skip this one")
			continue
		}
	}
}

// Locking is up to the caller.
func readCBZImageIdx(cbzPath string) ([]int, []string, error) {
	// Open the CBZ file
	zr, err := zip.OpenReader(cbzPath)
	if err != nil {
		return nil, nil, err
	}
	defer zr.Close()

	// Find embedded images, with an idx like title. And note others.
	var found []int
	var others []string
	for _, f := range zr.File {
		// Filter out irrelevant listings
		if f.FileInfo().IsDir() {
			continue
		}
		if !idxImageRegex.MatchString(f.Name) {
			// Is it perhaps an other image though?
			if imageRegex.MatchString(f.Name) {
				others = append(others, f.Name)
			}

			continue
		}

		// Get the idx part
		idxStr := idxImageRegex.FindStringSubmatch(f.Name)
		idx, err := strconv.Atoi(idxStr[1])
		if err != nil {
			return nil, nil, err
		}
		found = append(found, idx)
	}

	return found, others, nil
}

type cbzItem struct {
	Path   string
	Number int
}

func filterMatchingCBZ(cfgDir string, cfg Config, all []os.DirEntry) ([]cbzItem, error) {
	// Generate hypothetical titles up to 999 numbers, as a match set.
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

func cbzTitle(cfgDir string, cfg Config, number int) (string, error) {
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
		Number int
	}{
		Title:  cfg.Title,
		Number: number,
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
